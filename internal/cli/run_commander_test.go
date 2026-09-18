package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/remote"
)

var inACheckout = map[string]string{
	"rev-parse --git-dir":         ".git",
	"rev-parse --git-common-dir":  "/home/alex/code/shop/.git",
	"rev-parse --show-toplevel":   "/home/alex/code/shop",
	"rev-parse --abbrev-ref HEAD": "feat-x",
}

var inAnEnvWorktree = map[string]string{
	"rev-parse --git-dir":        "/mnt/caramelo/apps/shop/repo.git/worktrees/feat-x",
	"rev-parse --git-common-dir": "/mnt/caramelo/apps/shop/repo.git",
	"rev-parse --show-toplevel":  "/mnt/caramelo/apps/shop/envs/feat-x/src",
}

func TestUpInjectsTheAppAndTheEnvironment(t *testing.T) {
	t.Setenv("CARAMELO_APP", "")
	t.Setenv("CARAMELO_ENV", "feat-y")
	noCommanderConfig(t)
	useSystemConfigDir(t, t.TempDir())
	fakeGit(t, inACheckout)
	fwd := &fakeForward{}
	fwd.install(t)

	if code, _, stderr := run(t, "up", "--no-push"); code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	got := strings.Join(fwd.args, " ")
	if !strings.Contains(got, "--app shop") {
		t.Errorf("forwarded %v, want the app from the checkout", fwd.args)
	}

	if !strings.Contains(got, "--env feat-y") {
		t.Errorf("forwarded %v, want CARAMELO_ENV to have been injected", fwd.args)
	}
}

func TestAPositionalEnvironmentIsNotInjectedAgain(t *testing.T) {
	t.Setenv("CARAMELO_APP", "shop")
	t.Setenv("CARAMELO_ENV", "feat-y")
	noCommanderConfig(t)
	useSystemConfigDir(t, t.TempDir())
	fakeGit(t, inACheckout)
	fwd := &fakeForward{}
	fwd.install(t)

	if code, _, stderr := run(t, "up", "feat-x", "--no-push"); code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if strings.Contains(strings.Join(fwd.args, " "), "--env") {
		t.Errorf("forwarded %v, want the positional environment to travel as itself", fwd.args)
	}
}

func TestTheWorktreeNamesTheEnvironment(t *testing.T) {
	t.Setenv("CARAMELO_APP", "")
	t.Setenv("CARAMELO_ENV", "")
	noCommanderConfig(t)
	useSystemConfigDir(t, t.TempDir())
	fakeGit(t, inAnEnvWorktree)
	fwd := &fakeForward{}
	fwd.install(t)

	if code, _, stderr := run(t, "logs"); code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	got := strings.Join(fwd.args, " ")
	if !strings.Contains(got, "--app shop") || !strings.Contains(got, "--env feat-x") {
		t.Errorf("forwarded %v, want both resolved from the worktree", fwd.args)
	}
}

func TestNothingNamesTheEnvironment(t *testing.T) {
	t.Setenv("CARAMELO_APP", "shop")
	t.Setenv("CARAMELO_ENV", "")
	noCommanderConfig(t)
	useSystemConfigDir(t, t.TempDir())
	fakeGit(t, inACheckout)
	fwd := &fakeForward{}
	fwd.install(t)

	code, _, stderr := run(t, "up", "--no-push")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	for _, want := range []string{"caramelo up ENV", "--env", "CARAMELO_ENV", "worktree"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to mention %q", stderr, want)
		}
	}
	if fwd.called {
		t.Error("an unresolvable environment was still forwarded")
	}
}

