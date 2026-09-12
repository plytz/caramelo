package vpn

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

type fakeLedger struct {
	taken []string
	err   error
	calls int
}

func (l *fakeLedger) TakenVPNIPs(ctx context.Context) ([]string, error) {
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	return append([]string(nil), l.taken...), nil
}

func mustSubnet(t *testing.T, cidr string) netip.Prefix {
	t.Helper()
	p, err := Subnet(cidr)
	if err != nil {
		t.Fatalf("Subnet(%q): %v", cidr, err)
	}
	return p
}

func TestSubnet(t *testing.T) {
	p := mustSubnet(t, DefaultSubnet)
	if p.String() != "10.86.0.0/16" {
		t.Fatalf("subnet = %s", p)
	}

	if got := mustSubnet(t, "10.86.4.7/16"); got != p {
		t.Errorf("10.86.4.7/16 = %s, want %s", got, p)
	}
	for name, cidr := range map[string]string{
		"nonsense":  "not a subnet",
		"bare addr": "10.86.0.0",
		"ipv6":      "fd00::/64",
		"too small": "10.86.0.0/24",
	} {
		if _, err := Subnet(cidr); err == nil {
			t.Errorf("Subnet(%s = %q) was accepted", name, cidr)
		}
	}

	if _, err := Subnet("10.86.0.0/23"); err != nil {
		t.Errorf("Subnet(/23): %v", err)
	}
}

func TestLayoutIsWhatEveryDocQuotes(t *testing.T) {
	p := mustSubnet(t, DefaultSubnet)
	if got := MachineIP(p).String(); got != "10.86.0.1" {
		t.Errorf("machine = %s, want 10.86.0.1", got)
	}
	first, last := PeerRange(p)
	if first.String() != "10.86.0.2" || last.String() != "10.86.0.254" {
		t.Errorf("peer range = %s-%s, want 10.86.0.2-10.86.0.254", first, last)
	}
	efirst, elast := EnvRange(p)
	if efirst.String() != "10.86.1.1" || elast.String() != "10.86.255.254" {
		t.Errorf("env range = %s-%s, want 10.86.1.1-10.86.255.254", efirst, elast)
	}
}

func TestAllocatePeerTakesTheLowestFreeAddress(t *testing.T) {
	ctx := context.Background()
	l := &fakeLedger{}
	a := NewAllocator(mustSubnet(t, DefaultSubnet), l)

	ip, err := a.AllocatePeer(ctx, "laptop")
	if err != nil {
		t.Fatalf("AllocatePeer: %v", err)
	}
	if ip.String() != "10.86.0.2" {
		t.Fatalf("first peer = %s, want 10.86.0.2", ip)
	}

	again, err := a.AllocatePeer(ctx, "laptop")
	if err != nil {
		t.Fatalf("AllocatePeer: %v", err)
	}
	if again != ip {
		t.Fatalf("second call = %s, want the same %s", again, ip)
	}

	l.taken = []string{"10.86.0.2", "10.86.0.3", "10.86.0.5"}
	ip, err = a.AllocatePeer(ctx, "agent-7")
	if err != nil {
		t.Fatalf("AllocatePeer: %v", err)
	}
	if ip.String() != "10.86.0.4" {
		t.Fatalf("next peer = %s, want the gap at 10.86.0.4", ip)
	}
}

