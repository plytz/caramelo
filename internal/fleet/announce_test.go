package fleet

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

func machineAt(name string, role Role, subnet string, lastSeen time.Time) Machine {
	return Machine{
		Name: name, Role: role, PublicKey: name + "-key",
		Subnet: netip.MustParsePrefix(subnet), LastSeen: lastSeen,
	}
}

func TestSeenFoldsHandshakesIn(t *testing.T) {
	old := testNow.Add(-time.Minute)
	machines := []Machine{
		machineAt("hub", RoleHub, HubSubnet, testNow),
		machineAt("m1", RoleMember, "10.87.0.0/16", old),
		machineAt("m2", RoleMember, "10.88.0.0/16", old),
		machineAt("m3", RoleMember, "10.89.0.0/16", time.Time{}),
	}
	got := Seen(machines, map[string]time.Time{
		"m1-key": testNow,
		"m2-key": testNow.Add(-time.Hour),
		"m3-key": testNow.Add(-time.Second),
		"gone":   testNow,
	})
	if !got[1].LastSeen.Equal(testNow) {
		t.Errorf("m1 last seen %v, want %v", got[1].LastSeen, testNow)
	}
	if !got[2].LastSeen.Equal(old) {
		t.Errorf("m2 last seen %v, want the newer %v it already had", got[2].LastSeen, old)
	}
	if got[3].LastSeen.IsZero() {
		t.Error("m3 never got its first handshake")
	}

	if !machines[1].LastSeen.Equal(old) {
		t.Error("Seen wrote through to its argument")
	}
}

func TestUnreachableAndFindAndHub(t *testing.T) {
	machines := []Machine{
		machineAt("hub", RoleHub, HubSubnet, time.Time{}),
		machineAt("m2", RoleMember, "10.88.0.0/16", testNow.Add(-2*UnreachableAfter)),
		machineAt("m1", RoleMember, "10.87.0.0/16", testNow),
	}
	gone := Unreachable(machines, testNow)
	if len(gone) != 1 || gone[0].Name != "m2" {
		t.Fatalf("unreachable = %+v, want just m2 (the hub is the machine asking)", gone)
	}
	if m, ok := Find(machines, "m1"); !ok || m.Name != "m1" {
		t.Errorf("Find(m1) = %+v, %v", m, ok)
	}
	if _, ok := Find(machines, "nx9"); ok {
		t.Error("Find invented a machine")
	}
	if h, ok := HubOf(machines); !ok || h.Name != "hub" {
		t.Errorf("HubOf = %+v, %v", h, ok)
	}
}

func entry(app, env, machine string) DirectoryEntry {
	return DirectoryEntry{App: app, Env: env, Machine: machine, Address: "10.87.1.1", Owner: "agent-a", Mode: "dev"}
}

func TestReconcileWithAFullAnnouncement(t *testing.T) {
	existing := []DirectoryEntry{
		entry("shop", "feat-x", "m1"),
		entry("shop", "feat-old", "m1"),
		entry("shop", "feat-y", "m2"),
	}
	up, rm, err := Reconcile(existing, Announcement{
		Machine: "m1", Full: true, At: testNow,
		Envs: []DirectoryEntry{entry("shop", "feat-x", ""), entry("shop", "feat-z", "")},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(up) != 2 || up[0].Env != "feat-x" || up[1].Env != "feat-z" {
		t.Fatalf("upsert = %+v, want feat-x and feat-z", up)
	}
	for _, e := range up {
		if e.Machine != "m1" || !e.UpdatedAt.Equal(testNow) {
			t.Errorf("upsert %+v, want it stamped with the announcing machine and time", e)
		}
	}
	if len(rm) != 1 || rm[0].Env != "feat-old" {
		t.Fatalf("remove = %+v, want just feat-old (m2's row is not m1's business)", rm)
	}
}

func TestReconcileWithAPartialAnnouncement(t *testing.T) {
	existing := []DirectoryEntry{entry("shop", "feat-x", "m1"), entry("shop", "feat-old", "m1")}
	up, rm, err := Reconcile(existing, Announcement{
		Machine: "m1", At: testNow, Envs: []DirectoryEntry{entry("shop", "feat-z", "")},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(up) != 1 || len(rm) != 0 {
		t.Fatalf("upsert = %+v, remove = %+v, want one added and nothing forgotten", up, rm)
	}
}

func TestReconcileRebuildsAnEmptyDirectory(t *testing.T) {
	up, rm, err := Reconcile(nil, Announcement{
		Machine: "m1", Full: true, At: testNow,
		Envs: []DirectoryEntry{entry("shop", "feat-x", ""), entry("shop", "production", "")},
	})
	if err != nil || len(up) != 2 || len(rm) != 0 {
		t.Fatalf("Reconcile = %+v, %+v, %v", up, rm, err)
	}
}

func TestAnnouncementValidate(t *testing.T) {
	cases := map[string]struct {
		a    Announcement
		want string
	}{
		"no machine": {Announcement{Envs: []DirectoryEntry{entry("shop", "x", "")}}, "which machine"},
		"no env name": {
			Announcement{Machine: "m1", Envs: []DirectoryEntry{{App: "shop"}}}, "no name",
		},
		"somebody else's environment": {
			Announcement{Machine: "m1", Envs: []DirectoryEntry{entry("shop", "x", "m2")}}, "belonging to m2",
		},
		"the same environment twice": {
			Announcement{Machine: "m1", Envs: []DirectoryEntry{entry("shop", "x", ""), entry("shop", "x", "")}}, "twice",
		},
		"an address that is not one": {
			Announcement{Machine: "m1", Envs: []DirectoryEntry{{App: "shop", Env: "x", Address: "over there"}}}, "over there",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.a.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %+v", tc.a)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate said %q, want it to mention %q", err, tc.want)
			}
			if _, _, err := Reconcile(nil, tc.a); err == nil {
				t.Fatal("Reconcile acted on an announcement Validate refuses")
			}
		})
	}
}

func TestMachineValidate(t *testing.T) {
	good := machineAt("m1", RoleMember, "10.87.0.0/16", testNow)
	if err := good.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	cases := map[string]struct {
		m    Machine
		want string
	}{
		"no name":    {Machine{PublicKey: "k", Subnet: netip.MustParsePrefix("10.87.0.0/16")}, "needs a name"},
		"no key":     {Machine{Name: "m1", Subnet: netip.MustParsePrefix("10.87.0.0/16")}, "is its key"},
		"no subnet":  {Machine{Name: "m1", PublicKey: "k"}, "no subnet"},
		"bad subnet": {Machine{Name: "m1", PublicKey: "k", Subnet: netip.MustParsePrefix("192.168.0.0/16")}, "fleet's range"},
		"bad role": {
			Machine{Name: "m1", PublicKey: "k", Role: "leader", Subnet: netip.MustParsePrefix("10.87.0.0/16")},
			"unknown fleet role",
		},
		"a private hub": {
			Machine{Name: "hub", PublicKey: "k", Role: RoleHub, Private: true, Subnet: netip.MustParsePrefix(HubSubnet)},
			"cannot be private",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.m.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %+v", tc.m)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate said %q, want it to mention %q", err, tc.want)
			}
		})
	}
}
