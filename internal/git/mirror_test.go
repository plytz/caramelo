package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
)

func TestEnsureMirrorAddsMovesAndLeavesTheRemoteAlone(t *testing.T) {
	ctx := context.Background()
	url := "ssh://caramelo@10.86.0.1:4022/shop"

	f := (&fakeRunner{}).on("git -C /repo remote get-url origin", runner.Result{ExitCode: 2})
	c := NewCLI(f, "caramelo")
	changed, err := c.EnsureMirror(ctx, "/repo", url)
	if err != nil || !changed {
		t.Fatalf("EnsureMirror = %v, %v; want a change", changed, err)
	}
	if got := cmdKey(f.calls[1]); got != "git -C /repo remote add origin "+url {
		t.Fatalf("second call = %q, want the remote added", got)
	}

	if got := cmdKey(f.calls[2]); !strings.Contains(got, "config remote.origin.fetch +refs/heads/*:refs/remotes/origin/*") {
		t.Fatalf("third call = %q, want the mirror refspec", got)
	}

	f = (&fakeRunner{}).on("git -C /repo remote get-url origin", runner.Result{Stdout: url + "\n"})
	c = NewCLI(f, "caramelo")
	if changed, err := c.EnsureMirror(ctx, "/repo", url); err != nil || changed {
		t.Fatalf("EnsureMirror twice = %v, %v; want no change", changed, err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("a mirror that was already right ran %d commands", len(f.calls))
	}

	f = (&fakeRunner{}).on("git -C /repo remote get-url origin", runner.Result{Stdout: "ssh://caramelo@10.86.0.1:4022/old\n"})
	c = NewCLI(f, "caramelo")
	if changed, err := c.EnsureMirror(ctx, "/repo", url); err != nil || !changed {
		t.Fatalf("EnsureMirror after a move = %v, %v; want a change", changed, err)
	}
	if got := cmdKey(f.calls[1]); got != "git -C /repo remote set-url origin "+url {
		t.Fatalf("second call = %q, want the URL moved", got)
	}
}

func TestFetchBranchSaysWhenTheHubHasNoSuchBranch(t *testing.T) {
	f := (&fakeRunner{}).on("git -C /repo fetch --no-tags origin +refs/heads/feat-x:refs/remotes/origin/feat-x",
		runner.Result{ExitCode: 128, Stderr: "fatal: couldn't find remote ref refs/heads/feat-x\n"})
	c := NewCLI(f, "caramelo")
	err := c.FetchBranch(context.Background(), "/repo", "feat-x")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("FetchBranch = %v, want ErrNotFound", err)
	}
}

func TestMirrorURLIsTheHubsRepositoryInsideTheTunnel(t *testing.T) {
	if got := MirrorURL("10.86.0.1:4022", "shop"); got != "ssh://caramelo@10.86.0.1:4022/shop" {
		t.Fatalf("MirrorURL = %q", got)
	}
}

