package git

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
)

func configureArgv(repo string) []string {
	return []string{
		"git -C " + repo + " config --local --get receive.denyCurrentBranch",
		"git -C " + repo + " config --local receive.denyCurrentBranch updateInstead",
		"cat " + repo + "/hooks/push-to-checkout",
		"install -d -m 0750 " + repo + "/hooks",
		"tee -- " + repo + "/hooks/push-to-checkout",
		"chmod 0755 " + repo + "/hooks/push-to-checkout",
	}
}

func unconfigured() *fakeRunner {
	return (&fakeRunner{}).
		on("git -C "+repo+" config --local --get", fail(1, "")).
		on("cat "+repo+"/hooks/push-to-checkout", fail(1, "cat: no such file or directory"))
}

func configured() *fakeRunner {
	return (&fakeRunner{}).
		on("git -C "+repo+" config --local --get", ok("updateInstead\n")).
		on("cat "+repo+"/hooks/push-to-checkout", ok(PushToCheckoutHook))
}

func TestEnsureUpdateInsteadConfiguresAFreshRepository(t *testing.T) {
	f := unconfigured()
	changed, err := testCLI(f).EnsureUpdateInstead(context.Background(), repo)
	if err != nil || !changed {
		t.Fatalf("EnsureUpdateInstead() = %v, %v; want changed", changed, err)
	}
	wantArgv(t, f, configureArgv(repo)...)

	last := f.calls[len(f.calls)-2]
	if last.Stdin == nil {
		t.Fatal("the hook was not written from stdin")
	}
	body, err := readAll(last.Stdin)
	if err != nil || body != PushToCheckoutHook {
		t.Fatalf("stdin held %q, %v; want the hook", body, err)
	}
}

func TestEnsureUpdateInsteadIsIdempotent(t *testing.T) {
	f := configured()
	changed, err := testCLI(f).EnsureUpdateInstead(context.Background(), repo)
	if err != nil || changed {
		t.Fatalf("EnsureUpdateInstead() on a configured repository = %v, %v; want no change", changed, err)
	}
	wantArgv(t, f,
		"git -C "+repo+" config --local --get receive.denyCurrentBranch",
		"cat "+repo+"/hooks/push-to-checkout")
}

func TestEnsureUpdateInsteadAcceptsAnyCasing(t *testing.T) {
	f := (&fakeRunner{}).
		on("git -C "+repo+" config --local --get", ok("UPDATEINSTEAD\n")).
		on("cat "+repo+"/hooks/push-to-checkout", ok(PushToCheckoutHook))
	if changed, err := testCLI(f).EnsureUpdateInstead(context.Background(), repo); err != nil || changed {
		t.Fatalf("EnsureUpdateInstead() = %v, %v; want no change", changed, err)
	}
}

func TestEnsureUpdateInsteadRewritesWhatItFinds(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    *fakeRunner
		want []string
	}{
		{
			"another value",
			(&fakeRunner{}).
				on("git -C "+repo+" config --local --get", ok("refuse\n")).
				on("cat "+repo+"/hooks/push-to-checkout", ok(PushToCheckoutHook)),
			[]string{
				"git -C " + repo + " config --local --get receive.denyCurrentBranch",
				"git -C " + repo + " config --local receive.denyCurrentBranch updateInstead",
				"cat " + repo + "/hooks/push-to-checkout",
			},
		},
		{
			"an older hook",
			(&fakeRunner{}).
				on("git -C "+repo+" config --local --get", ok("updateInstead\n")).
				on("cat "+repo+"/hooks/push-to-checkout", ok("#!/bin/sh\n# an older caramelo\n")),
			[]string{
				"git -C " + repo + " config --local --get receive.denyCurrentBranch",
				"cat " + repo + "/hooks/push-to-checkout",
				"install -d -m 0750 " + repo + "/hooks",
				"tee -- " + repo + "/hooks/push-to-checkout",
				"chmod 0755 " + repo + "/hooks/push-to-checkout",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed, err := testCLI(tc.f).EnsureUpdateInstead(context.Background(), repo)
			if err != nil || !changed {
				t.Fatalf("EnsureUpdateInstead() = %v, %v; want changed", changed, err)
			}
			wantArgv(t, tc.f, tc.want...)
		})
	}
}

