package env

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/ports"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vault"
)

const (
	mainCommit    = "1111111111111111111111111111111111111111"
	featureCommit = "2222222222222222222222222222222222222222"
)

type allowAll struct{}

func (allowAll) CanBind(int) bool { return true }

type harness struct {
	t       *testing.T
	m       *Manager
	store   *fakeStore
	driver  *fakeDriver
	repo    *fakeRepo
	data    string
	run     string
	cfg     *config.App
	loadErr error
	out     bytes.Buffer

	daemons map[*Manager]context.CancelFunc
}

func (h *harness) daemon(m *Manager) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m.Background = ctx
	if h.daemons == nil {
		h.daemons = map[*Manager]context.CancelFunc{}
	}
	h.daemons[m] = cancel
	h.t.Cleanup(cancel)
	return m
}

func (h *harness) stop(m *Manager) {
	cancel, ok := h.daemons[m]
	if !ok {
		return
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		watching := len(m.watches)
		m.mu.Unlock()
		if watching == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("a daemon that was stopped is still watching %d deploy(s)", len(m.watches))
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	data := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(data); err == nil {
		data = resolved
	}
	store := newStore(state.App{Name: "shop", RepoPath: RepoPath(data, "shop"), DefaultBranch: "main"})
	driver := newDriver()
	repo := newRepo("main", map[string]string{"main": mainCommit, "feature": featureCommit})
	h := &harness{t: t, store: store, driver: driver, repo: repo,
		data: data, cfg: sampleConfig()}

	run := t.TempDir()
	h.run = run
	m := New(store, driver, repo, ports.New(store, allowAll{}), nil, Dirs{Data: data, User: "caramelo", Run: run})

	cipher, _, err := vault.CipherFromKeyFile(vault.KeyPath(t.TempDir()))
	if err != nil {
		t.Fatalf("build the vault cipher: %v", err)
	}
	v, err := vault.New(vault.Options{Rows: store, Cipher: cipher, Identity: IdentityFrom})
	if err != nil {
		t.Fatalf("build the vault: %v", err)
	}
	m.Secrets = v
	m.Version = "0.0.1-test"
	m.ReadyInterval = time.Millisecond
	m.Timeout = 30 * time.Millisecond
	m.LoadConfig = func(path string) (*config.App, error) {
		if h.loadErr != nil {
			return nil, h.loadErr
		}
		cfg := *h.cfg
		return &cfg, nil
	}
	h.m = h.daemon(m)
	return h
}

func (h *harness) create(req CreateRequest) (*Env, error) {
	h.t.Helper()
	return h.m.Create(context.Background(), req, &h.out)
}

func (h *harness) mustCreate(req CreateRequest) *Env {
	h.t.Helper()
	e, err := h.create(req)
	if err != nil {
		h.t.Fatalf("Create(%s): %v\n%s", req.Name, err, h.out.String())
	}
	return e
}

func (h *harness) status(name string) string {
	h.t.Helper()
	rec, err := h.store.Env(context.Background(), "shop", name)
	if err != nil {
		h.t.Fatalf("read env %q: %v", name, err)
	}
	return rec.Status
}

func TestCreateTwoEnvsGetDisjointBlocksAndDistinctVars(t *testing.T) {
	h := newHarness(t)
	ctx := WithIdentity(context.Background(), "laptop")

	x, err := h.m.Create(ctx, CreateRequest{App: "shop", Name: "feat-x", From: "main"}, &h.out)
	if err != nil {
		t.Fatalf("Create feat-x: %v\n%s", err, h.out.String())
	}
	y := h.mustCreate(CreateRequest{App: "shop", Name: "feat-y", From: "feature"})

	if x.PortBase != ports.Base || y.PortBase != ports.Base+ports.BlockSize {
		t.Fatalf("port bases = %d and %d, want %d and %d", x.PortBase, y.PortBase, ports.Base, ports.Base+ports.BlockSize)
	}
	if x.Status != StatusReady || y.Status != StatusReady {
		t.Fatalf("statuses = %q and %q, want ready", x.Status, y.Status)
	}
	if x.Commit != mainCommit || y.Commit != featureCommit {
		t.Errorf("commits = %q and %q, want %q and %q", x.Commit, y.Commit, mainCommit, featureCommit)
	}
	if x.CreatedBy != "laptop" {
		t.Errorf("created_by = %q, want the session identity", x.CreatedBy)
	}
	if x.Worktree != WorktreePath(h.data, "shop", "feat-x") {
		t.Errorf("worktree = %q, want it under the data dir", x.Worktree)
	}

	want := map[string]string{
		"PORT":         fmt.Sprint(ports.Base),
		"CARAMELO_APP": "shop",
		"CARAMELO_ENV": "feat-x",
		"DATABASE_URL": fmt.Sprintf("postgres://postgres:caramelo@127.0.0.1:%d/postgres", ports.Base+1),
		"REDIS_URL":    fmt.Sprintf("redis://127.0.0.1:%d", ports.Base+2),
		"SELF":         fmt.Sprintf("shop/feat-x on %d", ports.Base),
	}
	for k, v := range want {
		if x.Vars[k] != v {
			t.Errorf("feat-x %s = %q, want %q", k, x.Vars[k], v)
		}
	}
	if x.Vars["DATABASE_URL"] == y.Vars["DATABASE_URL"] {
		t.Errorf("both envs point at the same database: %q", x.Vars["DATABASE_URL"])
	}

	wantContainers := []string{
		"caramelo-shop-feat-x-cache", "caramelo-shop-feat-x-db",
		"caramelo-shop-feat-y-cache", "caramelo-shop-feat-y-db",
	}
	if got := h.driver.containerNames(); !equal(got, wantContainers) {
		t.Errorf("containers = %v, want %v", got, wantContainers)
	}

	wantVolumes := []string{
		"caramelo-shop-feat-x--cache", "caramelo-shop-feat-x-cache", "caramelo-shop-feat-x-db",
		"caramelo-shop-feat-y--cache", "caramelo-shop-feat-y-cache", "caramelo-shop-feat-y-db",
	}
	if got := h.driver.volumeNames(); !equal(got, wantVolumes) {
		t.Errorf("volumes = %v, want %v", got, wantVolumes)
	}
	spec, ok := h.driver.spec("caramelo-shop-feat-x-db")
	if !ok {
		t.Fatal("the db container was never run")
	}
	if len(spec.Publish) != 1 || spec.Publish[0].HostIP != "127.0.0.1" || spec.Publish[0].HostPort != ports.Base+1 || spec.Publish[0].ContainerPort != 5432 {
		t.Errorf("publish = %+v, want 127.0.0.1:%d:5432", spec.Publish, ports.Base+1)
	}
	if spec.Restart != "unless-stopped" {
		t.Errorf("restart = %q, want unless-stopped", spec.Restart)
	}
	if len(spec.Volumes) != 1 || spec.Volumes[0].Volume != "caramelo-shop-feat-x-db" || spec.Volumes[0].Path != "/var/lib/postgresql/data" {
		t.Errorf("volumes = %+v, want the named volume at the dep's data path", spec.Volumes)
	}
	for k, v := range map[string]string{LabelApp: "shop", LabelEnv: "feat-x", LabelDep: "db", LabelVersion: "0.0.1-test"} {
		if spec.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, spec.Labels[k], v)
		}
	}

	if spec.Env["POSTGRES_DB"] != "shop_feat-x" {
		t.Errorf("db POSTGRES_DB = %q, want the expanded %q", spec.Env["POSTGRES_DB"], "shop_feat-x")
	}
	if spec.Env["POSTGRES_PASSWORD"] != "caramelo" {
		t.Errorf("db POSTGRES_PASSWORD = %q, want caramelo", spec.Env["POSTGRES_PASSWORD"])
	}

	if got := h.store.resourceNames(x.ID, state.ResourceContainer); len(got) != 2 {
		t.Errorf("recorded containers = %v, want two", got)
	}
	if got := h.store.resourceNames(x.ID, state.ResourceWorktree); len(got) != 1 {
		t.Errorf("recorded worktrees = %v, want one", got)
	}
	if got := h.store.resourceNames(x.ID, state.ResourceBranch); !equal(got, []string{"feat-x"}) {
		t.Errorf("recorded branches = %v, want the branch create made", got)
	}

	if _, err := os.Stat(x.Worktree); err != nil {
		t.Errorf("worktree not on disk: %v", err)
	}
	for _, line := range []string{"[changed] worktree", "[changed] container", "[ok] ready: db", "[changed] env"} {
		if !strings.Contains(h.out.String(), line) {
			t.Errorf("progress does not mention %q:\n%s", line, h.out.String())
		}
	}

	envs, err := h.m.List(context.Background(), "shop")
	if err != nil || len(envs) != 2 {
		t.Fatalf("List = %d envs, %v; want 2, nil", len(envs), err)
	}
}

