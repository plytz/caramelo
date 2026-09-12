//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	capi "github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/machine"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/test/integration/itest"
)

const suite = "api"

func box(t *testing.T) (*itest.Machine, itest.SSHAPIOptions) {
	t.Helper()
	lab := itest.New(t, itest.Options{Suite: suite, State: itest.StateProvisioned})
	return ready(t, lab.Machine(itest.RoleHub))
}

func ready(t *testing.T, m *itest.Machine) (*itest.Machine, itest.SSHAPIOptions) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
	defer cancel()
	if err := itest.WaitForPort(ctx, m, itest.CarameloSSHPort); err != nil {
		t.Fatalf("caramelod on %s never listened on %d: %v", m.Alias, itest.CarameloSSHPort, err)
	}
	return m, itest.SSHAPIFor(t, m)
}

func withStdin(o itest.SSHAPIOptions, in string) itest.SSHAPIOptions {
	o.Stdin = strings.NewReader(in)
	o.Timeout = time.Minute
	return o
}

func TestMachineShow(t *testing.T) {
	m, o := box(t)
	res := itest.SSHAPIRun(t, o, "machine", "show", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("machine show: exit %d\nstdout:%s\nstderr:%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	var rec machine.Record
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &rec); err != nil {
		t.Fatalf("machine show --json: %v\nstdout: %q", err, res.Stdout)
	}
	nproc := strings.TrimSpace(m.MustRun(t, "nproc").Stdout)
	if want, err := strconv.Atoi(nproc); err != nil {
		t.Errorf("nproc said %q: %v", nproc, err)
	} else if rec.CPU.Count != want {
		t.Errorf("cpu.count = %d, want %d (what nproc says on %s)", rec.CPU.Count, want, m.Name)
	}
	if !rec.Docker.Rootless {
		t.Errorf("docker.rootless = false, want true: %+v", rec.Docker)
	}
	if rec.Dirs.Data != "/mnt/caramelo" {
		t.Errorf("dirs.data = %q, want /mnt/caramelo", rec.Dirs.Data)
	}
	if rec.Hostname == "" || rec.MachineID == "" {
		t.Errorf("empty hostname or machine_id: %+v", rec)
	}
}

func TestStatusOverSSH(t *testing.T) {
	_, o := box(t)
	res := itest.SSHAPIRun(t, o, "status", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("status: exit %d\nstderr:%s", res.ExitCode, res.Stderr)
	}
	st := decodeStatus(t, res.Stdout)
	if st.Transport != "ssh" {
		t.Errorf("transport = %q, want ssh", st.Transport)
	}
	if st.SSHPort != itest.CarameloSSHPort {
		t.Errorf("ssh_port = %d, want %d", st.SSHPort, itest.CarameloSSHPort)
	}
	if st.HostKeyFingerprint == "" {
		t.Error("empty host key fingerprint")
	}
}

func TestExitCodes(t *testing.T) {
	_, o := box(t)

	t.Run("unknown command", func(t *testing.T) {
		res := itest.SSHAPIRun(t, o, "nope")
		if res.ExitCode != 2 {
			t.Errorf("exit = %d, want 2\nstdout:%s\nstderr:%s", res.ExitCode, res.Stdout, res.Stderr)
		}
	})

	t.Run("no command", func(t *testing.T) {
		res := itest.SSHAPIRun(t, o)
		if res.ExitCode != 2 {
			t.Errorf("exit = %d, want 2\nstdout:%s\nstderr:%s", res.ExitCode, res.Stdout, res.Stderr)
		}
		if !strings.Contains(strings.ToLower(res.Stderr), "no interactive shell") {
			t.Errorf("stderr does not explain that there is no shell: %q", res.Stderr)
		}
	})

	t.Run("server commands are refused", func(t *testing.T) {
		res := itest.SSHAPIRun(t, o, "server", "setup")
		if res.ExitCode != 2 {
			t.Errorf("exit = %d, want 2\nstdout:%s\nstderr:%s", res.ExitCode, res.Stdout, res.Stderr)
		}
	})
}

func TestEdgeOverTheAPI(t *testing.T) {
	_, o := box(t)

	t.Run("the edge process is refused", func(t *testing.T) {
		res := itest.SSHAPIRun(t, o, "edge")
		if res.ExitCode != 2 {
			t.Errorf("exit = %d, want 2\nstdout:%s\nstderr:%s", res.ExitCode, res.Stdout, res.Stderr)
		}
		if !strings.Contains(strings.ToLower(res.Stderr), "edge") {
			t.Errorf("stderr does not say what was refused: %q", res.Stderr)
		}
	})

	t.Run("edge status answers", func(t *testing.T) {
		res := itest.SSHAPIRun(t, o, "edge", "status", "--json")
		if res.ExitCode != 0 {
			t.Fatalf("edge status: exit %d\nstdout:%s\nstderr:%s", res.ExitCode, res.Stdout, res.Stderr)
		}
		var st edge.Status
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &st); err != nil {
			t.Fatalf("edge status --json: %v\nstdout: %q", err, res.Stdout)
		}
		if !st.Running {
			t.Errorf("edge status says the edge is not running on a machine set up with --edge: %s", st.Error)
		}
	})

	t.Run("a subcommand is not refused, whatever it answers", func(t *testing.T) {
		res := itest.SSHAPIRun(t, o, "edge", "ca")
		if res.ExitCode == 2 {
			t.Errorf("edge ca was refused as a command (exit 2); subcommands go through\nstderr:%s", res.Stderr)
		}
	})
}

