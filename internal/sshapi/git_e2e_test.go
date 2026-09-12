package sshapi_test

import (
	"context"
	"errors"
	"github.com/plytz/caramelo/internal/testutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/plytz/caramelo/internal/cli"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/sshapi"
	"github.com/plytz/caramelo/internal/state"
)

type asMe struct{ runner.Runner }

func (r asMe) Run(ctx context.Context, c runner.Cmd) (runner.Result, error) {
	c.User = ""
	return r.Runner.Run(ctx, c)
}

type gitLab struct {
	store state.Store
	data  string
	url   func(app string) string
	env   []string

	ssh func(args ...string) (int, string, string)
}

func startGitLab(t *testing.T) *gitLab {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git binary on this machine")
	}
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh binary on this machine")
	}

	dir := t.TempDir()
	cfg := serverconfig.Default()
	cfg.APIListen = serverconfig.APIListenPublic
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.RunDir = filepath.Join(testutil.ShortDir(t), "run")
	cfg.Bind = "127.0.0.1"
	cfg.SSHPort = 0
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		t.Fatal(err)
	}

	store, err := state.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	clientSigner, keyFile := newClientKey(t, dir)
	line := strings.TrimSpace(string(gossh.MarshalAuthorizedKey(clientSigner.PublicKey())))
	if _, err := sshapi.AppendKey(cfg.AuthorizedKeysPath(), line, "test-key"); err != nil {
		t.Fatal(err)
	}

	daemon := sshapi.NewDaemon(cfg, filepath.Join(dir, "etc"), store, asMe{runner.Exec{}}, "test")
	srv := &sshapi.Server{
		Config:   cfg,
		Service:  daemon,
		Version:  "test",
		Hostname: "testbox",
		Log:      &testWriter{t},
		Exec: func(ctx context.Context, c sshapi.Command) int {
			return cli.RunWith(ctx, c.Args, c.Stdout, c.Stderr, cli.Options{Service: c.Service, Session: c.Session})
		},
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not stop within 10s")
		}
	})

	host, port, err := net.SplitHostPort(srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	sshOpts := []string{
		"-F", "/dev/null",
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-i", keyFile,
	}
	lab := &gitLab{
		store: store,
		data:  cfg.DataDir,
		url: func(app string) string {
			return "ssh://" + serverconfig.DefaultUser + "@" + host + ":" + port + "/" + app
		},
		env: append(os.Environ(),
			"GIT_SSH_COMMAND="+strings.Join(append([]string{sshBin}, sshOpts...), " "),
			"GIT_TERMINAL_PROMPT=0",
		),
	}
	lab.ssh = func(args ...string) (int, string, string) {
		t.Helper()
		full := append(append([]string{}, sshOpts...), "-p", port,
			serverconfig.DefaultUser+"@"+host, "--")
		full = append(full, args...)
		cmd := exec.Command(sshBin, full...)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		code := 0
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("running ssh %v: %v", args, err)
			}
			code = ee.ExitCode()
		}
		t.Logf("ssh %v -> exit %d\nstdout: %q\nstderr: %q", args, code, stdout.String(), stderr.String())
		return code, stdout.String(), stderr.String()
	}
	return lab
}

func (l *gitLab) git(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = l.env
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running git %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	t.Logf("git %v (in %s) -> exit %d\nstdout: %q\nstderr: %q", args, dir, code, stdout.String(), stderr.String())
	return code, stdout.String(), stderr.String()
}

