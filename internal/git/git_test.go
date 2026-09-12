package git

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const testUser = "caramelo"

const repo = "/mnt/caramelo/apps/shop/repo.git"

func testCLI(f *fakeRunner) *CLI { return NewCLI(f, testUser) }

func wantArgv(t *testing.T, f *fakeRunner, want ...string) {
	t.Helper()
	got := f.log()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands\n got: %s\nwant: %s", strings.Join(got, "\n      "), strings.Join(want, "\n      "))
	}
	for _, c := range f.calls {
		if c.User != testUser {
			t.Errorf("command %q ran as %q, want %q", cmdKey(c), c.User, testUser)
		}
	}
}

func TestInitBare(t *testing.T) {

	f := unconfigured()
	if err := testCLI(f).InitBare(context.Background(), repo); err != nil {
		t.Fatalf("InitBare() = %v", err)
	}
	wantArgv(t, f, append([]string{"git init --bare " + repo}, configureArgv(repo)...)...)
}

func TestInitBareReportsGitsMessage(t *testing.T) {
	f := (&fakeRunner{}).on("git init", fail(128, "fatal: cannot mkdir /mnt/caramelo/apps/shop: Permission denied"))
	err := testCLI(f).InitBare(context.Background(), repo)
	if err == nil {
		t.Fatal("InitBare() on an unwritable path succeeded")
	}
	for _, want := range []string{"exit 128", "Permission denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q does not mention %q", err, want)
		}
	}
}

func TestExists(t *testing.T) {
	f := &fakeRunner{}
	got, err := testCLI(f).Exists(context.Background(), repo)
	if err != nil || !got {
		t.Fatalf("Exists() = %v, %v; want true", got, err)
	}
	wantArgv(t, f, "git rev-parse --resolve-git-dir "+repo)

	f = (&fakeRunner{}).on("git rev-parse", fail(128, "fatal: not a gitdir '"+repo+"'"))
	got, err = testCLI(f).Exists(context.Background(), repo)
	if err != nil || got {
		t.Fatalf("Exists() of a missing repo = %v, %v; want false and no error", got, err)
	}
}

func TestBranchExists(t *testing.T) {
	f := &fakeRunner{}
	got, err := testCLI(f).BranchExists(context.Background(), repo, "feat-x")
	if err != nil || !got {
		t.Fatalf("BranchExists() = %v, %v", got, err)
	}
	wantArgv(t, f, "git -C "+repo+" show-ref --verify --quiet refs/heads/feat-x")

	f = (&fakeRunner{}).on("git", fail(1, ""))
	got, err = testCLI(f).BranchExists(context.Background(), repo, "feat-x")
	if err != nil || got {
		t.Fatalf("BranchExists() of a missing branch = %v, %v; want false and no error", got, err)
	}

	f = (&fakeRunner{}).on("git", fail(128, "fatal: not a git repository"))
	if _, err := testCLI(f).BranchExists(context.Background(), repo, "feat-x"); err == nil {
		t.Fatal("BranchExists() hid a broken repository behind false")
	}
}

func TestCreateBranch(t *testing.T) {
	f := &fakeRunner{}
	if err := testCLI(f).CreateBranch(context.Background(), repo, "feat-x", "main"); err != nil {
		t.Fatalf("CreateBranch() = %v", err)
	}
	wantArgv(t, f, "git -C "+repo+" branch feat-x main")

	if err := testCLI(&fakeRunner{}).CreateBranch(context.Background(), repo, "feat-x", ""); err == nil {
		t.Fatal("CreateBranch() without a starting point succeeded")
	}
}

func TestDeleteBranch(t *testing.T) {
	f := &fakeRunner{}
	if err := testCLI(f).DeleteBranch(context.Background(), repo, "feat-x", true); err != nil {
		t.Fatalf("DeleteBranch() = %v", err)
	}
	wantArgv(t, f, "git -C "+repo+" branch -D feat-x")

	f = &fakeRunner{}
	if err := testCLI(f).DeleteBranch(context.Background(), repo, "feat-x", false); err != nil {
		t.Fatalf("DeleteBranch() = %v", err)
	}
	wantArgv(t, f, "git -C "+repo+" branch --delete feat-x")
}

func TestDeleteBranchToleratesAMissingBranch(t *testing.T) {
	f := (&fakeRunner{}).on("git", fail(1, "error: branch 'feat-x' not found"))
	if err := testCLI(f).DeleteBranch(context.Background(), repo, "feat-x", true); err != nil {
		t.Fatalf("DeleteBranch() of a missing branch = %v, want nil", err)
	}

	f = (&fakeRunner{}).on("git", fail(1, "error: cannot delete branch 'feat-x' used by worktree at '/x'"))
	if err := testCLI(f).DeleteBranch(context.Background(), repo, "feat-x", true); err == nil {
		t.Fatal("DeleteBranch() swallowed a real failure")
	}
}

