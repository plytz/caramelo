package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/vpnclient"
)

func freshBox(t *testing.T) (config, cache string) {
	t.Helper()
	dir := isolate(t)
	return filepath.Join(dir, "config", remote.CommanderDirName), filepath.Join(dir, "cache", remote.CommanderDirName)
}

func readCommanderConfig(t *testing.T, dir string) remote.CommanderConfig {
	t.Helper()
	c, err := remote.LoadCommanderConfigFrom(filepath.Join(dir, remote.CommanderConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func wantMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if got := fi.Mode().Perm(); got != mode {
		t.Errorf("%s mode = %v, want %v", path, got, mode)
	}
}

func TestCommanderInitWritesEveryFileACommanderNeeds(t *testing.T) {
	config, cache := freshBox(t)
	code, stdout, stderr := run(t, "commander", "init", "--name", "laptop")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}

	wantMode(t, config, 0o700)
	wantMode(t, filepath.Join(config, remote.CommanderConfigFile), 0o600)
	wantMode(t, filepath.Join(config, vpnclient.IdentityKeyFile), 0o600)
	wantMode(t, filepath.Join(config, vpnclient.KeyDir), 0o700)
	wantMode(t, cache, 0o700)

	cfg := readCommanderConfig(t, config)
	if cfg.Name != "laptop" || cfg.Role != remote.RoleCommander {
		t.Errorf("config = %+v, want laptop as a commander", cfg)
	}
	entries, err := os.ReadDir(filepath.Join(config, vpnclient.KeyDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the fleet record dir holds %d entries, want it empty", len(entries))
	}
	if !strings.Contains(stdout, "laptop is a commander") {
		t.Errorf("stdout = %q, want it to say the box is a commander", stdout)
	}
}

func TestCommanderInitDefaultsTheNameToTheHostname(t *testing.T) {
	config, _ := freshBox(t)
	if code, _, stderr := run(t, "commander", "init"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	host, _ := os.Hostname()
	if want := thisMachineName(""); readCommanderConfig(t, config).Name != want {
		t.Errorf("name = %q, want %q (from hostname %q)", readCommanderConfig(t, config).Name, want, host)
	}
}

func TestCommanderInitRunAgainChangesNothing(t *testing.T) {
	config, _ := freshBox(t)
	if code, _, stderr := run(t, "commander", "init", "--name", "laptop"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	key, err := os.ReadFile(filepath.Join(config, vpnclient.IdentityKeyFile))
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := run(t, "commander", "init")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "already a commander") {
		t.Errorf("stdout = %q, want it to say the box already is one", stdout)
	}
	again, err := os.ReadFile(filepath.Join(config, vpnclient.IdentityKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(key) {
		t.Error("the identity key was replaced by the second run")
	}
	if got := readCommanderConfig(t, config).Name; got != "laptop" {
		t.Errorf("name = %q, want the first run's laptop", got)
	}
}

func TestCommanderInitNameAloneRenames(t *testing.T) {
	config, _ := freshBox(t)
	if code, _, stderr := run(t, "commander", "init", "--name", "laptop"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	before, err := os.ReadFile(filepath.Join(config, vpnclient.IdentityKeyFile))
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := run(t, "commander", "init", "--name", "desk")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "renamed laptop to desk") {
		t.Errorf("stdout = %q, want the rename line", stdout)
	}
	if got := readCommanderConfig(t, config).Name; got != "desk" {
		t.Errorf("name = %q, want desk", got)
	}
	after, err := os.ReadFile(filepath.Join(config, vpnclient.IdentityKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("a rename generated a new identity key")
	}
}

func TestCommanderInitJSONReportsWhatItWrote(t *testing.T) {
	config, cache := freshBox(t)
	code, stdout, stderr := run(t, "commander", "init", "--name", "laptop", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var got commanderInitResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("%v: %s", err, stdout)
	}
	if got.Name != "laptop" || got.Role != remote.RoleCommander || !got.Changed {
		t.Errorf("result = %+v", got)
	}
	if !vpnclient.ValidKey(got.PublicKey) {
		t.Errorf("public key = %q, want a WireGuard key", got.PublicKey)
	}
	want := commanderPaths{
		Dir:         config,
		Config:      filepath.Join(config, remote.CommanderConfigFile),
		IdentityKey: filepath.Join(config, vpnclient.IdentityKeyFile),
		VPNDir:      filepath.Join(config, vpnclient.KeyDir),
		CacheDir:    cache,
	}
	if got.Paths != want {
		t.Errorf("paths = %+v, want %+v", got.Paths, want)
	}
	for _, path := range []string{want.Dir, want.Config, want.IdentityKey, want.VPNDir, want.CacheDir} {
		if !slices.Contains(got.Created, path) {
			t.Errorf("created = %v, want it to hold %s", got.Created, path)
		}
	}

	stdout = ""
	code, stdout, stderr = run(t, "commander", "init", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	got = commanderInitResult{}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("%v: %s", err, stdout)
	}
	if got.Changed || len(got.Created) != 0 {
		t.Errorf("the second run reported %+v, want nothing changed", got)
	}
}

func TestCommanderInitRefusesANameThatIsNoName(t *testing.T) {
	freshBox(t)
	code, _, stderr := run(t, "commander", "init", "--name", "!!!")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "--name") {
		t.Errorf("stderr = %q, want it to blame --name", stderr)
	}
}

func TestTheCommandsThatSetUpAnotherMachineNeedACommander(t *testing.T) {
	for _, args := range [][]string{
		{"hub", "setup", "--target", "admin@box.example", "--yes"},
		{"member", "add", "admin@box.example"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			freshBox(t)
			code, _, stderr := run(t, args...)
			if code != ExitUsage {
				t.Errorf("exit %d, want %d", code, ExitUsage)
			}
			if !strings.Contains(stderr, "caramelo commander init") {
				t.Errorf("stderr = %q, want it to name 'caramelo commander init'", stderr)
			}
		})
	}
}

func TestHubSetupTakesItsNameFromTheCommanderConfig(t *testing.T) {
	freshBox(t)
	if code, _, stderr := run(t, "commander", "init", "--name", "laptop"); code != ExitOK {
		t.Fatalf("commander init: exit %d: %s", code, stderr)
	}
	got, err := commanderMachineName("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "laptop" {
		t.Errorf("the machine's name = %q, want the commander's laptop", got)
	}
	if got, err := commanderMachineName("box"); err != nil || got != "box" {
		t.Errorf("--name box gave %q (%v), want box", got, err)
	}
}

func TestWithoutACommanderTheMachineIsNamedAfterItsHost(t *testing.T) {
	freshBox(t)
	host, _ := os.Hostname()
	got, err := commanderMachineName("")
	if err != nil {
		t.Fatal(err)
	}
	if want := machineNameSlug(host); want != "" && got != want {
		t.Errorf("the machine's name = %q, want %q", got, want)
	}
}

func TestABrokenCommanderConfigStopsHubSetup(t *testing.T) {
	freshBox(t)
	path, err := remote.CommanderConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("machines:\n  box: alex@box:4022\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run(t, "hub", "setup", "--yes")
	if code == ExitOK {
		t.Fatal("hub setup ran with a commander config it could not read")
	}
	for _, want := range []string{"machines", "retired", "fleets"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to name %s", stderr, want)
		}
	}
}
