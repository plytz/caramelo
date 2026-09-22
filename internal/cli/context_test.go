package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/place"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/serverconfig"
)

func freshPlace(t *testing.T) {
	t.Helper()
	noCommanderConfig(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("SUDO_USER", "")
	t.Setenv("CARAMELO_APP", "")
	t.Setenv("CARAMELO_ENV", "")
	useSystemConfigDir(t, t.TempDir())
	fakeGit(t, nil)
}

func contextOf(t *testing.T, args ...string) place.Context {
	t.Helper()
	code, stdout, stderr := run(t, append(args, "--json")...)
	if code != ExitOK {
		t.Fatalf("caramelo context --json: exit %d: %s", code, stderr)
	}
	var c place.Context
	if err := json.Unmarshal([]byte(stdout), &c); err != nil {
		t.Fatalf("context --json is not JSON: %v\n%s", err, stdout)
	}
	return c
}

func TestContextOnAFreshBoxSaysSoAndNeverForwards(t *testing.T) {
	freshPlace(t)
	fwd := &fakeForward{}
	fwd.install(t)

	code, stdout, stderr := run(t, "context")
	if code != ExitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if fwd.called {
		t.Error("context asked a daemon where this machine is")
	}
	for _, want := range []string{"fresh box: no config", "commander init", "looked for", "talks to"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
}

func TestContextJSONCarriesWhatTheTextSays(t *testing.T) {
	freshPlace(t)
	useCommanderConfig(t, remote.CommanderConfig{
		Name: "laptop",
		Role: remote.RoleCommander,
		Commander: remote.Commander{
			DefaultFleet: "home",
			Fleets:       map[string]remote.Fleet{"home": {Hub: "caramelo@box.example.com:4022"}},
		},
	})

	c := contextOf(t, "context")
	if c.Role != place.RoleCommander || c.Name != "laptop" {
		t.Fatalf("context = %+v, want the commander laptop", c)
	}
	if c.Commander == nil || len(c.Commander.Fleets) != 1 || c.Commander.Fleets[0].Name != "home" {
		t.Errorf("commander = %+v, want the one fleet home", c.Commander)
	}
	if c.TalksTo.Fleet != "home" || c.TalksTo.Why != "commander.default_fleet" {
		t.Errorf("talks to = %+v, want fleet home because it is the default", c.TalksTo)
	}

	_, stdout, _ := run(t, "context")
	if !strings.Contains(stdout, c.Header()) {
		t.Errorf("the text output does not open with the header %q:\n%s", c.Header(), stdout)
	}
}

func TestContextOnAServerReadsThatMachinesConfig(t *testing.T) {
	freshPlace(t)
	dir := t.TempDir()
	body := "name: box\nrole: hub\nhub:\n  fleet: home\n"
	if err := os.WriteFile(filepath.Join(dir, serverconfig.ConfigFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(serverconfig.ConfigDirEnv, dir)

	c := contextOf(t, "context")
	if c.Role != place.RoleHub || c.Name != "box" || c.Server == nil {
		t.Fatalf("context = %+v, want the hub box", c)
	}
	if c.Server.Fleet != "home" || c.Server.Paths.Config != dir {
		t.Errorf("server = %+v, want fleet home read from %s", c.Server, dir)
	}
	if c.Server.Services.VPNMode != serverconfig.VPNModeUserspace {
		t.Errorf("services = %+v, want vpn_mode %q", c.Server.Services, serverconfig.VPNModeUserspace)
	}

	_, stdout, _ := run(t, "context")
	for _, want := range []string{"box, hub of fleet home", "caramelod", "vpn dir", "vpn mode", "api listen"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
}

func TestContextOnAServerThatIsAlsoACommanderNamesBothFiles(t *testing.T) {
	freshPlace(t)
	dir := t.TempDir()
	body := "name: box\nrole: hub\nhub:\n  fleet: home\n"
	if err := os.WriteFile(filepath.Join(dir, serverconfig.ConfigFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(serverconfig.ConfigDirEnv, dir)
	useCommanderConfig(t, oneFleet())

	c := contextOf(t, "context")
	if c.Role != place.RoleHub || c.Commander == nil || len(c.Commander.Fleets) != 1 {
		t.Fatalf("context = %+v, want the hub box with the fleets of its commander config", c)
	}
	if c.TalksTo.Fleet != "home" {
		t.Errorf("talks to = %+v, want fleet home: no socket answers here", c.TalksTo)
	}

	_, stdout, _ := run(t, "context")
	for _, want := range []string{"commander", c.CommanderConfig, "FLEET", "home"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
}

func TestContextFollowsTheFleetAndMachineFlags(t *testing.T) {
	freshPlace(t)
	useCommanderConfig(t, remote.CommanderConfig{
		Name: "laptop",
		Role: remote.RoleCommander,
		Commander: remote.Commander{
			Fleets: map[string]remote.Fleet{
				"home": {Hub: "caramelo@box.example.com:4022"},
				"work": {Hub: "ops@hub.work.example:4022"},
			},
		},
	})

	if c := contextOf(t, "context", "--fleet", "work"); c.TalksTo.Fleet != "work" {
		t.Errorf("talks to = %+v, want the fleet --fleet names", c.TalksTo)
	}
	if c := contextOf(t, "context", "--machine", "alex@10.0.0.5"); c.TalksTo.Target != "alex@10.0.0.5:4022" {
		t.Errorf("talks to = %+v, want the raw ssh target", c.TalksTo)
	}
	c := contextOf(t, "context")
	if c.TalksTo.Kind != place.TalksNothing || !strings.Contains(c.TalksTo.Problem, "home, work") {
		t.Errorf("talks to = %+v, want a refusal naming both fleets", c.TalksTo)
	}
}
