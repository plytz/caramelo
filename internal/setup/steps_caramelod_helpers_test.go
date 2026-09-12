package setup

import (
	"context"
	"os/user"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup/testutil"
)

func TestBinaryUpToDateComparesContent(t *testing.T) {
	cases := []struct {
		name      string
		installed string
		src       string
		want      bool
	}{
		{"nothing installed yet", "", "abc  /tmp/caramelo", false},
		{"same content", "abc  " + serverconfig.BinaryPath, "abc  /tmp/caramelo", true},
		{"a newer build", "abc  " + serverconfig.BinaryPath, "def  /tmp/caramelo", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := testutil.New()
			if c.installed == "" {
				run.Exit("sha256sum -- "+serverconfig.BinaryPath, 1)
			} else {
				run.Stdout("sha256sum -- "+serverconfig.BinaryPath, c.installed+"\n")
			}
			run.Stdout("sha256sum -- /tmp/caramelo", c.src+"\n")
			env, _ := testEnv(t, run)

			got, err := (&CaramelodStep{}).binaryUpToDate(context.Background(), env)
			if err != nil {
				t.Fatalf("binaryUpToDate: %v", err)
			}
			if got != c.want {
				t.Errorf("binaryUpToDate = %v, want %v", got, c.want)
			}
		})
	}
}

func TestBinaryUpToDateWithoutABinaryToInstall(t *testing.T) {
	env, _ := testEnv(t, testutil.New())
	env.BinaryPath = ""
	if _, err := (&CaramelodStep{}).binaryUpToDate(context.Background(), env); err == nil {
		t.Fatal("an empty BinaryPath must be an error, not a silent no-op")
	}
}

func TestBinaryUpToDateWhenSetupIsTheInstalledBinary(t *testing.T) {
	run := testutil.New()
	run.Strict = true
	env, _ := testEnv(t, run)
	env.BinaryPath = serverconfig.BinaryPath

	got, err := (&CaramelodStep{}).binaryUpToDate(context.Background(), env)
	if err != nil || !got {
		t.Fatalf("binaryUpToDate = %v, %v; want true with no commands run", got, err)
	}
}

func TestBinaryUpToDateFailsWhenTheSourceCannotBeRead(t *testing.T) {
	run := testutil.New()
	run.Stdout("sha256sum -- "+serverconfig.BinaryPath, "abc  "+serverconfig.BinaryPath+"\n")
	run.Exit("sha256sum -- /tmp/caramelo", 1)
	env, _ := testEnv(t, run)

	_, err := (&CaramelodStep{}).binaryUpToDate(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "/tmp/caramelo") {
		t.Fatalf("err = %v, want it to name the binary it could not read", err)
	}
}

func TestFileSHA256RejectsUnexpectedOutput(t *testing.T) {
	run := testutil.New()
	run.Stdout("sha256sum -- /x", "   \n")
	env, _ := testEnv(t, run)

	if _, err := fileSHA256(context.Background(), env, "/x"); err == nil {
		t.Fatal("empty sha256sum output must be an error")
	}
}

func TestInstallBinaryReportsAFailedCopy(t *testing.T) {
	run := testutil.New()
	run.Exit("sha256sum -- "+serverconfig.BinaryPath, 1)
	run.Stdout("sha256sum -- /tmp/caramelo", "abc  /tmp/caramelo\n")
	run.Respond("install -m 0755 -o root -g root /tmp/caramelo "+serverconfig.BinaryPath,
		runner.Result{ExitCode: 1, Stderr: "install: cannot create regular file: Read-only file system\n"})
	env, _ := testEnv(t, run)

	changed, err := (&CaramelodStep{}).installBinary(context.Background(), env)
	if err == nil {
		t.Fatal("a failing install must be an error")
	}
	if changed {
		t.Error("installBinary claimed a change it did not make")
	}
	if !strings.Contains(err.Error(), "Read-only file system") {
		t.Errorf("err = %v, want the reason install gave", err)
	}
}

func TestUserExists(t *testing.T) {
	run := testutil.New()
	run.Stdout("getent passwd caramelo", "caramelo:x:999:989::/var/lib/caramelo:/bin/sh\n")
	run.Exit("getent passwd ghost", 2)
	env, _ := testEnv(t, run)

	for name, want := range map[string]bool{"caramelo": true, "ghost": false} {
		got, err := userExists(context.Background(), env, name)
		if err != nil {
			t.Fatalf("userExists(%s): %v", name, err)
		}
		if got != want {
			t.Errorf("userExists(%s) = %v, want %v", name, got, want)
		}
	}
}

func TestHomeOf(t *testing.T) {
	run := testutil.New()
	run.Stdout("getent passwd caramelo", "caramelo:x:999:989::/var/lib/caramelo:/bin/sh\n")
	run.Exit("getent passwd ghost", 2)
	run.Stdout("getent passwd broken", "broken:x:1\n")
	env, _ := testEnv(t, run)
	ctx := context.Background()

	got, err := homeOf(ctx, env, "caramelo")
	if err != nil || got != "/var/lib/caramelo" {
		t.Errorf("homeOf(caramelo) = %q, %v; want /var/lib/caramelo", got, err)
	}
	if _, err := homeOf(ctx, env, "ghost"); err == nil || !strings.Contains(err.Error(), "unknown user") {
		t.Errorf("homeOf(ghost) err = %v, want an unknown-user error", err)
	}
	if _, err := homeOf(ctx, env, "broken"); err == nil {
		t.Error("a truncated passwd entry must be an error")
	}
}

