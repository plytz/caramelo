package remote

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadAndSaveCommanderConfigUseTheUsersConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	got, err := LoadCommanderConfig()
	if err != nil {
		t.Fatalf("loading a config that was never written must not fail: %v", err)
	}
	if !reflect.DeepEqual(got, CommanderConfig{}) {
		t.Errorf("config = %+v, want the zero config", got)
	}

	want := CommanderConfig{
		Name: "eric-laptop",
		Role: RoleCommander,
		Commander: Commander{
			DefaultFleet: "home",
			Fleets: map[string]Fleet{
				"home": {Hub: "caramelo@192.168.56.11:4022", Apps: []string{"shop"}},
			},
		},
	}
	if err := SaveCommanderConfig(want); err != nil {
		t.Fatalf("SaveCommanderConfig: %v", err)
	}

	path := filepath.Join(dir, "caramelo", CommanderConfigFile)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("SaveCommanderConfig did not write %s: %v", path, err)
	}
	got, err = LoadCommanderConfig()
	if err != nil {
		t.Fatalf("LoadCommanderConfig: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}

	fi, err := os.Stat(filepath.Join(dir, "caramelo"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("config dir mode = %v, want 0700", perm)
	}

	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error("the .tmp file of the atomic write was left behind")
	}
}

func TestCommanderConfigPathFailsWithoutAHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	if got, err := CommanderConfigPath(); err == nil {
		t.Fatalf("CommanderConfigPath() = %q, want an error when there is no home", got)
	}
}

func TestLoadCommanderConfigRejectsBrokenYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("commander: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadCommanderConfigFrom(path)
	if err == nil {
		t.Fatal("a broken config file must be an error, not a silent zero config")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want it to name the file", err)
	}
}

func TestLoadCommanderConfigReportsAnUnreadableFile(t *testing.T) {

	dir := t.TempDir()
	if _, err := LoadCommanderConfigFrom(dir); err == nil {
		t.Fatal("an unreadable config path must be an error")
	}
}

func TestSaveCommanderConfigToReportsAnUnwritableLocation(t *testing.T) {

	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := SaveCommanderConfigTo(filepath.Join(file, "caramelo", "config.yaml"), CommanderConfig{})
	if err == nil {
		t.Fatal("writing under a plain file must fail")
	}
}

func TestSSHClientDefaultsToSSH(t *testing.T) {
	t.Setenv("CARAMELO_SSH", "")
	t.Setenv("CARAMELO_SSH_OPTS", "")

	bin, extra, err := SSHClient()
	if err != nil {
		t.Fatalf("SSHClient: %v", err)
	}
	if bin != "ssh" {
		t.Errorf("bin = %q, want ssh", bin)
	}
	if len(extra) != 0 {
		t.Errorf("extra = %q, want none", extra)
	}
}

func TestSSHClientHonoursTheEscapeHatches(t *testing.T) {
	t.Setenv("CARAMELO_SSH", "/usr/bin/ssh-wrapper")
	t.Setenv("CARAMELO_SSH_OPTS", `-F "/tmp/lab ssh-config" -i /tmp/key`)

	bin, extra, err := SSHClient()
	if err != nil {
		t.Fatalf("SSHClient: %v", err)
	}
	if bin != "/usr/bin/ssh-wrapper" {
		t.Errorf("bin = %q, want the binary named by CARAMELO_SSH", bin)
	}
	want := []string{"-F", "/tmp/lab ssh-config", "-i", "/tmp/key"}
	if !reflect.DeepEqual(extra, want) {
		t.Errorf("extra = %q, want %q (quotes group, they do not survive)", extra, want)
	}
}

func TestSSHClientRejectsUnparseableOptions(t *testing.T) {
	t.Setenv("CARAMELO_SSH_OPTS", `-F "/tmp/unterminated`)
	_, _, err := SSHClient()
	if err == nil {
		t.Fatal("unparseable CARAMELO_SSH_OPTS must be an error")
	}
	if !strings.Contains(err.Error(), "CARAMELO_SSH_OPTS") {
		t.Errorf("error = %v, want it to name the variable", err)
	}
}

func TestSplitWordsHandlesEscapes(t *testing.T) {
	got, err := SplitWords(`a\ b "c\"d" 'e f'	g`)
	if err != nil {
		t.Fatalf("SplitWords: %v", err)
	}
	want := []string{"a b", `c"d`, "e f", "g"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SplitWords = %q, want %q", got, want)
	}
	if _, err := SplitWords(`trailing\`); err == nil {
		t.Error("a trailing backslash must be an error")
	}
	if _, err := SplitWords(`'unterminated`); err == nil {
		t.Error("an unterminated quote must be an error")
	}
}

func TestParseTargetRejectsJunkAfterAnIPv6Address(t *testing.T) {
	if got, err := ParseTarget("[::1]junk"); err == nil {
		t.Errorf("ParseTarget = %+v, want an error for trailing junk after ]", got)
	}
}
