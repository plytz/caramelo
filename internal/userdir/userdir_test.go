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

func TestRelativeXDGIsNeverReturned(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "relative/path")
	t.Setenv("HOME", t.TempDir())
	got, err := Config()
	if err == nil && (!filepath.IsAbs(got) || got == "relative/path") {
		t.Errorf("Config() = %q, want an absolute path or an error", got)
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