func TestCreateNoDepsSkipsContainers(t *testing.T) {
	h := newHarness(t)
	e := h.mustCreate(CreateRequest{App: "shop", Name: "bare", From: "main", NoDeps: true})
	if len(h.driver.containerNames()) != 0 {
		t.Errorf("containers = %v, want none", h.driver.containerNames())
	}
	if e.Vars["PORT"] == "" || e.Vars["DATABASE_URL"] == "" {
		t.Errorf("variables are still computed without deps: %v", e.Vars)
	}
	if !strings.Contains(h.out.String(), "[skipped] deps") {
		t.Errorf("progress does not say deps were skipped:\n%s", h.out.String())
	}
}

func TestCreateWithoutConfigIsJustAWorktreeAndAPort(t *testing.T) {
	h := newHarness(t)
	h.loadErr = fmt.Errorf("open caramelo.yaml: %w", os.ErrNotExist)

	e := h.mustCreate(CreateRequest{App: "shop", Name: "plain", From: "main"})
	if len(h.driver.containerNames()) != 0 {
		t.Errorf("containers = %v, want none", h.driver.containerNames())
	}
	if got, want := e.Vars["PORT"], fmt.Sprint(ports.Base); got != want {
		t.Errorf("PORT = %q, want %q", got, want)
	}
	if !strings.Contains(h.out.String(), "[skipped] config") {
		t.Errorf("progress does not say there was no config:\n%s", h.out.String())
	}
}

