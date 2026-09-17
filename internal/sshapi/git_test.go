package sshapi

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/git"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/state"
)

func TestParseGit(t *testing.T) {
	tests := []struct {
		name string
		args []string
		kind gitKind
		verb string
		path string
	}{
		{"push, the spelling git sends", []string{"git-receive-pack", "/shop.git"}, gitServe, verbReceivePack, "/shop.git"},
		{"fetch, the spelling git sends", []string{"git-upload-pack", "/shop.git"}, gitServe, verbUploadPack, "/shop.git"},
		{"the two-token spelling", []string{"git", "upload-pack", "/shop"}, gitServe, verbUploadPack, "/shop"},
		{"two-token push", []string{"git", "receive-pack", "shop.git"}, gitServe, verbReceivePack, "shop.git"},
		{"quoted path as a client may send it", []string{"git-upload-pack", "'/shop.git'"}, gitServe, verbUploadPack, "'/shop.git'"},
		{"options are ignored", []string{"git-upload-pack", "--strict", "/shop.git"}, gitServe, verbUploadPack, "/shop.git"},

		{"archives stay refused", []string{"git-upload-archive", "/shop.git"}, gitRefused, "upload-archive", ""},
		{"two-token archive stays refused", []string{"git", "upload-archive", "/shop.git"}, gitRefused, "upload-archive", ""},
		{"an arbitrary git command is refused", []string{"git", "log"}, gitRefused, "log", ""},
		{"bare git is refused", []string{"git"}, gitRefused, "", ""},
		{"no repository is refused", []string{"git-receive-pack"}, gitRefused, verbReceivePack, ""},
		{"two repositories are refused", []string{"git-receive-pack", "/a", "/b"}, gitRefused, verbReceivePack, ""},

		{"an ordinary command is not git", []string{"env", "list"}, notGit, "", ""},
		{"nothing is not git", nil, notGit, "", ""},
		{"a command that merely starts with git", []string{"gitless"}, notGit, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, kind := parseGit(tt.args)
			if kind != tt.kind {
				t.Fatalf("kind = %v, want %v (req %+v)", kind, tt.kind, req)
			}
			if kind == gitServe && (req.Verb != tt.verb || req.Path != tt.path) {
				t.Errorf("got verb %q path %q, want %q %q", req.Verb, req.Path, tt.verb, tt.path)
			}
			if kind == gitRefused && req.Refused == "" {
				t.Error("a refused request must say why")
			}
		})
	}
}

func TestAppFromGitPath(t *testing.T) {
	ok := map[string]string{
		"/shop":       "shop",
		"/shop.git":   "shop",
		"'/shop.git'": "shop",
		`"/shop.git"`: "shop",
		"shop":        "shop",
		"shop.git":    "shop",
		"~/shop.git":  "shop",
		"/shop/":      "shop",
		" /shop.git ": "shop",
		"/my-app-2":   "my-app-2",
	}
	for in, want := range ok {
		got, err := appFromGitPath(in)
		if err != nil {
			t.Errorf("appFromGitPath(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("appFromGitPath(%q) = %q, want %q", in, got, want)
		}
	}
	bad := []string{"", "/", "'/'", "/Shop", "/shop app", "/a/b", "/../etc", "/-shop", "/" + strings.Repeat("x", 40)}
	for _, in := range bad {
		if got, err := appFromGitPath(in); err == nil {
			t.Errorf("appFromGitPath(%q) = %q, want an error", in, got)
		}
	}
}

type gitStore struct {
	state.Store

	mu   sync.Mutex
	apps map[string]state.App

	addErr error
}

func newGitStore(apps ...state.App) *gitStore {
	s := &gitStore{apps: map[string]state.App{}}
	for _, a := range apps {
		s.apps[a.Name] = a
	}
	return s
}

func (s *gitStore) App(_ context.Context, name string) (*state.App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[name]
	if !ok {
		return nil, state.ErrNotFound
	}
	return &a, nil
}

func (s *gitStore) AddApp(_ context.Context, a state.App) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.addErr != nil {
		return s.addErr
	}
	if _, ok := s.apps[a.Name]; ok {
		return state.ErrExists
	}
	s.apps[a.Name] = a
	return nil
}

