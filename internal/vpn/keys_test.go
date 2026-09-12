package vpn

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/curve25519"
)

func TestGenerateKeyIsClamped(t *testing.T) {

	for i := 0; i < 32; i++ {
		k, err := GenerateKey()
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		if k[0]&7 != 0 {
			t.Fatalf("key %d: low three bits of byte 0 set: %08b", i, k[0])
		}
		if k[31]&128 != 0 || k[31]&64 == 0 {
			t.Fatalf("key %d: byte 31 not clamped: %08b", i, k[31])
		}
	}
}

func TestGenerateKeyIsRandom(t *testing.T) {
	a, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	b, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if a == b {
		t.Fatal("two generated keys are the same")
	}
	if a.IsZero() || b.IsZero() {
		t.Fatal("a generated key is all zeroes")
	}
}

func TestPublicMatchesCurve25519(t *testing.T) {
	priv, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pub, err := priv.Public()
	if err != nil {
		t.Fatalf("Public: %v", err)
	}
	want, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		t.Fatalf("X25519: %v", err)
	}
	if got := pub[:]; string(got) != string(want) {
		t.Fatalf("Public = %x, want %x", got, want)
	}

	otherPriv, _ := GenerateKey()
	otherPub, _ := otherPriv.Public()
	s1, err := curve25519.X25519(priv[:], otherPub[:])
	if err != nil {
		t.Fatalf("X25519: %v", err)
	}
	s2, err := curve25519.X25519(otherPriv[:], pub[:])
	if err != nil {
		t.Fatalf("X25519: %v", err)
	}
	if string(s1) != string(s2) {
		t.Fatal("the two sides derive different shared secrets")
	}
}

func TestParseKeyBothForms(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if len(k.Base64()) != 44 {
		t.Fatalf("Base64 = %q, want 44 characters", k.Base64())
	}
	if len(k.Hex()) != 64 {
		t.Fatalf("Hex = %q, want 64 characters", k.Hex())
	}
	if k.String() != k.Base64() {
		t.Fatalf("String = %q, want the base64 form %q", k.String(), k.Base64())
	}
	for name, s := range map[string]string{
		"base64":             k.Base64(),
		"hex":                k.Hex(),
		"base64 with spaces": "  " + k.Base64() + "\n",
		"hex with spaces":    "\t" + k.Hex() + "\n",
	} {
		got, err := ParseKey(s)
		if err != nil {
			t.Fatalf("ParseKey(%s): %v", name, err)
		}
		if got != k {
			t.Fatalf("ParseKey(%s) = %x, want %x", name, got, k)
		}
	}
}

func TestParseKeyRejectsRubbish(t *testing.T) {
	for name, s := range map[string]string{
		"empty":     "",
		"short":     "abcd",
		"not hex":   strings.Repeat("z", 64),
		"not b64":   strings.Repeat("!", 44),
		"too long":  base64.StdEncoding.EncodeToString(make([]byte, 64)),
		"ssh key":   "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA",
		"truncated": strings.Repeat("a", 63),
	} {
		if _, err := ParseKey(s); err == nil {
			t.Errorf("ParseKey(%s) accepted %q", name, s)
		}
	}
}

func TestWriteAndReadPrivateKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vpn", "private.key")
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if err := WritePrivateKey(path, k); err != nil {
		t.Fatalf("WritePrivateKey: %v", err)
	}

	if err := WritePrivateKey(path, k); err != nil {
		t.Fatalf("WritePrivateKey twice: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if di, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("stat dir: %v", err)
	} else if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", di.Mode().Perm())
	}

	ents, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(ents) != 1 || ents[0].Name() != "private.key" {
		t.Errorf("directory holds %v, want just private.key", ents)
	}
	got, err := ReadPrivateKey(path)
	if err != nil {
		t.Fatalf("ReadPrivateKey: %v", err)
	}
	if got != k {
		t.Fatalf("ReadPrivateKey = %x, want %x", got, k)
	}
}

func TestReadPrivateKeyErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadPrivateKey(filepath.Join(dir, "missing")); err == nil {
		t.Error("reading a missing key file succeeded")
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateKey(empty); err == nil {
		t.Error("reading an empty key file succeeded")
	}
	zero := filepath.Join(dir, "zero")
	if err := os.WriteFile(zero, []byte(Key{}.Base64()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateKey(zero); err == nil {
		t.Error("reading an all-zero key succeeded")
	}
}

func TestReadPrivateKeyClamps(t *testing.T) {

	dir := t.TempDir()
	path := filepath.Join(dir, "k")
	var raw Key
	for i := range raw {
		raw[i] = 0xff
	}
	if err := os.WriteFile(path, []byte(raw.Base64()), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := ReadPrivateKey(path)
	if err != nil {
		t.Fatalf("ReadPrivateKey: %v", err)
	}
	if k[0]&7 != 0 || k[31]&128 != 0 || k[31]&64 == 0 {
		t.Fatalf("key not clamped on read: %x", k)
	}
}

func TestKeyFromSecretIsTheSameOnBothMachinesAndClamped(t *testing.T) {
	const secret = "9f8f1a1e0c0b4a6d8e2f3a4b5c6d7e8f"
	a, b := KeyFromSecret(secret), KeyFromSecret(secret)
	if a != b {
		t.Fatalf("the same secret gave two keys: %s and %s", a, b)
	}
	if other := KeyFromSecret(secret + "x"); other == a {
		t.Fatalf("two secrets gave one key: %s", a)
	}
	if a != a.clamped() {
		t.Fatalf("the derived key is not clamped: %s", a)
	}
	if _, err := a.Public(); err != nil {
		t.Fatalf("Public: %v", err)
	}
	if a.IsZero() {
		t.Fatal("the derived key is zero")
	}
}