func TestCreateBranchRules(t *testing.T) {
	t.Run("existing branch is built on", func(t *testing.T) {
		h := newHarness(t)
		h.repo.branches["feat-x"] = featureCommit
		e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
		if e.Commit != featureCommit {
			t.Errorf("commit = %q, want the branch's own tip %q", e.Commit, featureCommit)
		}
		for _, c := range h.repo.calls {
			if strings.HasPrefix(c, "create-branch") {
				t.Errorf("an existing branch was recreated: %v", h.repo.calls)
			}
		}
	})

	t.Run("--from on an existing branch is refused", func(t *testing.T) {
		h := newHarness(t)
		h.repo.branches["feat-x"] = featureCommit
		_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", From: "main"})
		if err == nil || !strings.Contains(err.Error(), "--reset") {
			t.Fatalf("Create = %v, want a refusal pointing at --reset", err)
		}
		if len(h.store.envs) != 0 {
			t.Errorf("a row was written for a create that never started: %+v", h.store.envs)
		}
	})

	t.Run("--reset moves it", func(t *testing.T) {
		h := newHarness(t)
		h.repo.branches["feat-x"] = featureCommit
		e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main", Reset: true})
		if e.Commit != mainCommit {
			t.Errorf("commit = %q, want %q", e.Commit, mainCommit)
		}

		if !contains(h.repo.calls, "delete-branch feat-x") ||
			!contains(h.repo.calls, "create-branch feat-x "+mainCommit) {
			t.Errorf("branch was not moved: %v", h.repo.calls)
		}
	})

	t.Run("--reset of a branch onto itself", func(t *testing.T) {

		h := newHarness(t)
		h.repo.branches["oom"] = featureCommit
		e := h.mustCreate(CreateRequest{App: "shop", Name: "oom", From: "oom", Reset: true})
		if e.Commit != featureCommit {
			t.Errorf("commit = %q, want %q", e.Commit, featureCommit)
		}
		if !contains(h.repo.calls, "create-branch oom "+featureCommit) {
			t.Errorf("branch was not recreated from the commit: %v", h.repo.calls)
		}
	})

	t.Run("--reset without --from is refused", func(t *testing.T) {
		h := newHarness(t)
		h.repo.branches["feat-x"] = featureCommit
		if _, err := h.create(CreateRequest{App: "shop", Name: "feat-x", Reset: true}); err == nil {
			t.Fatal("Create = nil, want an error")
		}
	})

	t.Run("no --from uses the app's default branch", func(t *testing.T) {
		h := newHarness(t)
		e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
		if e.Commit != mainCommit {
			t.Errorf("commit = %q, want main's %q", e.Commit, mainCommit)
		}
		if !contains(h.repo.calls, "create-branch feat-x "+mainCommit) {
			t.Errorf("branch was not created from main's commit: %v", h.repo.calls)
		}
	})

	t.Run("unknown ref", func(t *testing.T) {
		h := newHarness(t)
		_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", From: "nope"})
		if err == nil || !strings.Contains(err.Error(), "no such ref") {
			t.Fatalf("Create = %v, want \"no such ref\"", err)
		}
	})
}

