//go:build integration

package prod

import (
	"encoding/json"
	"strings"
	"testing"

	cfleet "github.com/plytz/caramelo/internal/fleet"
)

func TestZZProductionOnAFleetOfOne(t *testing.T) {
	begin(t)
	needProduction(t)

	res := mustInRepo(t, "machine", "list", "--json")
	var list []cfleet.Machine
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &list); err != nil {
		t.Fatalf("machine list --json: %v\nstdout: %q", err, res.Stdout)
	}
	if len(list) != 1 {
		t.Fatalf("machine list = %d rows on a machine of one, want 1: %+v", len(list), list)
	}
	self := list[0]
	if !self.Role.IsHub() {
		t.Errorf("the machine has role %q, want %q", self.Role, cfleet.RoleHub)
	}
	if got := self.Subnet.String(); got != cfleet.HubSubnet {
		t.Errorf("the machine holds %s, want %s", got, cfleet.HubSubnet)
	}
	if self.Arch == "" {
		t.Error("the machine reports no architecture; a release is built for one")
	}

	left := listEnvs(t)
	if len(left) == 0 {
		t.Fatal("the suite left no environments to say anything about")
	}
	detail := mustInRepo(t, "machine", "show", self.Name, "--json")
	for _, e := range left {
		t.Logf("%s (%s) is on this machine", e.Name, e.Mode)
		if !strings.Contains(detail.Stdout, e.Name) {
			t.Errorf("machine show %s does not list %s:\n%s", self.Name, e.Name, detail.Stdout)
		}
	}
	if self.Envs != len(left) {
		t.Errorf("machine list says %d environments and env list says %d", self.Envs, len(left))
	}

	res = mustInRepo(t, "releases", "--json")
	var doc struct {
		Releases []struct {
			Tree    string `json:"tree"`
			Machine string `json:"machine"`
			Built   []struct {
				Arch    string `json:"arch"`
				Machine string `json:"machine"`
			} `json:"built"`
		} `json:"releases"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &doc); err != nil {
		t.Fatalf("releases --json: %v\nstdout: %q", err, res.Stdout)
	}
	if len(doc.Releases) == 0 {
		t.Fatal("the app has no releases after a suite that deployed several")
	}
	for _, rel := range doc.Releases {
		if rel.Machine != self.Name {
			t.Errorf("release %s says it was built on %q, want %q", rel.Tree, rel.Machine, self.Name)
		}
		if len(rel.Built) == 0 {
			t.Errorf("release %s records no images; the index is what a fleet copies from", rel.Tree)
			continue
		}
		for _, im := range rel.Built {
			if im.Arch != self.Arch || im.Machine != self.Name {
				t.Errorf("release %s has an image on %s/%s, want %s/%s",
					rel.Tree, im.Machine, im.Arch, self.Name, self.Arch)
			}
		}
	}

	for _, e := range events(t, left[0].Name, "--limit", "100") {
		if e.Machine != "" {
			t.Errorf("an event on a machine of one names machine %q; the field is for a fleet: %+v", e.Machine, e)
			break
		}
	}
}
