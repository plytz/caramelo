package fleet

import (
	"errors"
	"net/netip"
	"testing"
	"time"
)

func TestAllocateSubnet(t *testing.T) {
	hub := Hub()
	if hub.String() != "10.86.0.0/16" {
		t.Fatalf("the hub keeps %s, want the subnet a machine of one already had", hub)
	}
	if got := len(Subnets()); got != 16 {
		t.Fatalf("%d /16s in %s, want 16", got, FleetRange)
	}

	taken := []netip.Prefix{hub}
	first, err := AllocateSubnet(taken)
	if err != nil {
		t.Fatal(err)
	}
	if first.String() != "10.80.0.0/16" {
		t.Errorf("first member got %s, want the lowest free /16", first)
	}
	taken = append(taken, first)
	second, err := AllocateSubnet(taken)
	if err != nil || second.String() != "10.81.0.0/16" {
		t.Errorf("second member got %s (%v)", second, err)
	}

	again, err := AllocateSubnet([]netip.Prefix{hub, second})
	if err != nil || again != first {
		t.Errorf("after a machine left, allocation gave %s, want %s back", again, first)
	}
}

func TestAllocateSubnetRunsOut(t *testing.T) {
	if _, err := AllocateSubnet(Subnets()); !errors.Is(err, ErrNoSubnet) {
		t.Errorf("a full range gave %v, want ErrNoSubnet", err)
	}
}

func TestAllocateIgnoresForeignPrefixes(t *testing.T) {
	got, err := AllocateSubnet([]netip.Prefix{
		netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("10.80.0.0/24"), {},
	})
	if err != nil || got.String() != "10.80.0.0/16" {
		t.Errorf("AllocateSubnet = %s, %v", got, err)
	}
}

func TestContainsAndParseSubnet(t *testing.T) {
	for _, s := range []string{"10.80.0.0/16", "10.86.0.0/16", "10.95.0.0/16"} {
		if p := netip.MustParsePrefix(s); !Contains(p) {
			t.Errorf("%s is one of the fleet's /16s", s)
		}
		if _, err := ParseSubnet(s); err != nil {
			t.Errorf("ParseSubnet(%q): %v", s, err)
		}
	}
	for _, s := range []string{"10.96.0.0/16", "10.79.0.0/16", "10.86.0.0/24", "10.86.0.0/12"} {
		if p := netip.MustParsePrefix(s); Contains(p) {
			t.Errorf("%s is not one of the fleet's /16s", s)
		}
		if _, err := ParseSubnet(s); err == nil {
			t.Errorf("ParseSubnet(%q) accepted a subnet that is not the fleet's", s)
		}
	}

	got, err := ParseSubnet("10.87.4.7/16")
	if err != nil || got.String() != "10.87.0.0/16" {
		t.Errorf("ParseSubnet of an unmasked prefix = %s, %v", got, err)
	}
}

func TestMachineAddress(t *testing.T) {
	m := Machine{Subnet: netip.MustParsePrefix("10.87.0.0/16")}
	if got := m.Address(); got.String() != "10.87.0.1" {
		t.Errorf("Address() = %s, want 10.87.0.1", got)
	}
	if (Machine{}).Address().IsValid() {
		t.Error("a machine with no subnet has no address")
	}
}

func TestMachineReachable(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	hub := Machine{Name: "hub", Role: RoleHub}
	if !hub.Reachable(now) {
		t.Error("the hub is the machine asking; it is always reachable")
	}
	fresh := Machine{Name: "m1", Role: RoleMember, LastSeen: now.Add(-30 * time.Second)}
	if !fresh.Reachable(now) {
		t.Error("a member seen 30s ago is reachable")
	}
	gone := Machine{Name: "m1", Role: RoleMember, LastSeen: now.Add(-UnreachableAfter - time.Second)}
	if gone.Reachable(now) {
		t.Error("a member past the window is unreachable")
	}
	if (Machine{Name: "m1", Role: RoleMember}).Reachable(now) {
		t.Error("a member never heard from is not reachable")
	}
}

func TestParseRole(t *testing.T) {
	for in, want := range map[string]Role{"": RoleHub, "hub": RoleHub, "MEMBER": RoleMember} {
		got, err := ParseRole(in)
		if err != nil || got != want {
			t.Errorf("ParseRole(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseRole("leader"); err == nil {
		t.Error("an invented role was accepted")
	}
	if !Role("").IsHub() || !RoleHub.IsHub() || RoleMember.IsHub() {
		t.Error("a machine that was never told about a fleet is a hub of one")
	}
}

func TestToken(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	tok, err := NewToken("s3cret", "laptop", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Hash == "s3cret" || tok.Hash != Hash(" s3cret ") {
		t.Errorf("a token is stored as its hash, got %q", tok.Hash)
	}
	if tok.ExpiresAt != now.Add(TokenTTL) {
		t.Errorf("expiry = %v, want the default TTL", tok.ExpiresAt)
	}
	if err := tok.Check(now); err != nil {
		t.Errorf("a fresh token: %v", err)
	}
	if err := tok.Check(now.Add(TokenTTL)); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("at its expiry: %v, want ErrTokenExpired", err)
	}
	used := tok
	used.UsedBy = "nx2"
	used.UsedAt = now

	if err := used.Check(now.Add(2 * TokenTTL)); !errors.Is(err, ErrTokenUsed) {
		t.Errorf("a used, expired token: %v, want ErrTokenUsed", err)
	}
	if err := (Token{}).Check(now); !errors.Is(err, ErrTokenUnknown) {
		t.Errorf("no row: %v, want ErrTokenUnknown", err)
	}
	if _, err := NewToken("s", "", now, MaxTokenTTL+time.Hour); err == nil {
		t.Error("a token that outlives the day was accepted")
	}
	if _, err := NewToken("  ", "", now, 0); err == nil {
		t.Error("a token with no secret was accepted")
	}
}

func TestCandidateFree(t *testing.T) {
	if _, ok := (Candidate{}).Free(); ok {
		t.Error("a machine that never announced a gauge cannot be shown to fit")
	}
	c := Candidate{Committed: 200 << 20}
	c.Gauge = gauge(2<<30, 256<<20)
	got, ok := c.Free()
	if !ok || got != 2<<30-(256<<20)-(200<<20) {
		t.Errorf("Free() = %d, %v", got, ok)
	}

	c.Committed = 8 << 30
	if got, ok := c.Free(); !ok || got != 0 {
		t.Errorf("an over-committed machine has %d bytes free", got)
	}
}
