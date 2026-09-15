package sshapi

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"github.com/plytz/caramelo/internal/testutil"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/git"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/state"
)

type envStore struct {
	*fakeStore

	mu         sync.Mutex
	apps       []state.App
	envs       []state.EnvRecord
	resources  []state.EnvResource
	services   []state.EnvService
	events     []state.EnvEvent
	routes     []state.Route
	targets    []state.EdgeTarget
	next       int64
	identities []string

	appsErr error
	envsErr error
}

func newEnvStore(apps ...state.App) *envStore {
	return &envStore{fakeStore: newFakeStore(), apps: apps}
}

func (s *envStore) saw(ctx context.Context) {
	s.identities = append(s.identities, env.IdentityFrom(ctx))
}

func (s *envStore) callers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for _, id := range s.identities {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func (s *envStore) Apps(ctx context.Context) ([]state.App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw(ctx)
	return append([]state.App(nil), s.apps...), s.appsErr
}

func (s *envStore) App(ctx context.Context, name string) (*state.App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw(ctx)
	for i := range s.apps {
		if s.apps[i].Name == name {
			a := s.apps[i]
			return &a, nil
		}
	}
	return nil, state.ErrNotFound
}

func (s *envStore) Envs(ctx context.Context, app string) ([]state.EnvRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw(ctx)
	if s.envsErr != nil {
		return nil, s.envsErr
	}
	out := []state.EnvRecord{}
	for _, e := range s.envs {
		if app == "" || e.App == app {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *envStore) Env(ctx context.Context, app, name string) (*state.EnvRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw(ctx)
	for i := range s.envs {
		if s.envs[i].App == app && s.envs[i].Name == name {
			e := s.envs[i]
			return &e, nil
		}
	}
	return nil, state.ErrNotFound
}

func (s *envStore) RoutesOfEnv(ctx context.Context, envID int64) ([]state.Route, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw(ctx)
	var out []state.Route
	for _, r := range s.routes {
		if r.EnvID == envID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *envStore) Targets(ctx context.Context, routeID int64) ([]state.EdgeTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []state.EdgeTarget
	for _, t := range s.targets {
		if t.RouteID == routeID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *envStore) CreateEnv(ctx context.Context, r state.EnvRecord) (*state.EnvRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw(ctx)
	for _, e := range s.envs {
		if (e.App == r.App && e.Name == r.Name) || e.PortBase == r.PortBase {
			return nil, state.ErrExists
		}
	}
	s.next++
	r.ID = s.next
	s.envs = append(s.envs, r)
	out := r
	return &out, nil
}

func (s *envStore) UpdateEnv(ctx context.Context, r state.EnvRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw(ctx)
	for i := range s.envs {
		if s.envs[i].ID == r.ID {
			s.envs[i] = r
			return nil
		}
	}
	return state.ErrNotFound
}

func (s *envStore) UpdateEnvStatus(ctx context.Context, id int64, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw(ctx)
	for i := range s.envs {
		if s.envs[i].ID == id {
			s.envs[i].Status = status
			return nil
		}
	}
	return state.ErrNotFound
}

func (s *envStore) DeleteEnv(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw(ctx)
	kept := s.envs[:0]
	for _, e := range s.envs {
		if e.ID != id {
			kept = append(kept, e)
		}
	}
	s.envs = kept
	return nil
}

func (s *envStore) AddResource(ctx context.Context, r state.EnvResource) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw(ctx)
	r.ID = int64(len(s.resources) + 1)
	s.resources = append(s.resources, r)
	return nil
}

func (s *envStore) Resources(ctx context.Context, envID int64) ([]state.EnvResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw(ctx)
	out := []state.EnvResource{}
	for _, r := range s.resources {
		if r.EnvID == envID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *envStore) DeleteResource(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw(ctx)
	kept := s.resources[:0]
	for _, r := range s.resources {
		if r.ID != id {
			kept = append(kept, r)
		}
	}
	s.resources = kept
	return nil
}

func (s *envStore) Services(ctx context.Context, envID int64) ([]state.EnvService, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw(ctx)
	out := []state.EnvService{}
	for _, svc := range s.services {
		if svc.EnvID == envID {
			out = append(out, svc)
		}
	}
	return out, nil
}

func (s *envStore) PutService(ctx context.Context, svc state.EnvService) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw(ctx)
	for i, cur := range s.services {
		if cur.EnvID == svc.EnvID && cur.Name == svc.Name {
			s.services[i] = svc
			return nil
		}
	}
	s.services = append(s.services, svc)
	return nil
}

func (s *envStore) DeleteService(ctx context.Context, envID int64, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw(ctx)
	for i, cur := range s.services {
		if cur.EnvID == envID && cur.Name == name {
			s.services = append(s.services[:i], s.services[i+1:]...)
			return nil
		}
	}
	return nil
}

func (s *envStore) SetAppStack(ctx context.Context, name, stack string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw(ctx)
	for i := range s.apps {
		if s.apps[i].Name == name {
			s.apps[i].Stack = stack
			return nil
		}
	}
	return state.ErrNotFound
}

func (s *envStore) AddEvent(ctx context.Context, e state.EnvEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw(ctx)
	s.events = append(s.events, e)
	return nil
}

func (s *envStore) Events(ctx context.Context, envID int64, limit int) ([]state.EnvEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw(ctx)
	out := []state.EnvEvent{}
	for i := len(s.events) - 1; i >= 0; i-- {
		if s.events[i].EnvID != envID {
			continue
		}
		out = append(out, s.events[i])
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

type envRepo struct {
	git.Repo

	mu        sync.Mutex
	branches  map[string]string
	worktrees map[string]bool

	size int64
}

func newEnvRepo() *envRepo {
	return &envRepo{
		branches:  map[string]string{"main": strings.Repeat("a", 40)},
		worktrees: map[string]bool{},
		size:      -1,
	}
}

func (r *envRepo) BranchExists(_ context.Context, _, branch string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.branches[branch]
	return ok, nil
}

func (r *envRepo) CreateBranch(_ context.Context, _, branch, from string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	commit, ok := r.branches[from]
	if !ok {
		for _, c := range r.branches {
			if c == from {
				commit, ok = c, true
				break
			}
		}
	}
	if !ok {
		return git.ErrNotFound
	}
	r.branches[branch] = commit
	return nil
}

func (r *envRepo) DeleteBranch(_ context.Context, _, branch string, _ bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.branches, branch)
	return nil
}

func (r *envRepo) RevParse(_ context.Context, _, ref string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	commit, ok := r.branches[ref]
	if !ok {
		return "", git.ErrNotFound
	}
	return commit, nil
}

func (r *envRepo) SymbolicRefHEAD(context.Context, string) (string, error) {
	return "refs/heads/main", nil
}

func (r *envRepo) WorktreeAdd(_ context.Context, _, path, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.MkdirAll(path, 0o750); err != nil {
		return err
	}
	r.worktrees[path] = true
	return nil
}

func (r *envRepo) WorktreeRemove(_ context.Context, _, path string, _ bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.worktrees, path)
	return os.RemoveAll(path)
}

func (r *envRepo) WorktreePrune(context.Context, string) error { return nil }

type sizedRepo struct{ *envRepo }

func (r sizedRepo) RepoSize(context.Context, string) (int64, error) {
	if r.size < 0 {
		return 0, errors.New("du: cannot read")
	}
	return r.size, nil
}

type emptyRuntime struct{ runtime.Driver }

func (emptyRuntime) CreateNetwork(context.Context, string, map[string]string) error { return nil }

func (emptyRuntime) RemoveNetwork(context.Context, string) error { return nil }

func (emptyRuntime) CreateVolume(context.Context, string, map[string]string) error { return nil }

func (emptyRuntime) RemoveVolume(context.Context, string) error { return nil }

func (emptyRuntime) ListByLabel(context.Context, map[string]string) ([]runtime.ContainerState, error) {
	return nil, nil
}

func (emptyRuntime) ListVolumesByLabel(context.Context, map[string]string) ([]string, error) {
	return nil, nil
}

func (emptyRuntime) Inspect(context.Context, string) (runtime.ContainerState, error) {
	return runtime.ContainerState{}, runtime.ErrNotFound
}

type fixedPorts struct {
	mu   sync.Mutex
	next int
}

func (p *fixedPorts) Allocate(context.Context, int64) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.next == 0 {
		p.next = 20000
	}
	base := p.next
	p.next += 16
	return base, nil
}

func (p *fixedPorts) Release(context.Context, int64) error { return nil }

func newEnvDaemon(t *testing.T) (*Daemon, *envStore, *envRepo) {
	t.Helper()
	d, _ := newTestDaemon(t)
	store := newEnvStore(state.App{Name: "shop", DefaultBranch: "main"})
	d.Store = store
	repo := newEnvRepo()

	m := env.New(store, emptyRuntime{}, repo, &fixedPorts{}, nil,
		env.Dirs{Data: d.Config.DataDir, User: "", Run: d.Config.RunDir})
	m.Version = "test"

	m.LoadConfig = func(string) (*config.App, error) { return nil, os.ErrNotExist }
	d.EnvManager = m
	return d, store, repo
}

func sessionCtx(identity string) context.Context {
	return WithSession(context.Background(), api.Session{Transport: TransportSSH, Identity: identity})
}

func TestEnvCommandsSayWhenTheDaemonHasNoEnvironments(t *testing.T) {
	d, _ := newTestDaemon(t)
	if d.EnvManager != nil {
		t.Fatal("the test daemon was built with an env manager")
	}
	ctx := context.Background()

	calls := map[string]func() error{
		"CreateEnv": func() error {
			_, err := d.CreateEnv(ctx, env.CreateRequest{App: "shop", Name: "feat-x"}, io.Discard)
			return err
		},
		"Envs": func() error { _, err := d.Envs(ctx, "shop"); return err },
		"Env":  func() error { _, err := d.Env(ctx, "shop", "feat-x"); return err },
		"DestroyEnv": func() error {
			return d.DestroyEnv(ctx, env.DestroyRequest{App: "shop", Name: "feat-x", DeleteBranch: false}, io.Discard)
		},
		"ExecEnv": func() error {
			_, err := d.ExecEnv(ctx, env.ExecRequest{App: "shop", Name: "feat-x", Argv: []string{"true"}})
			return err
		},
		"ExportEnv": func() error {
			_, err := d.ExportEnv(ctx, "shop", "feat-x", env.FormatShell, config.ViewHost, false)
			return err
		},
	}
	for name, call := range calls {
		err := call()
		if err == nil {
			t.Errorf("%s on a daemon without environments returned no error", name)
			continue
		}
		if !strings.Contains(err.Error(), "environments are not available") {
			t.Errorf("%s = %v, want it to say environments are unavailable", name, err)
		}
	}
}

func TestCreateEnvStampsTheSessionsIdentity(t *testing.T) {
	d, store, _ := newEnvDaemon(t)

	e, err := d.CreateEnv(sessionCtx("alex@laptop"), env.CreateRequest{App: "shop", Name: "feat-x", NoDeps: true}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if e.CreatedBy != "alex@laptop" {
		t.Errorf("CreatedBy = %q, want the session's key name", e.CreatedBy)
	}
	if got := store.callers(); len(got) != 1 || got[0] != "alex@laptop" {
		t.Errorf("the store saw callers %v, want only alex@laptop", got)
	}
	store.mu.Lock()
	events := append([]state.EnvEvent(nil), store.events...)
	store.mu.Unlock()
	if len(events) == 0 {
		t.Fatal("no audit trail was written")
	}
	for _, ev := range events {
		if ev.Identity != "alex@laptop" {
			t.Errorf("event %+v does not say who did it", ev)
		}
	}
}

func TestCommandsOverTheLocalSocketHaveNoIdentity(t *testing.T) {
	d, store, _ := newEnvDaemon(t)

	e, err := d.CreateEnv(context.Background(), env.CreateRequest{App: "shop", Name: "feat-x", NoDeps: true}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if e.CreatedBy != "" {
		t.Errorf("CreatedBy = %q, want empty over the socket", e.CreatedBy)
	}
	if got := store.callers(); len(got) != 1 || got[0] != "" {
		t.Errorf("the store saw callers %v, want one anonymous caller", got)
	}

	ctx := WithSession(context.Background(), api.Session{Transport: TransportSocket})
	if _, err := d.Envs(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
}

func TestEnvsListsWhatTheManagerHas(t *testing.T) {
	d, _, _ := newEnvDaemon(t)
	ctx := sessionCtx("commander")
	for _, name := range []string{"feat-x", "feat-y"} {
		if _, err := d.CreateEnv(ctx, env.CreateRequest{App: "shop", Name: name, NoDeps: true}, io.Discard); err != nil {
			t.Fatal(err)
		}
	}

	envs, err := d.Envs(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 2 {
		t.Fatalf("Envs = %+v, want two", envs)
	}
	if envs[0].PortBase == envs[1].PortBase {
		t.Errorf("both envs got port base %d", envs[0].PortBase)
	}

	other, err := d.Envs(ctx, "nosuchapp")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Errorf("Envs of an unknown app = %+v, want none", other)
	}

	if _, err := d.Envs(ctx, "../etc"); err == nil {
		t.Error("a traversal app name was accepted")
	}
}

func TestEnvReturnsTheDetailTheCommanderPrints(t *testing.T) {
	d, _, _ := newEnvDaemon(t)
	ctx := sessionCtx("commander")
	created, err := d.CreateEnv(ctx, env.CreateRequest{App: "shop", Name: "feat-x", NoDeps: true}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	detail, err := d.Env(ctx, "shop", "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Env.ID != created.ID || detail.Env.Name != "feat-x" {
		t.Errorf("detail.Env = %+v, want the created env", detail.Env)
	}
	if len(detail.Deps) != 0 {
		t.Errorf("deps = %+v, want none for a repo with no config", detail.Deps)
	}
	if len(detail.Events) == 0 {
		t.Error("no events in the detail; `env show` prints the audit trail")
	}

	_, err = d.Env(ctx, "shop", "nope")
	if err == nil || !strings.Contains(err.Error(), `no such env "nope"`) {
		t.Errorf("Env of a missing env = %v, want it named", err)
	}
}

func TestDestroyEnvRemovesItAndIsIdempotent(t *testing.T) {
	d, _, repo := newEnvDaemon(t)
	ctx := sessionCtx("commander")
	e, err := d.CreateEnv(ctx, env.CreateRequest{App: "shop", Name: "feat-x", NoDeps: true}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.Worktree); err != nil {
		t.Fatalf("worktree was not created: %v", err)
	}

	var progress bytes.Buffer
	if err := d.DestroyEnv(ctx, env.DestroyRequest{App: "shop", Name: "feat-x", DeleteBranch: false}, &progress); err != nil {
		t.Fatal(err)
	}
	if envs, _ := d.Envs(ctx, "shop"); len(envs) != 0 {
		t.Errorf("Envs after destroy = %+v", envs)
	}
	if _, err := os.Stat(e.Worktree); !os.IsNotExist(err) {
		t.Errorf("worktree %s survived destroy (%v)", e.Worktree, err)
	}
	repo.mu.Lock()
	_, branchKept := repo.branches["feat-x"]
	repo.mu.Unlock()
	if !branchKept {
		t.Error("destroy deleted the branch without being asked")
	}

	if err := d.DestroyEnv(ctx, env.DestroyRequest{App: "shop", Name: "feat-x", DeleteBranch: true}, io.Discard); err != nil {
		t.Errorf("second destroy = %v, want nil", err)
	}
	repo.mu.Lock()
	_, branchGone := repo.branches["feat-x"]
	repo.mu.Unlock()
	if branchGone {
		t.Error("--delete-branch left the branch behind")
	}
}

func TestExecEnvPassesArgvAndTheExitCodeBack(t *testing.T) {
	d, _, _ := newEnvDaemon(t)
	ctx := sessionCtx("commander")
	e, err := d.CreateEnv(ctx, env.CreateRequest{App: "shop", Name: "feat-x", NoDeps: true}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code, err := d.ExecEnv(ctx, env.ExecRequest{
		App: "shop", Name: "feat-x",
		Argv:   []string{"sh", "-c", "printf %s \"$CARAMELO_ENV:$PORT\"; exit 3"},
		Stdout: &out, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code != 3 {
		t.Errorf("exit code = %d, want the child's 3", code)
	}
	if want := "feat-x:" + strconv.Itoa(e.PortBase); out.String() != want {
		t.Errorf("stdout = %q, want %q: the env's variables must reach the child", out.String(), want)
	}

	if _, err := d.ExecEnv(ctx, env.ExecRequest{App: "shop", Name: "feat-x", Argv: []string{"definitely-not-a-command"}}); err == nil {
		t.Error("running a missing binary returned no error")
	}
	if _, err := d.ExecEnv(ctx, env.ExecRequest{App: "shop", Name: "nope", Argv: []string{"true"}}); err == nil {
		t.Error("exec in a missing env returned no error")
	}
}

func TestExportEnvRendersTheVariables(t *testing.T) {
	d, _, _ := newEnvDaemon(t)
	ctx := sessionCtx("commander")
	if _, err := d.CreateEnv(ctx, env.CreateRequest{App: "shop", Name: "feat-x", NoDeps: true}, io.Discard); err != nil {
		t.Fatal(err)
	}

	shell, err := d.ExportEnv(ctx, "shop", "feat-x", env.FormatShell, config.ViewHost, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(shell, "export CARAMELO_ENV='feat-x'") {
		t.Errorf("shell export = %q", shell)
	}
	dotenv, err := d.ExportEnv(ctx, "shop", "feat-x", env.FormatDotenv, config.ViewHost, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dotenv, "CARAMELO_ENV=feat-x") {
		t.Errorf("dotenv export = %q", dotenv)
	}
	if _, err := d.ExportEnv(ctx, "shop", "feat-x", env.ExportFormat("toml"), config.ViewHost, false); err == nil {
		t.Error("an unknown format was accepted")
	}
	if _, err := d.ExportEnv(ctx, "shop", "nope", env.FormatJSON, config.ViewHost, false); err == nil {
		t.Error("exporting a missing env returned no error")
	}
}

func TestAppsCountsEnvsAndMeasuresRepositories(t *testing.T) {
	d, store, repo := newEnvDaemon(t)
	store.mu.Lock()
	store.apps = append(store.apps, state.App{Name: "blog", DefaultBranch: "trunk", RepoPath: "/elsewhere/blog.git"})
	store.mu.Unlock()
	ctx := sessionCtx("commander")
	if _, err := d.CreateEnv(ctx, env.CreateRequest{App: "shop", Name: "feat-x", NoDeps: true}, io.Discard); err != nil {
		t.Fatal(err)
	}

	repo.size = 4096
	d.EnvManager.Git = sizedRepo{repo}

	apps, err := d.Apps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 {
		t.Fatalf("Apps = %+v, want two", apps)
	}
	byName := map[string]api.AppInfo{}
	for _, a := range apps {
		byName[a.Name] = a
	}
	if got := byName["shop"]; got.EnvCount != 1 || got.RepoBytes != 4096 {
		t.Errorf("shop = %+v, want one env and the size du reported", got)
	}
	if got := byName["blog"]; got.EnvCount != 0 || got.DefaultBranch != "trunk" {
		t.Errorf("blog = %+v, want no envs and its own default branch", got)
	}
}

func TestAppsFallsBackToWalkingTheRepository(t *testing.T) {
	d, store, repo := newEnvDaemon(t)
	ctx := context.Background()

	dir := env.RepoPath(d.Config.DataDir, "shop")
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "objects", "blob"), bytes.Repeat([]byte("x"), 1234), 0o640); err != nil {
		t.Fatal(err)
	}

	apps, err := d.Apps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if apps[0].RepoBytes != 1234 {
		t.Errorf("RepoBytes = %d, want the walked 1234", apps[0].RepoBytes)
	}

	repo.size = -1
	d.EnvManager.Git = sizedRepo{repo}
	apps, err = d.Apps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if apps[0].RepoBytes != 1234 {
		t.Errorf("RepoBytes after a failed du = %d, want the walked 1234", apps[0].RepoBytes)
	}

	store.appsErr = errors.New("database is locked")
	if _, err := d.Apps(ctx); err == nil || !strings.Contains(err.Error(), "list apps") {
		t.Errorf("Apps = %v, want it to name the failed listing", err)
	}
	store.appsErr = nil
	store.envsErr = errors.New("database is locked")
	if _, err := d.Apps(ctx); err == nil || !strings.Contains(err.Error(), "count envs") {
		t.Errorf("Apps = %v, want it to name the failed count", err)
	}
}

func TestAppsNeedsAStore(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.Store = nil
	if _, err := d.Apps(context.Background()); err == nil || !strings.Contains(err.Error(), "no state store") {
		t.Errorf("Apps without a store = %v", err)
	}
}

func TestDirBytesCountsWhatItCanRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "one"), bytes.Repeat([]byte("x"), 10), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "b", "two"), bytes.Repeat([]byte("y"), 25), 0o640); err != nil {
		t.Fatal(err)
	}
	if got := dirBytes(dir); got != 35 {
		t.Errorf("dirBytes = %d, want 35", got)
	}
	if got := dirBytes(filepath.Join(dir, "nothing-here")); got != 0 {
		t.Errorf("dirBytes of a missing directory = %d, want 0", got)
	}
}

func TestNewEnvManagerWiresTheDaemonsPieces(t *testing.T) {
	d, _ := newTestDaemon(t)
	var log bytes.Buffer
	m := newEnvManager(d.Config, newFakeStore(), runner.Exec{}, "v1.2.3", &log)

	if m.Dirs.Data != d.Config.DataDir || m.Dirs.User != d.Config.User {
		t.Errorf("dirs = %+v, want the daemon's data dir and user", m.Dirs)
	}
	if m.Version != "v1.2.3" {
		t.Errorf("version = %q, want the daemon's", m.Version)
	}
	if m.Driver == nil || m.Git == nil || m.Ports == nil || m.Store == nil {
		t.Errorf("manager left a piece unwired: %+v", m)
	}
	if m.Timeout != env.DefaultTimeout {
		t.Errorf("timeout = %v, want the package default", m.Timeout)
	}
}

func TestRunServesUntilTheContextIsCancelled(t *testing.T) {
	dir := t.TempDir()
	cfg := testServerConfig(t, dir)

	reports := SetupReportsDir(cfg)
	if err := os.MkdirAll(reports, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reports, "run-1.json"),
		[]byte(`{"run_id":"run-1","version":"test","results":[{"step":"preflight","status":"ok"}]}`), 0o640); err != nil {
		t.Fatal(err)
	}

	pr, pw := io.Pipe()
	lines := logLines(pr)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, filepath.Join(dir, "etc"), "test",
			pw, func(_ context.Context, c Command) int {
				_, _ = io.WriteString(c.Stdout, "pong "+strings.Join(c.Args, " ")+"\n")
				return 0
			})
	}()

	if got := waitForLine(t, lines, "host key"); !strings.Contains(got, "SHA256:") {
		t.Errorf("startup line = %q, want the host key fingerprint", got)
	}

	waitForLine(t, lines, "imported setup report run-1.json")
	waitForLine(t, lines, "listening on")

	if got := runOverSocket(t, cfg.SocketPath(), "version"); !strings.Contains(got, "pong") {
		t.Errorf("the daemon answered %q, want the exec stub's output", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want a clean stop", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not stop when the context was cancelled")
	}
	_ = pw.Close()

	store, err := state.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runs, err := store.SetupRuns(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Step != "preflight" {
		t.Errorf("setup runs = %+v, want the imported report", runs)
	}

	if v, err := store.Setting(context.Background(), "setup_imported:run-1.json"); err != nil || v == "" {
		t.Errorf("import marker = %q, %v; want it recorded", v, err)
	}
}

func TestRunReportsAnUnusableStore(t *testing.T) {
	dir := t.TempDir()
	cfg := testServerConfig(t, dir)

	if err := os.WriteFile(filepath.Join(dir, "state"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), cfg, dir, "test", io.Discard, func(context.Context, Command) int { return 0 })
	if err == nil || !strings.Contains(err.Error(), "open state store") {
		t.Errorf("Run = %v, want it to name the store it could not open", err)
	}
}

func runOverSocket(t *testing.T, path string, args ...string) string {
	t.Helper()
	conn, err := net.DialTimeout("unix", path, 10*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	cconn, chans, reqs, err := gossh.NewClientConn(conn, path, &gossh.ClientConfig{
		User:            "caramelo",
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("handshake on %s: %v", path, err)
	}
	client := gossh.NewClient(cconn, chans, reqs)
	defer func() { _ = client.Close() }()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("open a session: %v", err)
	}
	defer func() { _ = sess.Close() }()
	out, err := sess.Output(strings.Join(args, " "))
	if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return string(out)
}

func logLines(r io.Reader) <-chan string {
	ch := make(chan string, 64)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			select {
			case ch <- sc.Text():
			default:
			}
		}
	}()
	return ch
}

func waitForLine(t *testing.T, lines <-chan string, want string) string {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("the daemon log ended before %q", want)
			}
			if strings.Contains(line, want) {
				return line
			}
		case <-deadline:
			t.Fatalf("no daemon log line contained %q", want)
		}
	}
}

func testServerConfig(t *testing.T, dir string) serverconfig.Config {
	t.Helper()
	cfg := serverconfig.Default()
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.RunDir = filepath.Join(testutil.ShortDir(t), "run")
	cfg.Bind = "127.0.0.1"
	cfg.SSHPort = 0

	cfg.APIListen = serverconfig.APIListenPublic
	return cfg
}
