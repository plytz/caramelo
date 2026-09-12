package setup

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup/testutil"
)

func dirsEnv(t *testing.T, run *testutil.FakeRunner) (*Env, string) {
	t.Helper()
	env, _ := testEnv(t, run)
	dir := t.TempDir()
	env.ConfigDir = dir
	return env, dir
}

func TestDirsCheckOnABareBox(t *testing.T) {
	run := testutil.New()
	run.ExitPrefix("stat -c %U:%G:%a:%F -- ", 1)
	env, dir := dirsEnv(t, run)

	done, detail, err := (&DirsStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if done {
		t.Fatal("Check() said done on a box with no directories")
	}
	for _, want := range []string{dir + " missing", "/var/lib/caramelo missing", "/mnt/caramelo/apps missing", "config.yaml missing"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not mention %q", detail, want)
		}
	}
}

func TestDirsApplyCreatesTheLayout(t *testing.T) {
	run := testutil.New()
	run.ExitPrefix("stat -c %U:%G:%a:%F -- ", 1)
	env, dir := dirsEnv(t, run)

	if err := (&DirsStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v\n%s", err, run.Transcript())
	}

	wantRoot := map[string]string{
		"mkdir -p -- " + dir:            "",
		"mkdir -p -- /var/lib/caramelo": "",
		"mkdir -p -- /mnt/caramelo":     "",
	}
	wantUser := []string{
		"mkdir -p -- /var/lib/caramelo/ssh",
		"mkdir -p -- /var/lib/caramelo/setup",
		"mkdir -p -- /mnt/caramelo/apps",
		"mkdir -p -- /mnt/caramelo/docker",
	}
	for _, c := range run.Calls() {
		if _, ok := wantRoot[c.Line]; ok && c.Cmd.User != "" {
			t.Errorf("%q ran as %q, want root", c.Line, c.Cmd.User)
		}
		delete(wantRoot, c.Line)
	}
	for line := range wantRoot {
		t.Errorf("Apply did not run %q:\n%s", line, run.Transcript())
	}
	for _, line := range wantUser {
		call, found := run.Find(line)
		if !found {
			t.Errorf("Apply did not run %q:\n%s", line, run.Transcript())
			continue
		}
		if call.Cmd.User != "caramelo" {
			t.Errorf("%q ran as %q, want the caramelo user", line, call.Cmd.User)
		}
	}
	for _, line := range []string{
		"chown root:caramelo -- " + dir,
		"chmod 0750 -- " + dir,
		"chown caramelo:caramelo -- /var/lib/caramelo",
		"chmod 0700 -- /var/lib/caramelo/ssh",
		"chown caramelo:caramelo -- /mnt/caramelo",
	} {
		if !run.Ran(line) {
			t.Errorf("Apply did not run %q:\n%s", line, run.Transcript())
		}
	}

	got, err := serverconfig.Load(dir)
	if err != nil {
		t.Fatalf("config.yaml unreadable: %v", err)
	}
	if got != env.Config {
		t.Errorf("config.yaml = %+v, want %+v", got, env.Config)
	}
	st, err := os.Stat(serverconfig.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o640 {
		t.Errorf("config.yaml mode = %o, want 640", st.Mode().Perm())
	}
	if !run.Ran("chown root:caramelo -- " + serverconfig.Path(dir)) {
		t.Errorf("config.yaml was not given to the daemon's group:\n%s", run.Transcript())
	}
}

func TestDirsCheckIsDoneOnAFinishedBox(t *testing.T) {
	run := testutil.New()
	env, dir := dirsEnv(t, run)
	if err := serverconfig.Save(dir, env.Config, 0o640); err != nil {
		t.Fatal(err)
	}
	run.Stdout("stat -c %U:%G:%a:%F -- "+dir, "root:caramelo:750:directory\n")
	run.Stdout("stat -c %U:%G:%a:%F -- "+serverconfig.Path(dir), "root:caramelo:640:regular file\n")
	for _, d := range []string{"/var/lib/caramelo", "/var/lib/caramelo/setup", "/mnt/caramelo", "/mnt/caramelo/apps"} {
		run.Stdout("stat -c %U:%G:%a:%F -- "+d, "caramelo:caramelo:750:directory\n")
	}

	run.Stdout("stat -c %U:%G:%a:%F -- /mnt/caramelo/docker", "caramelo:caramelo:710:directory\n")
	run.Stdout("stat -c %U:%G:%a:%F -- /var/lib/caramelo/ssh", "caramelo:caramelo:700:directory\n")
	run.Stdout("stat -c %U:%G:%a:%F -- /var/lib/caramelo/vpn", "caramelo:caramelo:700:directory\n")

	done, detail, err := (&DirsStep{}).Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %q, %v; want done", done, detail, err)
	}
}

