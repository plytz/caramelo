package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func realPath(t *testing.T, dir string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("eval symlinks %q: %v", dir, err)
	}
	return p
}

func TestRunCapturesOutputAndExitCode(t *testing.T) {
	tests := []struct {
		name     string
		cmd      Cmd
		stdout   string
		stderr   string
		exitCode int
	}{
		{
			name:   "stdout",
			cmd:    Cmd{Name: "sh", Args: []string{"-c", "printf hi"}},
			stdout: "hi",
		},
		{
			name:   "stderr",
			cmd:    Cmd{Name: "sh", Args: []string{"-c", "printf oops >&2"}},
			stderr: "oops",
		},
		{
			name: "both streams",
			cmd:  Cmd{Name: "sh", Args: []string{"-c", "printf out; printf err >&2"}},

			stdout: "out",
			stderr: "err",
		},
		{
			name: "success",
			cmd:  Cmd{Name: "true"},
		},
		{
			name:     "failure is not an error",
			cmd:      Cmd{Name: "false"},
			exitCode: 1,
		},
		{
			name:     "explicit exit code",
			cmd:      Cmd{Name: "sh", Args: []string{"-c", "exit 42"}},
			exitCode: 42,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Exec{}.Run(context.Background(), tc.cmd)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if res.Stdout != tc.stdout {
				t.Errorf("stdout = %q, want %q", res.Stdout, tc.stdout)
			}
			if res.Stderr != tc.stderr {
				t.Errorf("stderr = %q, want %q", res.Stderr, tc.stderr)
			}
			if res.ExitCode != tc.exitCode {
				t.Errorf("exit code = %d, want %d", res.ExitCode, tc.exitCode)
			}
		})
	}
}

func TestRunStdin(t *testing.T) {
	res, err := Exec{}.Run(context.Background(), Cmd{
		Name:  "cat",
		Stdin: strings.NewReader("piped\n"),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Stdout != "piped\n" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "piped\n")
	}
}

