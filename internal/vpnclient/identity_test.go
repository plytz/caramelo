package vpnclient

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/plytz/caramelo/internal/remote"
)

func TestTheIdentityKeyIsGeneratedOnceAndKept(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	path, err := IdentityKeyPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, remote.CommanderDirName, IdentityKeyFile); path != want {
		t.Fatalf("IdentityKeyPath() = %q, want %q", path, want)
	}

	first, created, err := EnsureIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("the first EnsureIdentity did not report creating the key")
	}
	if !ValidKey(first.Private) || !ValidKey(first.Public) {
		t.Fatalf("EnsureIdentity returned %+v", first)
	}

	again, created, err := EnsureIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("the second EnsureIdentity generated another key")
	}
	if again != first {
		t.Errorf("EnsureIdentity moved: %+v, then %+v", first, again)
	}

	loaded, err := LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if loaded != first {
		t.Errorf("LoadIdentity = %+v, want %+v", loaded, first)
	}
}

func TestTheIdentityKeyIsReadableOnlyByItsOwner(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, _, err := EnsureIdentity(); err != nil {
		t.Fatal(err)
	}
	path, err := IdentityKeyPath()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestTheIdentityKeyIsNotTheKeyOfAnyOneMachine(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	identity, _, err := EnsureIdentity()
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := (&FileKeyStore{}).Ensure("box")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Public == machine.Public {
		t.Error("the identity key and a machine key are the same key")
	}
}
