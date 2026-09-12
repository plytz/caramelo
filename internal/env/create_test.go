package env

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/state"
)

type hookedStore struct {
	*fakeStore

	onCreateEnv func(state.EnvRecord) (*state.EnvRecord, error)
	onUpdateEnv func(state.EnvRecord) error
	onStatus    func(int64, string) error
}

func (s *hookedStore) CreateEnv(ctx context.Context, r state.EnvRecord) (*state.EnvRecord, error) {
	if s.onCreateEnv != nil {
		return s.onCreateEnv(r)
	}
	return s.fakeStore.CreateEnv(ctx, r)
}

func (s *hookedStore) UpdateEnv(ctx context.Context, r state.EnvRecord) error {
	if s.onUpdateEnv != nil {
		if err := s.onUpdateEnv(r); err != nil {
			return err
		}
	}
	return s.fakeStore.UpdateEnv(ctx, r)
}

func (s *hookedStore) UpdateEnvStatus(ctx context.Context, id int64, status string) error {
	if s.onStatus != nil {
		if err := s.onStatus(id, status); err != nil {
			return err
		}
	}
	return s.fakeStore.UpdateEnvStatus(ctx, id, status)
}

func (h *harness) hookStore() *hookedStore {
	hs := &hookedStore{fakeStore: h.store}
	h.m.Store = hs
	return hs
}

type fixedAlloc struct {
	blocks []int
	err    error
	calls  int
}

func (a *fixedAlloc) Allocate(context.Context, int64) (int, error) {
	if a.err != nil {
		return 0, a.err
	}
	a.calls++
	if len(a.blocks) == 0 {
		return 0, errors.New("no blocks left")
	}
	base := a.blocks[0]
	if len(a.blocks) > 1 {
		a.blocks = a.blocks[1:]
	}
	return base, nil
}

func (a *fixedAlloc) Release(context.Context, int64) error { return nil }

func TestCreateFallsBackToHEADWhenTheAppHasNoDefaultBranch(t *testing.T) {
	h := newHarness(t)

	h.store.apps["shop"] = state.App{Name: "shop", RepoPath: RepoPath(h.data, "shop")}
	h.repo.head = "main"

	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", NoDeps: true})
	if e.Commit != mainCommit {
		t.Errorf("commit = %q, want HEAD's %q", e.Commit, mainCommit)
	}

	if !contains(h.repo.calls, "create-branch feat-x "+mainCommit) {
		t.Errorf("branch calls = %v, want it branched from main's commit", h.repo.calls)
	}
}

func TestCreateSaysToPassFromWhenHEADDangles(t *testing.T) {
	h := newHarness(t)
	h.store.apps["shop"] = state.App{Name: "shop", RepoPath: RepoPath(h.data, "shop")}
	h.repo.head = ""

	_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", NoDeps: true})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "no default branch yet") || !strings.Contains(err.Error(), "--from") {
		t.Errorf("error = %v, want it to name the fix (--from)", err)
	}

	if envs, _ := h.store.Envs(context.Background(), "shop"); len(envs) != 0 {
		t.Errorf("store holds %+v after a refused create", envs)
	}
}

