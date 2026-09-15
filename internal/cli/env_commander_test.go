package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/remote"
)

func TestInjectFlag(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			"appended when there is no dash-dash",
			[]string{"env", "list"},
			[]string{"env", "list", "--app", "shop"},
		},
		{

			"before the child's argv",
			[]string{"env", "exec", "feat-x", "--", "sh", "-c", "echo hi"},
			[]string{"env", "exec", "feat-x", "--app", "shop", "--", "sh", "-c", "echo hi"},
		},
		{
			"only the first dash-dash counts",
			[]string{"env", "exec", "feat-x", "--", "sh", "-c", "cmd -- more"},
			[]string{"env", "exec", "feat-x", "--app", "shop", "--", "sh", "-c", "cmd -- more"},
		},
		{
			"global flags are left where they are",
			[]string{"--json", "env", "show", "feat-x"},
			[]string{"--json", "env", "show", "feat-x", "--app", "shop"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := injectFlag(tt.args, "--app", "shop")
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func fakeGit(t *testing.T, answers map[string]string) {
	t.Helper()
	old := runGit
	runGit = func(_ context.Context, _ string, args ...string) (string, error) {
		out, ok := answers[strings.Join(args, " ")]
		if !ok {
			return "", errors.New("not a git repository")
		}
		return out, nil
	}
	t.Cleanup(func() { runGit = old })
}

func TestAppFromCheckoutUsesTheCarameloWorktree(t *testing.T) {

	fakeGit(t, map[string]string{
		"rev-parse --git-common-dir":  "/mnt/caramelo/apps/shop/repo.git",
		"rev-parse --show-toplevel":   "/mnt/caramelo/apps/shop/envs/feat-x/src",
		"rev-parse --abbrev-ref HEAD": "feat-x",
	})
	if got := appFromCheckout(context.Background(), "."); got != "shop" {
		t.Errorf("app = %q, want shop", got)
	}
}

func TestAppFromCheckoutReadsTheConfigThenTheDirectory(t *testing.T) {
	dir := t.TempDir()
	top := filepath.Join(dir, "my-checkout")
	if err := os.MkdirAll(top, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeGit(t, map[string]string{
		"rev-parse --git-common-dir": filepath.Join(top, ".git"),
		"rev-parse --show-toplevel":  top,
	})

	if got := appFromCheckout(context.Background(), "."); got != "my-checkout" {
		t.Errorf("app = %q, want my-checkout", got)
	}

	if err := os.WriteFile(filepath.Join(top, "caramelo.yaml"), []byte("name: shop\ndeps: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := appFromCheckout(context.Background(), "."); got != "shop" {
		t.Errorf("app = %q, want shop from caramelo.yaml", got)
	}

	for _, body := range []string{"name: Not A Slug\n", "name: [1,2\n"} {
		if err := os.WriteFile(filepath.Join(top, "caramelo.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := appFromCheckout(context.Background(), "."); got != "my-checkout" {
			t.Errorf("config %q: app = %q, want the fallback my-checkout", body, got)
		}
	}
}

func TestAppFromCheckoutOutsideARepository(t *testing.T) {
	fakeGit(t, nil)
	if got := appFromCheckout(context.Background(), "."); got != "" {
		t.Errorf("app = %q, want none", got)
	}
}

func TestAppFromRepoPath(t *testing.T) {
	ok := map[string]string{
		"/mnt/caramelo/apps/shop/repo.git": "shop",
		"/var/data/apps/my-app/repo.git":   "my-app",
	}
	for in, want := range ok {
		if got := appFromRepoPath(in); got != want {
			t.Errorf("appFromRepoPath(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"", "/home/alex/code/shop/.git", "/mnt/caramelo/shop/repo.git", "/apps/Shop/repo.git"} {
		if got := appFromRepoPath(in); got != "" {
			t.Errorf("appFromRepoPath(%q) = %q, want none", in, got)
		}
	}
}

type fakeForward struct {
	called bool
	args   []string

	machine string
	code    int
}

func (f *fakeForward) install(t *testing.T) {
	t.Helper()
	old := forward
	forward = func(_ context.Context, a *app) (int, error) {
		f.called = true
		f.args = append([]string(nil), a.args...)
		f.machine = a.machine
		return f.code, nil
	}
	t.Cleanup(func() { forward = old })
}

func TestEnvCommandInjectsTheResolvedApp(t *testing.T) {
	t.Setenv("CARAMELO_APP", "")
	fakeGit(t, map[string]string{
		"rev-parse --git-common-dir": "/mnt/caramelo/apps/shop/repo.git",
		"rev-parse --show-toplevel":  "/mnt/caramelo/apps/shop/envs/feat-x/src",
	})
	fwd := &fakeForward{}
	fwd.install(t)

	code, _, _ := run(t, "env", "exec", "feat-x", "--", "sh", "-c", "echo hi")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	want := []string{"env", "exec", "feat-x", "--app", "shop", "--", "sh", "-c", "echo hi"}
	if strings.Join(fwd.args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("forwarded %v, want %v", fwd.args, want)
	}
}

func TestEnvCommandKeepsAnExplicitApp(t *testing.T) {
	fakeGit(t, map[string]string{
		"rev-parse --git-common-dir": "/mnt/caramelo/apps/other/repo.git",
	})
	fwd := &fakeForward{}
	fwd.install(t)

	if code, _, _ := run(t, "env", "list", "--app", "shop"); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if n := strings.Count(strings.Join(fwd.args, " "), "--app"); n != 1 {
		t.Errorf("forwarded %v, want exactly one --app", fwd.args)
	}
	if !strings.Contains(strings.Join(fwd.args, " "), "--app shop") {
		t.Errorf("forwarded %v, want the explicit app", fwd.args)
	}
}

func TestEnvCommandTakesTheAppFromTheEnvironment(t *testing.T) {
	t.Setenv("CARAMELO_APP", "shop")
	fakeGit(t, nil)
	fwd := &fakeForward{}
	fwd.install(t)

	if code, _, _ := run(t, "env", "list"); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(strings.Join(fwd.args, " "), "--app shop") {
		t.Errorf("forwarded %v, want CARAMELO_APP to have been injected", fwd.args)
	}
}

func TestEnvListWithoutAnAppListsEverything(t *testing.T) {
	t.Setenv("CARAMELO_APP", "")
	fakeGit(t, nil)
	fwd := &fakeForward{}
	fwd.install(t)

	code, _, _ := run(t, "env", "list")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(strings.Join(fwd.args, " "), "--app") {
		t.Errorf("forwarded %v, want no app", fwd.args)
	}
}

func TestEnvWithoutAnAppIsAUsageError(t *testing.T) {
	t.Setenv("CARAMELO_APP", "")
	fakeGit(t, nil)
	fwd := &fakeForward{}
	fwd.install(t)

	for _, args := range [][]string{
		{"env", "show", "feat-x"},
		{"env", "destroy", "feat-x", "--yes"},
		{"env", "exec", "feat-x", "--", "true"},
		{"env", "export", "feat-x"},
	} {
		code, _, stderr := run(t, args...)
		if code != ExitUsage {
			t.Errorf("%v: exit = %d, want %d", args, code, ExitUsage)
		}
		if !strings.Contains(stderr, "--app") {
			t.Errorf("%v: stderr = %q", args, stderr)
		}
	}
	if fwd.called {
		t.Error("an unresolvable app was still forwarded")
	}
}

func TestEnvCommandPassesTheExitCodeBack(t *testing.T) {
	t.Setenv("CARAMELO_APP", "shop")
	fakeGit(t, nil)
	fwd := &fakeForward{code: 3}
	fwd.install(t)

	if code, _, _ := run(t, "env", "exec", "feat-x", "--", "false"); code != 3 {
		t.Errorf("exit = %d, want the daemon's 3", code)
	}
}

func sshTransport() transport {
	return transport{kind: kindSSH, target: remote.Target{User: "caramelo", Host: "box", Port: 4022}}
}

func newEnvCmd(from string, noPush, force bool) *envCmd {
	return &envCmd{
		a:      &app{stdout: io.Discard, stderr: io.Discard},
		app:    "shop",
		create: createFlags{from: from, noPush: noPush, force: force},
	}
}

func TestPlanPush(t *testing.T) {
	inCheckout := map[string]string{
		"rev-parse --git-dir":         ".git",
		"rev-parse --abbrev-ref HEAD": "feat-x",
	}

	t.Run("the current branch by default", func(t *testing.T) {
		fakeGit(t, inCheckout)
		got, err := newEnvCmd("", false, false).planPush(context.Background(), sshTransport())
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			t.Fatal("want a push")
		}
		if got.Ref != "feat-x" {
			t.Errorf("ref = %q, want the current branch", got.Ref)
		}
		if got.Remote != "ssh://caramelo@box:4022/shop" {
			t.Errorf("remote = %q", got.Remote)
		}
		if got.Force {
			t.Error("the default push is fast-forward only")
		}
	})

	t.Run("--from wins", func(t *testing.T) {
		fakeGit(t, inCheckout)
		got, err := newEnvCmd("main", false, true).planPush(context.Background(), sshTransport())
		if err != nil {
			t.Fatal(err)
		}
		if got.Ref != "main" || !got.Force {
			t.Errorf("plan = %+v", got)
		}
	})

	t.Run("--no-push", func(t *testing.T) {
		fakeGit(t, inCheckout)
		got, err := newEnvCmd("main", true, false).planPush(context.Background(), sshTransport())
		if err != nil || got != nil {
			t.Errorf("plan = %+v, err = %v; want no push", got, err)
		}
	})

	t.Run("the tunnel pushes through git-ssh", func(t *testing.T) {

		fakeGit(t, inCheckout)
		tun := transport{kind: kindTunnel, target: remote.Target{User: "caramelo", Host: "box", Port: 4022}}
		got, err := newEnvCmd("", false, false).planPush(context.Background(), tun)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			t.Fatal("want a push through the tunnel")
		}
		if !got.Tunnel {
			t.Error("the plan does not say it goes through the tunnel")
		}
		if got.Remote != "ssh://caramelo@box:4022/shop" {
			t.Errorf("remote = %q", got.Remote)
		}
		env, err := pushEnv(*got)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(env, " ")
		if !strings.Contains(joined, "git-ssh") {
			t.Errorf("push environment = %q, want this binary's git-ssh", joined)
		}

		if !strings.Contains(joined, "GIT_SSH_VARIANT=ssh") {
			t.Errorf("push environment = %q, want GIT_SSH_VARIANT=ssh", joined)
		}
	})

	t.Run("nothing to push over the local socket", func(t *testing.T) {

		fakeGit(t, inCheckout)
		got, err := newEnvCmd("main", false, false).planPush(context.Background(), transport{kind: kindSocket})
		if err != nil || got != nil {
			t.Errorf("plan = %+v, err = %v; want no push", got, err)
		}
	})

	t.Run("outside a checkout", func(t *testing.T) {
		fakeGit(t, nil)
		got, err := newEnvCmd("main", false, false).planPush(context.Background(), sshTransport())
		if err != nil || got != nil {
			t.Errorf("plan = %+v, err = %v; want no push", got, err)
		}
	})

	t.Run("a detached HEAD needs --from", func(t *testing.T) {
		fakeGit(t, map[string]string{
			"rev-parse --git-dir":         ".git",
			"rev-parse --abbrev-ref HEAD": "HEAD",
		})
		_, err := newEnvCmd("", false, false).planPush(context.Background(), sshTransport())
		var ue *usageError
		if !errors.As(err, &ue) {
			t.Fatalf("err = %v, want a usage error", err)
		}
		if !strings.Contains(err.Error(), "--from") {
			t.Errorf("err = %v, want it to say what to pass", err)
		}
	})
}

func TestEnvCreateForwardsTheCurrentBranch(t *testing.T) {

	t.Setenv("CARAMELO_APP", "shop")
	noCommanderConfig(t)
	useSystemConfigDir(t, t.TempDir())
	fakeGit(t, map[string]string{
		"rev-parse --git-dir":         ".git",
		"rev-parse --abbrev-ref HEAD": "wip",
	})
	fwd := &fakeForward{}
	fwd.install(t)

	if code, _, stderr := run(t, "env", "create", "feat-x", "--no-push"); code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if got := strings.Join(fwd.args, " "); !strings.Contains(got, "--from wip") {
		t.Errorf("forwarded %v, want the current branch as --from", fwd.args)
	}
}

func TestEnvCreateKeepsAnExplicitFrom(t *testing.T) {
	t.Setenv("CARAMELO_APP", "shop")
	noCommanderConfig(t)
	useSystemConfigDir(t, t.TempDir())
	fakeGit(t, map[string]string{
		"rev-parse --git-dir":         ".git",
		"rev-parse --abbrev-ref HEAD": "wip",
	})
	fwd := &fakeForward{}
	fwd.install(t)

	if code, _, stderr := run(t, "env", "create", "feat-x", "--from", "main", "--no-push"); code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if n := strings.Count(strings.Join(fwd.args, " "), "--from"); n != 1 {
		t.Errorf("forwarded %v, want exactly one --from", fwd.args)
	}
	if !strings.Contains(strings.Join(fwd.args, " "), "--from main") {
		t.Errorf("forwarded %v, want the explicit ref", fwd.args)
	}
}

func TestEnvCreateOutsideACheckoutLeavesFromToTheDaemon(t *testing.T) {

	for name, answers := range map[string]map[string]string{
		"outside a checkout": nil,
		"detached HEAD": {
			"rev-parse --git-dir":         ".git",
			"rev-parse --abbrev-ref HEAD": "HEAD",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CARAMELO_APP", "shop")
			noCommanderConfig(t)
			useSystemConfigDir(t, t.TempDir())
			fakeGit(t, answers)
			fwd := &fakeForward{}
			fwd.install(t)

			if code, _, stderr := run(t, "env", "create", "feat-x", "--no-push"); code != ExitOK {
				t.Fatalf("exit = %d (stderr %q)", code, stderr)
			}
			if strings.Contains(strings.Join(fwd.args, " "), "--from") {
				t.Errorf("forwarded %v, want no --from", fwd.args)
			}
		})
	}
}

func TestEnvDestroyNeedsYesWithNoTerminal(t *testing.T) {

	t.Setenv("CARAMELO_APP", "shop")
	noCommanderConfig(t)
	useSystemConfigDir(t, t.TempDir())
	fakeGit(t, nil)
	fwd := &fakeForward{}
	fwd.install(t)

	code, _, stderr := run(t, "env", "destroy", "feat-x")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Errorf("stderr = %q, want it to name --yes", stderr)
	}
	if fwd.called {
		t.Error("an unconfirmed destroy was forwarded")
	}
}

func TestEnvDestroyWithYesIsForwarded(t *testing.T) {
	t.Setenv("CARAMELO_APP", "shop")
	noCommanderConfig(t)
	useSystemConfigDir(t, t.TempDir())
	fakeGit(t, nil)
	fwd := &fakeForward{}
	fwd.install(t)

	if code, _, stderr := run(t, "env", "destroy", "feat-x", "--yes"); code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if !fwd.called {
		t.Error("a confirmed destroy was not forwarded")
	}
}

func TestInjectSwitch(t *testing.T) {
	got := injectSwitch([]string{"env", "exec", "feat-x", "--", "sh"}, "--yes")
	want := []string{"env", "exec", "feat-x", "--yes", "--", "sh"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestPushURLQuotesAnIPv6Host(t *testing.T) {
	got := pushURL(remote.Target{User: "caramelo", Host: "fd00::1", Port: 4022}, "shop")
	if got != "ssh://caramelo@[fd00::1]:4022/shop" {
		t.Errorf("url = %q", got)
	}
}

func TestGitSSHCommandCarriesTheClientsOptions(t *testing.T) {
	t.Setenv("CARAMELO_SSH", "/usr/bin/ssh")
	t.Setenv("CARAMELO_SSH_OPTS", "-F '/tmp/ssh config' -i /tmp/key")
	got, err := gitSSHCommand()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/usr/bin/ssh", "BatchMode=yes", "StrictHostKeyChecking=accept-new", "-F", "-i"} {
		if !strings.Contains(got, want) {
			t.Errorf("GIT_SSH_COMMAND = %q, want it to contain %q", got, want)
		}
	}

	if !strings.Contains(got, "'/tmp/ssh config'") {
		t.Errorf("GIT_SSH_COMMAND = %q, want the spaced path quoted", got)
	}
}

func TestEnvCreatePushesBeforeForwarding(t *testing.T) {
	t.Setenv("CARAMELO_APP", "shop")
	noCommanderConfig(t)
	useSystemConfigDir(t, t.TempDir())
	useCommanderConfig(t, remote.CommanderConfig{
		DefaultMachine: "box",
		Machines:       map[string]string{"box": "caramelo@box:4022"},
	})
	fakeGit(t, map[string]string{
		"rev-parse --git-dir":         ".git",
		"rev-parse --abbrev-ref HEAD": "feat-x",
	})
	fwd := &fakeForward{}
	fwd.install(t)

	var pushed []pushPlan
	old := gitPush
	gitPush = func(_ context.Context, p pushPlan, _ io.Writer) error {
		pushed = append(pushed, p)
		return nil
	}
	t.Cleanup(func() { gitPush = old })

	var stdout, stderr bytes.Buffer
	code := Run([]string{"env", "create", "feat-x", "--from", "main"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr.String())
	}
	if len(pushed) != 1 {
		t.Fatalf("%d pushes, want 1", len(pushed))
	}
	if pushed[0].Ref != "main" || pushed[0].Remote != "ssh://caramelo@box:4022/shop" {
		t.Errorf("pushed %+v", pushed[0])
	}
	if !fwd.called {
		t.Error("the command was not forwarded after the push")
	}

	if !strings.Contains(stderr.String(), "push main to ssh://caramelo@box:4022/shop") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want the forwarded command to own it", stdout.String())
	}
}

func TestEnvCreateReportsAFailedPushAndDoesNotForward(t *testing.T) {
	t.Setenv("CARAMELO_APP", "shop")
	noCommanderConfig(t)
	useSystemConfigDir(t, t.TempDir())
	useCommanderConfig(t, remote.CommanderConfig{
		DefaultMachine: "box",
		Machines:       map[string]string{"box": "caramelo@box:4022"},
	})
	fakeGit(t, map[string]string{
		"rev-parse --git-dir":         ".git",
		"rev-parse --abbrev-ref HEAD": "feat-x",
	})
	fwd := &fakeForward{}
	fwd.install(t)
	old := gitPush
	gitPush = func(context.Context, pushPlan, io.Writer) error {
		return errors.New("git push feat-x failed (exit 1)")
	}
	t.Cleanup(func() { gitPush = old })

	code, _, stderr := run(t, "env", "create", "feat-x")
	if code != ExitError {
		t.Fatalf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "git push") {
		t.Errorf("stderr = %q", stderr)
	}
	if fwd.called {
		t.Error("the env was created even though the code never arrived")
	}
}