func TestEnsureUpdateInsteadReportsGitsMessage(t *testing.T) {
	f := (&fakeRunner{}).on("git -C "+repo+" config --local --get",
		fail(128, "fatal: not in a git directory"))
	if _, err := testCLI(f).EnsureUpdateInstead(context.Background(), repo); err == nil ||
		!strings.Contains(err.Error(), "not in a git directory") {
		t.Fatalf("EnsureUpdateInstead() = %v, want git's message", err)
	}
	if _, err := testCLI(&fakeRunner{}).EnsureUpdateInstead(context.Background(), ""); err == nil {
		t.Fatal("EnsureUpdateInstead of no repository succeeded")
	}
	broken := unconfigured().on("tee --", fail(1, "tee: cannot create regular file: Permission denied"))
	if _, err := testCLI(broken).EnsureUpdateInstead(context.Background(), repo); err == nil ||
		!strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("EnsureUpdateInstead() with an unwritable hooks directory = %v", err)
	}
}

type fakeUpdater struct {
	changed map[string]bool
	errs    map[string]error
	seen    []string
}

func (f *fakeUpdater) EnsureUpdateInstead(_ context.Context, repo string) (bool, error) {
	f.seen = append(f.seen, repo)
	if err := f.errs[repo]; err != nil {
		return false, err
	}
	return f.changed[repo], nil
}

func TestFixUpUpdateInstead(t *testing.T) {
	u := &fakeUpdater{
		changed: map[string]bool{"/a": true, "/c": true},
		errs:    map[string]error{"/b": errors.New("no such file")},
	}
	fixed, err := FixUpUpdateInstead(context.Background(), u, []string{"/a", "/b", "/c"})
	if fixed != 2 {
		t.Errorf("fixed = %d, want 2", fixed)
	}
	if err == nil || !strings.Contains(err.Error(), "/b") {
		t.Errorf("err = %v, want it to name the repository that failed", err)
	}
	if len(u.seen) != 3 {
		t.Errorf("visited %v, want every repository even after a failure", u.seen)
	}

	quiet := &fakeUpdater{}
	if fixed, err := FixUpUpdateInstead(context.Background(), quiet, []string{"/a"}); fixed != 0 || err != nil {
		t.Errorf("FixUpUpdateInstead() on a configured machine = %d, %v; want 0, nil", fixed, err)
	}
	if _, err := FixUpUpdateInstead(context.Background(), nil, []string{"/a"}); err == nil {
		t.Error("FixUpUpdateInstead without a driver succeeded")
	}
}

