package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
)

func TestThePostReceiveHookIsInstalledAndIdempotent(t *testing.T) {
	c, _ := realGit(t)
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "shop.git")
	if err := c.InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}

	changed, err := c.EnsurePostReceive(ctx, bare)
	if err != nil || !changed {
		t.Fatalf("EnsurePostReceive = %v, %v; want it installed", changed, err)
	}
	if changed, err := c.EnsurePostReceive(ctx, bare); err != nil || changed {
		t.Fatalf("EnsurePostReceive twice = %v, %v; want no change", changed, err)
	}

	path := filepath.Join(bare, "hooks", "post-receive")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the hook: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("the hook is not executable (%v): git would ignore it", info.Mode())
	}
	if body := read(t, path); body != PostReceiveHook {
		t.Fatal("the hook on disk is not the one this build ships")
	}

	drop, err := os.Stat(filepath.Join(bare, PushRecordDir))
	if err != nil || !drop.IsDir() {
		t.Fatalf("the drop directory is not there: %v", err)
	}

	write(t, path, "#!/bin/sh\n# an older caramelo\n")
	if changed, err := c.EnsurePostReceive(ctx, bare); err != nil || !changed {
		t.Fatalf("EnsurePostReceive over a hand-edited hook = %v, %v; want it rewritten", changed, err)
	}
	if body := read(t, path); body != PostReceiveHook {
		t.Fatal("a hand-edited hook was left alone")
	}

	if _, err := c.EnsurePostReceive(ctx, ""); err == nil {
		t.Error("EnsurePostReceive of no repository succeeded")
	}
}

func pushLab(t *testing.T) (*CLI, runner.Runner, string, string) {
	t.Helper()
	c, raw := realGit(t)
	ctx := context.Background()
	root := t.TempDir()
	bare := filepath.Join(root, "repo.git")
	laptop := filepath.Join(root, "laptop")
	wt := filepath.Join(root, "src")

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
	return c, raw, bare, laptop
}

func push(t *testing.T, r runner.Runner, dir string, env []string, args ...string) runner.Result {
	t.Helper()
	res, err := r.Run(context.Background(), runner.Cmd{
		Name: "git", Args: append([]string{"push", "--quiet"}, args...), Dir: dir, Env: env,
	})
	if err != nil {
		t.Fatalf("git push %v: %v", args, err)
	}
	return res
}

func TestAPushOptionReachesTheHook(t *testing.T) {
	c, raw, bare, laptop := pushLab(t)
	stub := filepath.Join(bare, "hooks", "post-receive")
	seen := filepath.Join(t.TempDir(), "options")
	write(t, stub, "#!/bin/sh\n"+
		"cat >/dev/null\n"+
		"printf 'count=%s first=%s\\n' \"${GIT_PUSH_OPTION_COUNT:-0}\" \"${GIT_PUSH_OPTION_0:-}\" > "+seen+"\n"+
		"exit 0\n")
	if err := os.Chmod(stub, 0o755); err != nil {
		t.Fatal(err)
	}

	commit(t, raw, laptop, "app.py", "second\n", "second")

	mustRun(t, raw, bare, "git", "config", "--local", AdvertisePushOptionsKey, "false")
	res := push(t, raw, laptop, nil, bare, "main:feat-x", "-o", "caramelo.branch=blob-store")
	if res.ExitCode == 0 {
		t.Fatal("a push carrying -o landed on a repository that does not advertise push options; " +
			"the key would then be free to forget")
	}
	if changed, err := c.EnsureUpdateInstead(context.Background(), bare); err != nil || !changed {
		t.Fatalf("EnsureUpdateInstead did not put %s back: %v, %v", AdvertisePushOptionsKey, changed, err)
	}

	if res := push(t, raw, laptop, nil, bare, "main:feat-x", "-o", "caramelo.branch=blob-store"); res.ExitCode != 0 {
		t.Fatalf("a push carrying -o was refused (exit %d):\n%s", res.ExitCode, res.Stderr)
	}
	if got := strings.TrimSpace(read(t, seen)); got != "count=1 first=caramelo.branch=blob-store" {
		t.Fatalf("the hook saw %q; receive.advertisePushOptions is what makes -o arrive", got)
	}

	commit(t, raw, laptop, "app.py", "third\n", "third")
	if res := push(t, raw, laptop, nil, bare, "main:feat-x"); res.ExitCode != 0 {
		t.Fatalf("a plain push was refused (exit %d):\n%s", res.ExitCode, res.Stderr)
	}
	if got := strings.TrimSpace(read(t, seen)); got != "count=0 first=" {
		t.Fatalf("a push with no option left %q", got)
	}

	if _, err := c.EnsurePostReceive(context.Background(), bare); err != nil {
		t.Fatal(err)
	}
}