func TestPortForwardingRefused(t *testing.T) {
	_, o := box(t)

	t.Run("remote forward", func(t *testing.T) {
		opts := o
		opts.Extra = []string{"-o", "ExitOnForwardFailure=yes", "-R", "19999:127.0.0.1:4022", "-N"}
		opts.Timeout = 10 * time.Second
		opts.AllowTimeout = true
		res := itest.SSHAPIRun(t, opts)
		switch {
		case res.ExitCode == itest.SSHTimedOut:
			t.Errorf("ssh -R stayed connected: remote forwarding was accepted")
		case res.ExitCode == 0:
			t.Errorf("ssh -R exited 0: remote forwarding was accepted")
		default:
			t.Logf("ssh -R refused with exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
		}
	})

	t.Run("local forward", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		local := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()

		ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(20*time.Second))
		defer cancel()
		known := filepath.Join(t.TempDir(), "known_hosts")
		cmd := exec.CommandContext(ctx, "ssh",
			"-F", "/dev/null", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
			"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile="+known,
			"-i", o.Key, "-p", strconv.Itoa(o.Port),
			"-N", "-L", fmt.Sprintf("%d:127.0.0.1:%d", local, itest.CarameloSSHPort),
			itest.CarameloUser+"@"+o.Host)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

		var conn net.Conn
		for i := 0; i < 50; i++ {
			conn, err = net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", local), 200*time.Millisecond)
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("ssh -L never listened locally: %v\nstderr: %s", err, stderr.String())
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		if strings.HasPrefix(string(buf[:n]), "SSH-") {
			t.Fatalf("a local port forward reached caramelod (banner %q); forwarding must be refused",
				strings.TrimSpace(string(buf[:n])))
		}
		t.Logf("local forward carried no data (%d bytes); ssh said: %s", n, strings.TrimSpace(stderr.String()))
	})
}