func TestCreateKeepsTheManagedVariables(t *testing.T) {

	h := newHarness(t)
	h.cfg.Env["PORT"] = "3000"
	h.cfg.Env["CARAMELO_APP"] = "not-shop"
	h.cfg.Env["CARAMELO_ENV"] = "not-feat-x"

	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})
	if got, want := e.Vars["PORT"], strconv.Itoa(e.PortBase); got != want {
		t.Errorf("PORT = %q, want the allocated %q", got, want)
	}
	if e.Vars["CARAMELO_APP"] != "shop" || e.Vars["CARAMELO_ENV"] != "feat-x" {
		t.Errorf("CARAMELO_APP/CARAMELO_ENV = %q/%q, want shop/feat-x",
			e.Vars["CARAMELO_APP"], e.Vars["CARAMELO_ENV"])
	}
}

func TestCreateRefusesUnknownAppAndBadNames(t *testing.T) {
	h := newHarness(t)
	if _, err := h.create(CreateRequest{App: "ghost", Name: "feat-x"}); err == nil || !strings.Contains(err.Error(), "no such app") {
		t.Fatalf("Create = %v, want \"no such app\"", err)
	}
	if _, err := h.create(CreateRequest{App: "shop", Name: "Feat X"}); err == nil || !strings.Contains(err.Error(), "invalid env name") {
		t.Fatalf("Create = %v, want an invalid-name error", err)
	}
}

func TestCreateRefusesAnExistingEnv(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})
	_, err := h.create(CreateRequest{App: "shop", Name: "feat-x"})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Create = %v, want \"already exists\"", err)
	}
}

func TestCreateRetriesTheNextBlockWhenTheRowLosesTheRace(t *testing.T) {
	h := newHarness(t)
	h.store.createErr = state.ErrExists
	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})
	if e.Status != StatusReady {
		t.Fatalf("status = %q, want ready", e.Status)
	}
}

func TestCreateFailsOnTheSecondContainerAndDestroyCleansUp(t *testing.T) {
	h := newHarness(t)
	h.driver.RunErr = func(spec runtime.ContainerSpec) error {
		if strings.HasSuffix(spec.Name, "-cache") {
			return errors.New("no such image")
		}
		return nil
	}

	_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", From: "main"})
	if err == nil || !strings.Contains(err.Error(), "caramelo-shop-feat-x-cache") {
		t.Fatalf("Create = %v, want the failing container named", err)
	}
	if got := h.status("feat-x"); got != state.EnvFailed {
		t.Fatalf("status = %q, want failed", got)
	}

	rec, _ := h.store.Env(context.Background(), "shop", "feat-x")
	if got := h.store.resourceNames(rec.ID, state.ResourceContainer); !equal(got, []string{"caramelo-shop-feat-x-db"}) {
		t.Errorf("recorded containers = %v, want the one that did start", got)
	}
	if got := h.store.resourceNames(rec.ID, state.ResourceVolume); len(got) != 2 {
		t.Errorf("recorded volumes = %v, want both (the second dep's volume exists too)", got)
	}

	if !contains(h.driver.containerNames(), "caramelo-shop-feat-x-cache") {
		t.Fatal("the fake driver was expected to leave the failed container behind")
	}

	h.out.Reset()
	if err := h.m.Destroy(context.Background(), DestroyRequest{App: "shop", Name: "feat-x", DeleteBranch: false}, &h.out); err != nil {
		t.Fatalf("Destroy: %v\n%s", err, h.out.String())
	}
	if got := h.driver.containerNames(); len(got) != 0 {
		t.Errorf("containers left = %v, want none", got)
	}
	if got := h.driver.volumeNames(); len(got) != 0 {
		t.Errorf("volumes left = %v, want none", got)
	}
	if _, err := h.store.Env(context.Background(), "shop", "feat-x"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("env row still there: %v", err)
	}
	if _, err := os.Stat(EnvDir(h.data, "shop", "feat-x")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("env directory still there: %v", err)
	}
	if _, ok := h.repo.branches["feat-x"]; !ok {
		t.Error("destroy deleted the branch without being asked")
	}
}

