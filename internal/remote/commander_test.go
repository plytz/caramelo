package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCommander(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCommanderConfigRefusesTheRetiredKeys(t *testing.T) {
	for _, body := range []string{
		"default_machine: box\n",
		"machines:\n  box: caramelo@box:4022\n",
	} {
		_, err := LoadCommanderConfigFrom(writeCommander(t, body))
		if err == nil {
			t.Fatalf("%q loaded: the retired keys must be refused by name", body)
		}
		for _, want := range []string{"default_machine", "machines", "commander"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to mention %q", err, want)
			}
		}
	}
}

func TestCommanderConfigRefusesAServerRole(t *testing.T) {
	_, err := LoadCommanderConfigFrom(writeCommander(t, "name: box\nrole: hub\n"))
	if err == nil {
		t.Fatal("a commander file claiming role: hub must be refused")
	}
	if !strings.Contains(err.Error(), RoleCommander) {
		t.Errorf("error = %v, want it to name the role a commander file takes", err)
	}
}

func TestCommanderConfigRefusesADefaultThatIsNoFleet(t *testing.T) {
	_, err := LoadCommanderConfigFrom(writeCommander(t,
		"name: laptop\nrole: commander\ncommander:\n  default_fleet: work\n  fleets:\n    home:\n      hub: caramelo@box:4022\n"))
	if err == nil {
		t.Fatal("a default_fleet naming no fleet must be refused")
	}
	if !strings.Contains(err.Error(), "default_fleet") || !strings.Contains(err.Error(), "home") {
		t.Errorf("error = %v, want it to name the key and the fleets there are", err)
	}
}

func TestCommanderConfigRefusesAFleetWithNoHub(t *testing.T) {
	_, err := LoadCommanderConfigFrom(writeCommander(t,
		"name: laptop\nrole: commander\ncommander:\n  fleets:\n    home: {}\n"))
	if err == nil {
		t.Fatal("a fleet with no hub must be refused")
	}
	if !strings.Contains(err.Error(), "commander.fleets.home.hub") {
		t.Errorf("error = %v, want it to name the missing key", err)
	}
}

func TestCommanderConfigRefusesAFleetNameThatIsNoSlug(t *testing.T) {
	_, err := LoadCommanderConfigFrom(writeCommander(t,
		"name: laptop\nrole: commander\ncommander:\n  fleets:\n    Home Fleet:\n      hub: caramelo@box:4022\n"))
	if err == nil {
		t.Fatal("a fleet name that is no slug must be refused")
	}
	if !strings.Contains(err.Error(), "slug") {
		t.Errorf("error = %v, want it to say what a fleet's name is", err)
	}
}

func TestMachineNameFallsBackToTheHostname(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Skip("no hostname here")
	}
	if i := strings.Index(host, "."); i > 0 {
		host = host[:i]
	}
	if got := (CommanderConfig{}).MachineName(); got != host {
		t.Errorf("MachineName() = %q, want the hostname %q", got, host)
	}
	if got := (CommanderConfig{Name: "eric-laptop"}).MachineName(); got != "eric-laptop" {
		t.Errorf("MachineName() = %q, want the name the file gives", got)
	}
}

func TestPinAdoptsOnFirstContactAndRefusesAChangedKey(t *testing.T) {
	const first = "Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE="
	const other = "aB2wE4hG6uI8oP0Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF="
	c := CommanderConfig{Commander: Commander{Fleets: map[string]Fleet{
		"home": {Hub: "caramelo@box:4022"},
	}}}

	pinned, err := c.Pin("home", first)
	if err != nil {
		t.Fatal(err)
	}
	if !pinned {
		t.Fatal("the first key a fleet answers with must be pinned")
	}
	if got := c.Commander.Fleets["home"].PublicKey; got != first {
		t.Errorf("public_key = %q, want %q", got, first)
	}

	pinned, err = c.Pin("home", first)
	if err != nil || pinned {
		t.Fatalf("Pin with the same key = %v, %v; want no change and no error", pinned, err)
	}

	_, err = c.Pin("home", other)
	if err == nil {
		t.Fatal("a hub answering with another key must be refused, not adopted")
	}
	for _, want := range []string{"home", "caramelo@box:4022", "fleet remove home"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to say %q", err, want)
		}
	}
	if got := c.Commander.Fleets["home"].PublicKey; got != first {
		t.Errorf("public_key = %q, want the pinned key to survive a refusal", got)
	}
}

func TestPinOfAFleetThatIsNotConfigured(t *testing.T) {
	c := CommanderConfig{}
	if _, err := c.Pin("home", "Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE="); err == nil {
		t.Fatal("pinning a key against no fleet must fail")
	}
}

func TestRecordAppAndFleetForApp(t *testing.T) {
	c := CommanderConfig{Commander: Commander{Fleets: map[string]Fleet{
		"home": {Hub: "caramelo@box:4022"},
		"work": {Hub: "ops@hub.work.example:4022"},
	}}}
	if !c.RecordApp("home", "shop") {
		t.Fatal("the first record of an app must be a change")
	}
	if c.RecordApp("home", "shop") {
		t.Error("recording an app twice must be no change")
	}
	if c.RecordApp("nowhere", "shop") {
		t.Error("recording an app on a fleet that is in no config must be no change")
	}
	if got := c.FleetForApp("shop"); got != "home" {
		t.Errorf("FleetForApp(shop) = %q, want home", got)
	}
	if got := c.FleetForApp("blog"); got != "" {
		t.Errorf("FleetForApp(blog) = %q, want none", got)
	}
	c.RecordApp("work", "shop")
	if got := c.FleetForApp("shop"); got != "" {
		t.Errorf("FleetForApp(shop) = %q, want none: an app seen on two fleets picks neither", got)
	}
}

func TestSetAndRemoveFleet(t *testing.T) {
	var c CommanderConfig
	c.SetFleet("home", Fleet{Hub: "caramelo@box:4022"})
	if c.Commander.DefaultFleet != "home" {
		t.Errorf("default_fleet = %q, want the first fleet recorded", c.Commander.DefaultFleet)
	}
	c.SetFleet("work", Fleet{Hub: "ops@hub.work.example:4022"})
	if c.Commander.DefaultFleet != "home" {
		t.Errorf("default_fleet = %q, want the first fleet to stay the default", c.Commander.DefaultFleet)
	}
	if err := c.SetDefaultFleet("nowhere"); err == nil {
		t.Error("making an unknown fleet the default must fail")
	}
	if err := c.SetDefaultFleet("work"); err != nil {
		t.Fatal(err)
	}
	if !c.RemoveFleet("work") {
		t.Fatal("removing a fleet that is there must be a change")
	}
	if c.Commander.DefaultFleet != "" {
		t.Errorf("default_fleet = %q, want it cleared with the fleet it named", c.Commander.DefaultFleet)
	}
	if c.RemoveFleet("work") {
		t.Error("removing a fleet twice must be no change")
	}
}
