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

func TestRelativeXDGIsNeverReturned(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "relative/path")
	t.Setenv("HOME", t.TempDir())
	got, err := Config()
	if err == nil && (!filepath.IsAbs(got) || got == "relative/path") {
		t.Errorf("Config() = %q, want an absolute path or an error", got)
	}
}