func TestCreateFailsWhenADependencyNeverBecomesReady(t *testing.T) {
	h := newHarness(t)
	h.driver.logs["caramelo-shop-feat-x-db"] = "FATAL: database files are incompatible\n"
	h.driver.ExecResult = func(name string, argv []string) (runner.Result, error) {
		return runner.Result{ExitCode: 1, Stderr: "pg_isready: no response\n"}, nil
	}

	start := time.Now()
	_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", From: "main", Timeout: 20 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("Create = %v, want a readiness timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the readiness wait ignored the timeout (%s)", elapsed)
	}
	if got := h.status("feat-x"); got != state.EnvFailed {
		t.Fatalf("status = %q, want failed", got)
	}

	if !contains(h.driver.containerNames(), "caramelo-shop-feat-x-db") {
		t.Error("the container was cleaned up; it should be left for inspection")
	}
	if !strings.Contains(h.out.String(), "database files are incompatible") {
		t.Errorf("the container's logs were not shown:\n%s", h.out.String())
	}
}

func TestCreateOnAFailedEnvCleansUpAndRetries(t *testing.T) {
	h := newHarness(t)
	h.driver.ExecResult = func(name string, argv []string) (runner.Result, error) {
		return runner.Result{ExitCode: 1}, nil
	}
	if _, err := h.create(CreateRequest{App: "shop", Name: "feat-x", From: "main"}); err == nil {
		t.Fatal("Create = nil, want a readiness failure")
	}
	failed, _ := h.store.Env(context.Background(), "shop", "feat-x")

	h.driver.ExecResult = nil
	h.out.Reset()
	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	if e.Status != StatusReady {
		t.Fatalf("status = %q, want ready", e.Status)
	}
	if e.ID == failed.ID {
		t.Error("the failed row was reused instead of being destroyed and rewritten")
	}
	if !strings.Contains(h.out.String(), "[changed] retry") {
		t.Errorf("progress does not mention the clean-up:\n%s", h.out.String())
	}
	if got := len(h.driver.containerNames()); got != 2 {
		t.Errorf("containers = %d, want the two of the new env", got)
	}
}

func TestCreateRetryAcceptsTheSameCommandLine(t *testing.T) {
	h := newHarness(t)
	h.driver.ExecResult = func(name string, argv []string) (runner.Result, error) {
		return runner.Result{ExitCode: 1}, nil
	}
	if _, err := h.create(CreateRequest{App: "shop", Name: "feat-x", From: "main"}); err == nil {
		t.Fatal("Create = nil, want a readiness failure")
	}
	if _, ok := h.repo.branches["feat-x"]; !ok {
		t.Fatal("destroy deleted the branch of the failed env; it must survive")
	}

	h.driver.ExecResult = nil
	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})
	if e.Status != StatusReady {
		t.Fatalf("status = %q, want ready", e.Status)
	}
	if e.Commit != mainCommit {
		t.Errorf("commit = %q, want main's %q", e.Commit, mainCommit)
	}
}

func TestDestroyIsIdempotentAndSweepsByLabel(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})

	if err := h.m.Destroy(context.Background(), DestroyRequest{App: "shop", Name: "feat-x", DeleteBranch: false}, &h.out); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := h.m.Destroy(context.Background(), DestroyRequest{App: "shop", Name: "feat-x", DeleteBranch: false}, &h.out); err != nil {
		t.Fatalf("second Destroy: %v", err)
	}
	if !strings.Contains(h.out.String(), "already gone") {
		t.Errorf("the second destroy did not say the env was gone:\n%s", h.out.String())
	}
	if !contains(h.repo.calls, "worktree-prune") {
		t.Errorf("worktrees were never pruned: %v", h.repo.calls)
	}
}