func (l *gitLab) sampleRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main", "."},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
	} {
		if code, _, stderr := l.git(t, dir, args...); code != 0 {
			t.Fatalf("git %v: exit %d, %s", args, code, stderr)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "caramelo.yaml"), []byte("name: shop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.git(t, dir, "add", ".")
	if code, _, stderr := l.git(t, dir, "commit", "--quiet", "-m", "first"); code != 0 {
		t.Fatalf("commit: %s", stderr)
	}
	if code, _, stderr := l.git(t, dir, "branch", "feature"); code != 0 {
		t.Fatalf("branch: %s", stderr)
	}
	return dir
}

func TestGitPushCreatesTheAppAndFetchBringsItBack(t *testing.T) {
	lab := startGitLab(t)
	repo := lab.sampleRepo(t)
	ctx := context.Background()

	if code, _, stderr := lab.git(t, repo, "push", lab.url("shop"), "main"); code != 0 {
		t.Fatalf("push: exit %d, %s", code, stderr)
	}
	app, err := lab.store.App(ctx, "shop")
	if err != nil {
		t.Fatalf("the push did not record the app: %v", err)
	}
	want := filepath.Join(lab.data, "apps", "shop", "repo.git")
	if app.RepoPath != want {
		t.Errorf("repo path = %q, want %q", app.RepoPath, want)
	}
	if _, err := os.Stat(filepath.Join(want, "HEAD")); err != nil {
		t.Fatalf("the bare repository was not created: %v", err)
	}

	if app.DefaultBranch != "main" {
		t.Errorf("default branch = %q, want main", app.DefaultBranch)
	}
	head, err := os.ReadFile(filepath.Join(want, "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(head)) != "ref: refs/heads/main" {
		t.Errorf("HEAD = %q", strings.TrimSpace(string(head)))
	}

	if code, _, stderr := lab.git(t, repo, "push", lab.url("shop"), "feature"); code != 0 {
		t.Fatalf("second push: exit %d, %s", code, stderr)
	}
	code, stdout, _ := lab.git(t, repo, "ls-remote", "--heads", lab.url("shop"))
	if code != 0 {
		t.Fatalf("ls-remote: exit %d", code)
	}
	for _, ref := range []string{"refs/heads/main", "refs/heads/feature"} {
		if !strings.Contains(stdout, ref) {
			t.Errorf("ls-remote = %q, want %s", stdout, ref)
		}
	}

	clone := t.TempDir()
	if code, _, stderr := lab.git(t, clone, "clone", "--quiet", lab.url("shop"), "shop"); code != 0 {
		t.Fatalf("clone: exit %d, %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(clone, "shop", "caramelo.yaml")); err != nil {
		t.Errorf("the clone is missing the pushed file: %v", err)
	}
}

func TestGitFetchFromAnUnknownAppFails(t *testing.T) {
	lab := startGitLab(t)
	dir := t.TempDir()
	code, _, stderr := lab.git(t, dir, "ls-remote", lab.url("nosuchapp"))
	if code == 0 {
		t.Fatal("fetching an app nobody pushed must fail")
	}
	if !strings.Contains(stderr, "no such app") {
		t.Errorf("stderr = %q, want it to say the app is unknown", stderr)
	}
	if _, err := os.Stat(filepath.Join(lab.data, "apps", "nosuchapp")); err == nil {
		t.Error("a fetch created the app's directory")
	}
}

func TestGitPushToANonSlugPathIsRefused(t *testing.T) {
	lab := startGitLab(t)
	repo := lab.sampleRepo(t)
	for _, app := range []string{"Shop", "shop/inner", "../etc"} {
		code, _, stderr := lab.git(t, repo, "push", lab.url(app), "main")
		if code == 0 {
			t.Errorf("push to %q succeeded; it must be refused", app)
		}
		if !strings.Contains(stderr, "caramelo:") {
			t.Errorf("push to %q: stderr = %q, want caramelod's own message", app, stderr)
		}
	}
	if apps, err := lab.store.Apps(context.Background()); err != nil || len(apps) != 0 {
		t.Errorf("apps = %+v, %v; want none", apps, err)
	}
}

func TestGitUploadArchiveIsRefused(t *testing.T) {
	lab := startGitLab(t)
	for _, args := range [][]string{
		{"git-upload-archive", "'/shop.git'"},
		{"git", "upload-archive", "'/shop.git'"},
	} {
		code, _, stderr := lab.ssh(args...)
		if code != cli.ExitUsage {
			t.Errorf("%v: exit = %d, want %d", args, code, cli.ExitUsage)
		}
		if !strings.Contains(stderr, "not available") {
			t.Errorf("%v: stderr = %q", args, stderr)
		}
	}
}