func (s *gitStore) SetAppDefaultBranch(_ context.Context, name, branch string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[name]
	if !ok {
		return state.ErrNotFound
	}
	a.DefaultBranch = branch
	s.apps[name] = a
	return nil
}

type scriptRunner struct {
	mu    sync.Mutex
	cmds  []runner.Cmd
	reply func(c runner.Cmd) (runner.Result, error)
}

func (r *scriptRunner) Run(_ context.Context, c runner.Cmd) (runner.Result, error) {
	r.mu.Lock()
	r.cmds = append(r.cmds, c)
	r.mu.Unlock()
	if r.reply == nil {
		return runner.Result{}, nil
	}
	return r.reply(c)
}

func (r *scriptRunner) ran(want ...string) *runner.Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.cmds {
		if containsArgs(r.cmds[i].Args, want) {
			return &r.cmds[i]
		}
	}
	return nil
}

func (r *scriptRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cmds)
}

func containsArgs(args, want []string) bool {
	if len(want) > len(args) {
		return false
	}
	for i := 0; i+len(want) <= len(args); i++ {
		match := true
		for j := range want {
			if args[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func gitDaemon(t *testing.T, store state.Store, run runner.Runner) *Daemon {
	t.Helper()
	cfg := serverconfig.Default()
	cfg.APIListen = serverconfig.APIListenPublic
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	return &Daemon{Config: cfg, Store: store, Runner: run, Version: "test"}
}

type gitSession struct {
	stdin          *bytes.Buffer
	stdout, stderr bytes.Buffer
}

func newGitSession() *gitSession {
	return &gitSession{stdin: bytes.NewBufferString("push data")}
}

func (s *gitSession) req(verb, path string) GitRequest {
	return GitRequest{Verb: verb, Path: path, Stdin: s.stdin, Stdout: &s.stdout, Stderr: &s.stderr}
}

func TestServeGitPushCreatesTheApp(t *testing.T) {
	store := newGitStore()
	run := &scriptRunner{reply: func(c runner.Cmd) (runner.Result, error) {
		switch {
		case containsArgs(c.Args, []string{"symbolic-ref", "--quiet", "HEAD"}):

			return runner.Result{Stdout: "refs/heads/master\n"}, nil
		case containsArgs(c.Args, []string{"rev-parse"}):
			return runner.Result{ExitCode: 1}, nil
		case containsArgs(c.Args, []string{"for-each-ref"}):
			return runner.Result{Stdout: "feature\nmain\n"}, nil
		}
		return runner.Result{}, nil
	}}
	d := gitDaemon(t, store, run)
	sess := newGitSession()

	if code := d.ServeGit(context.Background(), sess.req(verbReceivePack, "'/shop.git'")); code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, sess.stderr.String())
	}

	repo := filepath.Join(d.Config.DataDir, "apps", "shop", "repo.git")
	app, err := store.App(context.Background(), "shop")
	if err != nil {
		t.Fatalf("the push did not create the app: %v", err)
	}
	if app.RepoPath != repo {
		t.Errorf("repo path = %q, want %q", app.RepoPath, repo)
	}

	if app.DefaultBranch != "main" {
		t.Errorf("default branch = %q, want main", app.DefaultBranch)
	}

	init := run.ran("init", "--bare")
	if init == nil {
		t.Fatal("the bare repository was never created")
	}
	if !containsArgs(init.Args, []string{repo}) {
		t.Errorf("git init args = %v, want them to name %s", init.Args, repo)
	}
	if init.User != d.Config.User {
		t.Errorf("git init ran as %q, want the Caramelo user %q", init.User, d.Config.User)
	}

	serve := run.ran(verbReceivePack, repo)
	if serve == nil {
		t.Fatalf("git receive-pack never ran; commands: %+v", run.cmds)
	}

	if serve.Stdin != sess.stdin || serve.Stdout != &sess.stdout || serve.Stderr != &sess.stderr {
		t.Error("git was not wired to the session's streams")
	}
	if serve.User != d.Config.User {
		t.Errorf("git ran as %q, want %q", serve.User, d.Config.User)
	}
	if pointed := run.ran("symbolic-ref", "HEAD", "refs/heads/main"); pointed == nil {
		t.Error("HEAD was left dangling")
	}
}

func TestServeGitPushKeepsAnExistingHEAD(t *testing.T) {
	store := newGitStore(state.App{Name: "shop", RepoPath: "/data/apps/shop/repo.git"})
	run := &scriptRunner{reply: func(c runner.Cmd) (runner.Result, error) {
		switch {
		case containsArgs(c.Args, []string{"symbolic-ref", "--quiet", "HEAD"}):
			return runner.Result{Stdout: "refs/heads/trunk\n"}, nil
		case containsArgs(c.Args, []string{"rev-parse"}):
			return runner.Result{Stdout: "0123456789abcdef0123456789abcdef01234567\n"}, nil
		}
		return runner.Result{}, nil
	}}
	d := gitDaemon(t, store, run)
	sess := newGitSession()

	if code := d.ServeGit(context.Background(), sess.req(verbReceivePack, "/shop")); code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, sess.stderr.String())
	}
	if run.ran("for-each-ref") != nil {
		t.Error("a HEAD that resolves must not be repointed")
	}
	app, _ := store.App(context.Background(), "shop")
	if app.DefaultBranch != "trunk" {
		t.Errorf("default branch = %q, want trunk", app.DefaultBranch)
	}
}

