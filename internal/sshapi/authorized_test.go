package sshapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

func newKeyLine(t *testing.T, comment string) (string, gossh.Signer) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(gossh.MarshalAuthorizedKey(signer.PublicKey())))
	if comment != "" {
		line += " " + comment
	}
	return line, signer
}

func TestAppendListRemoveKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh", "authorized_keys")

	if keys, err := ListKeys(path); err != nil || keys != nil {
		t.Fatalf("ListKeys on a missing file = %v, %v; want nil, nil", keys, err)
	}

	line, _ := newKeyLine(t, "alex@laptop")
	entry, err := AppendKey(path, line, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Name != "laptop" {
		t.Errorf("name = %q, want laptop (the --name wins over the key comment)", entry.Name)
	}
	if !strings.HasPrefix(entry.Fingerprint, "SHA256:") {
		t.Errorf("fingerprint = %q", entry.Fingerprint)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}

	keys, err := ListKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Name != "laptop" {
		t.Fatalf("ListKeys = %+v, want one key named laptop", keys)
	}

	other, _ := newKeyLine(t, "")
	if _, err := AppendKey(path, other, "laptop"); !errors.Is(err, ErrKeyExists) {
		t.Errorf("duplicate name error = %v, want ErrKeyExists", err)
	}
	if _, err := AppendKey(path, line, "laptop2"); !errors.Is(err, ErrKeyExists) {
		t.Errorf("duplicate key error = %v, want ErrKeyExists", err)
	}

	if _, err := AppendKey(path, other, "agent"); err != nil {
		t.Fatal(err)
	}
	if keys, _ := ListKeys(path); len(keys) != 2 {
		t.Fatalf("want two keys, got %+v", keys)
	}

	if _, err := RemoveKey(path, "laptop"); err != nil {
		t.Fatal(err)
	}
	keys, _ = ListKeys(path)
	if len(keys) != 1 || keys[0].Name != "agent" {
		t.Fatalf("after remove = %+v, want only agent", keys)
	}
	if _, err := RemoveKey(path, "laptop"); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("removing twice = %v, want ErrKeyNotFound", err)
	}
}

func TestAppendKeyWithOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authorized_keys")
	line, _ := newKeyLine(t, "")
	entry, err := AppendKeyWithOptions(path, line, "admin", "caramelo-role=admin")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Options != "caramelo-role=admin" {
		t.Errorf("options = %q", entry.Options)
	}
	keys, err := ListKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Options != "caramelo-role=admin" || keys[0].Name != "admin" {
		t.Fatalf("re-read = %+v, want the options preserved", keys)
	}
}

func TestParseKeyRejects(t *testing.T) {
	line, _ := newKeyLine(t, "")
	cases := map[string]struct{ line, name string }{
		"not a key":     {"hello world", "n"},
		"empty":         {"", "n"},
		"name with tab": {line, "a\tb"},
		"two lines":     {line + "\n" + line, "n"},
	}
	for what, c := range cases {
		if _, err := ParseKey(c.line, c.name, ""); err == nil {
			t.Errorf("%s: want an error", what)
		}
	}
}

func TestParseKeyFallsBackToTheComment(t *testing.T) {
	line, _ := newKeyLine(t, "alex@laptop")
	e, err := ParseKey(line, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "alex@laptop" {
		t.Errorf("name = %q, want the key comment", e.Name)
	}
	bare, _ := newKeyLine(t, "")
	e, err = ParseKey(bare, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(e.Name, "SHA256:") {
		t.Errorf("name = %q, want the fingerprint when there is no comment", e.Name)
	}
}

func TestListKeysSkipsJunkAndComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authorized_keys")
	line, _ := newKeyLine(t, "alex@laptop")
	content := "# a comment\n\nthis line is broken\n" + line + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := ListKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Name != "alex@laptop" {
		t.Fatalf("ListKeys = %+v, want the one good key named by its comment", keys)
	}
}

func TestListKeysNamesUnnamedKeyByFingerprint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authorized_keys")
	line, signer := newKeyLine(t, "")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := ListKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("want one key, got %+v", keys)
	}
	if want := gossh.FingerprintSHA256(signer.PublicKey()); keys[0].Name != want {
		t.Errorf("name = %q, want the fingerprint %q", keys[0].Name, want)
	}
}