func TestPushUpdatesTheWorktree(t *testing.T) {
	c, raw := realGit(t)
	ctx := context.Background()
	root := t.TempDir()
	bare := filepath.Join(root, "apps", "shop", "repo.git")
	laptop := filepath.Join(root, "laptop")
	wt := filepath.Join(root, "apps", "shop", "envs", "feat-x", "src")

	if err := c.InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	firstCommit(t, raw, laptop, "app.py", "first\n")
	mustRun(t, raw, laptop, "git", "push", "--quiet", bare, "main")

	if err := c.CreateBranch(ctx, bare, "feat-x", "main"); err != nil {
		t.Fatal(err)
	}
	if err := c.WorktreeAdd(ctx, bare, wt, "feat-x"); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(wt, "__pycache__"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wt, "__pycache__", "app.pyc"), "compiled\n")

	commit(t, raw, laptop, "app.py", "second\n", "second")
	mustRun(t, raw, laptop, "git", "push", "--quiet", bare, "main:feat-x")

	if got := read(t, filepath.Join(wt, "app.py")); got != "second\n" {
		t.Fatalf("the worktree still holds %q: the push did not update it", got)
	}
	if got := read(t, filepath.Join(wt, "__pycache__", "app.pyc")); got != "compiled\n" {
		t.Errorf("the push removed the toolchain's untracked output: %q", got)
	}
	if got := mustRun(t, raw, wt, "git", "status", "--porcelain", "--untracked-files=no"); got != "" {
		t.Errorf("the worktree is dirty after the push:\n%s", got)
	}
	if head := mustRun(t, raw, wt, "git", "rev-parse", "HEAD"); head != mustRun(t, raw, laptop, "git", "rev-parse", "HEAD") {
		t.Error("the worktree's HEAD did not move with the push")
	}

	write(t, filepath.Join(wt, "app.py"), "the agent was here\n")
	commit(t, raw, laptop, "app.py", "third\n", "third")

	res, err := raw.Run(ctx, runner.Cmd{
		Name: "git", Args: []string{"push", bare, "main:feat-x"}, Dir: laptop,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Fatal("a push over a dirty worktree succeeded; the agent's work was overwritten")
	}
	if !strings.Contains(res.Stderr, wt) {
		t.Errorf("the refusal does not name the worktree:\n%s", res.Stderr)
	}
	if got := read(t, filepath.Join(wt, "app.py")); got != "the agent was here\n" {
		t.Errorf("the worktree holds %q, want the agent's uncommitted edit", got)
	}

	if got, err := c.RevParse(ctx, bare, "feat-x"); err != nil ||
		got != mustRun(t, raw, wt, "git", "rev-parse", "HEAD") {
		t.Errorf("the branch moved although the push was refused: %v, %v", got, err)
	}

	mustRun(t, raw, wt, "git", "add", "app.py")
	mustRun(t, raw, wt, "git", "commit", "--quiet", "-m", "the agent's work")
	mustRun(t, raw, laptop, "git", "push", "--quiet", "--force", bare, "main:feat-x")
	if got := read(t, filepath.Join(wt, "app.py")); got != "third\n" {
		t.Errorf("the worktree holds %q after a forced push, want the pushed content", got)
	}

	if changed, err := c.EnsureUpdateInstead(ctx, bare); err != nil || changed {
		t.Errorf("EnsureUpdateInstead() on a repository InitBare made = %v, %v; want no change", changed, err)
	}
}

func TestFixUpMakesAnM3RepositoryAcceptPushes(t *testing.T) {
	c, raw := realGit(t)
	ctx := context.Background()
	root := t.TempDir()
	bare := filepath.Join(root, "repo.git")
	laptop := filepath.Join(root, "laptop")
	wt := filepath.Join(root, "wt")

	mustRun(t, raw, root, "git", "init", "--bare", "--quiet", bare)
	firstCommit(t, raw, laptop, "f", "one\n")
	mustRun(t, raw, laptop, "git", "push", "--quiet", bare, "main")
	if err := c.WorktreeAdd(ctx, bare, wt, "main"); err != nil {
		t.Fatal(err)
	}

	commit(t, raw, laptop, "f", "two\n", "two")
	res, err := raw.Run(ctx, runner.Cmd{Name: "git", Args: []string{"push", bare, "main"}, Dir: laptop})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Fatal("git accepted a push to a checked-out branch unconfigured; the fix-up would be pointless")
	}

	fixed, err := FixUpUpdateInstead(ctx, c, []string{bare})
	if err != nil || fixed != 1 {
		t.Fatalf("FixUpUpdateInstead() = %d, %v; want 1, nil", fixed, err)
	}
	mustRun(t, raw, laptop, "git", "push", "--quiet", bare, "main")
	if got := read(t, filepath.Join(wt, "f")); got != "two\n" {
		t.Errorf("the worktree holds %q, want the pushed content", got)
	}

	if fixed, err := FixUpUpdateInstead(ctx, c, []string{bare}); err != nil || fixed != 0 {
		t.Errorf("FixUpUpdateInstead() twice = %d, %v; want 0, nil", fixed, err)
	}
}

func firstCommit(t *testing.T, r runner.Runner, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustRun(t, r, dir, "git", "init", "--quiet", "--initial-branch=main")
	write(t, filepath.Join(dir, name), content)
	mustRun(t, r, dir, "git", "add", name)
	mustRun(t, r, dir, "git", "commit", "--quiet", "-m", "first")
}

func commit(t *testing.T, r runner.Runner, dir, name, content, message string) {
	t.Helper()
	write(t, filepath.Join(dir, name), content)
	mustRun(t, r, dir, "git", "add", name)
	mustRun(t, r, dir, "git", "commit", "--quiet", "-m", message)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func readAll(r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	return string(b), err
}