func TestServeGitFetchNeedsTheApp(t *testing.T) {
	store := newGitStore()
	run := &scriptRunner{}
	d := gitDaemon(t, store, run)
	sess := newGitSession()

	code := d.ServeGit(context.Background(), sess.req(verbUploadPack, "/shop.git"))
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(sess.stderr.String(), "no such app") {
		t.Errorf("stderr = %q, want it to say the app is unknown", sess.stderr.String())
	}
	if run.count() != 0 {
		t.Errorf("nothing should have run: %+v", run.cmds)
	}

	if _, err := store.App(context.Background(), "shop"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("the app was created by a fetch: %v", err)
	}
}

func TestServeGitFetchServesAKnownApp(t *testing.T) {
	store := newGitStore(state.App{Name: "shop", RepoPath: "/data/apps/shop/repo.git"})
	run := &scriptRunner{}
	d := gitDaemon(t, store, run)
	sess := newGitSession()

	if code := d.ServeGit(context.Background(), sess.req(verbUploadPack, "/shop.git")); code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, sess.stderr.String())
	}
	if run.ran(verbUploadPack) == nil {
		t.Fatal("git upload-pack never ran")
	}

	if run.ran("init", "--bare") != nil || run.ran("symbolic-ref") != nil {
		t.Errorf("a fetch touched the repository: %+v", run.cmds)
	}
}

func TestServeGitRejectsABadPath(t *testing.T) {
	store := newGitStore()
	run := &scriptRunner{}
	d := gitDaemon(t, store, run)
	for _, path := range []string{"/Shop", "/a/b", "/", "/../etc/passwd"} {
		sess := newGitSession()
		code := d.ServeGit(context.Background(), sess.req(verbReceivePack, path))
		if code != 2 {
			t.Errorf("%q: exit = %d, want 2 (usage)", path, code)
		}
		if sess.stderr.Len() == 0 {
			t.Errorf("%q: nothing on stderr", path)
		}
	}
	if run.count() != 0 {
		t.Errorf("a rejected path still ran something: %+v", run.cmds)
	}
	if len(store.apps) != 0 {
		t.Errorf("a rejected path created an app: %+v", store.apps)
	}
}

func TestServeGitPassesGitsExitCode(t *testing.T) {
	store := newGitStore(state.App{Name: "shop", RepoPath: "/data/apps/shop/repo.git"})
	run := &scriptRunner{reply: func(c runner.Cmd) (runner.Result, error) {
		if containsArgs(c.Args, []string{verbReceivePack}) {
			return runner.Result{ExitCode: 128, Stderr: "fatal: not a repository\n"}, nil
		}
		return runner.Result{}, nil
	}}
	d := gitDaemon(t, store, run)
	sess := newGitSession()

	if code := d.ServeGit(context.Background(), sess.req(verbReceivePack, "/shop")); code != 128 {
		t.Fatalf("exit = %d, want git's own 128", code)
	}

	if run.ran("symbolic-ref") != nil {
		t.Error("HEAD was settled after a failed push")
	}
	if app, _ := store.App(context.Background(), "shop"); app.DefaultBranch != "" {
		t.Errorf("default branch = %q after a failed push", app.DefaultBranch)
	}
}