func TestWorktreeAddAndRemove(t *testing.T) {
	const path = "/mnt/caramelo/apps/shop/envs/feat-x/src"
	f := &fakeRunner{}
	if err := testCLI(f).WorktreeAdd(context.Background(), repo, path, "feat-x"); err != nil {
		t.Fatalf("WorktreeAdd() = %v", err)
	}
	wantArgv(t, f, "git -C "+repo+" worktree add "+path+" feat-x")

	f = &fakeRunner{}
	if err := testCLI(f).WorktreeRemove(context.Background(), repo, path, true); err != nil {
		t.Fatalf("WorktreeRemove() = %v", err)
	}
	wantArgv(t, f, "git -C "+repo+" worktree remove --force "+path)

	f = &fakeRunner{}
	if err := testCLI(f).WorktreeRemove(context.Background(), repo, path, false); err != nil {
		t.Fatalf("WorktreeRemove() = %v", err)
	}
	wantArgv(t, f, "git -C "+repo+" worktree remove "+path)
}

func TestWorktreeRemoveToleratesAnUnknownWorktree(t *testing.T) {
	f := (&fakeRunner{}).on("git", fail(128, "fatal: '/gone' is not a working tree"))
	if err := testCLI(f).WorktreeRemove(context.Background(), repo, "/gone", true); err != nil {
		t.Fatalf("WorktreeRemove() of a missing worktree = %v, want nil", err)
	}

	f = (&fakeRunner{}).on("git", fail(128, "fatal: validation failed, cannot remove working directory"))
	if err := testCLI(f).WorktreeRemove(context.Background(), repo, "/x", false); err == nil {
		t.Fatal("WorktreeRemove() swallowed a real failure")
	}
}

func TestWorktreePrune(t *testing.T) {
	f := &fakeRunner{}
	if err := testCLI(f).WorktreePrune(context.Background(), repo); err != nil {
		t.Fatalf("WorktreePrune() = %v", err)
	}
	wantArgv(t, f, "git -C "+repo+" worktree prune")
}

func TestRevParse(t *testing.T) {
	const sha = "8f1d9f0b7f1e4c6a2b3c4d5e6f708192a3b4c5d6"
	f := (&fakeRunner{}).on("git", ok(sha+"\n"))
	got, err := testCLI(f).RevParse(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("RevParse() = %v", err)
	}
	if got != sha {
		t.Errorf("RevParse() = %q, want %q", got, sha)
	}
	wantArgv(t, f, "git -C "+repo+" rev-parse --verify --end-of-options main")
}

func TestRevParseOfAnUnknownRefIsErrNotFound(t *testing.T) {
	f := (&fakeRunner{}).on("git", fail(128, "fatal: Needed a single revision"))
	if _, err := testCLI(f).RevParse(context.Background(), repo, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RevParse() = %v, want ErrNotFound", err)
	}

	f = (&fakeRunner{}).on("git", fail(128, "fatal: not a git repository: '"+repo+"'"))
	if _, e := testCLI(f).RevParse(context.Background(), repo, "main"); errors.Is(e, ErrNotFound) {
		t.Fatalf("RevParse() on a broken repository = %v, want a real failure", e)
	}
}

func TestSymbolicRefHEAD(t *testing.T) {
	f := (&fakeRunner{}).
		on("git -C "+repo+" symbolic-ref HEAD", ok("refs/heads/main\n")).
		on("git -C "+repo+" show-ref", ok(""))
	got, err := testCLI(f).SymbolicRefHEAD(context.Background(), repo)
	if err != nil {
		t.Fatalf("SymbolicRefHEAD() = %v", err)
	}
	if got != "refs/heads/main" {
		t.Errorf("SymbolicRefHEAD() = %q", got)
	}
	wantArgv(t, f,
		"git -C "+repo+" symbolic-ref HEAD",
		"git -C "+repo+" show-ref --verify --quiet refs/heads/main")
}

func TestSymbolicRefHEADIsErrNotFoundWhenItDangles(t *testing.T) {

	f := (&fakeRunner{}).
		on("git -C "+repo+" symbolic-ref HEAD", ok("refs/heads/master\n")).
		on("git -C "+repo+" show-ref", fail(1, ""))
	_, err := testCLI(f).SymbolicRefHEAD(context.Background(), repo)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("SymbolicRefHEAD() = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "refs/heads/master") {
		t.Errorf("err %q does not say what HEAD points at", err)
	}
}