func TestAllocateEnvUsesItsOwnRange(t *testing.T) {
	ctx := context.Background()
	l := &fakeLedger{taken: []string{"10.86.0.2"}}
	a := NewAllocator(mustSubnet(t, DefaultSubnet), l)

	ip, err := a.AllocateEnv(ctx, 1)
	if err != nil {
		t.Fatalf("AllocateEnv: %v", err)
	}
	if ip.String() != "10.86.1.1" {
		t.Fatalf("first env = %s, want 10.86.1.1", ip)
	}

	l.taken = []string{"10.86.1.1", "10.86.1.2", "10.86.1.3"}
	if ip, err = a.AllocateEnv(ctx, 2); err != nil || ip.String() != "10.86.1.4" {
		t.Fatalf("AllocateEnv = %s, %v; want 10.86.1.4", ip, err)
	}
	l.taken = []string{"10.86.1.1", "10.86.1.3"}
	if ip, err = a.AllocateEnv(ctx, 3); err != nil || ip.String() != "10.86.1.2" {
		t.Fatalf("after a destroy AllocateEnv = %s, %v; want the freed 10.86.1.2", ip, err)
	}
}

func TestAllocatorNeverHandsOutTheMachinesOwnAddress(t *testing.T) {

	ctx := context.Background()
	a := NewAllocator(mustSubnet(t, "10.86.0.0/23"), &fakeLedger{})
	ip, err := a.AllocatePeer(ctx, "laptop")
	if err != nil {
		t.Fatalf("AllocatePeer: %v", err)
	}
	if ip == MachineIP(a.Subnet) {
		t.Fatalf("allocated the machine's own address %s", ip)
	}
}

func TestAllocatorExhaustion(t *testing.T) {
	ctx := context.Background()
	l := &fakeLedger{}
	sub := mustSubnet(t, "10.86.0.0/23")
	for i := 2; i <= 254; i++ {
		l.taken = append(l.taken, fmt.Sprintf("10.86.0.%d", i))
	}
	a := NewAllocator(sub, l)
	if _, err := a.AllocatePeer(ctx, "one-too-many"); !errors.Is(err, ErrExhausted) {
		t.Fatalf("AllocatePeer error = %v, want ErrExhausted", err)
	}

	l.taken = nil
	for i := 1; i <= 254; i++ {
		l.taken = append(l.taken, fmt.Sprintf("10.86.1.%d", i))
	}
	if _, err := a.AllocateEnv(ctx, 1); !errors.Is(err, ErrExhausted) {
		t.Fatalf("AllocateEnv error = %v, want ErrExhausted", err)
	}
}

func TestAllocatorReportsLedgerTrouble(t *testing.T) {
	ctx := context.Background()
	a := NewAllocator(mustSubnet(t, DefaultSubnet), &fakeLedger{err: errors.New("database is locked")})
	_, err := a.AllocatePeer(ctx, "laptop")
	if err == nil {
		t.Fatal("a broken ledger produced an address")
	}
	if !strings.Contains(err.Error(), "database is locked") {
		t.Errorf("error %q does not carry the cause", err)
	}
	a = NewAllocator(mustSubnet(t, DefaultSubnet), &fakeLedger{taken: []string{"not an address"}})
	if _, err := a.AllocatePeer(ctx, "laptop"); err == nil {
		t.Fatal("a ledger entry that is not an address was accepted")
	}
	a = NewAllocator(mustSubnet(t, DefaultSubnet), nil)
	if _, err := a.AllocatePeer(ctx, "laptop"); err == nil {
		t.Fatal("an allocator with no ledger produced an address")
	}
}

func TestAllocatorIgnoresEmptyLedgerEntries(t *testing.T) {

	ctx := context.Background()
	a := NewAllocator(mustSubnet(t, DefaultSubnet), &fakeLedger{taken: []string{"", "10.86.1.1"}})
	ip, err := a.AllocateEnv(ctx, 1)
	if err != nil || ip.String() != "10.86.1.2" {
		t.Fatalf("AllocateEnv = %s, %v; want 10.86.1.2", ip, err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	ctx := context.Background()
	a := NewAllocator(mustSubnet(t, DefaultSubnet), &fakeLedger{})
	ip := netip.MustParseAddr("10.86.1.1")
	for i := 0; i < 3; i++ {
		if err := a.Release(ctx, ip); err != nil {
			t.Fatalf("Release %d: %v", i, err)
		}
	}
}
