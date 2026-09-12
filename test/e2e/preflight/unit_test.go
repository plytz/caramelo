//go:build e2e

package preflight

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUnitParseNTPSynchronized(t *testing.T) {
	cases := []struct {
		out    string
		synced bool
		fails  bool
	}{
		{out: "NTPSynchronized=yes\n", synced: true},
		{out: "NTPSynchronized=no\n", synced: false},
		{out: "yes\n", synced: true},
		{out: " no ", synced: false},
		{out: "NTPSynchronized=true", synced: true},
		{out: "", fails: true},
		{out: "NTPSynchronized=maybe", fails: true},
	}
	for _, c := range cases {
		synced, err := parseNTPSynchronized(c.out)
		if c.fails {
			if err == nil {
				t.Errorf("parseNTPSynchronized(%q) = %v, want an error", c.out, synced)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseNTPSynchronized(%q): %v", c.out, err)
			continue
		}
		if synced != c.synced {
			t.Errorf("parseNTPSynchronized(%q) = %v, want %v", c.out, synced, c.synced)
		}
	}
}

func TestUnitRemoteEpoch(t *testing.T) {
	got, err := remoteEpoch(" 1757600000\n")
	if err != nil {
		t.Fatalf("remoteEpoch: %v", err)
	}
	if want := time.Unix(1757600000, 0).UTC(); !got.Equal(want) {
		t.Errorf("remoteEpoch = %s, want %s", got, want)
	}
	if _, err := remoteEpoch("Thu Sep 11 10:00:00 UTC 2026"); err == nil {
		t.Errorf("remoteEpoch on a human date = nil error, want an error naming what was read")
	}
}

func TestUnitClockSkew(t *testing.T) {
	before := time.Unix(1000, 0).UTC()
	after := time.Unix(1002, 0).UTC()
	cases := []struct {
		remote time.Time
		want   time.Duration
	}{
		{remote: time.Unix(1001, 0).UTC(), want: 0},
		{remote: before, want: 0},
		{remote: after, want: 0},
		{remote: time.Unix(990, 0).UTC(), want: 10 * time.Second},
		{remote: time.Unix(1010, 0).UTC(), want: 8 * time.Second},
	}
	for _, c := range cases {
		if got := clockSkew(c.remote, before, after); got != c.want {
			t.Errorf("clockSkew(%s) = %s, want %s", c.remote, got, c.want)
		}
	}
}

func TestUnitArchFromUname(t *testing.T) {
	cases := map[string]string{
		"x86_64\n":  "amd64",
		"aarch64\n": "arm64",
		"arm64":     "arm64",
		"armv7l":    "arm",
	}
	for in, want := range cases {
		got, err := archFromUname(in)
		if err != nil {
			t.Errorf("archFromUname(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("archFromUname(%q) = %s, want %s", in, got, want)
		}
	}
	for _, in := range []string{"", "mips64"} {
		if got, err := archFromUname(in); err == nil {
			t.Errorf("archFromUname(%q) = %s, want an error", in, got)
		}
	}
}

func TestUnitReachCommandAgainstALocalListener(t *testing.T) {
	if _, err := exec.LookPath("nc"); err != nil {
		t.Skipf("nc is not on PATH, so the command cannot be run here: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	open := ln.Addr().(*net.TCPAddr).Port

	if code := runShell(t, reachCommand("127.0.0.1", open)); code != 0 {
		t.Errorf("reachCommand against an open port exited %d, want 0", code)
	}

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	shut := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	if code := runShell(t, reachCommand("127.0.0.1", shut)); code == 0 {
		t.Errorf("reachCommand against a closed port exited 0, want a non-zero exit")
	}
}

func runShell(t *testing.T, cmd string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	err := c.Run()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("running %q: %v", cmd, err)
	return -1
}

func TestUnitCommandStrings(t *testing.T) {
	if got, want := hasCommand("nc"), "command -v nc >/dev/null 2>&1"; got != want {
		t.Errorf("hasCommand = %q, want %q", got, want)
	}
	reach := reachCommand("203.0.113.10", 2222)
	for _, want := range []string{"203.0.113.10 2222", "/dev/tcp/203.0.113.10/2222", "nc -z -w 5"} {
		if !strings.Contains(reach, want) {
			t.Errorf("reachCommand = %q, want it to contain %q", reach, want)
		}
	}
	ping := pingCommand("203.0.113.10")
	if !strings.HasPrefix(ping, "ping -n -c ") || !strings.HasSuffix(ping, " 203.0.113.10") {
		t.Errorf("pingCommand = %q, want a non-resolving ping of the address", ping)
	}
}

func TestUnitTargetAddress(t *testing.T) {
	for _, ip := range []string{"203.0.113.10", "2001:db8::1"} {
		got, err := targetAddress(ip)
		if err != nil {
			t.Errorf("targetAddress(%q): %v", ip, err)
			continue
		}
		if got != ip {
			t.Errorf("targetAddress(%q) = %q, want the literal back", ip, got)
		}
	}
	got, err := targetAddress("localhost")
	if err != nil {
		t.Skipf("localhost does not resolve here: %v", err)
	}
	if net.ParseIP(got) == nil {
		t.Errorf("targetAddress(\"localhost\") = %q, want an address", got)
	}
}

func TestUnitFindMachineBinary(t *testing.T) {
	dir := t.TempDir()
	general := filepath.Join(dir, "caramelo-general")
	perArch := filepath.Join(dir, "caramelo-arm64")
	writeFile(t, general)
	writeFile(t, perArch)

	t.Setenv(BinEnv, general)
	t.Setenv(BinEnv+"_ARM64", perArch)
	path, source, err := findMachineBinary("arm64")
	if err != nil {
		t.Fatalf("findMachineBinary: %v", err)
	}
	if path != general || source != BinEnv {
		t.Errorf("findMachineBinary = %q from %q, want %q from %q", path, source, general, BinEnv)
	}

	t.Setenv(BinEnv, "")
	path, source, err = findMachineBinary("arm64")
	if err != nil {
		t.Fatalf("findMachineBinary: %v", err)
	}
	if path != perArch || source != BinEnv+"_ARM64" {
		t.Errorf("findMachineBinary = %q from %q, want %q from %q", path, source, perArch, BinEnv+"_ARM64")
	}

	t.Setenv(BinEnv, filepath.Join(dir, "missing"))
	if _, _, err := findMachineBinary("arm64"); err == nil || !strings.Contains(err.Error(), BinEnv) {
		t.Errorf("findMachineBinary with a missing %s = %v, want an error naming the variable", BinEnv, err)
	}

	t.Setenv(BinEnv, "")
	t.Setenv(BinEnv+"_ARM64", "")
	sibling := filepath.Join(dir, "caramelo-linux-arm64")
	writeFile(t, sibling)
	t.Setenv(HostBinEnv, filepath.Join(dir, "caramelo"))
	path, source, err = findMachineBinary("arm64")
	if err != nil {
		t.Fatalf("findMachineBinary: %v", err)
	}
	if path != sibling {
		t.Errorf("findMachineBinary = %q from %q, want the sibling %q", path, source, sibling)
	}

	if _, _, err := findMachineBinary(""); err == nil {
		t.Errorf("findMachineBinary(\"\") = nil error, want an error")
	}
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestUnitRepoRoot(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repoRoot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repoRoot = %s, which has no go.mod: %v", root, err)
	}
	if _, err := os.Stat(filepath.Join(root, "cmd", "caramelo")); err != nil {
		t.Fatalf("repoRoot = %s, which has no cmd/caramelo: %v", root, err)
	}
	t.Logf("the repository root is %s", root)
}
