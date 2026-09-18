package userdir

import (
	"path/filepath"
	"testing"
)

func TestConfigHonoursXDGOnEveryPlatform(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	got, err := Config()
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Errorf("Config() = %q, want %q", got, dir)
	}
}

func TestCacheHonoursXDGOnEveryPlatform(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	got, err := Cache()
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Errorf("Cache() = %q, want %q", got, dir)
	}
}

func TestWithoutXDGTheDirsAreUnderTheHomeOnEveryPlatform(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")

	cfg, err := Config()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(h, ".config"); cfg != want {
		t.Errorf("Config() = %q, want %q: the commander's config lives in the same place everywhere", cfg, want)
	}
	cache, err := Cache()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(h, ".cache"); cache != want {
		t.Errorf("Cache() = %q, want %q", cache, want)
	}
}

func TestRelativeXDGIsNeverReturned(t *testing.T) {
	h := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "relative/path")
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	got, err := Config()
	if err != nil {
		t.Fatalf("Config(): %v", err)
	}
	if want := filepath.Join(h, ".config"); got != want {
		t.Errorf("Config() = %q, want %q: a relative XDG_CONFIG_HOME is ignored", got, want)
	}
}