func TestGitTransportRefusals(t *testing.T) {
	_, o := box(t)

	for _, argv := range [][]string{
		{"git-upload-archive", "/refused"},
		{"git", "upload-archive", "/refused"},
	} {
		res := itest.SSHAPIRun(t, o, argv...)
		if res.ExitCode != 2 {
			t.Errorf("%v: exit = %d, want 2\nstdout:%s\nstderr:%s", argv, res.ExitCode, res.Stdout, res.Stderr)
		}
		if !strings.Contains(res.Stderr, "upload-archive") {
			t.Errorf("%v: stderr does not name the verb it refused: %q", argv, res.Stderr)
		}
	}

	before := appNames(t, o)
	for _, path := range []string{"/Not_A_Slug", "/../escape", "/a/b"} {
		res := itest.SSHAPIRun(t, o, "git-receive-pack", path)
		if res.ExitCode == 0 {
			t.Errorf("git-receive-pack %q was accepted (exit 0)\nstdout:%s", path, res.Stdout)
		}
		if !strings.Contains(res.Stderr, "caramelo:") {
			t.Errorf("git-receive-pack %q: stderr does not explain the refusal: %q", path, res.Stderr)
		}
	}
	if after := appNames(t, o); !equalStrings(before, after) {
		t.Errorf("a refused push changed the app list: %v -> %v", before, after)
	}

	res := itest.SSHAPIRun(t, o, "git-upload-pack", "/never-pushed")
	if res.ExitCode != 1 {
		t.Errorf("git-upload-pack of an unknown app: exit = %d, want 1\nstderr:%s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "no such app") {
		t.Errorf("git-upload-pack of an unknown app: stderr = %q, want 'no such app'", res.Stderr)
	}
}

func appNames(t *testing.T, o itest.SSHAPIOptions) []string {
	t.Helper()
	res := itest.SSHAPIRun(t, o, "app", "list", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("app list: exit %d\nstderr:%s", res.ExitCode, res.Stderr)
	}
	var apps []capi.AppInfo
	if err := json.Unmarshal([]byte(res.Stdout), &apps); err != nil {
		t.Fatalf("app list --json: %v\n%s", err, res.Stdout)
	}
	out := make([]string, 0, len(apps))
	for _, a := range apps {
		out = append(out, a.Name)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestUnauthorisedKey(t *testing.T) {
	_, o := box(t)
	priv, _ := generateKey(t)
	opts := o
	opts.Key = priv
	opts.Timeout = 20 * time.Second
	res := itest.SSHAPIRun(t, opts, "status", "--json")
	if res.ExitCode != 255 {
		t.Errorf("exit = %d, want 255 for an unauthorised key\nstdout:%s\nstderr:%s",
			res.ExitCode, res.Stdout, res.Stderr)
	}
}

func TestSocketTransportOnTheBox(t *testing.T) {
	m, _ := box(t)
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(time.Minute))
	defer cancel()
	res, err := m.Run(ctx, itest.CarameloBinary+" status --json")
	if err != nil {
		t.Fatalf("status on the box: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("status on the box as %s: exit %d\nstdout:%s\nstderr:%s",
			m.User(), res.ExitCode, res.Stdout, res.Stderr)
	}
	st := decodeStatus(t, res.Stdout)
	if st.Transport != "socket" {
		t.Errorf("transport = %q on the box itself, want socket", st.Transport)
	}
}

func TestClientWithoutMachine(t *testing.T) {
	home := t.TempDir()
	res := itest.MustRunClient(t, itest.ClientOptions{Env: itest.ClientEnv(home)}, "status", "--json")
	if res.ExitCode != 1 {
		t.Errorf("exit = %d, want 1\nstdout:%s\nstderr:%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	got := strings.ToLower(res.Stdout + res.Stderr)
	for _, want := range []string{"server setup", "--machine"} {
		if !strings.Contains(got, strings.ToLower(want)) {
			t.Errorf("error does not mention %q:\n%s%s", want, res.Stdout, res.Stderr)
		}
	}
}

func TestClientWithMachineFlag(t *testing.T) {
	m, _ := box(t)
	home := itest.ClientHomeNoPeer(t, m)
	res := itest.ClientOK(t, itest.ClientOptions{Env: itest.ClientEnv(home)},
		"--machine", m.ClientTarget(t), "status", "--json")
	st := decodeStatus(t, res.Stdout)
	if st.Transport != "ssh" {
		t.Errorf("transport = %q, want ssh", st.Transport)
	}
}

func TestKeyRoundTrip(t *testing.T) {
	_, o := box(t)
	priv, pub := generateKey(t)
	const name = "itest-roundtrip"

	t.Cleanup(func() {
		itest.SSHAPIRun(t, o, "key", "remove", name)
	})

	pubKey, err := os.ReadFile(pub)
	if err != nil {
		t.Fatal(err)
	}
	add := itest.SSHAPIRun(t, withStdin(o, string(pubKey)), "key", "add", "--name", name, "--json")
	if add.ExitCode != 0 {
		t.Fatalf("key add: exit %d\nstdout:%s\nstderr:%s", add.ExitCode, add.Stdout, add.Stderr)
	}

	list := itest.SSHAPIRun(t, o, "key", "list", "--json")
	if list.ExitCode != 0 {
		t.Fatalf("key list: exit %d\nstderr:%s", list.ExitCode, list.Stderr)
	}
	var keys []state.Key
	if err := json.Unmarshal([]byte(strings.TrimSpace(list.Stdout)), &keys); err != nil {
		t.Fatalf("key list --json: %v\nstdout: %q", err, list.Stdout)
	}
	found := false
	for _, k := range keys {
		if k.Name == name {
			found = true
			if k.Fingerprint == "" {
				t.Errorf("key %s has no fingerprint: %+v", name, k)
			}
		}
	}
	if !found {
		t.Fatalf("key %q not in key list: %+v", name, keys)
	}

	asNew := o
	asNew.Key = priv
	used := itest.SSHAPIRun(t, asNew, "status", "--json")
	if used.ExitCode != 0 {
		t.Fatalf("new key cannot authenticate: exit %d\nstderr:%s", used.ExitCode, used.Stderr)
	}
	if st := decodeStatus(t, used.Stdout); st.Identity != name {
		t.Errorf("identity = %q, want %q", st.Identity, name)
	}

	rm := itest.SSHAPIRun(t, o, "key", "remove", name)
	if rm.ExitCode != 0 {
		t.Fatalf("key remove: exit %d\nstderr:%s", rm.ExitCode, rm.Stderr)
	}
	asNew.Timeout = 20 * time.Second
	gone := itest.SSHAPIRun(t, asNew, "status", "--json")
	if gone.ExitCode != 255 {
		t.Errorf("removed key still works: exit %d\nstdout:%s\nstderr:%s", gone.ExitCode, gone.Stdout, gone.Stderr)
	}
}

func TestGossAPI(t *testing.T) {
	m, _ := box(t)
	itest.RunGoss(t, m, itest.MustGossSpec(t, "api.yaml"))
}

func TestAPISurvivesAPowerCycle(t *testing.T) {
	m, o := box(t)
	before := decodeStatus(t, mustAPI(t, o, "status", "--json").Stdout)
	if before.HostKeyFingerprint == "" {
		t.Fatal("the machine reports no host key fingerprint before the power cycle")
	}

	itest.MustRestart(t, m)
	_, after := ready(t, m)

	res := itest.SSHAPIRun(t, after, "status", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("status after the power cycle: exit %d\nstdout:%s\nstderr:%s",
			res.ExitCode, res.Stdout, res.Stderr)
	}
	st := decodeStatus(t, res.Stdout)
	if st.HostKeyFingerprint != before.HostKeyFingerprint {
		t.Errorf("the host key changed across a power cycle: %q -> %q; every agent's known_hosts would break",
			before.HostKeyFingerprint, st.HostKeyFingerprint)
	}
	if st.Transport != "ssh" {
		t.Errorf("transport = %q, want ssh", st.Transport)
	}
	if st.SSHPort != itest.CarameloSSHPort {
		t.Errorf("ssh_port = %d, want %d", st.SSHPort, itest.CarameloSSHPort)
	}
	if st.VPN == nil || !st.VPN.Enabled {
		t.Errorf("the network did not come back after the power cycle: %+v", st.VPN)
	}

	local, err := m.Run(context.Background(), itest.CarameloBinary+" status --json")
	if err != nil {
		t.Fatalf("status on the box after the power cycle: %v", err)
	}
	if local.ExitCode != 0 {
		t.Fatalf("the local socket did not come back: exit %d\nstderr:%s", local.ExitCode, local.Stderr)
	}
	if got := decodeStatus(t, local.Stdout); got.Transport != "socket" {
		t.Errorf("transport on the box = %q, want socket", got.Transport)
	}
}

func TestConcurrentSessions(t *testing.T) {
	_, o := box(t)
	const sessions = 6

	var wg sync.WaitGroup
	results := make([]itest.Result, sessions)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			opts := o
			opts.Timeout = itest.Scale(time.Minute)
			results[i] = itest.SSHAPIRun(t, opts, "status", "--json")
		}(i)
	}
	wg.Wait()

	fingerprints := map[string]int{}
	for i, res := range results {
		if res.ExitCode != 0 {
			t.Errorf("session %d: exit %d\nstdout:%s\nstderr:%s", i, res.ExitCode, res.Stdout, res.Stderr)
			continue
		}
		st := decodeStatus(t, res.Stdout)
		if st.Transport != "ssh" {
			t.Errorf("session %d: transport = %q, want ssh", i, st.Transport)
		}
		fingerprints[st.HostKeyFingerprint]++
	}
	if len(fingerprints) > 1 {
		t.Errorf("%d concurrent sessions saw %d different host keys: %v", sessions, len(fingerprints), fingerprints)
	}
}

func mustAPI(t *testing.T, o itest.SSHAPIOptions, args ...string) itest.Result {
	t.Helper()
	res := itest.SSHAPIRun(t, o, args...)
	if res.ExitCode != 0 {
		t.Fatalf("%s: exit %d\nstdout:%s\nstderr:%s", strings.Join(args, " "), res.ExitCode, res.Stdout, res.Stderr)
	}
	return res
}

func decodeStatus(t *testing.T, out string) capi.Status {
	t.Helper()
	var st capi.Status
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &st); err != nil {
		t.Fatalf("status --json: %v\nstdout: %q", err, out)
	}
	return st
}

func generateKey(t *testing.T) (priv, pub string) {
	t.Helper()
	dir := t.TempDir()
	priv = filepath.Join(dir, "id_ed25519")
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-C", "itest@dev", "-f", priv)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	return priv, priv + ".pub"
}