func TestDestroyCleansLeftoversOfAnEnvWithNoRow(t *testing.T) {
	h := newHarness(t)
	labels := Labels("shop", "ghost", "db", "0.0.1-test")
	ctx := context.Background()
	if _, err := h.driver.Run(ctx, runtime.ContainerSpec{Name: ContainerName("shop", "ghost", "db"), Image: "postgres:16-alpine", Labels: labels}); err != nil {
		t.Fatalf("seed container: %v", err)
	}
	if err := h.driver.CreateVolume(ctx, VolumeName("shop", "ghost", "db"), labels); err != nil {
		t.Fatalf("seed volume: %v", err)
	}

	if err := h.m.Destroy(ctx, DestroyRequest{App: "shop", Name: "ghost", DeleteBranch: false}, &h.out); err != nil {
		t.Fatalf("Destroy: %v\n%s", err, h.out.String())
	}
	if got := h.driver.containerNames(); len(got) != 0 {
		t.Errorf("containers left = %v, want none", got)
	}
	if got := h.driver.volumeNames(); len(got) != 0 {
		t.Errorf("volumes left = %v, want none", got)
	}
}

func TestDestroyDeletesTheBranchOnlyWhenAsked(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})
	if err := h.m.Destroy(context.Background(), DestroyRequest{App: "shop", Name: "feat-x", DeleteBranch: true}, &h.out); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, ok := h.repo.branches["feat-x"]; ok {
		t.Error("branch survived --delete-branch")
	}
}

func TestDestroyKeepsTheRowWhenSomethingCouldNotBeRemoved(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})
	h.driver.ListErr = errors.New("docker daemon is not running")

	err := h.m.Destroy(context.Background(), DestroyRequest{App: "shop", Name: "feat-x", DeleteBranch: false}, &h.out)
	if err == nil || !strings.Contains(err.Error(), "docker daemon") {
		t.Fatalf("Destroy = %v, want the driver error", err)
	}
	if _, err := h.store.Env(context.Background(), "shop", "feat-x"); err != nil {
		t.Errorf("the row was dropped even though clean-up failed: %v", err)
	}
}

func TestShowReportsLiveDependencyState(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})

	if err := h.driver.Remove(ctx, "caramelo-shop-feat-x-cache", true); err != nil {
		t.Fatal(err)
	}
	h.driver.containers["caramelo-shop-feat-x-db"] = runtime.ContainerState{Name: "caramelo-shop-feat-x-db", Status: "exited"}

	e, deps, events, err := h.m.Show(ctx, "shop", "feat-x")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if e.Name != "feat-x" {
		t.Fatalf("Show returned %q", e.Name)
	}
	want := []DepState{
		{Name: "db", Container: "caramelo-shop-feat-x-db", Status: DepExited, Port: ports.Base + 1},
		{Name: "cache", Container: "caramelo-shop-feat-x-cache", Status: DepMissing, Port: ports.Base + 2},
	}
	if len(deps) != len(want) {
		t.Fatalf("deps = %+v, want %+v", deps, want)
	}
	for i := range want {
		if deps[i] != want[i] {
			t.Errorf("dep %d = %+v, want %+v", i, deps[i], want[i])
		}
	}

	if len(events) == 0 || events[0].Action != "env" || !strings.Contains(events[0].Detail, "ready") {
		t.Fatalf("newest event = %+v, want the env ready", events)
	}
	var create, detail bool
	for _, e := range events {
		switch {
		case e.Action == "create":
			create = true
		case e.Action == "container":
			detail = true
		}
	}
	if !create || !detail {
		t.Errorf("events = %+v, want the create trail and its detail lines", events)
	}
}

func TestShowAndExportOnAnUnknownEnv(t *testing.T) {
	h := newHarness(t)
	if _, _, _, err := h.m.Show(context.Background(), "shop", "ghost"); err == nil || !strings.Contains(err.Error(), "no such env") {
		t.Fatalf("Show = %v, want \"no such env\"", err)
	}
	if _, err := h.m.Export(context.Background(), "shop", "ghost", FormatJSON); err == nil {
		t.Fatal("Export = nil, want an error")
	}
}

