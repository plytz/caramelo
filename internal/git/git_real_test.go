package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
)

type isolated struct {
	inner runner.Runner
	env   []string
}

func (i isolated) Run(ctx context.Context, c runner.Cmd) (runner.Result, error) {
	c.Env = append(append([]string(nil), i.env...), c.Env...)
	return i.inner.Run(ctx, c)
}

func realGit(t *testing.T) (*CLI, runner.Runner) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	r := isolated{
		inner: runner.Exec{},
		env: []string{
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=Caramelo Test",
			"GIT_AUTHOR_EMAIL=test@caramelo.invalid",
			"GIT_COMMITTER_NAME=Caramelo Test",
			"GIT_COMMITTER_EMAIL=test@caramelo.invalid",
		},
	}
	return NewCLI(r, ""), r
}

func mustRun(t *testing.T, r runner.Runner, dir, name string, args ...string) string {
	t.Helper()
	res, err := r.Run(context.Background(), runner.Cmd{Name: name, Args: args, Dir: dir})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("%s %s: %v (exit %d)\n%s", name, strings.Join(args, " "), err, res.ExitCode, res.Stderr)
	}
	return strings.TrimSpace(res.Stdout)
}

func TestAgainstRealGit(t *testing.T) {
	c, raw := realGit(t)
	ctx := context.Background()
	root := t.TempDir()
	bare := filepath.Join(root, "apps", "shop", "repo.git")
	src := filepath.Join(root, "src")

	if err := c.InitBare(ctx, bare); err != nil {
		t.Fatalf("InitBare() = %v", err)
	}
	if err := c.InitBare(ctx, bare); err != nil {
		t.Fatalf("InitBare() twice = %v, want it to be idempotent", err)
	}
	if ok, err := c.Exists(ctx, bare); err != nil || !ok {
		t.Fatalf("Exists(bare) = %v, %v; want true", ok, err)
	}
	if ok, err := c.Exists(ctx, filepath.Join(root, "nothing")); err != nil || ok {
		t.Fatalf("Exists(missing) = %v, %v; want false and no error", ok, err)
	}
	if ok, err := c.Exists(ctx, root); err != nil || ok {
		t.Fatalf("Exists(a plain directory) = %v, %v; want false", ok, err)
	}

	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	mustRun(t, raw, src, "git", "init", "--quiet", "--initial-branch=trunk")
	if err := os.WriteFile(filepath.Join(src, "README"), []byte("shop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, raw, src, "git", "add", "README")
	mustRun(t, raw, src, "git", "commit", "--quiet", "-m", "first")
	mustRun(t, raw, src, "git", "push", "--quiet", bare, "trunk")
	head := mustRun(t, raw, src, "git", "rev-parse", "HEAD")

	if _, err := c.SymbolicRefHEAD(ctx, bare); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SymbolicRefHEAD() of a repository whose HEAD dangles = %v, want ErrNotFound", err)
	}
	if err := c.SetHEAD(ctx, bare, "trunk"); err != nil {
		t.Fatalf("SetHEAD() = %v", err)
	}
	got, err := c.SymbolicRefHEAD(ctx, bare)
	if err != nil || got != "refs/heads/trunk" {
		t.Fatalf("SymbolicRefHEAD() = %q, %v; want refs/heads/trunk", got, err)
	}

	if sha, err := c.RevParse(ctx, bare, "trunk"); err != nil || sha != head {
		t.Fatalf("RevParse(trunk) = %q, %v; want %q", sha, err, head)
	}
	if _, err := c.RevParse(ctx, bare, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RevParse(nope) = %v, want ErrNotFound", err)
	}
	if ok, err := c.BranchExists(ctx, bare, "trunk"); err != nil || !ok {
		t.Fatalf("BranchExists(trunk) = %v, %v", ok, err)
	}
	if ok, err := c.BranchExists(ctx, bare, "nope"); err != nil || ok {
		t.Fatalf("BranchExists(nope) = %v, %v; want false and no error", ok, err)
	}

	if err := c.CreateBranch(ctx, bare, "feat-x", "trunk"); err != nil {
		t.Fatalf("CreateBranch() = %v", err)
	}
	if err := c.CreateBranch(ctx, bare, "feat-x", "trunk"); err == nil {
		t.Fatal("CreateBranch() over an existing branch succeeded; --reset has to be explicit")
	}
	branches, err := c.Branches(ctx, bare)
	if err != nil || len(branches) != 2 || branches[0] != "feat-x" || branches[1] != "trunk" {
		t.Fatalf("Branches() = %v, %v", branches, err)
	}
	refs, err := c.Refs(ctx, bare)
	if err != nil || len(refs) != 2 {
		t.Fatalf("Refs() = %+v, %v", refs, err)
	}
	for _, r := range refs {
		if r.Commit != head || !strings.HasPrefix(r.Name, "refs/heads/") {
			t.Errorf("ref %+v", r)
		}
	}

	wt := filepath.Join(root, "apps", "shop", "envs", "feat-x", "src")
	if err := c.WorktreeAdd(ctx, bare, wt, "feat-x"); err != nil {
		t.Fatalf("WorktreeAdd() = %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "README")); err != nil {
		t.Fatalf("the worktree has no checkout: %v", err)
	}

	dotgit, err := os.ReadFile(filepath.Join(wt, ".git"))
	if err != nil {
		t.Fatalf("worktree .git: %v", err)
	}
	if !strings.Contains(string(dotgit), bare) {
		t.Errorf(".git = %q, want it to point into %s", dotgit, bare)
	}

	if err := c.WorktreeAdd(ctx, bare, wt+"2", "feat-x"); err == nil {
		t.Error("WorktreeAdd() checked one branch out twice")
	}

	if err := os.WriteFile(filepath.Join(wt, "NEW"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, raw, wt, "git", "add", "NEW")
	mustRun(t, raw, wt, "git", "commit", "--quiet", "-m", "in the env")
	inEnv := mustRun(t, raw, wt, "git", "rev-parse", "HEAD")
	if sha, err := c.RevParse(ctx, bare, "feat-x"); err != nil || sha != inEnv {
		t.Fatalf("RevParse(feat-x) = %q, %v; want the commit made in the worktree %q", sha, err, inEnv)
	}

	if err := c.WorktreeRemove(ctx, bare, wt, true); err != nil {
		t.Fatalf("WorktreeRemove() = %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("the worktree is still there: %v", err)
	}
	if err := c.WorktreeRemove(ctx, bare, wt, true); err != nil {
		t.Fatalf("WorktreeRemove() twice = %v, want nil", err)
	}
	if err := c.WorktreePrune(ctx, bare); err != nil {
		t.Fatalf("WorktreePrune() = %v", err)
	}
	if ok, err := c.BranchExists(ctx, bare, "feat-x"); err != nil || !ok {
		t.Fatalf("destroy deleted the branch: BranchExists() = %v, %v", ok, err)
	}
	if err := c.DeleteBranch(ctx, bare, "feat-x", true); err != nil {
		t.Fatalf("DeleteBranch() = %v", err)
	}
	if err := c.DeleteBranch(ctx, bare, "feat-x", true); err != nil {
		t.Fatalf("DeleteBranch() twice = %v, want nil", err)
	}
	if ok, _ := c.BranchExists(ctx, bare, "feat-x"); ok {
		t.Error("the branch survived --delete-branch")
	}

	size, err := c.RepoSize(ctx, bare)
	if err != nil {
		t.Fatalf("RepoSize() = %v", err)
	}
	if size <= 0 {
		t.Errorf("RepoSize() = %d, want the bytes of a repository with a commit in it", size)
	}
}

func TestWorktreeRemoveAfterTheDirectoryVanished(t *testing.T) {
	c, raw := realGit(t)
	ctx := context.Background()
	root := t.TempDir()
	bare := filepath.Join(root, "repo.git")
	src := filepath.Join(root, "src")

	if err := c.InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	mustRun(t, raw, src, "git", "init", "--quiet", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, raw, src, "git", "add", "f")
	mustRun(t, raw, src, "git", "commit", "--quiet", "-m", "first")
	mustRun(t, raw, src, "git", "push", "--quiet", bare, "main")

	wt := filepath.Join(root, "wt")
	if err := c.WorktreeAdd(ctx, bare, wt, "main"); err != nil {
		t.Fatalf("WorktreeAdd() = %v", err)
	}
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	if err := c.WorktreeRemove(ctx, bare, wt, true); err != nil {
		t.Fatalf("WorktreeRemove() of a vanished worktree = %v, want nil", err)
	}
	if err := c.WorktreePrune(ctx, bare); err != nil {
		t.Fatalf("WorktreePrune() = %v", err)
	}

	if err := c.WorktreeAdd(ctx, bare, wt, "main"); err != nil {
		t.Fatalf("WorktreeAdd() after the prune = %v", err)
	}
}