func TestTrimRefsHeads(t *testing.T) {
	cases := map[string]string{
		"refs/heads/main":         "main",
		"refs/heads/feat/x":       "feat/x",
		"main":                    "main",
		"refs/heads/":             "refs/heads/",
		"refs/remotes/origin/foo": "refs/remotes/origin/foo",
	}
	for in, want := range cases {
		if got := trimRefsHeads(in); got != want {
			t.Errorf("trimRefsHeads(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCreateReportsAnUnknownRef(t *testing.T) {
	h := newHarness(t)
	_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", From: "nope", NoDeps: true})
	if err == nil || !strings.Contains(err.Error(), `no such ref "nope"`) {
		t.Fatalf("error = %v, want it to name the missing ref", err)
	}
}

func TestCreateReportsAFailedAllocation(t *testing.T) {
	h := newHarness(t)
	h.m.Ports = &fixedAlloc{err: errors.New("every block is taken")}

	_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", NoDeps: true})
	if err == nil || !strings.Contains(err.Error(), "allocate a port block") {
		t.Fatalf("error = %v, want it to name the allocation", err)
	}
}

func TestCreateStopsWhenTheNameIsTakenBetweenTheTwoWrites(t *testing.T) {
	h := newHarness(t)

	hs := h.hookStore()
	hs.onCreateEnv = func(r state.EnvRecord) (*state.EnvRecord, error) {
		h.store.mu.Lock()
		h.store.nextEnv++
		h.store.envs = append(h.store.envs, state.EnvRecord{ID: h.store.nextEnv, App: r.App, Name: r.Name, Status: state.EnvReady})
		h.store.mu.Unlock()
		hs.onCreateEnv = nil
		return nil, state.ErrExists
	}

	_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", NoDeps: true})
	if err == nil || !strings.Contains(err.Error(), `env "feat-x" already exists`) {
		t.Fatalf("error = %v, want it to say the env already exists", err)
	}
}

func TestCreateGivesUpAfterTooManyLostRaces(t *testing.T) {
	h := newHarness(t)
	hs := h.hookStore()

	hs.onCreateEnv = func(state.EnvRecord) (*state.EnvRecord, error) { return nil, state.ErrExists }
	alloc := &fixedAlloc{blocks: []int{20000}}
	h.m.Ports = alloc

	_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", NoDeps: true})
	if err == nil || !strings.Contains(err.Error(), "port blocks in a row were taken") {
		t.Fatalf("error = %v, want the give-up message", err)
	}
	if alloc.calls != allocAttempts {
		t.Errorf("allocated %d times, want the %d the loop allows", alloc.calls, allocAttempts)
	}
}

func TestCreateReportsAStoreFailureOnTheRow(t *testing.T) {
	h := newHarness(t)
	hs := h.hookStore()
	hs.onCreateEnv = func(state.EnvRecord) (*state.EnvRecord, error) { return nil, errors.New("disk full") }

	_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", NoDeps: true})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error = %v, want the store's error", err)
	}
}

func TestCreateMarksTheEnvFailedWhenTheConfigCannotBeSaved(t *testing.T) {
	h := newHarness(t)
	hs := h.hookStore()
	hs.onUpdateEnv = func(state.EnvRecord) error { return errors.New("disk full") }

	_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", NoDeps: true})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error = %v, want the store's error", err)
	}
	if got := h.status("feat-x"); got != state.EnvFailed {
		t.Errorf("status = %q, want %q so that destroy can clean it up", got, state.EnvFailed)
	}
}

func TestCreateStillReportsTheCauseWhenItCannotEvenRecordTheFailure(t *testing.T) {
	h := newHarness(t)
	hs := h.hookStore()
	hs.onUpdateEnv = func(state.EnvRecord) error { return errors.New("disk full") }
	hs.onStatus = func(int64, string) error { return errors.New("still full") }

	_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", NoDeps: true})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error = %v, want the original cause", err)
	}
	if !strings.Contains(h.out.String(), "could not mark env") {
		t.Errorf("progress = %q, want a warning that the status could not be written", h.out.String())
	}
}

func TestCreateReportsAFailedBranchAndWorktree(t *testing.T) {
	t.Run("branch", func(t *testing.T) {
		h := newHarness(t)

		h.repo.branches["feat-x"] = mainCommit
		failing := &failingRepo{fakeRepo: h.repo, deleteErr: errors.New("branch is checked out")}
		h.m.Git = failing

		_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", From: "main", Reset: true, NoDeps: true})
		if err == nil || !strings.Contains(err.Error(), "reset branch") {
			t.Fatalf("error = %v, want it to name the reset", err)
		}
		if got := h.status("feat-x"); got != state.EnvFailed {
			t.Errorf("status = %q, want failed", got)
		}
	})

	t.Run("worktree", func(t *testing.T) {
		h := newHarness(t)
		h.repo.WorktreeAddErr = errors.New("no space left")
		_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", NoDeps: true})
		if err == nil || !strings.Contains(err.Error(), "add worktree") {
			t.Fatalf("error = %v, want it to name the worktree", err)
		}
	})
}

type failingRepo struct {
	*fakeRepo
	deleteErr error
	createErr error
}

func (r *failingRepo) DeleteBranch(ctx context.Context, repo, branch string, force bool) error {
	if r.deleteErr != nil {
		return r.deleteErr
	}
	return r.fakeRepo.DeleteBranch(ctx, repo, branch, force)
}