func TestServeGitReportsAGitThatCannotRun(t *testing.T) {
	store := newGitStore(state.App{Name: "shop", RepoPath: "/data/apps/shop/repo.git"})
	run := &scriptRunner{reply: func(c runner.Cmd) (runner.Result, error) {
		return runner.Result{}, errors.New("exec: git: not found")
	}}
	d := gitDaemon(t, store, run)
	sess := newGitSession()

	if code := d.ServeGit(context.Background(), sess.req(verbUploadPack, "/shop")); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(sess.stderr.String(), "not found") {
		t.Errorf("stderr = %q, want it to carry git's failure", sess.stderr.String())
	}
}

func TestServeGitSurvivesAFailureToSettleHEAD(t *testing.T) {

	store := newGitStore(state.App{Name: "shop", RepoPath: "/data/apps/shop/repo.git"})
	run := &scriptRunner{reply: func(c runner.Cmd) (runner.Result, error) {
		if containsArgs(c.Args, []string{"symbolic-ref"}) {
			return runner.Result{}, errors.New("git is gone")
		}
		return runner.Result{}, nil
	}}
	d := gitDaemon(t, store, run)
	sess := newGitSession()

	if code := d.ServeGit(context.Background(), sess.req(verbReceivePack, "/shop")); code != 0 {
		t.Fatalf("exit = %d, want 0: the push itself succeeded", code)
	}
	if !strings.Contains(sess.stderr.String(), "warning") {
		t.Errorf("stderr = %q, want a warning", sess.stderr.String())
	}
}

type pushStore struct {
	state.Store

	mu     sync.Mutex
	envs   []state.EnvRecord
	events []state.EnvEvent
	pushes []state.EnvRecord
}

func (s *pushStore) Env(_ context.Context, app, name string) (*state.EnvRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.envs {
		if s.envs[i].App == app && s.envs[i].Name == name {
			e := s.envs[i]
			return &e, nil
		}
	}
	return nil, state.ErrNotFound
}

func (s *pushStore) RecordEnvPush(_ context.Context, id int64, commit, sourceBranch, pushedBy string,
	at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushes = append(s.pushes, state.EnvRecord{
		ID: id, Commit: commit, SourceBranch: sourceBranch, PushedBy: pushedBy, PushedAt: at,
	})
	return nil
}

func (s *pushStore) AddEvent(_ context.Context, e state.EnvEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

func (s *pushStore) seen() ([]state.EnvRecord, []state.EnvEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]state.EnvRecord(nil), s.pushes...), append([]state.EnvEvent(nil), s.events...)
}

func pushDaemon(t *testing.T, write func(path string)) (*Daemon, *pushStore, *scriptRunner, *gitSession) {
	t.Helper()
	store := newGitStore(state.App{Name: "shop"})
	run := &scriptRunner{reply: func(c runner.Cmd) (runner.Result, error) {
		if write != nil && len(c.Args) > 0 && c.Args[0] == verbReceivePack {
			for _, kv := range c.Env {
				if path, ok := strings.CutPrefix(kv, git.PushRecordEnv+"="); ok {
					write(path)
				}
			}
		}
		if containsArgs(c.Args, []string{"symbolic-ref", "--quiet", "HEAD"}) {
			return runner.Result{Stdout: "refs/heads/main\n"}, nil
		}
		return runner.Result{}, nil
	}}
	d := gitDaemon(t, store, run)
	pushes := &pushStore{envs: []state.EnvRecord{{ID: 3, App: "shop", Name: "feat-x", Branch: "feat-x"}}}
	d.EnvManager = &env.Manager{Store: pushes}
	return d, pushes, run, newGitSession()
}