func TestDefaultKeyNameIdentifiesThePersonAndTheBox(t *testing.T) {
	sudoUser(t, "alice")
	run := testutil.New()
	run.Stdout("hostname", "box\n")
	env, _ := testEnv(t, run)

	if got := (&CaramelodStep{}).defaultKeyName(context.Background(), env); got != "alice@box" {
		t.Errorf("defaultKeyName = %q, want alice@box", got)
	}
}

func TestDefaultKeyNameFallsBackWhenThereIsNoHostname(t *testing.T) {
	sudoUser(t, "alice")
	run := testutil.New()
	run.Exit("hostname", 1)
	env, _ := testEnv(t, run)

	if got := (&CaramelodStep{}).defaultKeyName(context.Background(), env); got != "alice" {
		t.Errorf("defaultKeyName = %q, want alice", got)
	}
}

func TestDefaultKeyNameFallsBackToRootWithoutSudo(t *testing.T) {
	sudoUser(t, "")
	run := testutil.New()
	run.Stdout("hostname", "box\n")
	env, _ := testEnv(t, run)

	if got := (&CaramelodStep{}).defaultKeyName(context.Background(), env); got != "root@box" {
		t.Errorf("defaultKeyName = %q, want root@box", got)
	}
}

func TestPortListeningReportsWhatSSSays(t *testing.T) {
	run := testutil.New()
	run.Stdout("ss -ltn", "State Recv-Q Send-Q Local Address:Port\nLISTEN 0 128 [::]:4022 [::]:*\n")
	env, _ := testEnv(t, run)

	got, err := portListening(context.Background(), env, 4022)
	if err != nil || !got {
		t.Errorf("portListening(4022) = %v, %v; want true", got, err)
	}
	got, err = portListening(context.Background(), env, 22)
	if err != nil || got {
		t.Errorf("portListening(22) = %v, %v; want false", got, err)
	}
}

func TestPortListeningFailsWhenSSDoes(t *testing.T) {
	run := testutil.New()
	run.Respond("ss -ltn", runner.Result{ExitCode: 127, Stderr: "ss: command not found\n"})
	env, _ := testEnv(t, run)

	if _, err := portListening(context.Background(), env, 4022); err == nil {
		t.Fatal("a failing ss must be an error")
	}
}

func TestUnitActiveAsksTheUsersSystemd(t *testing.T) {
	run := testutil.New()
	run.Respond("systemctl --user is-active --quiet "+UserUnit, runner.Result{})
	env, _ := testEnv(t, run)

	got, err := (&CaramelodStep{}).unitActive(context.Background(), env)
	if err != nil || !got {
		t.Fatalf("unitActive = %v, %v; want true", got, err)
	}
	call, ok := run.Find("systemctl --user is-active")
	if !ok {
		t.Fatal("systemctl was never run")
	}
	if call.Cmd.User != env.Config.User {
		t.Errorf("asked as %q, want the daemon's own user %q", call.Cmd.User, env.Config.User)
	}
}

func TestUnitActiveReportsARunnerFailure(t *testing.T) {
	run := testutil.New()
	run.Fail("systemctl --user is-active --quiet "+UserUnit, errBoom)
	env, _ := testEnv(t, run)

	if _, err := (&CaramelodStep{}).unitActive(context.Background(), env); err == nil {
		t.Fatal("a runner failure must reach the caller")
	}
}

func TestSmokeTestFailureNamesTheCommand(t *testing.T) {
	run := testutil.New()
	run.Respond(serverconfig.BinaryPath+" status --json",
		runner.Result{ExitCode: 1, Stderr: "dial unix /run/caramelo/caramelod.sock: connection refused\n"})
	env, _ := testEnv(t, run)

	err := (&CaramelodStep{}).smokeTest(context.Background(), env)
	if err == nil {
		t.Fatal("a failing smoke test must be an error")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err = %v, want the reason the daemon gave", err)
	}
}

func shortStartTimeout(t *testing.T) {
	t.Helper()
	prev := startTimeout
	startTimeout = 10 * time.Millisecond
	t.Cleanup(func() { startTimeout = prev })
}

