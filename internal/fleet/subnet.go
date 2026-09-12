package fleet

import (
	"fmt"
	"net/netip"
	"sort"
)

const (
	FleetRange = "10.80.0.0/12"

	HubSubnet = "10.86.0.0/16"

	SubnetBits = 16
)

var ErrNoSubnet = fmt.Errorf("the fleet's range %s has no free /%d left", FleetRange, SubnetBits)

func Range() netip.Prefix { return netip.MustParsePrefix(FleetRange) }

func Hub() netip.Prefix { return netip.MustParsePrefix(HubSubnet) }

func Subnets() []netip.Prefix {
	r := Range()
	base := r.Masked().Addr().As4()
	count := 1 << (SubnetBits - r.Bits())
	out := make([]netip.Prefix, 0, count)
	for i := 0; i < count; i++ {
		b := base
		b[1] += byte(i)
		out = append(out, netip.PrefixFrom(netip.AddrFrom4(b), SubnetBits))
	}
	return out
}

func Contains(p netip.Prefix) bool {
	if !p.IsValid() || p.Bits() != SubnetBits || !p.Addr().Is4() {
		return false
	}
	return Range().Overlaps(p) && Range().Contains(p.Masked().Addr())
}

func AllocateSubnet(taken []netip.Prefix) (netip.Prefix, error) {
	used := make(map[netip.Prefix]bool, len(taken))
	for _, p := range taken {
		if !p.IsValid() {
			continue
		}
		used[p.Masked()] = true
	}
	for _, p := range Subnets() {
		if !used[p] {
			return p, nil
		}
	}
	return netip.Prefix{}, ErrNoSubnet
}

func TakenSubnets(machines []Machine) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(machines))
	for _, m := range machines {
		if m.Subnet.IsValid() {
			out = append(out, m.Subnet.Masked())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr().Less(out[j].Addr()) })
	return out
}

func ParseSubnet(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("subnet %q: %w", s, err)
	}
	p = p.Masked()
	if !Contains(p) {
		return netip.Prefix{}, fmt.Errorf("subnet %q is not a /%d of the fleet's range %s",
			s, SubnetBits, FleetRange)
	}
	return p, nil
}
