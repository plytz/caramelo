//go:build e2e

package sshrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testHost(t *testing.T) Host {
	t.Helper()
	return Host{Name: "m1", Addr: "198.51.100.7", Port: 22, User: "debian", Key: keyFile(t), Arch: "amd64"}
}

func keyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, []byte("not-a-key\n"), 0o600); err != nil {
		t.Fatalf("write the key: %v", err)
	}
	return path
}

func optionValue(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-o" {
			continue
		}
		if key, value, found := strings.Cut(args[i+1], "="); found && key == name {
			return value
		}
	}
	return ""
}

func TestQuoteSurvivesTheShell(t *testing.T) {
	for _, in := range []string{
		"plain",
		"a b c",
		"it's",
		`$HOME`,
		"*.go",
		"a|b;c&d",
		`"double"`,
		"back\\slash",
		"new\nline",
	} {
		out, err := exec.Command("/bin/sh", "-c", "printf %s "+Quote(in)).Output()
		if err != nil {
			t.Fatalf("sh with %q: %v", in, err)
		}
		if string(out) != in {
			t.Errorf("Quote(%q) came back as %q", in, string(out))
		}
	}
}

func TestOptionsPinTheHostKeyWhenTheInventoryGivesOne(t *testing.T) {
	dir := t.TempDir()
	h := testHost(t)
	h.HostKey = "ssh-ed25519 AAAATESTKEY"
	args, err := Options(dir, h)
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	if got := optionValue(args, "StrictHostKeyChecking"); got != "yes" {
		t.Errorf("StrictHostKeyChecking is %q, want yes", got)
	}
	known := optionValue(args, "UserKnownHostsFile")
	if known != KnownHostsPath(dir, h) {
		t.Errorf("UserKnownHostsFile is %q, want %q", known, KnownHostsPath(dir, h))
	}
	body, err := os.ReadFile(known)
	if err != nil {
		t.Fatalf("read the known_hosts: %v", err)
	}
	if strings.TrimSpace(string(body)) != h.Addr+" ssh-ed25519 AAAATESTKEY" {
		t.Errorf("known_hosts holds %q", string(body))
	}
	if optionValue(args, "ControlPath") != ControlPath(dir, h) {
		t.Errorf("ControlPath is %q", optionValue(args, "ControlPath"))
	}
	if optionValue(args, "BatchMode") != "yes" || optionValue(args, "IdentitiesOnly") != "yes" {
		t.Errorf("BatchMode and IdentitiesOnly are %q and %q", optionValue(args, "BatchMode"), optionValue(args, "IdentitiesOnly"))
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-i "+h.Key) || !strings.HasSuffix(joined, "-p 22") {
		t.Errorf("args are %q", joined)
	}
}

func TestOptionsAcceptNewWhenNoHostKeyIsGiven(t *testing.T) {
	dir := t.TempDir()
	h := testHost(t)
	args, err := Options(dir, h)
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	if got := optionValue(args, "StrictHostKeyChecking"); got != "accept-new" {
		t.Errorf("StrictHostKeyChecking is %q, want accept-new", got)
	}
	body, err := os.ReadFile(KnownHostsPath(dir, h))
	if err != nil {
		t.Fatalf("read the known_hosts: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("known_hosts is not empty: %q", string(body))
	}
}

func TestKnownHostsBracketsANonDefaultPort(t *testing.T) {
	dir := t.TempDir()
	h := testHost(t)
	h.Port = 2222
	h.HostKey = "ssh-ed25519 AAAATESTKEY"
	if _, err := Options(dir, h); err != nil {
		t.Fatalf("Options: %v", err)
	}
	body, err := os.ReadFile(KnownHostsPath(dir, h))
	if err != nil {
		t.Fatalf("read the known_hosts: %v", err)
	}
	if !strings.HasPrefix(string(body), "[198.51.100.7]:2222 ") {
		t.Errorf("known_hosts holds %q", string(body))
	}
}

func TestForgetHostKeyEmptiesThePin(t *testing.T) {
	dir := t.TempDir()
	h := testHost(t)
	h.HostKey = "ssh-ed25519 AAAATESTKEY"
	if _, err := Options(dir, h); err != nil {
		t.Fatalf("Options: %v", err)
	}
	if err := ForgetHostKey(dir, h); err != nil {
		t.Fatalf("ForgetHostKey: %v", err)
	}
	body, err := os.ReadFile(KnownHostsPath(dir, h))
	if err != nil {
		t.Fatalf("read the known_hosts: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("the pin is still there: %q", string(body))
	}
}

func TestOptionsRefuseAnIncompleteHost(t *testing.T) {
	dir := t.TempDir()
	if _, err := Options(dir, Host{User: "debian"}); err == nil {
		t.Error("a host with no address was accepted")
	}
	if _, err := Options(dir, Host{Addr: "198.51.100.7"}); err == nil {
		t.Error("a host with no user was accepted")
	}
}

func TestControlPathStaysUsableUnderALongDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("deep-directory-name/", 6))
	path := ControlPath(dir, testHost(t))
	if len(path) > maxControlPath {
		t.Errorf("the control path is %d bytes: %s", len(path), path)
	}
	if strings.HasPrefix(path, dir) {
		t.Errorf("the control path was left under the long directory: %s", path)
	}
}

func TestOptionStringQuotesWhatAShellWouldSplit(t *testing.T) {
	dir := t.TempDir()
	h := testHost(t)
	s, err := OptionString(dir, h)
	if err != nil {
		t.Fatalf("OptionString: %v", err)
	}
	out, err := exec.Command("/bin/sh", "-c", "for a in "+s+"; do printf '%s\\n' \"$a\"; done").Output()
	if err != nil {
		t.Fatalf("sh: %v", err)
	}
	fields := strings.Split(strings.TrimSpace(string(out)), "\n")
	args, err := Options(dir, h)
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	if len(fields) != len(args) {
		t.Fatalf("the shell split the options into %d words, want %d", len(fields), len(args))
	}
	for i := range args {
		if fields[i] != args[i] {
			t.Errorf("word %d is %q, want %q", i, fields[i], args[i])
		}
	}
}

func TestHostKeyChangedRecognisesOpenSSH(t *testing.T) {
	changed := "@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\nWARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!\n"
	if !HostKeyChanged(changed) {
		t.Error("the changed-key warning was not recognised")
	}
	if HostKeyChanged("ssh: connect to host 198.51.100.7 port 22: Connection refused") {
		t.Error("a refused connection was read as a changed key")
	}
}