func TestExec(t *testing.T) {
	h := newHarness(t)
	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})
	ctx := context.Background()

	t.Run("exit codes", func(t *testing.T) {
		cases := []struct {
			argv []string
			want int
		}{
			{[]string{"/bin/sh", "-c", "exit 0"}, 0},
			{[]string{"/bin/sh", "-c", "exit 3"}, 3},
			{[]string{"/bin/sh", "-c", "exit 255"}, 1},
		}
		for _, tc := range cases {
			code, err := h.m.Exec(ctx, ExecRequest{App: "shop", Name: "feat-x", Argv: tc.argv})
			if err != nil {
				t.Fatalf("Exec %v: %v", tc.argv, err)
			}
			if code != tc.want {
				t.Errorf("Exec %v = %d, want %d", tc.argv, code, tc.want)
			}
		}
	})

	t.Run("worktree and variables", func(t *testing.T) {
		var out bytes.Buffer
		code, err := h.m.Exec(ctx, ExecRequest{
			App: "shop", Name: "feat-x",
			Argv:   []string{"/bin/sh", "-c", "pwd; echo $DATABASE_URL; echo $CARAMELO_ENV; echo $PORT"},
			Stdout: &out,
		})
		if err != nil || code != 0 {
			t.Fatalf("Exec = %d, %v", code, err)
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 4 {
			t.Fatalf("output = %q", out.String())
		}
		if !strings.HasSuffix(lines[0], e.Worktree) {
			t.Errorf("cwd = %q, want the worktree %q", lines[0], e.Worktree)
		}
		if lines[1] != e.Vars["DATABASE_URL"] || lines[2] != "feat-x" || lines[3] != fmt.Sprint(ports.Base) {
			t.Errorf("variables = %q, want the env's own %v", out.String(), e.Vars)
		}
	})

	t.Run("stdin and stderr", func(t *testing.T) {
		var out, errb bytes.Buffer
		code, err := h.m.Exec(ctx, ExecRequest{
			App: "shop", Name: "feat-x",
			Argv:   []string{"/bin/sh", "-c", "cat; echo oops >&2"},
			Stdin:  strings.NewReader("hello\n"),
			Stdout: &out, Stderr: &errb,
		})
		if err != nil || code != 0 {
			t.Fatalf("Exec = %d, %v", code, err)
		}
		if out.String() != "hello\n" || !strings.Contains(errb.String(), "oops") {
			t.Errorf("stdout = %q, stderr = %q", out.String(), errb.String())
		}
	})

	t.Run("a child that exits is not held up by an open stdin", func(t *testing.T) {

		stdin := &blockingReader{ch: make(chan struct{})}
		defer stdin.Close()

		type result struct {
			code int
			err  error
		}
		done := make(chan result, 1)
		go func() {
			code, err := h.m.Exec(ctx, ExecRequest{
				App: "shop", Name: "feat-x",
				Argv:  []string{"/bin/sh", "-c", "exit 7"},
				Stdin: stdin,
			})
			done <- result{code, err}
		}()
		select {
		case got := <-done:
			if got.err != nil {
				t.Fatalf("Exec = %v, want the child's own exit code", got.err)
			}
			if got.code != 7 {
				t.Errorf("Exec = %d, want 7", got.code)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("Exec did not return after the child exited: it is waiting on standard input")
		}
	})

	t.Run("a command that cannot run at all is an error", func(t *testing.T) {
		if _, err := h.m.Exec(ctx, ExecRequest{App: "shop", Name: "feat-x", Argv: []string{"/nope/does-not-exist"}}); err == nil {
			t.Fatal("Exec = nil, want an error")
		}
		if _, err := h.m.Exec(ctx, ExecRequest{App: "shop", Name: "feat-x"}); err == nil {
			t.Fatal("Exec with no argv = nil, want an error")
		}
	})

	t.Run("a cancelled context kills the tree", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		done := make(chan int, 1)
		go func() {
			code, _ := h.m.Exec(cctx, ExecRequest{App: "shop", Name: "feat-x", Argv: []string{"/bin/sh", "-c", "sleep 30"}})
			done <- code
		}()
		time.Sleep(50 * time.Millisecond)
		cancel()
		select {
		case code := <-done:
			if code == 0 {
				t.Errorf("Exec = 0 after a cancel, want a non-zero code")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Exec did not return after the context was cancelled")
		}
	})
}

func TestExportFormats(t *testing.T) {
	h := newHarness(t)
	h.cfg.Env["QUOTED"] = "it's here"
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})
	ctx := context.Background()

	shell, err := h.m.Export(ctx, "shop", "feat-x", FormatShell)
	if err != nil {
		t.Fatalf("Export shell: %v", err)
	}
	if !strings.Contains(shell, `export QUOTED='it'\''s here'`) {
		t.Errorf("shell export does not escape the quote:\n%s", shell)
	}
	if !strings.Contains(shell, fmt.Sprintf("export PORT='%d'", ports.Base)) {
		t.Errorf("shell export = %q", shell)
	}
	if !sorted(shell) {
		t.Errorf("shell export is not sorted by key:\n%s", shell)
	}

	dotenv, err := h.m.Export(ctx, "shop", "feat-x", FormatDotenv)
	if err != nil {
		t.Fatalf("Export dotenv: %v", err)
	}
	if !strings.Contains(dotenv, "CARAMELO_ENV=feat-x\n") {
		t.Errorf("dotenv export = %q", dotenv)
	}

	js, err := h.m.Export(ctx, "shop", "feat-x", FormatJSON)
	if err != nil {
		t.Fatalf("Export json: %v", err)
	}
	if !strings.Contains(js, `"CARAMELO_APP": "shop"`) || !strings.HasSuffix(js, "}\n") {
		t.Errorf("json export = %q", js)
	}

	if _, err := h.m.Export(ctx, "shop", "feat-x", ExportFormat("toml")); err == nil {
		t.Fatal("Export toml = nil, want an error")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func sorted(s string) bool {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := 1; i < len(lines); i++ {
		if lines[i-1] > lines[i] {
			return false
		}
	}
	return true
}

func TestConcurrentCreatesGetDisjointBlocks(t *testing.T) {
	h := newHarness(t)
	const n = 4
	envs := make(chan *Env, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e, err := h.m.Create(context.Background(), CreateRequest{App: "shop", Name: fmt.Sprintf("feat-%d", i), From: "main"}, io.Discard)
			if err != nil {
				errs <- err
				return
			}
			envs <- e
		}(i)
	}
	wg.Wait()
	close(envs)
	close(errs)
	for err := range errs {
		t.Fatalf("Create: %v", err)
	}

	seen := map[int]bool{}
	count := 0
	for e := range envs {
		if e.Status != StatusReady {
			t.Errorf("env %s status = %q, want ready", e.Name, e.Status)
		}
		if seen[e.PortBase] {
			t.Errorf("port block %d was handed out twice", e.PortBase)
		}
		seen[e.PortBase] = true
		count++
	}
	if count != n {
		t.Fatalf("created %d envs, want %d", count, n)
	}
}

