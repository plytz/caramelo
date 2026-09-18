package cli

import (
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/vpnclient"
)

const (
	hubKey      = "Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE="
	anotherKey  = "aB2wE4hG6uI8oP0Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF="
	commanderIP = "10.86.0.2"
)

func homeFleet(t *testing.T) {
	t.Helper()
	cfg := remote.CommanderConfig{
		Name: "eric-laptop",
		Role: remote.RoleCommander,
		Commander: remote.Commander{
			DefaultFleet: "home",
			Fleets:       map[string]remote.Fleet{"home": {Hub: "eric@box.example.com:4022"}},
		},
	}
	if err := remote.SaveCommanderConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func homeRecord() vpnclient.Record {
	return vpnclient.Record{
		Fleet:      "home",
		Endpoint:   "box.example.com:4021",
		MachineKey: hubKey,
		Subnet:     netip.MustParsePrefix("10.86.0.0/16"),
		MachineIP:  netip.MustParseAddr("10.86.0.1"),
		PeerName:   "eric-laptop",
		IP:         netip.MustParseAddr(commanderIP),
		PublicKey:  anotherKey,
		APIPort:    4022,
	}
}

func TestSavingARecordPinsTheHubKeyForTheFleet(t *testing.T) {
	isolate(t)
	homeFleet(t)

	if err := (pinnedRecords{}).Save(homeRecord()); err != nil {
		t.Fatal(err)
	}
	cfg, err := remote.LoadCommanderConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Commander.Fleets["home"].PublicKey; got != hubKey {
		t.Errorf("public_key = %q, want the hub's key pinned on first contact", got)
	}
}

func TestAHubAnsweringWithAnotherKeyIsRefused(t *testing.T) {
	isolate(t)
	homeFleet(t)

	store := pinnedRecords{}
	if err := store.Save(homeRecord()); err != nil {
		t.Fatal(err)
	}
	moved := homeRecord()
	moved.MachineKey = anotherKey
	err := store.Save(moved)
	if err == nil {
		t.Fatal("a hub answering under the same address with another key must be refused")
	}
	for _, want := range []string{"home", "fleet remove home"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to say %q", err, want)
		}
	}

	rec, err := store.Load("home")
	if err != nil {
		t.Fatalf("the refused save must leave the good record alone: %v", err)
	}
	if rec.MachineKey != hubKey {
		t.Errorf("machine_key = %q, want the pinned key", rec.MachineKey)
	}
}

func TestLoadingARecordThatNoLongerMatchesThePinIsRefused(t *testing.T) {
	isolate(t)
	homeFleet(t)

	if err := (&vpnclient.FileRecordStore{Dir: vpnDirOf(t)}).Save(homeRecord()); err != nil {
		t.Fatal(err)
	}
	cfg, _ := remote.LoadCommanderConfig()
	if _, err := cfg.Pin("home", anotherKey); err != nil {
		t.Fatal(err)
	}
	if err := remote.SaveCommanderConfig(cfg); err != nil {
		t.Fatal(err)
	}

	if _, err := (pinnedRecords{}).Load("home"); err == nil {
		t.Fatal("a record whose hub key is not the pinned one must be refused")
	}
}

func TestARecordOfABoxInNoFleetIsNotPinned(t *testing.T) {
	isolate(t)
	homeFleet(t)

	raw := homeRecord()
	raw.Fleet = "alex@box"
	if err := (pinnedRecords{}).Save(raw); err != nil {
		t.Fatalf("a raw --machine target is in no fleet and must not be pinned against one: %v", err)
	}
	cfg, _ := remote.LoadCommanderConfig()
	if got := cfg.Commander.Fleets["home"].PublicKey; got != "" {
		t.Errorf("home public_key = %q, want nothing: the record was not that fleet's", got)
	}
}

func vpnDirOf(t *testing.T) string {
	t.Helper()
	dir, err := remote.CommanderDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir + "/" + vpnclient.KeyDir
}

func TestCommanderPeerNameIsTheCommandersOwnName(t *testing.T) {
	isolate(t)
	homeFleet(t)

	if got := commanderPeerName(""); got != "eric-laptop" {
		t.Errorf("commanderPeerName() = %q, want the commander's name", got)
	}
	if got := commanderPeerName("laptop"); got != "laptop" {
		t.Errorf("commanderPeerName(laptop) = %q, want the flag to win", got)
	}
}

func TestKeyAddDefaultsToTheCommandersName(t *testing.T) {
	isolate(t)
	homeFleet(t)
	fwd := &fakeForward{}
	fwd.install(t)

	code, _, stderr := runFleet(t, "key", "add", "--file", writeKeyFile(t))
	if code != ExitOK {
		t.Fatalf("key add: exit %d: %s", code, stderr)
	}
	if !fwd.called {
		t.Fatal("key add was not forwarded")
	}
	if !contains(fwd.args, "--name") || !contains(fwd.args, "eric-laptop") {
		t.Errorf("forwarded args = %q, want the commander's name injected", fwd.args)
	}
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func writeKeyFile(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/id.pub"
	line := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample eric@laptop\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