func TestLogsInsideAnEnvironmentTakesServices(t *testing.T) {
	t.Setenv("CARAMELO_APP", "shop")
	t.Setenv("CARAMELO_ENV", "feat-x")
	noCommanderConfig(t)
	useSystemConfigDir(t, t.TempDir())
	fakeGit(t, inAnEnvWorktree)
	fwd := &fakeForward{}
	fwd.install(t)

	if code, _, stderr := run(t, "logs", "web"); code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	got := strings.Join(fwd.args, " ")
	if !strings.Contains(got, "--env feat-x") {
		t.Errorf("forwarded %v, want the environment from CARAMELO_ENV", fwd.args)
	}
	if !strings.Contains(got, "logs web") {
		t.Errorf("forwarded %v, want the service to stay a positional argument", fwd.args)
	}
}

func TestTheInjectedFlagsStayOnCaramelosSideOfTheDash(t *testing.T) {
	t.Setenv("CARAMELO_APP", "")
	t.Setenv("CARAMELO_ENV", "feat-x")
	noCommanderConfig(t)
	useSystemConfigDir(t, t.TempDir())
	fakeGit(t, inAnEnvWorktree)
	fwd := &fakeForward{}
	fwd.install(t)

	if code, _, stderr := run(t, "run", "--", "npm", "run", "lint"); code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	want := "run --app shop --env feat-x -- npm run lint"
	if got := strings.Join(fwd.args, " "); got != want {
		t.Errorf("forwarded %q, want %q", got, want)
	}
}

func withSSHMachine(t *testing.T) {
	t.Helper()
	useSystemConfigDir(t, t.TempDir())
	useCommanderConfig(t, remote.CommanderConfig{
		Name: "laptop",
		Role: remote.RoleCommander,
		Commander: remote.Commander{
			DefaultFleet: "home",
			Fleets:       map[string]remote.Fleet{"home": {Hub: "caramelo@box:4022"}},
		},
	})
}

func capturePush(t *testing.T) *[]pushPlan {
	t.Helper()
	var pushed []pushPlan
	old := gitPush
	gitPush = func(_ context.Context, p pushPlan, _ io.Writer) error {
		pushed = append(pushed, p)
		return nil
	}
	t.Cleanup(func() { gitPush = old })
	return &pushed
}

func TestUpPushesHEADToTheEnvironmentsBranch(t *testing.T) {
	t.Setenv("CARAMELO_APP", "shop")
	t.Setenv("CARAMELO_ENV", "")
	withSSHMachine(t)
	fakeGit(t, inACheckout)
	fwd := &fakeForward{}
	fwd.install(t)
	pushed := capturePush(t)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"up", "feat-x"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr.String())
	}
	if len(*pushed) != 1 {
		t.Fatalf("%d pushes, want 1", len(*pushed))
	}
	p := (*pushed)[0]
	if p.Ref != "HEAD:feat-x" {
		t.Errorf("ref = %q, want HEAD onto the environment's branch", p.Ref)
	}
	if p.Remote != "ssh://caramelo@box:4022/shop" {
		t.Errorf("remote = %q", p.Remote)
	}
	if p.Force {
		t.Error("the default push is fast-forward only")
	}
	if p.SourceBranch != "feat-x" {
		t.Errorf("source = %q, want the branch up pushed from", p.SourceBranch)
	}
	if !fwd.called {
		t.Error("the command was not forwarded after the push")
	}

	if !strings.Contains(stderr.String(), "push HEAD:feat-x to ssh://caramelo@box:4022/shop") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want the forwarded command to own it", stdout.String())
	}
}

func TestUpForcePushes(t *testing.T) {
	t.Setenv("CARAMELO_APP", "shop")
	t.Setenv("CARAMELO_ENV", "")
	withSSHMachine(t)
	fakeGit(t, inACheckout)
	(&fakeForward{}).install(t)
	pushed := capturePush(t)

	if code, _, stderr := run(t, "up", "feat-x", "--force"); code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if len(*pushed) != 1 || !(*pushed)[0].Force {
		t.Errorf("pushes = %+v, want one forced push", *pushed)
	}
	if (*pushed)[0].SourceBranch != "feat-x" {
		t.Errorf("source = %q, want the branch up pushed from", (*pushed)[0].SourceBranch)
	}
}

