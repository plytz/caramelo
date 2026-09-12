package sshapi_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/plytz/caramelo/internal/testutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/cli"
	"github.com/plytz/caramelo/internal/machine"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/sshapi"
	"github.com/plytz/caramelo/internal/state"
)

type fakeService struct {
	api.Service

	err error
}

func (f *fakeService) Status(ctx context.Context) (*api.Status, error) {
	if f.err != nil {
		return nil, f.err
	}
	sess, _ := sshapi.SessionFrom(ctx)
	return &api.Status{Version: "test", Transport: sess.Transport, Identity: sess.Identity, Machine: sess.Machine}, nil
}

func (f *fakeService) Machine(ctx context.Context) (*machine.Record, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &machine.Record{Hostname: "fake"}, nil
}

func (f *fakeService) Keys(ctx context.Context) ([]state.Key, error) {
	if f.err != nil {
		return nil, f.err
	}
	sess, _ := sshapi.SessionFrom(ctx)
	return []state.Key{{Name: "transport=" + sess.Transport, Type: "identity=" + sess.Identity}}, nil
}

func (f *fakeService) AddKey(ctx context.Context, name, options, line string) (*state.Key, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &state.Key{Name: name, Options: options, Fingerprint: "SHA256:fake", Type: strings.Fields(line)[0]}, nil
}

func (f *fakeService) RemoveKey(ctx context.Context, name string) error { return f.err }

type harness struct {
	server     *sshapi.Server
	service    *fakeService
	addr       string
	socket     string
	clientKey  gossh.Signer
	keyFile    string
	authorized string

	commands *commandLog
}

type commandLog struct {
	mu   sync.Mutex
	args [][]string
}

func (l *commandLog) add(args []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.args = append(l.args, append([]string(nil), args...))
}

func (l *commandLog) last() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.args) == 0 {
		return nil
	}
	return l.args[len(l.args)-1]
}

func start(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	cfg := serverconfig.Default()
	cfg.APIListen = serverconfig.APIListenPublic
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.RunDir = filepath.Join(testutil.ShortDir(t), "run")
	cfg.Bind = "127.0.0.1"
	cfg.SSHPort = 0

	clientSigner, keyFile := newClientKey(t, dir)
	authorized := cfg.AuthorizedKeysPath()
	line := strings.TrimSpace(string(gossh.MarshalAuthorizedKey(clientSigner.PublicKey())))
	if _, err := sshapi.AppendKey(authorized, line, "test-key"); err != nil {
		t.Fatal(err)
	}

	svc := &fakeService{}
	log := &commandLog{}
	srv := &sshapi.Server{
		Config:   cfg,
		Service:  svc,
		Version:  "test",
		Hostname: "testbox",
		Log:      &testWriter{t},
		Exec: func(ctx context.Context, c sshapi.Command) int {
			log.add(c.Args)
			return cli.RunWith(ctx, c.Args, c.Stdout, c.Stderr, cli.Options{Service: c.Service, Session: c.Session})
		},
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	waitUntilServing(t, srv.Addr().String(), srv.SocketPath())
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Serve did not stop within 10s of cancelling its context")
		}
	})

	return &harness{
		server: srv, service: svc, addr: srv.Addr().String(), socket: srv.SocketPath(),
		clientKey: clientSigner, keyFile: keyFile, authorized: authorized, commands: log,
	}
}

