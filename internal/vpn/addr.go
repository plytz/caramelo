package vpn

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
)

const (
	machineOffset  = 1
	peerFirstOff   = 2
	peerLastOff    = 254
	envFirstOffset = 257
)

func Subnet(cidr string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("vpn: parse subnet %q: %w", cidr, err)
	}
	p = p.Masked()
	if !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("vpn: subnet %s: must be IPv4", cidr)
	}
	if p.Bits() > 23 {
		return netip.Prefix{}, fmt.Errorf(
			"vpn: subnet %s: too small, it must be a /23 or larger to hold the machine, its peers and one address per environment", cidr)
	}
	return p, nil
}

func MachineIP(subnet netip.Prefix) netip.Addr { return offset(subnet, machineOffset) }

func PeerRange(subnet netip.Prefix) (first, last netip.Addr) {
	return offset(subnet, peerFirstOff), offset(subnet, peerLastOff)
}

func EnvRange(subnet netip.Prefix) (first, last netip.Addr) {
	return offset(subnet, envFirstOffset), offset(subnet, size(subnet)-2)
}

func offset(subnet netip.Prefix, n uint32) netip.Addr {
	b := subnet.Addr().As4()
	base := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v := base + n
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

func size(subnet netip.Prefix) uint32 { return 1 << uint(32-subnet.Bits()) }

type Ledger interface {
	TakenVPNIPs(ctx context.Context) ([]string, error)
}

type RangeAllocator struct {
	Subnet netip.Prefix

	Ledger Ledger
}

var _ Allocator = (*RangeAllocator)(nil)

func NewAllocator(subnet netip.Prefix, ledger Ledger) *RangeAllocator {
	return &RangeAllocator{Subnet: subnet, Ledger: ledger}
}

func (a *RangeAllocator) AllocatePeer(ctx context.Context, name string) (netip.Addr, error) {
	first, last := PeerRange(a.Subnet)
	return a.free(ctx, first, last)
}

func (a *RangeAllocator) AllocateEnv(ctx context.Context, envID int64) (netip.Addr, error) {
	first, last := EnvRange(a.Subnet)
	return a.free(ctx, first, last)
}

func (a *RangeAllocator) Release(ctx context.Context, ip netip.Addr) error { return nil }

func (a *RangeAllocator) free(ctx context.Context, first, last netip.Addr) (netip.Addr, error) {
	taken, err := a.taken(ctx)
	if err != nil {
		return netip.Addr{}, err
	}
	for ip := first; ip.Compare(last) <= 0; ip = ip.Next() {
		if !taken[ip] {
			return ip, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("vpn: between %s and %s: %w", first, last, ErrExhausted)
}

func (a *RangeAllocator) taken(ctx context.Context) (map[netip.Addr]bool, error) {
	if a.Ledger == nil {
		return nil, fmt.Errorf("vpn: allocator has no ledger")
	}
	list, err := a.Ledger.TakenVPNIPs(ctx)
	if err != nil {
		return nil, fmt.Errorf("vpn: read allocated addresses: %w", err)
	}
	out := make(map[netip.Addr]bool, len(list)+1)

	out[MachineIP(a.Subnet)] = true
	for _, s := range list {
		if s == "" {
			continue
		}
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("vpn: allocated address %q is not an address: %w", s, err)
		}
		out[ip] = true
	}
	return out, nil
}

func sortAddresses(addrs []Address) {
	sort.Slice(addrs, func(i, j int) bool { return addrs[i].IP.Compare(addrs[j].IP) < 0 })
}
