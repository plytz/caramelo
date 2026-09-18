package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"github.com/plytz/caramelo/internal/testutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/machine"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/sshapi"
	"github.com/plytz/caramelo/internal/state"
)

func useSystemConfigDir(t *testing.T, dir string) {
	t.Helper()
	t.Setenv(serverconfig.ConfigDirEnv, dir)
}

func useCommanderConfig(t *testing.T, c remote.CommanderConfig) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "caramelo", "config.yaml")
	if err := remote.SaveCommanderConfigTo(path, c); err != nil {
		t.Fatal(err)
	}
}

func noCommanderConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CARAMELO_FLEET", "")
}

func oneFleet() remote.CommanderConfig {
	return remote.CommanderConfig{
		Name: "laptop",
		Role: remote.RoleCommander,
		Commander: remote.Commander{
			DefaultFleet: "home",
			Fleets:       map[string]remote.Fleet{"home": {Hub: "alex@10.0.0.5"}},
		},
	}
}

func twoFleets() remote.CommanderConfig {
	return remote.CommanderConfig{
		Name: "laptop",
		Role: remote.RoleCommander,
		Commander: remote.Commander{
			Fleets: map[string]remote.Fleet{
				"home": {Hub: "alex@10.0.0.5", Apps: []string{"shop"}},
				"work": {Hub: "ops@hub.work.example:4023", Apps: []string{"blog"}},
			},
		},
	}
}