func TestAMirrorFollowsTheHubAgainstARealGit(t *testing.T) {
	c, raw := realGit(t)
	ctx := context.Background()
	root := t.TempDir()
	hub := filepath.Join(root, "hub", "shop.git")
	member := filepath.Join(root, "member", "shop.git")
	laptop := filepath.Join(root, "laptop")

	if err := c.InitBare(ctx, hub); err != nil {
		t.Fatal(err)
	}
	if err := c.InitBare(ctx, member); err != nil {
		t.Fatal(err)
	}
	firstCommit(t, raw, laptop, "app.py", "first\n")
	mustRun(t, raw, laptop, "git", "push", "--quiet", hub, "main")
	mustRun(t, raw, laptop, "git", "push", "--quiet", hub, "main:feat-x")

	if changed, err := c.EnsureMirror(ctx, member, hub); err != nil || !changed {
		t.Fatalf("EnsureMirror = %v, %v", changed, err)
	}
	if changed, err := c.EnsureMirror(ctx, member, hub); err != nil || changed {
		t.Fatalf("EnsureMirror twice = %v, %v; want it idempotent", changed, err)
	}

	if err := c.FetchBranch(ctx, member, "feat-x"); err != nil {
		t.Fatalf("FetchBranch: %v", err)
	}

	if _, err := c.RevParse(ctx, member, "origin/feat-x"); err != nil {
		t.Fatalf("origin/feat-x on the member: %v", err)
	}
	if ok, err := c.BranchExists(ctx, member, "feat-x"); err != nil || ok {
		t.Fatalf("a fetch made a local head feat-x = %v, %v; it must not", ok, err)
	}
	if _, err := c.RevParse(ctx, member, "origin/main"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fetching one branch brought others: origin/main = %v", err)
	}
	if err := c.FetchBranch(ctx, member, "no-such-branch"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FetchBranch of a branch the hub does not have = %v, want ErrNotFound", err)
	}

	commit(t, raw, laptop, "app.py", "second\n", "second")
	mustRun(t, raw, laptop, "git", "push", "--quiet", hub, "main:feat-x")
	if err := c.FetchMirror(ctx, member); err != nil {
		t.Fatalf("FetchMirror: %v", err)
	}
	hubHead, err := c.RevParse(ctx, hub, "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	memberHead, err := c.RevParse(ctx, member, "origin/feat-x")
	if err != nil {
		t.Fatal(err)
	}
	if hubHead != memberHead {
		t.Fatalf("the member is at %s and the hub at %s", memberHead, hubHead)
	}
}

func TestDirtyIsWhatAPushAcrossTwoMachinesIsRefusedFor(t *testing.T) {
	c, raw := realGit(t)
	ctx := context.Background()
	root := t.TempDir()
	bare := filepath.Join(root, "shop.git")
	wt := filepath.Join(root, "envs", "feat-x", "src")

	if err := c.InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	firstCommit(t, raw, filepath.Join(root, "laptop"), "app.py", "first\n")
	mustRun(t, raw, filepath.Join(root, "laptop"), "git", "push", "--quiet", bare, "main")
	if err := c.CreateBranch(ctx, bare, "feat-x", "main"); err != nil {
		t.Fatal(err)
	}
	if err := c.WorktreeAdd(ctx, bare, wt, "feat-x"); err != nil {
		t.Fatal(err)
	}

	dirty, changed, err := c.Dirty(ctx, wt)
	if err != nil || dirty {
		t.Fatalf("Dirty(a fresh worktree) = %v, %v, %v; want clean", dirty, changed, err)
	}

	if err := os.MkdirAll(filepath.Join(wt, "__pycache__"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wt, "__pycache__", "app.pyc"), "compiled\n")
	if dirty, _, err := c.Dirty(ctx, wt); err != nil || dirty {
		t.Fatalf("Dirty(untracked output) = %v, %v; want clean", dirty, err)
	}

	write(t, filepath.Join(wt, "app.py"), "an agent was here\n")
	dirty, changed, err = c.Dirty(ctx, wt)
	if err != nil || !dirty {
		t.Fatalf("Dirty(uncommitted work) = %v, %v; want dirty", dirty, err)
	}
	if len(changed) != 1 || !strings.Contains(changed[0], "app.py") {
		t.Fatalf("changed = %v, want the file named", changed)
	}
}

func TestThePreReceiveHookIsInstalledAndIdempotent(t *testing.T) {
	c, _ := realGit(t)
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "shop.git")
	if err := c.InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	changed, err := c.EnsurePreReceive(ctx, bare)
	if err != nil || !changed {
		t.Fatalf("EnsurePreReceive = %v, %v; want it installed", changed, err)
	}
	if changed, err := c.EnsurePreReceive(ctx, bare); err != nil || changed {
		t.Fatalf("EnsurePreReceive twice = %v, %v; want no change", changed, err)
	}
	path := filepath.Join(bare, "hooks", "pre-receive")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the hook: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("the hook is not executable (%v): git would ignore it", info.Mode())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != PreReceiveHook {
		t.Fatal("the hook on disk is not the one this build ships")
	}

	if !strings.Contains(PreReceiveHook, "command -v caramelo") {
		t.Fatal("the hook does not tolerate a machine with no caramelo on PATH")
	}
}
