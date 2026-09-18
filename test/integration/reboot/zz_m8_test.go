//go:build integration

package reboot

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	cfleet "github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/test/integration/itest"
)

func TestZZTheFleetSurvivesAReboot(t *testing.T) {
	begin(t)

	before := fleetMachines(t)
	if len(before) != 1 {
		t.Fatalf("member list = %d rows on a machine of one, want 1: %+v", len(before), before)
	}
	if !before[0].Role.IsHub() {
		t.Fatalf("a machine nobody joined has role %q, want %q", before[0].Role, cfleet.RoleHub)
	}
	if before[0].Subnet.String() != cfleet.HubSubnet {
		t.Errorf("the machine holds %s, want %s: a machine that was on its own keeps every address "+
			"it had already handed out", before[0].Subnet, cfleet.HubSubnet)
	}

	powerCycle(t)
	waitUntil(t, time.Now().Add(itest.Scale(2*time.Minute)), "the API answers after the power cycle", func() error {
		if res := apiTry(t, itest.Scale(10*time.Second), "status", "--json"); res.ExitCode != 0 {
			return errExit(res.ExitCode, res.Stderr)
		}
		return nil
	})

	after := fleetMachines(t)
	if len(after) != len(before) {
		t.Fatalf("member list = %d rows after the reboot, want %d", len(after), len(before))
	}
	if after[0].Name != before[0].Name {
		t.Errorf("the machine is called %q after the reboot, was %q", after[0].Name, before[0].Name)
	}
	if after[0].Subnet != before[0].Subnet {
		t.Errorf("the machine holds %s after the reboot, held %s", after[0].Subnet, before[0].Subnet)
	}
	if !after[0].Role.IsHub() {
		t.Errorf("the machine has role %q after the reboot, want %q", after[0].Role, cfleet.RoleHub)
	}
	if !after[0].Reachable(time.Now()) {
		t.Error("a machine reports itself unreachable after coming back; it is the one asking")
	}
	if after[0].PublicKey != before[0].PublicKey {
		t.Errorf("the machine's public key changed across a reboot: %q -> %q",
			before[0].PublicKey, after[0].PublicKey)
	}
}

func fleetMachines(t *testing.T) []cfleet.Machine {
	t.Helper()
	res := api(t, itest.Scale(time.Minute), "member", "list", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("member list: exit %d\nstdout:%s\nstderr:%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	var list []cfleet.Machine
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &list); err != nil {
		t.Fatalf("member list --json: %v\nstdout: %q", err, res.Stdout)
	}
	return list
}
