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

func hubConfig(name string) Config {
	c := Default()
	c.Name = name
	c.Hub.Fleet = name
	return c
}

func memberConfig(t *testing.T) Config {
	t.Helper()
	c := Default()
	c.Name = "m1"
	c.Role = RoleMember
	c.Hub = Hub{}
	c.VPNSubnet = "10.87.0.0/16"
	c.Member = Member{
		Fleet:  "home",
		Subnet: "10.87.0.0/16",
		Hub:    MemberHub{Endpoint: "hub.example.com:4021", Address: "10.86.0.1", PublicKey: aKey(t)},
	}
	return c
}

func TestAHubNamesItselfAndItsFleetAndHasNoMemberBlock(t *testing.T) {
	c := hubConfig("box")
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !c.IsHub() || c.IsMember() {
		t.Errorf("role = %q, want %q", c.FleetRole(), RoleHub)
	}
	if c.FleetName() != "box" {
		t.Errorf("fleet = %q, want box", c.FleetName())
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
	for _, want := range []string{"name: box\n", "role: hub\n", "hub:\n", "    fleet: box\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("a hub's file has no %q:\n%s", want, b)
		}
	}
	if strings.Contains(string(b), "member:") {
		t.Errorf("a hub wrote a member block:\n%s", b)
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
	if back.Member != c.Member || back.Name != c.Name || back.Role != c.Role {
		t.Fatalf("member = %+v %q %q, want %+v %q %q",
			back.Member, back.Name, back.Role, c.Member, c.Name, c.Role)
	}
	if !back.IsMember() || back.IsHub() {
		t.Errorf("role = %q, want %q", back.FleetRole(), RoleMember)
	}
	if back.FleetName() != "home" {
		t.Errorf("fleet = %q, want home", back.FleetName())
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
	b, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "hub:\n    fleet") {
		t.Errorf("a member wrote a hub block of its own:\n%s", b)
	}
}

func TestValidateRefusesAMachineThatCannotBeTrue(t *testing.T) {
	key := aKey(t)
	hub := MemberHub{Endpoint: "hub.example.com:4021", Address: "10.86.0.1", PublicKey: key}
	cases := map[string]struct {
		change func(*Config)
		want   string
	}{
		"no role at all": {
			change: func(c *Config) { c.Role = "" },
			want:   "role must say what this machine is",
		},
		"the commander's role": {
			change: func(c *Config) { c.Role = RoleCommander },
			want:   "a commander is a person's machine",
		},
		"a role nobody plays": {
			change: func(c *Config) { c.Role = "leader" },
			want:   `role "leader"`,
		},
		"a hub with no fleet": {
			change: func(c *Config) { c.Hub.Fleet = "" },
			want:   "hub.fleet must name the fleet",
		},
		"a hub with a member block": {
			change: func(c *Config) { c.Member = Member{Fleet: "home", Hub: hub} },
			want:   "a member: block on a hub",
		},
		"a member with a hub block": {
			change: func(c *Config) {
				c.Role, c.Member = RoleMember, Member{Fleet: "home", Hub: hub}
			},
			want: "a hub: block on a member",
		},
		"a member with no fleet": {
			change: func(c *Config) {
				c.Role, c.Hub, c.Member = RoleMember, Hub{}, Member{Hub: hub}
			},
			want: "member.fleet must name the fleet",
		},
		"a member with no hub": {
			change: func(c *Config) {
				c.Role, c.Hub, c.Member = RoleMember, Hub{}, Member{Fleet: "home"}
			},
			want: "member.hub must say which machine",
		},
		"a member with no name": {
			change: func(c *Config) {
				c.Name, c.Role, c.Hub, c.Member = "", RoleMember, Hub{}, Member{Fleet: "home", Hub: hub}
			},
			want: "name must say what the hub calls this machine",
		},
		"a hub with no endpoint": {
			change: func(c *Config) {
				c.Role, c.Hub = RoleMember, Hub{}
				c.Member = Member{Fleet: "home", Hub: MemberHub{Address: "10.86.0.1", PublicKey: key}}
			},
			want: "member.hub.endpoint",
		},
		"a hub endpoint with no port": {
			change: func(c *Config) {
				c.Role, c.Hub = RoleMember, Hub{}
				c.Member = Member{Fleet: "home", Hub: MemberHub{
					Endpoint: "hub.example.com", Address: "10.86.0.1", PublicKey: key}}
			},
			want: "want host:port",
		},
		"a hub key that is not a key": {
			change: func(c *Config) {
				c.Role, c.Hub = RoleMember, Hub{}
				c.Member = Member{Fleet: "home", Hub: MemberHub{
					Endpoint: "h:4021", Address: "10.86.0.1", PublicKey: "not-a-key"}}
			},
			want: "member.hub.public_key",
		},
		"a name that is not a slug": {
			change: func(c *Config) { c.Name = "Pi Two" },
			want:   `name "Pi Two"`,
		},
		"a fleet name that is not a slug": {
			change: func(c *Config) { c.Hub.Fleet = "The Home" },
			want:   "a fleet's name is a slug",
		},
		"a range that is not a prefix": {
			change: func(c *Config) { c.Hub.Range = "10.80.0.0" },
			want:   "hub.range",
		},
		"a range too small for a machine": {
			change: func(c *Config) { c.Hub.Range = "10.80.0.0/20" },
			want:   "holds no /16",
		},
		"a subnet that is not a /16": {
			change: func(c *Config) {
				c.Role, c.Hub = RoleMember, Hub{}
				c.VPNSubnet = "10.87.0.0/24"
				c.Member = Member{Fleet: "home", Subnet: "10.87.0.0/24", Hub: hub}
			},
			want: "a machine's range is a /16",
		},
		"a subnet outside the range": {
			change: func(c *Config) {
				c.Role, c.Hub = RoleMember, Hub{}
				c.VPNSubnet = "10.200.0.0/16"
				c.Member = Member{Fleet: "home", Subnet: "10.200.0.0/16", Hub: hub}
			},
			want: "outside the fleet's range",
		},
		"a subnet that disagrees with vpn_subnet": {
			change: func(c *Config) {
				c.Role, c.Hub = RoleMember, Hub{}
				c.VPNSubnet = "10.88.0.0/16"
				c.Member = Member{Fleet: "home", Subnet: "10.87.0.0/16", Hub: hub}
			},
			want: "the same range said twice",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := hubConfig("m1")
			tc.change(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %+v", c)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate said %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestAPrivateMemberIsValid(t *testing.T) {
	c := memberConfig(t)
	c.Member.Private = true
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestTheRetiredFleetBlockIsRefusedByName(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "fleet:\n  role: member\n  name: m1\n")
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a config in the retired shape")
	}
	for _, want := range []string{"fleet:", "name:", "role:", "hub:", "member:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %q", err, want)
		}
	}
}

func TestConfigDirFollowsTheEnvironment(t *testing.T) {
	t.Setenv(ConfigDirEnv, "")
	if got := ConfigDir(); got != DefaultConfigDir {
		t.Errorf("ConfigDir() = %q with nothing set, want %q", got, DefaultConfigDir)
	}
	t.Setenv(ConfigDirEnv, "/opt/caramelo/etc")
	if got := ConfigDir(); got != "/opt/caramelo/etc" {
		t.Errorf("ConfigDir() = %q, want the environment's /opt/caramelo/etc", got)
	}
}
