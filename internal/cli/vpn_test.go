package cli

import (
	"bytes"
	"encoding/json"
	"github.com/plytz/caramelo/internal/testutil"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/vpnclient"
)

func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(testutil.ShortDir(t), "run"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("HOME", dir)
	t.Setenv("CARAMELO_MACHINE", "")
	return dir
}

func TestVPNStatusOfAMachineNeverJoined(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"vpn", "status", "--machine", "worker1"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit %d, want 0: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "not joined") {
		t.Errorf("stdout = %q, want it to say the machine is not joined", stdout.String())
	}
}

func TestVPNStatusJSONIsOneDocument(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"vpn", "status", "--machine", "worker1", "--json"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var st vpnclient.State
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	if err := dec.Decode(&st); err != nil {
		t.Fatalf("stdout is not one JSON document (%v):\n%s", err, stdout.String())
	}
	if dec.More() {
		t.Errorf("stdout carries more than one document:\n%s", stdout.String())
	}
	if st.Mode != vpnclient.ModeOff || st.Machine != "worker1" {
		t.Errorf("state = %+v, want the machine off", st)
	}
}

func TestVPNNeedsAMachine(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{
		{"vpn", "up"}, {"vpn", "status"}, {"vpn", "config"}, {"connect", "feat-x"},
	} {
		var stdout, stderr bytes.Buffer
		code := Run(args, &stdout, &stderr)
		if code != ExitUsage {
			t.Errorf("caramelo %s: exit %d, want %d\n%s",
				strings.Join(args, " "), code, ExitUsage, stderr.String())
		}
		if !strings.Contains(stderr.String(), "--machine") {
			t.Errorf("caramelo %s: stderr = %q, want it to say how to name a machine",
				strings.Join(args, " "), stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("caramelo %s wrote to stdout: %q", strings.Join(args, " "), stdout.String())
		}
	}
}

func TestVPNConfigWithoutAKey(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"vpn", "config", "--machine", "worker1"}, &stdout, &stderr)
	if code != ExitError {
		t.Fatalf("exit %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "vpn up") {
		t.Errorf("stderr = %q, want it to point at 'caramelo vpn up'", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
}

func TestVPNUsesTheDefaultMachine(t *testing.T) {
	dir := isolate(t)
	cfgDir := filepath.Join(dir, "config", "caramelo")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "default_machine: worker1\nmachines:\n  worker1: caramelo@192.168.56.11:4022\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"vpn", "status"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "worker1") {
		t.Errorf("stdout = %q, want the default machine", stdout.String())
	}
}

func TestConnectNeedsAnApp(t *testing.T) {
	dir := isolate(t)
	t.Setenv("CARAMELO_MACHINE", "worker1")
	t.Setenv("CARAMELO_APP", "")

	t.Chdir(dir)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"connect", "feat-x"}, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d: %s", code, ExitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--app") {
		t.Errorf("stderr = %q, want it to name --app", stderr.String())
	}
}

func TestConnectWithoutAKey(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"connect", "feat-x", "--app", "shop", "--machine", "worker1"}, &stdout, &stderr)
	if code != ExitError {
		t.Fatalf("exit %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "vpn up") {
		t.Errorf("stderr = %q, want it to point at 'caramelo vpn up'", stderr.String())
	}
}

func TestKeyPathsStayInsideTheConfigDir(t *testing.T) {
	dir := isolate(t)
	path, err := vpnclient.KeyPath("caramelo@192.168.56.11:4022")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "config", "caramelo", "vpn")
	if filepath.Dir(path) != want {
		t.Fatalf("KeyPath = %q, want it under %q", path, want)
	}
}