func TestSymbolicRefHEADIsErrNotFoundWhenDetached(t *testing.T) {
	f := (&fakeRunner{}).on("git", fail(1, "fatal: ref HEAD is not a symbolic ref"))
	if _, err := testCLI(f).SymbolicRefHEAD(context.Background(), repo); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SymbolicRefHEAD() = %v, want ErrNotFound", err)
	}
}

func TestSetHEAD(t *testing.T) {
	f := &fakeRunner{}
	if err := testCLI(f).SetHEAD(context.Background(), repo, "main"); err != nil {
		t.Fatalf("SetHEAD() = %v", err)
	}
	wantArgv(t, f, "git -C "+repo+" symbolic-ref HEAD refs/heads/main")

	f = &fakeRunner{}
	if err := testCLI(f).SetHEAD(context.Background(), repo, "refs/heads/main"); err != nil {
		t.Fatalf("SetHEAD() = %v", err)
	}
	wantArgv(t, f, "git -C "+repo+" symbolic-ref HEAD refs/heads/main")

	if err := testCLI(&fakeRunner{}).SetHEAD(context.Background(), repo, ""); err == nil {
		t.Fatal("SetHEAD() without a branch succeeded")
	}
}

func TestBranches(t *testing.T) {
	f := (&fakeRunner{}).on("git", ok("main\nfeat-x\n\n"))
	got, err := testCLI(f).Branches(context.Background(), repo)
	if err != nil {
		t.Fatalf("Branches() = %v", err)
	}
	if want := []string{"feat-x", "main"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Branches() = %v, want %v (sorted)", got, want)
	}
	wantArgv(t, f, "git -C "+repo+" for-each-ref --format=%(refname:short) refs/heads")
}

func TestRefs(t *testing.T) {
	out := "8f1d9f0b7f1e4c6a2b3c4d5e6f708192a3b4c5d6 refs/heads/main\n" +
		"1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d refs/heads/feat-x\n"
	f := (&fakeRunner{}).on("git", ok(out))
	got, err := testCLI(f).Refs(context.Background(), repo)
	if err != nil {
		t.Fatalf("Refs() = %v", err)
	}
	want := []Ref{
		{Name: "refs/heads/main", Commit: "8f1d9f0b7f1e4c6a2b3c4d5e6f708192a3b4c5d6"},
		{Name: "refs/heads/feat-x", Commit: "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Refs() = %+v, want %+v", got, want)
	}
	wantArgv(t, f, "git -C "+repo+" for-each-ref --format=%(objectname) %(refname)")
}

func TestRepoSize(t *testing.T) {
	f := (&fakeRunner{}).on("du", ok("26229\t"+repo+"\n"))
	got, err := testCLI(f).RepoSize(context.Background(), repo)
	if err != nil {
		t.Fatalf("RepoSize() = %v", err)
	}
	if want := int64(26229 * 1024); got != want {
		t.Errorf("RepoSize() = %d, want %d", got, want)
	}
	if want := []string{"du -sk " + repo}; !reflect.DeepEqual(f.log(), want) {
		t.Errorf("commands = %v, want %v", f.log(), want)
	}
	if f.calls[0].User != testUser {
		t.Errorf("du ran as %q, want %q", f.calls[0].User, testUser)
	}
}

func TestRepoSizeOfAMissingRepo(t *testing.T) {
	f := (&fakeRunner{}).on("du", fail(1, "du: cannot access '"+repo+"': No such file or directory"))
	if _, err := testCLI(f).RepoSize(context.Background(), repo); err == nil {
		t.Fatal("RepoSize() of a missing repository succeeded")
	}
}

func TestARunnerThatCannotRunIsWrapped(t *testing.T) {
	f := (&fakeRunner{}).onErr("git", errors.New(`exec: "git": executable file not found in $PATH`))
	err := testCLI(f).InitBare(context.Background(), repo)
	if err == nil || !strings.Contains(err.Error(), "git init --bare") {
		t.Fatalf("InitBare() = %v, want the argv in the message", err)
	}
}

func TestADriverWithoutARunnerSaysSo(t *testing.T) {
	c := &CLI{User: testUser}
	if err := c.InitBare(context.Background(), repo); err == nil || !strings.Contains(err.Error(), "no command runner") {
		t.Fatalf("InitBare() = %v", err)
	}
	if _, err := c.RepoSize(context.Background(), repo); err == nil {
		t.Fatal("RepoSize() without a runner succeeded")
	}
}