func TestWaitListeningNamesThePortItGaveUpOn(t *testing.T) {
	stubSleep(t)
	shortStartTimeout(t)
	run := testutil.New()
	run.Respond("test -e /run/caramelo/caramelod.sock", runner.Result{})
	run.Stdout("ss -ltn", "State Recv-Q Send-Q Local Address:Port\n")
	run.Stdout("ss -lun", "State Recv-Q Send-Q Local Address:Port\n")
	env, _ := testEnv(t, run)
	env.Config.APIListen = serverconfig.APIListenBoth

	err := (&CaramelodStep{}).waitListening(context.Background(), env)
	if err == nil {
		t.Fatal("waitListening must give up when nothing ever listens")
	}
	if !strings.Contains(err.Error(), "port 4022") {
		t.Errorf("err = %v, want it to name the port", err)
	}

	env.Config.APIListen = serverconfig.APIListenVPN
	err = (&CaramelodStep{}).waitListening(context.Background(), env)
	if err == nil {
		t.Fatal("waitListening must give up when the tunnel never comes up")
	}
	if !strings.Contains(err.Error(), "udp 4021") {
		t.Errorf("err = %v, want it to name the tunnel's port", err)
	}
}

func TestUDPListeningReportsWhatSSSays(t *testing.T) {
	run := testutil.New()
	run.Stdout("ss -lun", "State Recv-Q Send-Q Local Address:Port Peer Address:Port\nUNCONN 0 0 0.0.0.0:4021 0.0.0.0:*\nUNCONN 0 0 [::]:4021 [::]:*\n")
	env, _ := testEnv(t, run)

	if got, err := udpListening(context.Background(), env, "4021"); err != nil || !got {
		t.Errorf("udpListening(4021) = %v, %v; want true", got, err)
	}
	if got, err := udpListening(context.Background(), env, "4022"); err != nil || got {
		t.Errorf("udpListening(4022) = %v, %v; want false", got, err)
	}
}

func TestWaitListeningNamesTheSocketItGaveUpOn(t *testing.T) {
	stubSleep(t)
	shortStartTimeout(t)
	run := testutil.New()
	run.Exit("test -e /run/caramelo/caramelod.sock", 1)
	env, _ := testEnv(t, run)

	err := (&CaramelodStep{}).waitListening(context.Background(), env)
	if err == nil {
		t.Fatal("waitListening must give up when the socket never appears")
	}
	for _, want := range []string{"/run/caramelo/caramelod.sock", "journalctl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
}

func withUser(t *testing.T) {
	t.Helper()
	prev := lookupUser
	lookupUser = func(name string) (*user.User, error) { return &user.User{Username: name}, nil }
	t.Cleanup(func() { lookupUser = prev })
}

func TestExecuteRecordsTheRunUnderTheStateDirectory(t *testing.T) {
	withUser(t)
	run := testutil.New()
	env, _ := testEnv(t, run)

	rep := Execute(context.Background(), []Step{&fakeStep{name: "one", done: true}}, env, nil)

	call, ok := run.Find("tee -- /var/lib/caramelo/setup/")
	if !ok {
		t.Fatalf("the run was not recorded:\n%s", run.Transcript())
	}
	if !strings.Contains(call.Line, rep.RunID+".json") {
		t.Errorf("wrote %q, want the run id in the file name", call.Line)
	}
	if call.Cmd.User != env.Config.User {
		t.Errorf("wrote as %q, want the caramelo user", call.Cmd.User)
	}
	if !strings.Contains(call.Stdin, `"run_id"`) || !strings.Contains(call.Stdin, `"one"`) {
		t.Errorf("report content = %q, want the JSON report", call.Stdin)
	}

	if !run.Ran("mkdir -p -- /var/lib/caramelo/setup") {
		t.Errorf("the report directory was not created:\n%s", run.Transcript())
	}
	if !run.Ran("chmod 0640 -- /var/lib/caramelo/setup/") {
		t.Errorf("the report was left with the default mode:\n%s", run.Transcript())
	}
}

func TestExecuteDoesNotRecordADryRun(t *testing.T) {
	withUser(t)
	run := testutil.New()
	env, _ := testEnv(t, run)
	env.DryRun = true

	Execute(context.Background(), []Step{&fakeStep{name: "one", detail: "missing"}}, env, nil)
	if run.Ran("tee -- /var/lib/caramelo/setup/") {
		t.Errorf("a dry run wrote a report:\n%s", run.Transcript())
	}
}

func TestExecuteWarnsButSucceedsWhenTheReportCannotBeWritten(t *testing.T) {
	withUser(t)
	run := testutil.New()
	run.RespondPrefix("tee -- /var/lib/caramelo/setup/",
		runner.Result{ExitCode: 1, Stderr: "tee: no space left on device\n"})
	env, log := testEnv(t, run)

	rep := Execute(context.Background(), []Step{&fakeStep{name: "one", done: true}}, env, nil)
	if rep.Failed != 0 {
		t.Errorf("failed = %d, want 0: a report that cannot be written does not fail the run", rep.Failed)
	}
	if !strings.Contains(log.String(), "could not record this run") {
		t.Errorf("log = %q, want a warning", log.String())
	}
}

func TestNewRunIDIsSortableAndUsableAsAFileName(t *testing.T) {
	id := NewRunID(time.Date(2026, 9, 8, 14, 3, 4, 500_000_000, time.UTC))
	if want := "20260908T140304.500Z"; id != want {
		t.Errorf("NewRunID = %q, want %q", id, want)
	}
	if strings.ContainsAny(id, "/: ") {
		t.Errorf("NewRunID = %q, want nothing a file name cannot hold", id)
	}
}