func TestTwoCreatesOfTheSameNameDoNotRace(t *testing.T) {
	h := newHarness(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.m.Create(context.Background(), CreateRequest{App: "shop", Name: "feat-x", From: "main"}, io.Discard)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	ok, refused := 0, 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case strings.Contains(err.Error(), "already exists"), strings.Contains(err.Error(), "omit --from"):
			refused++
		default:
			t.Fatalf("Create: %v", err)
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("%d created and %d refused, want exactly one of each", ok, refused)
	}
	if got := len(h.driver.containerNames()); got != 2 {
		t.Errorf("containers = %d, want only the winner's two", got)
	}
}

type blockingReader struct{ ch chan struct{} }

func (b *blockingReader) Read([]byte) (int, error) {
	<-b.ch
	return 0, io.EOF
}

func (b *blockingReader) Close() { close(b.ch) }

func TestCreateMakesTheNetworkAndTheCacheVolume(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})

	network := NetworkName("shop", "feat-x")
	if !h.driver.hasNetwork(network) {
		t.Fatalf("network %s was not created", network)
	}
	for _, dep := range []string{"db", "cache"} {
		container := ContainerName("shop", "feat-x", dep)
		aliases, ok := h.driver.networkOf(network, container)
		if !ok {
			t.Errorf("%s is not on %s", container, network)
			continue
		}
		if len(aliases) != 1 || aliases[0] != dep {
			t.Errorf("%s is on %s as %v, want the alias %q", container, network, aliases, dep)
		}
	}
	if vol := CacheVolumeName("shop", "feat-x"); !contains(h.driver.volumeNames(), vol) {
		t.Errorf("cache volume %s was not created; volumes = %v", vol, h.driver.volumeNames())
	}
}

func TestCreateNoDepsStillMakesTheNetwork(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "bare", From: "main", NoDeps: true})

	if network := NetworkName("shop", "bare"); !h.driver.hasNetwork(network) {
		t.Errorf("network %s was not created", network)
	}
	if vol := CacheVolumeName("shop", "bare"); !contains(h.driver.volumeNames(), vol) {
		t.Errorf("cache volume %s was not created; volumes = %v", vol, h.driver.volumeNames())
	}
}