func TestThePostReceiveHookWritesWhatThePushCarried(t *testing.T) {
	c, raw, bare, laptop := pushLab(t)
	ctx := context.Background()
	if _, err := c.EnsurePostReceive(ctx, bare); err != nil {
		t.Fatal(err)
	}
	drop := filepath.Join(bare, PushRecordDir)

	record := func(name string) string { return filepath.Join(drop, name) }
	env := func(name string) []string { return []string{PushRecordEnv + "=" + record(name)} }

	commit(t, raw, laptop, "app.py", "second\n", "second")
	second := mustRun(t, raw, laptop, "git", "rev-parse", "HEAD")
	if res := push(t, raw, laptop, env("one"), bare, "main:feat-x", "-o", "caramelo.branch=blob-store"); res.ExitCode != 0 {
		t.Fatalf("push: exit %d\n%s", res.ExitCode, res.Stderr)
	}
	got := read(t, record("one"))
	if !strings.Contains(got, "option caramelo.branch=blob-store\n") {
		t.Errorf("the record holds no option line:\n%s", got)
	}
	if !strings.Contains(got, " "+second+" refs/heads/feat-x\n") {
		t.Errorf("the record does not name the commit that landed (%s):\n%s", second, got)
	}

	commit(t, raw, laptop, "app.py", "third\n", "third")
	if res := push(t, raw, laptop, env("two"), bare, "main:feat-x"); res.ExitCode != 0 {
		t.Fatalf("push: exit %d\n%s", res.ExitCode, res.Stderr)
	}
	if got := read(t, record("two")); strings.Contains(got, "option ") {
		t.Errorf("a bare push left an option line:\n%s", got)
	}

	write(t, filepath.Join(filepath.Dir(bare), "src", "app.py"), "the agent was here\n")
	commit(t, raw, laptop, "app.py", "fourth\n", "fourth")
	if res := push(t, raw, laptop, env("three"), bare, "main:feat-x"); res.ExitCode == 0 {
		t.Fatal("a push over a dirty worktree succeeded")
	}
	if b, err := os.ReadFile(record("three")); err == nil && strings.Contains(string(b), "refs/heads/feat-x") {
		t.Errorf("a refused push was recorded as if it had landed:\n%s", b)
	}
	mustRun(t, raw, filepath.Join(filepath.Dir(bare), "src"), "git", "checkout", "--", "app.py")

	before := mustRun(t, raw, laptop, "git", "rev-parse", "HEAD")
	commit(t, raw, laptop, "app.py", "fifth\n", "fifth")
	if res := push(t, raw, laptop, nil, bare, "main:feat-x"); res.ExitCode != 0 {
		t.Fatalf("a push with no drop file was refused (exit %d):\n%s", res.ExitCode, res.Stderr)
	}
	if entries, err := os.ReadDir(drop); err != nil || len(entries) != 2 {
		t.Errorf("the drop directory holds %d files, %v; the hook wrote with no path to write to", len(entries), err)
	}
	if before == mustRun(t, raw, laptop, "git", "rev-parse", "HEAD") {
		t.Fatal("the fifth commit was not made")
	}
}

func TestAnOptionCarryingANewlineCannotForgeARefLine(t *testing.T) {
	c, raw, bare, _ := pushLab(t)
	if _, err := c.EnsurePostReceive(context.Background(), bare); err != nil {
		t.Fatal(err)
	}
	forged := strings.Repeat("a", 40)
	path := filepath.Join(bare, PushRecordDir, "forged")
	res, err := raw.Run(context.Background(), runner.Cmd{
		Name: filepath.Join(bare, "hooks", "post-receive"),
		Dir:  bare,
		Env: []string{
			PushRecordEnv + "=" + path,
			"GIT_PUSH_OPTION_COUNT=1",
			"GIT_PUSH_OPTION_0=caramelo.branch=blob-store\nref " + zeroes + " " + forged + " refs/heads/feat-x",
		},
		Stdin: strings.NewReader(zeroes + " " + forged + " refs/heads/main\n"),
	})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("the hook exited %d, %v; it must never fail a push\n%s", res.ExitCode, err, res.Stderr)
	}
	if res.Stdout != "" || res.Stderr != "" {
		t.Errorf("the hook spoke to the client: stdout %q stderr %q", res.Stdout, res.Stderr)
	}
	got := read(t, path)
	if strings.Count(got, "\n") != 2 {
		t.Fatalf("the record holds %d lines, want one option and one ref:\n%s", strings.Count(got, "\n"), got)
	}
	if !strings.HasPrefix(got, "option caramelo.branch=blob-storeref "+zeroes+" "+forged+" refs/heads/feat-x\n") {
		t.Errorf("the newline was not stripped from the option value:\n%s", got)
	}
	if !strings.HasSuffix(got, "ref "+zeroes+" "+forged+" refs/heads/main\n") {
		t.Errorf("the ref this push carried is not in the record:\n%s", got)
	}
}

const zeroes = "0000000000000000000000000000000000000000"