func TestUpDoesNotPushWhenItShouldNot(t *testing.T) {
	tests := map[string]struct {
		args    []string
		answers map[string]string
		machine bool
	}{
		"--no-push":             {args: []string{"up", "feat-x", "--no-push"}, answers: inACheckout, machine: true},
		"outside a checkout":    {args: []string{"up", "feat-x"}, answers: nil, machine: true},
		"over the local socket": {args: []string{"up", "feat-x", "--machine", "local"}, answers: inACheckout},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CARAMELO_APP", "shop")
			t.Setenv("CARAMELO_ENV", "")
			if tc.machine {
				withSSHMachine(t)
			} else {
				noCommanderConfig(t)
				useSystemConfigDir(t, t.TempDir())
			}
			fakeGit(t, tc.answers)
			(&fakeForward{}).install(t)
			pushed := capturePush(t)

			if code, _, stderr := run(t, tc.args...); code != ExitOK {
				t.Fatalf("exit = %d (stderr %q)", code, stderr)
			}
			if len(*pushed) != 0 {
				t.Errorf("pushed %+v, want nothing", *pushed)
			}
		})
	}
}

func TestUpReportsAFailedPushAndDoesNotForward(t *testing.T) {
	t.Setenv("CARAMELO_APP", "shop")
	t.Setenv("CARAMELO_ENV", "")
	withSSHMachine(t)
	fakeGit(t, inACheckout)
	fwd := &fakeForward{}
	fwd.install(t)
	old := gitPush
	gitPush = func(context.Context, pushPlan, io.Writer) error {
		return errors.New("git push HEAD:feat-x failed (exit 1)")
	}
	t.Cleanup(func() { gitPush = old })

	code, _, stderr := run(t, "up", "feat-x")
	if code != ExitError {
		t.Fatalf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "git push") {
		t.Errorf("stderr = %q", stderr)
	}
	if fwd.called {
		t.Error("the services were started although the code never arrived")
	}
}

func TestTheOtherCommandsDoNotPush(t *testing.T) {
	for _, args := range [][]string{
		{"down", "feat-x"},
		{"logs", "feat-x"},
		{"test", "feat-x"},
		{"run", "feat-x", "--", "true"},
	} {
		t.Setenv("CARAMELO_APP", "shop")
		t.Setenv("CARAMELO_ENV", "")
		withSSHMachine(t)
		fakeGit(t, inACheckout)
		(&fakeForward{}).install(t)
		pushed := capturePush(t)

		if code, _, stderr := run(t, args...); code != ExitOK {
			t.Fatalf("%v: exit = %d (stderr %q)", args, code, stderr)
		}
		if len(*pushed) != 0 {
			t.Errorf("%v: pushed %+v, want nothing", args, *pushed)
		}
	}
}

func TestEnvFromCheckout(t *testing.T) {
	cases := map[string]struct {
		top  string
		want string
	}{
		"an env worktree":    {"/mnt/caramelo/apps/shop/envs/feat-x/src", "feat-x"},
		"another data dir":   {"/srv/data/apps/shop/envs/feat-x/src", "feat-x"},
		"an ordinary clone":  {"/home/alex/code/shop", ""},
		"the app directory":  {"/mnt/caramelo/apps/shop", ""},
		"not under envs":     {"/mnt/caramelo/apps/shop/other/feat-x/src", ""},
		"an unusable name":   {"/mnt/caramelo/apps/shop/envs/Feat_X/src", ""},
		"no checkout at all": {"", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			answers := map[string]string(nil)
			if tc.top != "" {
				answers = map[string]string{"rev-parse --show-toplevel": tc.top}
			}
			fakeGit(t, answers)
			if got := envFromCheckout(context.Background(), "."); got != tc.want {
				t.Errorf("envFromCheckout() = %q, want %q", got, tc.want)
			}
		})
	}
}