func makeSocket(t *testing.T, dir string, keep bool) string {
	t.Helper()
	path := filepath.Join(dir, "caramelod.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if keep {
		ln.(*net.UnixListener).SetUnlinkOnClose(false)
		if err := ln.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	t.Cleanup(func() { _ = ln.Close() })
	return path
}

func writeServerConfig(t *testing.T, runDir string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := serverconfig.Default()
	cfg.Name, cfg.Hub.Fleet = "box", "home"
	cfg.APIListen = serverconfig.APIListenPublic
	cfg.RunDir = runDir
	cfg.StateDir = filepath.Join(runDir, "state")
	cfg.DataDir = filepath.Join(runDir, "data")
	if err := serverconfig.Save(dir, cfg, 0o640); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveTransportUsesTheLocalSocket(t *testing.T) {
	noCommanderConfig(t)
	runDir := testutil.ShortDir(t)
	useSystemConfigDir(t, writeServerConfig(t, runDir))
	socket := makeSocket(t, runDir, false)

	got, err := resolveTransport(context.Background(), &app{})
	if err != nil {
		t.Fatal(err)
	}
	if got.kind != kindSocket {
		t.Fatalf("kind = %q, want %q", got.kind, kindSocket)
	}
	if got.socketPath != socket {
		t.Errorf("socket = %q, want %q", got.socketPath, socket)
	}
}

func TestResolveTransportMachineLocalForcesTheSocket(t *testing.T) {

	useCommanderConfig(t, oneFleet())
	useSystemConfigDir(t, t.TempDir())

	got, err := resolveTransport(context.Background(), &app{machine: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if got.kind != kindSocket {
		t.Fatalf("kind = %q, want %q", got.kind, kindSocket)
	}
	if want := serverconfig.Default().SocketPath(); got.socketPath != want {
		t.Errorf("socket = %q, want the default %q", got.socketPath, want)
	}
}

func TestResolveTransportMachineFlagBeatsTheSocket(t *testing.T) {
	noCommanderConfig(t)
	runDir := testutil.ShortDir(t)
	useSystemConfigDir(t, writeServerConfig(t, runDir))
	makeSocket(t, runDir, false)

	got, err := resolveTransport(context.Background(), &app{machine: "alex@box:2222"})
	if err != nil {
		t.Fatal(err)
	}
	if got.kind != kindSSH {
		t.Fatalf("kind = %q, want %q", got.kind, kindSSH)
	}
	if want := (remote.Target{User: "alex", Host: "box", Port: 2222}); got.target != want {
		t.Errorf("target = %+v, want %+v", got.target, want)
	}
}

func TestResolveTransportFleetFlag(t *testing.T) {
	useCommanderConfig(t, twoFleets())
	useSystemConfigDir(t, t.TempDir())

	got, err := resolveTransport(context.Background(), &app{fleet: "work"})
	if err != nil {
		t.Fatal(err)
	}
	want := remote.Target{User: "ops", Host: "hub.work.example", Port: 4023}
	if got.kind != kindSSH || got.target != want || got.fleet != "work" {
		t.Errorf("got %+v, want ssh to %+v for fleet work", got, want)
	}
}

func TestResolveTransportDefaultFleet(t *testing.T) {
	cfg := twoFleets()
	cfg.Commander.DefaultFleet = "home"
	useCommanderConfig(t, cfg)
	useSystemConfigDir(t, t.TempDir())

	got, err := resolveTransport(context.Background(), &app{})
	if err != nil {
		t.Fatal(err)
	}
	want := remote.Target{User: "alex", Host: "10.0.0.5", Port: serverconfig.DefaultSSHPort}
	if got.kind != kindSSH || got.target != want || got.fleet != "home" {
		t.Errorf("got %+v, want ssh to %+v for fleet home", got, want)
	}
}

func TestResolveTransportAppFleet(t *testing.T) {
	cfg := twoFleets()
	cfg.Commander.DefaultFleet = "home"
	useCommanderConfig(t, cfg)
	useSystemConfigDir(t, t.TempDir())

	got, err := resolveTransport(context.Background(),
		&app{appHint: func() string { return "blog" }})
	if err != nil {
		t.Fatal(err)
	}
	if got.fleet != "work" {
		t.Errorf("fleet = %q, want the fleet recorded for blog", got.fleet)
	}
}

func TestResolveTransportRefusesToGuessBetweenFleets(t *testing.T) {
	useCommanderConfig(t, twoFleets())
	useSystemConfigDir(t, t.TempDir())

	_, err := resolveTransport(context.Background(), &app{})
	if err == nil {
		t.Fatal("want a refusal when two fleets are configured and nothing picks one")
	}
	for _, want := range []string{"home, work", "--fleet"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestResolveTransportWithNothingConfigured(t *testing.T) {
	noCommanderConfig(t)
	useSystemConfigDir(t, t.TempDir())

	_, err := resolveTransport(context.Background(), &app{})
	if err == nil {
		t.Fatal("want an error when there is no socket and no machine")
	}
	msg := err.Error()
	for _, want := range []string{"hub setup", "--machine", "fleet add"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q; it must name both fixes", msg, want)
		}
	}
}

func TestForwardSocketReportsARefusedSocket(t *testing.T) {
	noCommanderConfig(t)
	runDir := testutil.ShortDir(t)
	useSystemConfigDir(t, writeServerConfig(t, runDir))
	socket := makeSocket(t, runDir, true)

	var stdout, stderr bytes.Buffer
	a := &app{stdout: &stdout, stderr: &stderr, args: []string{"key", "list"}}
	code, err := forwardImpl(context.Background(), a)
	if code != ExitError {
		t.Errorf("exit = %d, want %d", code, ExitError)
	}
	if err == nil {
		t.Fatal("want an error, not a silent fall-through to SSH")
	}
	if !strings.Contains(err.Error(), "refused the connection") || !strings.Contains(err.Error(), socket) {
		t.Errorf("error = %v, want it to say the socket refused the connection", err)
	}
}

func TestResolveTransportRejectsABadMachine(t *testing.T) {
	noCommanderConfig(t)
	useSystemConfigDir(t, t.TempDir())
	if _, err := resolveTransport(context.Background(), &app{machine: "alex@box:notaport"}); err == nil {
		t.Fatal("want an error for an unparseable machine")
	}
}

type forwardService struct{ api.Service }

func (forwardService) Status(ctx context.Context) (*api.Status, error) {
	sess, _ := sshapi.SessionFrom(ctx)
	return &api.Status{Transport: sess.Transport}, nil
}
func (forwardService) Machine(context.Context) (*machine.Record, error) {
	return &machine.Record{}, nil
}
func (forwardService) Keys(ctx context.Context) ([]state.Key, error) {
	sess, _ := sshapi.SessionFrom(ctx)
	return []state.Key{{Name: "over-" + sess.Transport, Type: "ssh-ed25519", Fingerprint: "SHA256:x"}}, nil
}
func (forwardService) AddKey(ctx context.Context, name, options, line string) (*state.Key, error) {
	return &state.Key{Name: name, Options: options, Type: strings.Fields(line)[0], Fingerprint: "SHA256:x"}, nil
}
func (forwardService) RemoveKey(context.Context, string) error { return nil }

func TestForwardOverTheSocketEndToEnd(t *testing.T) {
	noCommanderConfig(t)
	dir := t.TempDir()
	cfg := serverconfig.Default()
	cfg.Name, cfg.Hub.Fleet = "box", "home"
	cfg.APIListen = serverconfig.APIListenPublic
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.RunDir = filepath.Join(testutil.ShortDir(t), "run")
	cfg.Bind = "127.0.0.1"
	cfg.SSHPort = 0

	saved := cfg
	saved.SSHPort = serverconfig.DefaultSSHPort
	etc := t.TempDir()
	if err := serverconfig.Save(etc, saved, 0o640); err != nil {
		t.Fatal(err)
	}
	useSystemConfigDir(t, etc)

	srv := &sshapi.Server{
		Config:  cfg,
		Service: forwardService{},
		Version: "test",
		Exec:    execCommand,
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

	var stdout, stderr bytes.Buffer
	code := Run([]string{"key", "list", "--json"}, &stdout, &stderr)
	t.Logf("exit %d\nstdout: %q\nstderr: %q", code, stdout.String(), stderr.String())
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), `"name":"over-socket"`) {
		t.Errorf("stdout = %q, want the daemon's answer with transport socket", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"key", "remove"}, &stdout, &stderr); code != ExitUsage {
		t.Errorf("a usage error over the socket = %d, want %d", code, ExitUsage)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"hub", "run", "--config-dir", filepath.Join(dir, "missing")}, &stdout, &stderr); code != ExitError {
		t.Errorf("hub run with no config = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "config.yaml") {
		t.Errorf("stderr = %q, want it to name the missing config", stderr.String())
	}
}

func TestForwardStdinIgnoresATerminal(t *testing.T) {

	if r := forwardStdin(); r != nil && !isNullStdin() {
		t.Errorf("forwardStdin() = %v, want nil when nothing is piped in", r)
	}
}

func isNullStdin() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice == 0
}

func TestForwardOverSSHEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh binary on this machine")
	}
	noCommanderConfig(t)
	dir := t.TempDir()
	cfg := serverconfig.Default()
	cfg.Name, cfg.Hub.Fleet = "box", "home"
	cfg.APIListen = serverconfig.APIListenPublic
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.RunDir = filepath.Join(testutil.ShortDir(t), "run")
	cfg.Bind = "127.0.0.1"
	cfg.SSHPort = 0

	keyFile := writeClientKey(t, dir, cfg.AuthorizedKeysPath())

	useSSHWrapper(t, keyFile)

	useShortCacheDir(t)

	srv := &sshapi.Server{Config: cfg, Service: forwardService{}, Version: "test", Exec: execCommand}
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

	_, port, err := net.SplitHostPort(srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	target := serverconfig.DefaultUser + "@127.0.0.1:" + port

	var stdout, stderr bytes.Buffer
	a := &app{stdout: &stdout, stderr: &stderr, machine: target, args: []string{"key", "list", "--json"}}
	code, err := forwardImpl(context.Background(), a)
	t.Logf("exit %d err %v\nstdout: %q\nstderr: %q", code, err, stdout.String(), stderr.String())
	if err != nil {
		t.Fatal(err)
	}
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), `"name":"over-ssh"`) {
		t.Errorf("stdout = %q, want the daemon's answer with transport ssh", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	a = &app{stdout: &stdout, stderr: &stderr, machine: target, args: []string{"nope"}}
	if code, err := forwardImpl(context.Background(), a); err != nil || code != ExitUsage {
		t.Errorf("unknown command over ssh = %d, %v; want %d", code, err, ExitUsage)
	}

	stdout.Reset()
	stderr.Reset()
	a = &app{stdout: &stdout, stderr: &stderr, machine: "caramelo@127.0.0.1:1", args: []string{"version"}}
	if code, err := forwardImpl(context.Background(), a); err != nil || code != ExitError {
		t.Errorf("unreachable machine = %d, %v; want %d, the same as over the socket", code, err, ExitError)
	}

	if cp, err := controlPath(); err == nil {
		_ = exec.Command("ssh", "-o", "ControlPath="+cp, "-O", "exit",
			"-p", port, serverconfig.DefaultUser+"@127.0.0.1").Run()
	}
}

func writeClientKey(t *testing.T, dir, authorizedKeys string) string {
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
	line := strings.TrimSpace(string(gossh.MarshalAuthorizedKey(signer.PublicKey())))
	if _, err := sshapi.AppendKey(authorizedKeys, line, "test-key"); err != nil {
		t.Fatal(err)
	}
	return path
}

func useSSHWrapper(t *testing.T, keyFile string) {
	t.Helper()
	real, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh binary on this machine")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nexec " + real +
		" -F /dev/null -o IdentitiesOnly=yes -o StrictHostKeyChecking=no" +
		" -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -i " + keyFile + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func useShortCacheDir(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_CACHE_HOME", dir)
}

func TestControlPathRefusesADeepCacheDir(t *testing.T) {

	deep := filepath.Join(t.TempDir(), strings.Repeat("d", 47))
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", deep)
	if cp, err := controlPath(); err == nil {
		t.Errorf("controlPath() = %q (%d bytes), want an error", cp, len(cp)+17)
	}

	useShortCacheDir(t)
	cp, err := controlPath()
	if err != nil {
		t.Fatalf("controlPath() with a short cache dir: %v", err)
	}
	if len(cp)-len("%C")+40+17 > maxControlPath {
		t.Errorf("controlPath() = %q leaves no room for ssh's suffix", cp)
	}
}
