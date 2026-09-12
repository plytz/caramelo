package vpnclient

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/env"
)

func TestGenerateKeyIsClampedAndDerivable(t *testing.T) {
	for i := 0; i < 20; i++ {
		kp, err := GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		priv, err := ParseKey(kp.Private)
		if err != nil {
			t.Fatal(err)
		}
		if priv[0]&7 != 0 {
			t.Fatalf("private key is not clamped: first byte %08b", priv[0])
		}
		if priv[31]&128 != 0 || priv[31]&64 == 0 {
			t.Fatalf("private key is not clamped: last byte %08b", priv[31])
		}
		pub, err := PublicKey(kp.Private)
		if err != nil {
			t.Fatal(err)
		}
		if pub != kp.Public {
			t.Fatalf("PublicKey(private) = %q, want %q", pub, kp.Public)
		}
		if _, err := base64.StdEncoding.DecodeString(kp.Public); err != nil {
			t.Fatalf("the public key is not base64: %v", err)
		}
	}
}

func TestKeyFormsRoundTrip(t *testing.T) {
	kp, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	hexKey, err := KeyHex(kp.Public)
	if err != nil {
		t.Fatal(err)
	}
	if len(hexKey) != 64 {
		t.Fatalf("hex key is %d characters, want 64", len(hexKey))
	}
	back, err := KeyBase64(hexKey)
	if err != nil {
		t.Fatal(err)
	}
	if back != kp.Public {
		t.Fatalf("base64(hex(key)) = %q, want %q", back, kp.Public)
	}
	if again, err := KeyHex(hexKey); err != nil || again != hexKey {
		t.Fatalf("hex(hex(key)) = %q, %v", again, err)
	}
}

func TestParseKeyRejectsRubbish(t *testing.T) {
	for _, s := range []string{"", "   ", "not a key", "aGVsbG8=", strings.Repeat("z", 64)} {
		if _, err := ParseKey(s); err == nil {
			t.Errorf("ParseKey(%q) accepted it", s)
		}
		if ValidKey(s) {
			t.Errorf("ValidKey(%q) = true", s)
		}
	}
}

func TestParseKeyDoesNotEchoTheKey(t *testing.T) {
	secret := "SUPERSECRETVALUETHATMUSTNOTAPPEARINANERROR!!"
	_, err := ParseKey(secret)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the error quoted the whole key: %v", err)
	}
}

func TestKeyStoreEnsureIsIdempotentAndPrivate(t *testing.T) {
	dir := t.TempDir()
	ks := &FileKeyStore{Dir: dir}
	kp, created, err := ks.Ensure("worker1")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("the first Ensure must report that it created the key")
	}
	again, created, err := ks.Ensure("worker1")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("the second Ensure created a second key")
	}
	if again.Private != kp.Private || again.Public != kp.Public {
		t.Fatal("Ensure returned a different key the second time")
	}
	fi, err := os.Stat(filepath.Join(dir, "worker1.key"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("the key file is %o, want 600", mode)
	}

	other, _, err := ks.Ensure("worker2")
	if err != nil {
		t.Fatal(err)
	}
	if other.Private == kp.Private {
		t.Fatal("two machines share one private key")
	}
}

func TestKeyStoreLoadAndRemove(t *testing.T) {
	dir := t.TempDir()
	ks := &FileKeyStore{Dir: dir}
	if _, err := ks.Load("nobody"); err == nil {
		t.Fatal("loading a machine with no key succeeded")
	} else if !strings.Contains(err.Error(), ErrNoKey.Error()) {
		t.Fatalf("err = %v, want ErrNoKey", err)
	}
	if err := ks.Remove("nobody"); err != nil {
		t.Fatalf("removing a key that is not there: %v", err)
	}
	if _, _, err := ks.Ensure("worker1"); err != nil {
		t.Fatal(err)
	}
	if err := ks.Remove("worker1"); err != nil {
		t.Fatal(err)
	}
	if _, err := ks.Load("worker1"); err == nil {
		t.Fatal("the key survived Remove")
	}
}

func TestKeyStoreAcceptsAHexKey(t *testing.T) {
	dir := t.TempDir()
	kp, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	hexKey, err := KeyHex(kp.Private)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worker1.key"), []byte(hexKey), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := (&FileKeyStore{Dir: dir}).Load("worker1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Private != kp.Private || got.Public != kp.Public {
		t.Fatalf("Load = %+v, want the same key in base64", got)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"alex-laptop":                   "alex-laptop",
		"Éric's MacBook":                "ric-s-macbook",
		"root@box":                      "root-box",
		"---":                           "peer",
		"":                              "peer",
		strings.Repeat("abcdefghij", 4): strings.Repeat("abcdefghij", 3) + "ab",
	} {
		got := Slug(in)
		if got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
		if err := env.ValidateName("peer", got); err != nil {
			t.Errorf("Slug(%q) = %q, which the machine refuses: %v", in, got, err)
		}
	}
}