func TestDirsCheckNoticesAWrongOwner(t *testing.T) {
	run := testutil.New()
	env, dir := dirsEnv(t, run)
	if err := serverconfig.Save(dir, env.Config, 0o640); err != nil {
		t.Fatal(err)
	}
	run.RespondPrefix("stat -c %U:%G:%a:%F -- ", runner.Result{Stdout: "caramelo:caramelo:750:directory\n"})
	run.Stdout("stat -c %U:%G:%a:%F -- "+serverconfig.Path(dir), "root:caramelo:640:regular file\n")

	run.Stdout("stat -c %U:%G:%a:%F -- "+dir, "caramelo:caramelo:750:directory\n")

	done, detail, err := (&DirsStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if done {
		t.Fatal("Check() accepted a config directory owned by the wrong user")
	}
	if !strings.Contains(detail, "want root:caramelo") {
		t.Errorf("detail %q does not say what is wrong", detail)
	}
}

func TestDirsCheckNoticesAChangedConfig(t *testing.T) {
	run := testutil.New()
	env, dir := dirsEnv(t, run)
	other := env.Config
	other.SSHPort = 5022
	if err := serverconfig.Save(dir, other, 0o640); err != nil {
		t.Fatal(err)
	}
	run.RespondPrefix("stat -c %U:%G:%a:%F -- ", runner.Result{Stdout: "caramelo:caramelo:750:directory\n"})
	run.Stdout("stat -c %U:%G:%a:%F -- "+dir, "root:caramelo:750:directory\n")
	run.Stdout("stat -c %U:%G:%a:%F -- /var/lib/caramelo/ssh", "caramelo:caramelo:700:directory\n")
	run.Stdout("stat -c %U:%G:%a:%F -- /var/lib/caramelo/vpn", "caramelo:caramelo:700:directory\n")
	run.Stdout("stat -c %U:%G:%a:%F -- "+serverconfig.Path(dir), "root:caramelo:640:regular file\n")

	done, detail, err := (&DirsStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if done || !strings.Contains(detail, "differs") {
		t.Fatalf("Check() = %v, %q; want a difference on the port", done, detail)
	}
}

func TestDirsApplyReportsAChangedConfig(t *testing.T) {
	run := testutil.New()
	run.ExitPrefix("stat -c %U:%G:%a:%F -- ", 1)
	env, dir := dirsEnv(t, run)

	if err := (&DirsStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v\n%s", err, run.Transcript())
	}
	if !env.ConfigChanged {
		t.Error("writing config.yaml for the first time was not reported as a change")
	}

	env.ConfigChanged = false
	if err := (&DirsStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("second Apply() error: %v\n%s", err, run.Transcript())
	}
	if env.ConfigChanged {
		t.Error("a run that rewrote nothing claimed config.yaml had changed")
	}

	env.ConfigChanged = false
	env.Config.APIListen = serverconfig.APIListenBoth
	if err := (&DirsStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("third Apply() error: %v\n%s", err, run.Transcript())
	}
	if !env.ConfigChanged {
		t.Error("changing api_listen was not reported as a change")
	}
	got, err := serverconfig.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.APIListen != serverconfig.APIListenBoth {
		t.Errorf("api_listen = %q, want %q", got.APIListen, serverconfig.APIListenBoth)
	}
}