func TestServeGitHandsReceivePackAPlaceToWriteThePushDown(t *testing.T) {
	d, pushes, run, sess := pushDaemon(t, nil)

	if code := d.ServeGit(context.Background(), sess.req(verbReceivePack, "/shop")); code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, sess.stderr.String())
	}
	repo := filepath.Join(d.Config.DataDir, "apps", "shop", "repo.git")
	serve := run.ran(verbReceivePack, repo)
	if serve == nil {
		t.Fatal("git receive-pack never ran")
	}
	var path string
	for _, kv := range serve.Env {
		if p, ok := strings.CutPrefix(kv, git.PushRecordEnv+"="); ok {
			path = p
		}
	}
	if path == "" {
		t.Fatalf("receive-pack was given %v, want %s", serve.Env, git.PushRecordEnv)
	}
	if dir := filepath.Dir(path); dir != filepath.Join(repo, git.PushRecordDir) {
		t.Errorf("the drop file lands in %s, want the repository's own %s", dir, git.PushRecordDir)
	}
	if _, err := os.Stat(path); err == nil {
		t.Errorf("%s was left behind", path)
	}

	if fetch := newGitSession(); d.ServeGit(context.Background(), fetch.req(verbUploadPack, "/shop")) == 0 {
		if got := run.ran(verbUploadPack, repo); got != nil && len(got.Env) != 0 {
			t.Errorf("a fetch was given %v; only a push writes anything down", got.Env)
		}
	}

	if recorded, events := pushes.seen(); len(recorded) != 0 || len(events) != 0 {
		t.Errorf("a push whose hook never ran recorded %+v and %+v; it must record nothing", recorded, events)
	}
}

func TestServeGitRecordsWhatTheHookWroteDown(t *testing.T) {
	const landed = "0123456789abcdef0123456789abcdef01234567"
	d, pushes, _, sess := pushDaemon(t, func(path string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("option caramelo.branch=blob-store\n"+
			"ref "+strings.Repeat("0", 40)+" "+landed+" refs/heads/feat-x\n"+
			"ref "+strings.Repeat("0", 40)+" "+landed+" refs/tags/v1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	ctx := WithSession(context.Background(), api.Session{Transport: "ssh", Identity: "alex@laptop"})

	if code := d.ServeGit(ctx, sess.req(verbReceivePack, "/shop")); code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, sess.stderr.String())
	}
	recorded, events := pushes.seen()
	if len(recorded) != 1 {
		t.Fatalf("recorded %+v, want the one branch that landed", recorded)
	}
	got := recorded[0]
	if got.ID != 3 || got.Commit != landed || got.SourceBranch != "blob-store" || got.PushedBy != "alex@laptop" {
		t.Errorf("recorded %+v", got)
	}
	if got.PushedAt.IsZero() {
		t.Error("the push was recorded with no time")
	}
	if len(events) != 1 || events[0].Action != "push" || events[0].Identity != "alex@laptop" {
		t.Errorf("events = %+v, want one push by the peer that pushed", events)
	}
	if sess.stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing said to the client", sess.stderr.String())
	}
}

func TestServeGitSurvivesAStoreThatCannotRecordThePush(t *testing.T) {
	const landed = "0123456789abcdef0123456789abcdef01234567"
	d, pushes, _, sess := pushDaemon(t, func(path string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path,
			[]byte("ref "+strings.Repeat("0", 40)+" "+landed+" refs/heads/feat-x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	pushes.envs = nil
	d.EnvManager.Store = &brokenPushStore{pushStore: pushes}

	if code := d.ServeGit(context.Background(), sess.req(verbReceivePack, "/shop")); code != 0 {
		t.Fatalf("exit = %d, want 0: recording is not allowed to fail a push that landed", code)
	}
	if !strings.Contains(sess.stderr.String(), "warning") {
		t.Errorf("stderr = %q, want a warning and nothing worse", sess.stderr.String())
	}
}

type brokenPushStore struct {
	*pushStore
}

func (s *brokenPushStore) Env(context.Context, string, string) (*state.EnvRecord, error) {
	return nil, errors.New("the database is locked")
}
