package userdir

import (
	"os/user"
	"path/filepath"
	"strings"
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

func TestUnderSudoTheDirsAreTheInvokingUsersNotRoots(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user to look up")
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("SUDO_USER", me.Username)
	t.Setenv("HOME", "/root")
	ServerConfigured = func() bool { return false }
	t.Cleanup(func() { ServerConfigured = func() bool { return false } })

	config, err := Config()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(me.HomeDir, ".config"); config != want {
		t.Errorf("Config() = %q, want %q", config, want)
	}
	cache, err := Cache()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(me.HomeDir, ".cache"); cache != want {
		t.Errorf("Cache() = %q, want %q", cache, want)
	}
}

func TestOnAServerSudoDoesNotReachForTheInvokingUsersDirs(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user to look up")
	}
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("SUDO_USER", me.Username)
	t.Setenv("HOME", home)
	ServerConfigured = func() bool { return true }
	t.Cleanup(func() { ServerConfigured = func() bool { return false } })

	if _, ok := InvokingUserHome(); ok {
		t.Error("InvokingUserHome() answered on a machine that already has a server config")
	}
	got, err := Config()
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(got, me.HomeDir) && me.HomeDir != home {
		t.Errorf("Config() = %q, want a dir under %q", got, home)
	}
}

func TestAnExplicitXDGBeatsSudo(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user to look up")
	}
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("SUDO_USER", me.Username)
	ServerConfigured = func() bool { return false }
	t.Cleanup(func() { ServerConfigured = func() bool { return false } })

	got, err := Config()
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Errorf("Config() = %q, want %q", got, dir)
	}
}

func TestSudoAsRootIsNotASudoUser(t *testing.T) {
	t.Setenv("SUDO_USER", "root")
	ServerConfigured = func() bool { return false }
	t.Cleanup(func() { ServerConfigured = func() bool { return false } })
	if _, ok := InvokingUserHome(); ok {
		t.Error("InvokingUserHome() answered for SUDO_USER=root")
	}
}