func waitUntilServing(t *testing.T, addrs ...string) {
	t.Helper()
	for _, addr := range addrs {
		network := "tcp"
		if strings.HasPrefix(addr, "/") {
			network = "unix"
		}
		deadline := time.Now().Add(10 * time.Second)
		var last error
		for time.Now().Before(deadline) {
			c, err := net.Dial(network, addr)
			if err != nil {
				last = err
				time.Sleep(10 * time.Millisecond)
				continue
			}
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			var buf [1]byte
			_, last = c.Read(buf[:])
			_ = c.Close()
			if last == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if last != nil {
			t.Fatalf("the server never answered on %s: %v", addr, last)
		}
	}
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Logf("server: %s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func newClientKey(t *testing.T, dir string) (gossh.Signer, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	block, err := gossh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return signer, path
}

func (h *harness) dialSSH(t *testing.T, user string, signer gossh.Signer) (*gossh.Client, error) {
	t.Helper()
	cfg := &gossh.ClientConfig{
		User:            user,
		HostKeyCallback: gossh.FixedHostKey(h.server.HostKey.PublicKey()),
		Timeout:         10 * time.Second,
	}
	if signer != nil {
		cfg.Auth = []gossh.AuthMethod{gossh.PublicKeys(signer)}
	}
	return gossh.Dial("tcp", h.addr, cfg)
}

func (h *harness) dialSocket(t *testing.T) *gossh.Client {
	t.Helper()
	conn, err := net.DialTimeout("unix", h.socket, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c, chans, reqs, err := gossh.NewClientConn(conn, h.socket, &gossh.ClientConfig{
		User:            serverconfig.DefaultUser,
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return gossh.NewClient(c, chans, reqs)
}

func run(t *testing.T, client *gossh.Client, args ...string) (int, string, string) {
	t.Helper()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	var stdout, stderr strings.Builder
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	err = sess.Run(remote.JoinArgs(args))
	code := 0
	var ee *gossh.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitStatus()
	default:
		t.Fatalf("running %v: %v", args, err)
	}
	t.Logf("%v -> exit %d\nstdout: %q\nstderr: %q", args, code, stdout.String(), stderr.String())
	return code, stdout.String(), stderr.String()
}

func TestSSHRunsTheCLI(t *testing.T) {
	h := start(t)
	client, err := h.dialSSH(t, serverconfig.DefaultUser, h.clientKey)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	code, stdout, stderr := run(t, client, "version", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
	}

	var v map[string]any
	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if v["version"] == "" {
		t.Errorf("no version in %v", v)
	}

	if code, _, _ := run(t, client, "caramelo", "version"); code != 0 {
		t.Errorf("`caramelo version` exit = %d, want 0", code)
	}
}

func TestSSHExitCodesPropagate(t *testing.T) {
	h := start(t)
	client, err := h.dialSSH(t, serverconfig.DefaultUser, h.clientKey)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	if code, _, _ := run(t, client, "nope"); code != cli.ExitUsage {
		t.Errorf("unknown command exit = %d, want %d", code, cli.ExitUsage)
	}

	h.service.err = errors.New("the store is on fire")
	code, _, stderr := run(t, client, "key", "list")
	if code != cli.ExitError {
		t.Errorf("failing command exit = %d, want %d", code, cli.ExitError)
	}
	if !strings.Contains(stderr, "on fire") {
		t.Errorf("stderr = %q, want the service error", stderr)
	}
}

func TestSSHNoCommandIsUsage(t *testing.T) {
	h := start(t)
	client, err := h.dialSSH(t, serverconfig.DefaultUser, h.clientKey)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	var stderr strings.Builder
	sess.Stderr = &stderr
	err = sess.Shell()
	if err == nil {
		err = sess.Wait()
	}
	var ee *gossh.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("shell request error = %v, want an exit status", err)
	}
	if ee.ExitStatus() != cli.ExitUsage {
		t.Errorf("exit = %d, want %d", ee.ExitStatus(), cli.ExitUsage)
	}
	if !strings.Contains(stderr.String(), "no interactive shell here") {
		t.Errorf("stderr = %q, want the usage line", stderr.String())
	}
}

func TestSSHRefusesServerCommands(t *testing.T) {
	h := start(t)
	client, err := h.dialSSH(t, serverconfig.DefaultUser, h.clientKey)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	for _, args := range [][]string{{"server", "setup"}, {"caramelo", "server", "run"}} {
		code, stdout, stderr := run(t, client, args...)
		if code != cli.ExitUsage {
			t.Errorf("%v exit = %d, want %d", args, code, cli.ExitUsage)
		}
		if !strings.Contains(stderr, "not available over the API") {
			t.Errorf("%v stderr = %q", args, stderr)
		}
		if stdout != "" {
			t.Errorf("%v stdout = %q, want empty", args, stdout)
		}
	}
	if got := h.commands.last(); got != nil && len(got) > 0 && got[0] == "server" {
		t.Errorf("a server command reached the CLI: %v", got)
	}
}

func TestSSHRejectsUnauthorizedKeyAndUser(t *testing.T) {
	h := start(t)
	dir := t.TempDir()
	other, _ := newClientKey(t, dir)

	if client, err := h.dialSSH(t, serverconfig.DefaultUser, other); err == nil {
		_ = client.Close()
		t.Error("an unauthorized key was accepted")
	}
	if client, err := h.dialSSH(t, "root", h.clientKey); err == nil {
		_ = client.Close()
		t.Error("a key authorized for caramelo was accepted for root")
	}
	if client, err := h.dialSSH(t, serverconfig.DefaultUser, nil); err == nil {
		_ = client.Close()
		t.Error("the TCP listener accepted a client with no key")
	}
}

func TestSSHAuthorizedKeysAreRereadPerConnection(t *testing.T) {
	h := start(t)
	if _, err := sshapi.RemoveKey(h.authorized, "test-key"); err != nil {
		t.Fatal(err)
	}
	if client, err := h.dialSSH(t, serverconfig.DefaultUser, h.clientKey); err == nil {
		_ = client.Close()
		t.Fatal("a revoked key still worked; the file is not re-read")
	}
	line := strings.TrimSpace(string(gossh.MarshalAuthorizedKey(h.clientKey.PublicKey())))
	if _, err := sshapi.AppendKey(h.authorized, line, "test-key"); err != nil {
		t.Fatal(err)
	}
	client, err := h.dialSSH(t, serverconfig.DefaultUser, h.clientKey)
	if err != nil {
		t.Fatalf("re-adding the key did not restore access: %v", err)
	}
	_ = client.Close()
}

func TestTransportIsReportedToTheService(t *testing.T) {
	h := start(t)

	sshClient, err := h.dialSSH(t, serverconfig.DefaultUser, h.clientKey)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sshClient.Close() }()
	_, stdout, _ := run(t, sshClient, "key", "list", "--json")
	if !strings.Contains(stdout, `"name":"transport=ssh"`) {
		t.Errorf("over TCP the service saw %q, want transport=ssh", stdout)
	}
	if !strings.Contains(stdout, `"type":"identity=test-key"`) {
		t.Errorf("over TCP the service saw %q, want identity=test-key", stdout)
	}

	sockClient := h.dialSocket(t)
	defer func() { _ = sockClient.Close() }()
	_, stdout, _ = run(t, sockClient, "key", "list", "--json")
	if !strings.Contains(stdout, `"name":"transport=socket"`) {
		t.Errorf("over the socket the service saw %q, want transport=socket", stdout)
	}
	if !strings.Contains(stdout, `"type":"identity="`) {
		t.Errorf("over the socket the service saw %q, want an empty identity", stdout)
	}
}

func TestSocketMode(t *testing.T) {
	h := start(t)
	fi, err := os.Stat(h.socket)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket (%v)", h.socket, fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o660 {
		t.Errorf("socket mode = %v, want 0660", perm)
	}
}

func TestSocketReplacesAStaleOne(t *testing.T) {
	dir := t.TempDir()
	cfg := serverconfig.Default()
	cfg.APIListen = serverconfig.APIListenPublic
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.RunDir = filepath.Join(testutil.ShortDir(t), "run")
	cfg.Bind = "127.0.0.1"
	cfg.SSHPort = 0

	if err := os.MkdirAll(cfg.RunDir, 0o750); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", cfg.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	srv := &sshapi.Server{Config: cfg, Version: "test", Exec: func(context.Context, sshapi.Command) int { return 0 }}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen must clear a stale socket: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	client := (&harness{socket: srv.SocketPath()}).dialSocket(t)
	_ = client.Close()
	cancel()
	if err := <-done; err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(cfg.SocketPath()); err == nil {
		t.Errorf("%s survived shutdown; it must be removed", cfg.SocketPath())
	}
}

func TestArgumentsSurviveQuoting(t *testing.T) {
	h := start(t)
	client := h.dialSocket(t)
	defer func() { _ = client.Close() }()

	want := []string{"version", "an argument with spaces", "it's", "$HOME", "a\tb", "--name=x y"}
	run(t, client, want...)
	got := h.commands.last()
	if len(got) != len(want) {
		t.Fatalf("server saw %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRealSSHClient(t *testing.T) {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh binary on this machine")
	}
	h := start(t)
	host, port, err := net.SplitHostPort(h.addr)
	if err != nil {
		t.Fatal(err)
	}

	sshArgs := func(args ...string) []string {
		base := []string{
			"-F", "/dev/null",
			"-o", "BatchMode=yes",
			"-o", "IdentitiesOnly=yes",
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-i", h.keyFile,
			"-p", port,
			serverconfig.DefaultUser + "@" + host,
			"--",
		}
		return append(base, remote.QuoteArgs(args)...)
	}
	runSSH := func(args ...string) (int, string, string) {
		t.Helper()
		cmd := exec.Command(sshBin, sshArgs(args...)...)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("running ssh: %v", err)
		}
		t.Logf("ssh %v -> exit %d\nstdout: %q\nstderr: %q", args, code, stdout.String(), stderr.String())
		return code, stdout.String(), stderr.String()
	}

	code, stdout, stderr := runSSH("version", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, `"version"`) {
		t.Errorf("stdout = %q", stdout)
	}
	if code, _, _ := runSSH("nope"); code != cli.ExitUsage {
		t.Errorf("unknown command exit = %d, want %d", code, cli.ExitUsage)
	}
	if code, _, stderr := runSSH("server", "setup"); code != cli.ExitUsage || !strings.Contains(stderr, "not available over the API") {
		t.Errorf("server setup: exit %d, stderr %q", code, stderr)
	}

	want := []string{"version", "a b", "it's"}
	runSSH(want...)
	if got := h.commands.last(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("server saw %q, want %q", got, want)
	}

	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	forwardPort := local.Addr().(*net.TCPAddr).Port
	_ = local.Close()
	cmd := exec.Command(sshBin,
		"-F", "/dev/null", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "ExitOnForwardFailure=yes",
		"-i", h.keyFile, "-p", port,
		"-L", strconv.Itoa(forwardPort)+":127.0.0.1:22",
		serverconfig.DefaultUser+"@"+host, "--", "version",
	)
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	_ = cmd.Run()
	if !strings.Contains(errOut.String(), "administratively prohibited") &&
		!strings.Contains(errOut.String(), "open failed") &&
		!strings.Contains(errOut.String(), "channel") {
		t.Logf("local forward attempt: stderr %q", errOut.String())
	}

	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", forwardPort), 200*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Error("a local port forward was set up; forwarding must be refused")
	}
}
