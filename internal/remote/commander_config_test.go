package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestASavedCommanderConfigCarriesItsNameRoleAndBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), CommanderConfigFile)
	if err := SaveCommanderConfigTo(path, CommanderConfig{Name: "laptop", Role: RoleCommander}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name: laptop\n", "role: commander\n", "commander: {}\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the file lacks %q:\n%s", want, b)
		}
	}
}

func TestTheRetiredTopLevelKeysAreRefusedByName(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"default_machine", "default_machine: box\n"},
		{"machines", "machines:\n  box: alex@box:4022\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), CommanderConfigFile)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadCommanderConfigFrom(path)
			if err == nil {
				t.Fatalf("%s loaded; want a refusal naming it and the fleets that replace it", tc.name)
			}
			for _, want := range []string{tc.name, "retired", "fleets"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to name %s", err, want)
				}
			}
		})
	}
}

func TestOnlyACommanderRoleLoadsFromTheCommanderConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), CommanderConfigFile)
	if err := os.WriteFile(path, []byte("name: box\nrole: hub\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadCommanderConfigFrom(path)
	if err == nil {
		t.Fatal("a hub's role loaded from the commander config")
	}
	if !strings.Contains(err.Error(), RoleCommander) {
		t.Errorf("error = %v, want it to name %s", err, RoleCommander)
	}
}

func TestCommanderInitializedSaysWhereItLooked(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	want := filepath.Join(dir, CommanderDirName, CommanderConfigFile)

	ok, path, err := CommanderInitialized()
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("an empty config dir reported an initialized commander")
	}
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	if err := SaveCommanderConfigTo(want, CommanderConfig{Name: "laptop", Role: RoleCommander}); err != nil {
		t.Fatal(err)
	}
	ok, _, err = CommanderInitialized()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("a written config did not report an initialized commander")
	}
}
