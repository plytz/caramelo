package sshapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

func TestEnsureHostKeyCreatesAndReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh", "host_ed25519")

	first, err := EnsureHostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := first.PublicKey().Type(); got != "ssh-ed25519" {
		t.Errorf("host key type = %q, want ssh-ed25519", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("private key mode = %v, want 0600", fi.Mode().Perm())
	}

	pub, err := os.ReadFile(path + ".pub")
	if err != nil {
		t.Fatalf("public key not written: %v", err)
	}
	if _, _, _, _, err := gossh.ParseAuthorizedKey(pub); err != nil {
		t.Errorf("%s is not in authorized_keys format: %v\n%s", path+".pub", err, pub)
	}

	second, err := EnsureHostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(first) != Fingerprint(second) {
		t.Errorf("host key changed on reload: %s then %s", Fingerprint(first), Fingerprint(second))
	}
	if !strings.HasPrefix(Fingerprint(first), "SHA256:") {
		t.Errorf("fingerprint = %q, want a SHA256: prefix", Fingerprint(first))
	}
}

func TestEnsureHostKeyRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host_ed25519")
	if err := os.WriteFile(path, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureHostKey(path); err == nil {
		t.Fatal("want an error for an unparseable host key")
	}
}
