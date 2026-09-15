package cli

import (
	"bytes"
	"context"
	"errors"
	"github.com/plytz/caramelo/internal/testutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/sshapi"
)

func TestNewRootCmdBuildsTheWholeTree(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := NewRootCmd(&stdout, &stderr)

	if root.Use != "caramelo" {
		t.Errorf("root.Use = %q", root.Use)
	}
	if root.PersistentFlags().Lookup("json") == nil || root.PersistentFlags().Lookup("machine") == nil {
		t.Error("the global flags are missing")
	}
	names := map[string]bool{}
	for _, c := range root.Commands() {
		names[c.Name()] = true
	}
	for _, want := range []string{"version", "env", "server", "status"} {
		if !names[want] {
			t.Errorf("the tree has no %q command: %v", want, names)
		}
	}
	if names["completion"] {
		t.Error("the default completion command is not disabled")
	}

	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "caramelo "+version) {
		t.Errorf("stdout = %q, want the version on the writer passed in", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestExecuteRunsOsArgs(t *testing.T) {
	out := filepath.Join(t.TempDir(), "stdout")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	oldArgs, oldStdout := os.Args, os.Stdout
	os.Args, os.Stdout = []string{"caramelo", "version"}, f
	code := Execute()
	os.Args, os.Stdout = oldArgs, oldStdout

	if code != ExitOK {
		t.Fatalf("Execute() = %d, want %d", code, ExitOK)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "caramelo "+version) {
		t.Errorf("Execute wrote %q to stdout, want the version", b)
	}
}

func TestExecuteReportsAUsageError(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no %s: %v", os.DevNull, err)
	}
	defer devnull.Close()

	oldArgs, oldStdout, oldStderr := os.Args, os.Stdout, os.Stderr
	os.Args, os.Stdout, os.Stderr = []string{"caramelo", "nosuchcommand"}, devnull, devnull
	code := Execute()
	os.Args, os.Stdout, os.Stderr = oldArgs, oldStdout, oldStderr

	if code != ExitUsage {
		t.Errorf("Execute() on an unknown command = %d, want %d", code, ExitUsage)
	}
}

func TestRunWithToleratesTheLeadingProgramName(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWith(context.Background(), []string{"caramelo", "version"}, &stdout, &stderr, Options{})
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "caramelo "+version) {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestExitErrorCarriesTheCodeAndSaysSo(t *testing.T) {
	err := error(&exitError{7})
	if err.Error() != "exit 7" {
		t.Errorf("exitError.Error() = %q, want %q", err.Error(), "exit 7")
	}
	var xe *exitError
	if !errors.As(err, &xe) || xe.code != 7 {
		t.Errorf("errors.As did not recover the code from %v", err)
	}
}

func TestTransportStringNamesWhereTheCommandWent(t *testing.T) {
	socket := transport{kind: kindSocket, socketPath: "/run/caramelo/caramelod.sock"}
	if got := socket.String(); got != "socket /run/caramelo/caramelod.sock" {
		t.Errorf("socket transport = %q", got)
	}
	target := remote.Target{User: "caramelo", Host: "10.0.0.5", Port: 4022}
	ssh := transport{kind: kindSSH, target: target}
	if got, want := ssh.String(), "ssh "+target.String(); got != want {
		t.Errorf("ssh transport = %q, want %q", got, want)
	}
}

func TestTransportIsAnnouncedUnderCarameloDebug(t *testing.T) {
	noCommanderConfig(t)
	useSystemConfigDir(t, t.TempDir())
	t.Setenv("CARAMELO_DEBUG", "1")

	var stdout, stderr bytes.Buffer
	a := &app{stdout: &stdout, stderr: &stderr, machine: "caramelo@127.0.0.1:1", args: []string{"version"}}
	if _, err := forwardImpl(context.Background(), a); err != nil {

		t.Logf("forward: %v", err)
	}
	if !strings.Contains(stderr.String(), "transport: ssh caramelo@127.0.0.1:1") {
		t.Errorf("stderr = %q, want the transport line", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--machine") {
		t.Errorf("stderr = %q, want the rule that picked it", stderr.String())
	}
}

func TestExitCodeFromSSH(t *testing.T) {
	if code, err := exitCodeFromSSH(nil); code != ExitOK || err != nil {
		t.Errorf("a command that succeeded = %d, %v", code, err)
	}

	code, err := exitCodeFromSSH(&gossh.ExitMissingError{})
	if code != ExitError || err == nil {
		t.Errorf("a missing exit status = %d, %v; want an error", code, err)
	}
	if err != nil && !strings.Contains(err.Error(), "without an exit status") {
		t.Errorf("error = %v", err)
	}

	cause := errors.New("connection reset by peer")
	code, err = exitCodeFromSSH(cause)
	if code != ExitError || !errors.Is(err, cause) {
		t.Errorf("a transport failure = %d, %v; want it wrapped", code, err)
	}
	if err != nil && !strings.Contains(err.Error(), "caramelod") {
		t.Errorf("error = %v, want it to say where the command was running", err)
	}
}

func TestForwardSocketPassesTheDaemonsExitCodeBack(t *testing.T) {
	cases := []struct {
		daemon int
		want   int
	}{
		{daemon: 0, want: ExitOK},
		{daemon: 2, want: ExitUsage},
		{daemon: 7, want: 7},
		{daemon: 255, want: ExitError},
	}
	for _, c := range cases {
		socket := serveOnASocket(t, c.daemon)
		var stdout, stderr bytes.Buffer
		a := &app{stdout: &stdout, stderr: &stderr, args: []string{"version"}}
		code, err := forwardSocket(context.Background(), a,
			transport{kind: kindSocket, socketPath: socket, user: serverconfig.DefaultUser})
		if err != nil {
			t.Fatalf("daemon exit %d: %v", c.daemon, err)
		}
		if code != c.want {
			t.Errorf("daemon exit %d came back as %d, want %d", c.daemon, code, c.want)
		}
		if got := stdout.String(); !strings.Contains(got, "version") {
			t.Errorf("daemon exit %d: stdout = %q, want the command's output", c.daemon, got)
		}
	}
}

func TestForwardSocketReportsAMissingSocket(t *testing.T) {
	path := filepath.Join(testutil.ShortDir(t), "caramelod.sock")
	var stdout, stderr bytes.Buffer
	a := &app{stdout: &stdout, stderr: &stderr, args: []string{"version"}}
	code, err := forwardSocket(context.Background(), a, transport{kind: kindSocket, socketPath: path})
	if code != ExitError {
		t.Errorf("exit = %d, want %d", code, ExitError)
	}
	if err == nil || !strings.Contains(err.Error(), "no caramelod socket at "+path) {
		t.Fatalf("error = %v, want it to name the missing socket", err)
	}
	if !strings.Contains(err.Error(), "server setup") || !strings.Contains(err.Error(), "--machine") {
		t.Errorf("error = %v, want it to name both fixes", err)
	}
}

func serveOnASocket(t *testing.T, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	cfg := serverconfig.Default()
	cfg.APIListen = serverconfig.APIListenPublic
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.RunDir = filepath.Join(testutil.ShortDir(t), "run")
	cfg.Bind = "127.0.0.1"
	cfg.SSHPort = 0

	srv := &sshapi.Server{
		Config:  cfg,
		Service: forwardService{},
		Version: "test",
		Exec: func(_ context.Context, c sshapi.Command) int {
			_, _ = c.Stdout.Write([]byte(strings.Join(c.Args, " ") + "\n"))
			return exitCode
		},
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	return cfg.SocketPath()
}