func TestRunDir(t *testing.T) {
	dir := realPath(t, t.TempDir())
	res, err := Exec{}.Run(context.Background(), Cmd{
		Name: "sh", Args: []string{"-c", "pwd -P"}, Dir: dir,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != dir {
		t.Errorf("cwd = %q, want %q", got, dir)
	}
}

func TestRunEnv(t *testing.T) {

	t.Setenv("CARAMELO_TEST_INHERITED", "from-process")

	t.Setenv("CARAMELO_TEST_OVERRIDDEN", "from-process")
	res, err := Exec{}.Run(context.Background(), Cmd{
		Name: "sh",
		Args: []string{"-c", `printf '%s,%s,%s' "$CARAMELO_TEST_INHERITED" "$CARAMELO_TEST_OVERRIDDEN" "$CARAMELO_TEST_EXTRA"`},
		Env:  []string{"CARAMELO_TEST_OVERRIDDEN=from-cmd", "CARAMELO_TEST_EXTRA=extra"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if want := "from-process,from-cmd,extra"; res.Stdout != want {
		t.Errorf("env = %q, want %q", res.Stdout, want)
	}
}

func TestRunStreamsToWriters(t *testing.T) {
	var out, errb bytes.Buffer
	res, err := Exec{}.Run(context.Background(), Cmd{
		Name:   "sh",
		Args:   []string{"-c", "printf out; printf err >&2"},
		Stdout: &out,
		Stderr: &errb,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.String() != "out" || errb.String() != "err" {
		t.Errorf("writers got stdout=%q stderr=%q, want %q and %q", out.String(), errb.String(), "out", "err")
	}

	if res.Stdout != "" || res.Stderr != "" {
		t.Errorf("result = %+v, want empty streams", res)
	}
}

func TestRunLog(t *testing.T) {
	var log bytes.Buffer
	if _, err := (Exec{Log: &log}).Run(context.Background(), Cmd{
		Name: "sh", Args: []string{"-c", "exit 0"},
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if want := "+ sh -c exit 0\n"; log.String() != want {
		t.Errorf("log = %q, want %q", log.String(), want)
	}
}

func TestRunLogNotWrittenWhenPrepareFails(t *testing.T) {
	var log bytes.Buffer
	_, err := (Exec{Log: &log}).Run(context.Background(), Cmd{Name: "true", User: missingUser})
	if err == nil {
		t.Fatal("run: want error for an unknown user")
	}
	if log.Len() != 0 {
		t.Errorf("log = %q, want nothing logged for a command that never ran", log.String())
	}
}

func TestRunNotFound(t *testing.T) {
	res, err := Exec{}.Run(context.Background(), Cmd{Name: "caramelo-no-such-binary-xyz"})
	if err == nil {
		t.Fatal("run: want an error for a missing binary")
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("err = %v, want it to wrap exec.ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "caramelo-no-such-binary-xyz") {
		t.Errorf("err = %v, want the command name in the message", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0 for a command that never ran", res.ExitCode)
	}
}

func TestRunContextCancelled(t *testing.T) {
	t.Run("already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := (Exec{}).Run(ctx, Cmd{Name: "true"}); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	})
	t.Run("cancelled while running", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		start := time.Now()
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

		_, err := (Exec{}).Run(ctx, Cmd{Name: "sleep", Args: []string{"30"}})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("run took %v, want it to stop when the context was cancelled", d)
		}
	})
	t.Run("deadline exceeded", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if _, err := (Exec{}).Run(ctx, Cmd{Name: "sleep", Args: []string{"30"}}); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want context.DeadlineExceeded", err)
		}
	})
}

const missingUser = "caramelo-no-such-user-xyz"

func TestPrepareNoUser(t *testing.T) {
	c := Cmd{Name: "docker", Args: []string{"ps"}, Env: []string{"A=1"}}
	name, args, env, err := prepare(c)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if name != "docker" || !reflect.DeepEqual(args, []string{"ps"}) {
		t.Errorf("argv = %q %q, want docker [ps]", name, args)
	}

	if !reflect.DeepEqual(env, []string{"A=1"}) {
		t.Errorf("env = %q, want [A=1]", env)
	}
}

func TestPrepareUnknownUser(t *testing.T) {
	_, _, _, err := prepare(Cmd{Name: "true", User: missingUser})
	if err == nil {
		t.Fatal("prepare: want an error for an unknown user")
	}
	if !strings.Contains(err.Error(), missingUser) {
		t.Errorf("err = %v, want the user name in the message", err)
	}
}

func TestPrepareSameUserAddsSessionEnv(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Skipf("current user unavailable: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: prepare takes the runuser branch instead")
	}
	name, args, env, err := prepare(Cmd{
		Name: "docker", Args: []string{"ps"}, User: cur.Username, Env: []string{"DOCKER_HOST=override"},
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	if name != "docker" || !reflect.DeepEqual(args, []string{"ps"}) {
		t.Errorf("argv = %q %q, want docker [ps] unchanged", name, args)
	}
	want := append(userEnv(cur), "DOCKER_HOST=override")
	if !reflect.DeepEqual(env, want) {
		t.Errorf("env =\n %q\nwant\n %q", env, want)
	}

	if env[len(env)-1] != "DOCKER_HOST=override" {
		t.Errorf("last env entry = %q, want the caller's override", env[len(env)-1])
	}
}

func TestPrepareOtherUserWithoutRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: dropping privileges is allowed")
	}
	cur, err := user.Current()
	if err != nil {
		t.Skipf("current user unavailable: %v", err)
	}
	other := "root"
	if cur.Username == other {
		t.Skipf("current user is %q", other)
	}
	if _, err := user.Lookup(other); err != nil {
		t.Skipf("user %q not present: %v", other, err)
	}
	_, _, _, err = prepare(Cmd{Name: "true", User: other})
	if err == nil {
		t.Fatal("prepare: want an error when not root")
	}
	if !strings.Contains(err.Error(), "not root") {
		t.Errorf("err = %v, want a 'not root' error", err)
	}
}

func TestPrepareRunuserArgv(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("not root: prepare cannot build the runuser argv")
	}
	cur, err := user.Current()
	if err != nil {
		t.Fatalf("current user: %v", err)
	}
	name, args, env, err := prepare(Cmd{
		Name: "docker", Args: []string{"ps", "-a"}, User: cur.Username, Env: []string{"A=1"},
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if name != "runuser" {
		t.Errorf("name = %q, want runuser", name)
	}
	want := append([]string{"-u", cur.Username, "--", "env"}, userEnv(cur)...)
	want = append(want, "A=1", "docker", "ps", "-a")
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args =\n %q\nwant\n %q", args, want)
	}

	if env != nil {
		t.Errorf("env = %q, want nil: it is passed through env(1)", env)
	}
}

func TestSessionEnv(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Skipf("current user unavailable: %v", err)
	}
	got, err := SessionEnv(cur.Username)
	if err != nil {
		t.Fatalf("SessionEnv: %v", err)
	}
	want := []string{
		"HOME=" + cur.HomeDir,
		"USER=" + cur.Username,
		"LOGNAME=" + cur.Username,
		"XDG_RUNTIME_DIR=/run/user/" + cur.Uid,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + cur.Uid + "/bus",
		"DOCKER_HOST=unix:///run/user/" + cur.Uid + "/docker.sock",
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SessionEnv =\n %q\nwant\n %q", got, want)
	}
}

func TestSessionEnvUnknownUser(t *testing.T) {
	if _, err := SessionEnv(missingUser); err == nil {
		t.Fatal("SessionEnv: want an error for an unknown user")
	}
}

var _ Runner = Exec{}

func TestRunKillsTheWholeProcessGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := Exec{}.Run(ctx, Cmd{
		Name: "/bin/sh",
		Args: []string{"-c", "sh -c 'echo $$ > " + filepath.Join(t.TempDir(), "pid") + "; sleep 30' & wait"},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("the run took %v, want the timeout to kill the whole group", d)
	}
	if res.ExitCode == 0 {
		t.Errorf("exit code = %d, want the kill to show", res.ExitCode)
	}
}

func TestAKilledGroupLeavesNoGrandchildBehind(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "still-here")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := (Exec{}).Run(ctx, Cmd{
		Name: "/bin/sh",
		Args: []string{"-c", "sh -c 'sleep 1; touch " + marker + "' & wait"},
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("%s was written: a grandchild outlived the kill", marker)
	}
}
