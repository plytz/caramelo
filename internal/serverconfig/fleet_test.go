package serverconfig

import (
	"os"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/vpn"
)

func aKey(t *testing.T) string {
	t.Helper()
	priv, err := vpn.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := priv.Public()
	if err != nil {
		t.Fatal(err)
	}
	return pub.Base64()
}

func memberConfig(t *testing.T) Config {
	t.Helper()
	c := Default()
	c.VPNSubnet = "10.87.0.0/16"
	c.Fleet = Fleet{
		Role:   RoleMember,
		Name:   "m1",
		Subnet: "10.87.0.0/16",
		Hub:    FleetHub{Name: "nx1", Endpoint: "hub.example.com:4021", Address: "10.86.0.1", PublicKey: aKey(t)},
	}
	return c
}

func TestAMachineWithNoFleetBlockIsAHubOfOne(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !c.IsHub() || c.IsMember() {
		t.Errorf("role = %q, want %q", c.FleetRole(), RoleHub)
	}
	if r, err := c.FleetRangePrefix(); err != nil || r.String() != DefaultFleetRange {
		t.Errorf("range = %v (%v), want %s", r, err, DefaultFleetRange)
	}
	if _, err := c.HubAddress(); err == nil {
		t.Error("a hub was given a hub address")
	}

	dir := t.TempDir()
	if err := Save(dir, c, 0o644); err != nil {
		t.Fatalf("Save: %v", err)
	}
	b, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "fleet:") {
		t.Errorf("a hub of one wrote a fleet block:\n%s", b)
	}
}

func TestAMemberRoundTripsAndKnowsWhereItsHubIs(t *testing.T) {
	c := memberConfig(t)
	dir := t.TempDir()
	if err := Save(dir, c, 0o600); err != nil {
		t.Fatalf("Save: %v", err)
	}
	back, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if back.Fleet != c.Fleet {
		t.Fatalf("fleet = %+v, want %+v", back.Fleet, c.Fleet)
	}
	if !back.IsMember() || back.IsHub() {
		t.Errorf("role = %q, want %q", back.FleetRole(), RoleMember)
	}
	if got, err := back.FleetSubnetPrefix(); err != nil || got.String() != "10.87.0.0/16" {
		t.Errorf("subnet = %v (%v), want 10.87.0.0/16", got, err)
	}
	ip, err := back.HubAddress()
	if err != nil || ip.String() != "10.86.0.1" {
		t.Errorf("hub address = %v (%v), want 10.86.0.1", ip, err)
	}
	api, err := back.HubAPIAddrPort()
	if err != nil || api.String() != "10.86.0.1:4022" {
		t.Errorf("hub api = %v (%v), want 10.86.0.1:4022", api, err)
	}
	res, err := back.HubResolverAddrPort()
	if err != nil || res.String() != "10.86.0.1:53" {
		t.Errorf("hub resolver = %v (%v), want 10.86.0.1:53", res, err)
	}
}

func TestValidateRefusesAFleetBlockThatCannotBeTrue(t *testing.T) {
	key := aKey(t)
	hub := FleetHub{Name: "nx1", Endpoint: "hub.example.com:4021", Address: "10.86.0.1", PublicKey: key}
	cases := map[string]struct {
		fleet  Fleet
		subnet string
		want   string
	}{
		"unknown role": {
			fleet: Fleet{Role: "leader"}, want: "fleet.role",
		},
		"a member with no hub": {
			fleet: Fleet{Role: RoleMember}, want: "must say which machine",
		},
		"a hub with one": {
			fleet: Fleet{Role: RoleHub, Hub: hub}, want: "only a member has one",
		},
		"a private hub": {
			fleet: Fleet{Role: RoleHub, Private: true}, want: "fleet.private is set on a hub",
		},
		"a hub with no endpoint": {
			fleet: Fleet{Role: RoleMember, Hub: FleetHub{Name: "nx1", Address: "10.86.0.1", PublicKey: key}},
			want:  "fleet.hub.endpoint",
		},
		"a hub endpoint with no port": {
			fleet: Fleet{Role: RoleMember, Hub: FleetHub{Name: "nx1", Endpoint: "hub.example.com", Address: "10.86.0.1", PublicKey: key}},
			want:  "want host:port",
		},
		"a hub key that is not a key": {
			fleet: Fleet{Role: RoleMember, Hub: FleetHub{Name: "nx1", Endpoint: "h:4021", Address: "10.86.0.1", PublicKey: "not-a-key"}},
			want:  "fleet.hub.public_key",
		},
		"a range that is not a prefix": {
			fleet: Fleet{Range: "10.80.0.0"}, want: "fleet.range",
		},
		"a range too small for a machine": {
			fleet: Fleet{Range: "10.80.0.0/20"}, want: "holds no /16",
		},
		"a subnet that is not a /16": {
			fleet: Fleet{Role: RoleMember, Subnet: "10.87.0.0/24", Hub: hub}, want: "a machine's range is a /16",
		},
		"a subnet outside the range": {
			fleet: Fleet{Role: RoleMember, Subnet: "10.200.0.0/16", Hub: hub}, subnet: "10.200.0.0/16",
			want: "outside the fleet's range",
		},
		"a subnet that disagrees with vpn_subnet": {
			fleet: Fleet{Role: RoleMember, Subnet: "10.87.0.0/16", Hub: hub}, subnet: "10.88.0.0/16",
			want: "the same range said twice",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := Default()
			if tc.subnet != "" {
				c.VPNSubnet = tc.subnet
			} else if tc.fleet.Subnet != "" {
				c.VPNSubnet = tc.fleet.Subnet
			}
			c.Fleet = tc.fleet
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %+v", tc.fleet)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate said %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestAPrivateMemberIsValid(t *testing.T) {
	c := memberConfig(t)
	c.Fleet.Private = true
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestAMachineIsNamedBySlugAndAMemberMustBeNamed(t *testing.T) {
	c := memberConfig(t)
	c.Fleet.Name = "Pi Two"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "fleet.name") {
		t.Fatalf("Validate of a name that is not a slug = %v, want it refused", err)
	}
	c.Fleet.Name = ""
	err = c.Validate()
	if err == nil || !strings.Contains(err.Error(), "fleet.name must say") {
		t.Fatalf("Validate of a nameless member = %v, want it refused", err)
	}

	h := Default()
	if err := h.Validate(); err != nil {
		t.Fatalf("Validate of a hub of one: %v", err)
	}
}