func (r *failingRepo) CreateBranch(ctx context.Context, repo, branch, from string) error {
	if r.createErr != nil {
		return r.createErr
	}
	return r.fakeRepo.CreateBranch(ctx, repo, branch, from)
}

func TestCreateReportsAFailedBranchCreation(t *testing.T) {
	h := newHarness(t)
	h.m.Git = &failingRepo{fakeRepo: h.repo, createErr: errors.New("permission denied")}
	_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", NoDeps: true})
	if err == nil || !strings.Contains(err.Error(), `create branch "feat-x" from "main"`) {
		t.Fatalf("error = %v, want it to name both ends", err)
	}
}

func TestCreateReportsAnUnreadableConfig(t *testing.T) {
	h := newHarness(t)
	h.loadErr = errors.New("yaml: line 3: did not find expected key")

	_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", NoDeps: true})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), config.FileName) || !strings.Contains(err.Error(), "did not find expected key") {
		t.Errorf("error = %v, want the path and the parser's complaint", err)
	}
	if got := h.status("feat-x"); got != state.EnvFailed {
		t.Errorf("status = %q, want failed", got)
	}
}

func TestCreateNamesTheAppWhenTheConfigDoesNot(t *testing.T) {
	h := newHarness(t)
	cfg := sampleConfig()
	cfg.Name = ""
	cfg.Deps = nil
	cfg.Env = map[string]string{"GREETING": "hello ${app.name}"}
	h.cfg = cfg

	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", NoDeps: true})
	var stored config.App
	if err := json.Unmarshal(e.Config, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Name != "shop" {
		t.Errorf("stored config name = %q, want the app's", stored.Name)
	}
	if !strings.Contains(h.out.String(), "0 deps") {
		t.Errorf("progress = %q, want it to say how many dependencies were read", h.out.String())
	}
}

func TestCreateRefusesMoreDependenciesThanThePortBlockHolds(t *testing.T) {
	h := newHarness(t)
	cfg := sampleConfig()
	cfg.Deps = nil
	for i := 0; i < 32; i++ {
		cfg.Deps = append(cfg.Deps, config.Dep{Name: "dep" + strconv.Itoa(i), Image: "alpine", Port: 1000 + i})
	}
	h.cfg = cfg

	_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", NoDeps: true})
	if err == nil || !strings.Contains(err.Error(), "do not fit in a block") {
		t.Fatalf("error = %v, want it to say the block is too small", err)
	}
}

func TestCreateReportsAnUnresolvableVariable(t *testing.T) {
	t.Run("app", func(t *testing.T) {
		h := newHarness(t)
		cfg := sampleConfig()
		cfg.Deps = nil
		cfg.Env = map[string]string{"BROKEN": "${deps.nope.port}"}
		h.cfg = cfg
		_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", NoDeps: true})
		if err == nil || !strings.Contains(err.Error(), "expand the variables of env") {
			t.Fatalf("error = %v, want it to name the env's variables", err)
		}
	})

	t.Run("dep", func(t *testing.T) {
		h := newHarness(t)
		cfg := sampleConfig()
		cfg.Env = nil
		cfg.Deps = []config.Dep{{Name: "db", Image: "postgres:16", Port: 5432, Env: map[string]string{"X": "${deps.nope.port}"}}}
		h.cfg = cfg
		_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", NoDeps: true})
		if err == nil || !strings.Contains(err.Error(), `dependency "db"`) {
			t.Fatalf("error = %v, want it to name the dependency", err)
		}
	})
}

func TestReadyOnceFallsBackToTCP(t *testing.T) {
	h := newHarness(t)

	h.m.ReadyInterval = time.Second
	h.m.Timeout = time.Second
	ln, err := net.Listen("tcp", net.JoinHostPort(DepHost, "0"))
	if err != nil {
		t.Skipf("cannot listen on %s: %v", DepHost, err)
	}
	defer ln.Close()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	dep := config.Dep{Name: "mystery", Image: "some/image"}
	if ok, detail := h.m.readyOnce(context.Background(), dep, "c", port); !ok {
		t.Errorf("readyOnce on a listening port = false (%s), want true", detail)
	}

	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	ok, detail := h.m.readyOnce(context.Background(), dep, "c", port)
	if ok {
		t.Error("readyOnce on a closed port = true")
	}
	if detail == "" {
		t.Error("readyOnce gave no reason for the refusal")
	}
}

func TestReadyOnceReportsWhatTheCheckSaid(t *testing.T) {
	h := newHarness(t)
	dep := config.Dep{Name: "db", Image: "postgres:16", Ready: []string{"pg_isready"}}

	h.driver.ExecResult = func(string, []string) (runner.Result, error) {
		return runner.Result{ExitCode: 2, Stderr: "no response\nsecond line\n"}, nil
	}
	ok, detail := h.m.readyOnce(context.Background(), dep, "c", 20001)
	if ok || detail != "exit 2: no response" {
		t.Errorf("readyOnce = %v, %q; want a failure quoting the first stderr line", ok, detail)
	}

	h.driver.ExecResult = func(string, []string) (runner.Result, error) {
		return runner.Result{ExitCode: 1, Stdout: "starting up\n"}, nil
	}
	if ok, detail := h.m.readyOnce(context.Background(), dep, "c", 20001); ok || detail != "exit 1: starting up" {
		t.Errorf("readyOnce = %v, %q; want it to fall back to stdout", ok, detail)
	}

	h.driver.ExecResult = func(string, []string) (runner.Result, error) {
		return runner.Result{}, errors.New("container is not running")
	}
	if ok, detail := h.m.readyOnce(context.Background(), dep, "c", 20001); ok || !strings.Contains(detail, "not running") {
		t.Errorf("readyOnce = %v, %q; want the driver's error", ok, detail)
	}

	h.driver.ExecResult = func(string, []string) (runner.Result, error) { return runner.Result{}, nil }
	if ok, detail := h.m.readyOnce(context.Background(), dep, "c", 20001); !ok || detail != "" {
		t.Errorf("readyOnce = %v, %q; want ready", ok, detail)
	}
}

func TestWaitReadyStopsWhenTheSessionGoesAway(t *testing.T) {
	h := newHarness(t)
	h.m.Timeout = time.Hour
	h.driver.ExecResult = func(string, []string) (runner.Result, error) {
		return runner.Result{ExitCode: 1}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dep := config.Dep{Name: "db", Image: "postgres:16", Ready: []string{"pg_isready"}}
	err := h.m.waitReady(ctx, dep, "c", 20001, time.Hour, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("waitReady = %v, want the context's error", err)
	}
}

func TestProgressfIsQuietWithoutAReader(t *testing.T) {

	progressf(nil, "ok", "step", "detail %d", 1)

	var b bytes.Buffer
	progressf(&b, "ok", "ports", "")
	if b.String() != "[ok] ports\n" {
		t.Errorf("with no detail = %q, want just the step", b.String())
	}
	b.Reset()
	progressf(&b, "changed", "env", "%s ready", "feat-x")
	if b.String() != "[changed] env: feat-x ready\n" {
		t.Errorf("with a detail = %q", b.String())
	}
}

func TestShortAndPlural(t *testing.T) {
	if got := short("0123456789abcdef0123"); got != "0123456789ab" {
		t.Errorf("short = %q, want 12 characters", got)
	}
	if got := short("abc"); got != "abc" {
		t.Errorf("short of a shorter string = %q, want it unchanged", got)
	}
	if got := plural(1, "dep"); got != "1 dep" {
		t.Errorf("plural(1) = %q", got)
	}
	if got := plural(0, "dep"); got != "0 deps" {
		t.Errorf("plural(0) = %q", got)
	}
	if got := plural(2, "dep"); got != "2 deps" {
		t.Errorf("plural(2) = %q", got)
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("one\ntwo\n"); got != "one" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine("only"); got != "only" {
		t.Errorf("firstLine of a single line = %q", got)
	}
	if got := firstLine(""); got != "" {
		t.Errorf("firstLine of nothing = %q", got)
	}
}

func TestReadConfigWithoutAFileIsNotAnError(t *testing.T) {
	h := newHarness(t)
	h.loadErr = fmt.Errorf("open %s: %w", config.FileName, os.ErrNotExist)
	var out bytes.Buffer
	cfg, err := h.m.readConfig("/nowhere", "shop", &out)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "shop" || len(cfg.Deps) != 0 {
		t.Errorf("config = %+v, want an empty config named after the app", cfg)
	}
	if !strings.Contains(out.String(), "worktree and PORT only") {
		t.Errorf("progress = %q, want it to say what the env gets", out.String())
	}
}
