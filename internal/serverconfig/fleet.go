package serverconfig

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/plytz/caramelo/internal/vpn"
)

const (
	RoleHub = "hub"

	RoleMember = "member"
)

var RoleValues = []string{RoleHub, RoleMember}

const DefaultFleetRange = "10.80.0.0/12"

const FleetSubnetBits = 16

type Fleet struct {
	Role string `yaml:"role,omitempty"`

	Name string `yaml:"name,omitempty"`

	Range string `yaml:"range,omitempty"`

	Subnet string `yaml:"subnet,omitempty"`

	Private bool `yaml:"private,omitempty"`

	Hub FleetHub `yaml:"hub,omitempty"`
}

type FleetHub struct {
	Name string `yaml:"name,omitempty"`

	Endpoint string `yaml:"endpoint,omitempty"`

	Address string `yaml:"address,omitempty"`

	PublicKey string `yaml:"public_key,omitempty"`
}

func (h FleetHub) Empty() bool {
	return h.Name == "" && h.Endpoint == "" && h.Address == "" && h.PublicKey == ""
}

func (c Config) IsMember() bool { return c.FleetRole() == RoleMember }

func (c Config) IsHub() bool { return !c.IsMember() }

func (c Config) FleetRole() string {
	if strings.EqualFold(strings.TrimSpace(c.Fleet.Role), RoleMember) {
		return RoleMember
	}
	return RoleHub
}

func (c Config) FleetRangePrefix() (netip.Prefix, error) {
	s := strings.TrimSpace(c.Fleet.Range)
	if s == "" {
		s = DefaultFleetRange
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("fleet.range %q: %w", s, err)
	}
	return p.Masked(), nil
}

func (c Config) FleetSubnetPrefix() (netip.Prefix, error) {
	if s := strings.TrimSpace(c.Fleet.Subnet); s != "" {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("fleet.subnet %q: %w", s, err)
		}
		return p.Masked(), nil
	}
	return c.VPNSubnetPrefix()
}

func (c Config) HubAddress() (netip.Addr, error) {
	if !c.IsMember() {
		return netip.Addr{}, fmt.Errorf("this machine is a %s and has no hub", RoleHub)
	}
	s := strings.TrimSpace(c.Fleet.Hub.Address)
	if s == "" {
		return netip.Addr{}, fmt.Errorf("fleet.hub.address is empty: this member does not know where its hub answers inside the tunnel")
	}
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("fleet.hub.address %q: %w", s, err)
	}
	return ip, nil
}

func (c Config) HubSubnetPrefix() (netip.Prefix, error) {
	ip, err := c.HubAddress()
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(ip, FleetSubnetBits).Masked(), nil
}

func (c Config) HubAPIAddrPort() (netip.AddrPort, error) {
	ip, err := c.HubAddress()
	if err != nil {
		return netip.AddrPort{}, err
	}
	if c.SSHPort < 1 || c.SSHPort > 65535 {
		return netip.AddrPort{}, fmt.Errorf("ssh_port %d out of range", c.SSHPort)
	}
	return netip.AddrPortFrom(ip, uint16(c.SSHPort)), nil
}

func (c Config) HubResolverAddrPort() (netip.AddrPort, error) {
	ip, err := c.HubAddress()
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(ip, vpn.ResolverPort), nil
}

func validateFleet(c Config) []error {
	var errs []error
	f := c.Fleet
	if r := strings.TrimSpace(f.Role); r != "" && !slices.Contains(RoleValues, strings.ToLower(r)) {
		errs = append(errs, fmt.Errorf("fleet.role %q: want one of %s", r, strings.Join(RoleValues, ", ")))
	}
	if s := strings.TrimSpace(f.Range); s != "" {
		p, err := netip.ParsePrefix(s)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("fleet.range %q: %w", s, err))
		case !p.Addr().Is4():
			errs = append(errs, fmt.Errorf("fleet.range %q: must be IPv4 (IPv6 inside the tunnel is not in M8)", s))
		case p.Bits() > FleetSubnetBits:
			errs = append(errs, fmt.Errorf("fleet.range %q: /%d holds no /%d for a machine",
				s, p.Bits(), FleetSubnetBits))
		}
	}
	if n := strings.TrimSpace(f.Name); n != "" && !isSlug(n) {
		errs = append(errs, fmt.Errorf(
			"fleet.name %q: a machine's name is a slug — lowercase letters, digits and dashes", n))
	}
	errs = append(errs, validateFleetSubnet(c)...)
	member := c.FleetRole() == RoleMember
	if member && strings.TrimSpace(f.Name) == "" {
		errs = append(errs, fmt.Errorf("fleet.role %s: fleet.name must say what the hub calls this machine", RoleMember))
	}
	switch {
	case member && f.Hub.Empty():
		errs = append(errs, fmt.Errorf("fleet.role %s: fleet.hub must say which machine this one joined", RoleMember))
	case !member && !f.Hub.Empty():
		errs = append(errs, fmt.Errorf("fleet.hub is set on a %s: only a %s has one", RoleHub, RoleMember))
	case !member && f.Private && strings.TrimSpace(f.Role) != "":
		errs = append(errs, fmt.Errorf("fleet.private is set on a %s: only a %s is fronted by another machine", RoleHub, RoleMember))
	}
	if member {
		errs = append(errs, validateFleetHub(f.Hub)...)
	}
	return errs
}

func validateFleetSubnet(c Config) []error {
	s := strings.TrimSpace(c.Fleet.Subnet)
	if s == "" {
		return nil
	}
	p, err := netip.ParsePrefix(s)
	switch {
	case err != nil:
		return []error{fmt.Errorf("fleet.subnet %q: %w", s, err)}
	case !p.Addr().Is4():
		return []error{fmt.Errorf("fleet.subnet %q: must be IPv4 (IPv6 inside the tunnel is not in M8)", s)}
	case p.Bits() != FleetSubnetBits:
		return []error{fmt.Errorf("fleet.subnet %q: a machine's range is a /%d", s, FleetSubnetBits)}
	}
	var errs []error
	if r, err := c.FleetRangePrefix(); err == nil && !r.Contains(p.Masked().Addr()) {
		errs = append(errs, fmt.Errorf("fleet.subnet %q is outside the fleet's range %s", s, r))
	}
	if v, err := netip.ParsePrefix(strings.TrimSpace(c.VPNSubnet)); err == nil && v.Masked() != p.Masked() {
		errs = append(errs, fmt.Errorf("fleet.subnet %q and vpn_subnet %q are the same range said twice and they disagree",
			s, c.VPNSubnet))
	}
	return errs
}

func validateFleetHub(h FleetHub) []error {
	var errs []error
	if strings.TrimSpace(h.Name) == "" {
		errs = append(errs, fmt.Errorf("fleet.hub.name must not be empty"))
	}
	if e := strings.TrimSpace(h.Endpoint); e == "" {
		errs = append(errs, fmt.Errorf("fleet.hub.endpoint must not be empty: a member dials, so it needs somewhere to dial"))
	} else if host, port, err := net.SplitHostPort(e); err != nil {
		errs = append(errs, fmt.Errorf("fleet.hub.endpoint %q: want host:port: %w", e, err))
	} else if host == "" {
		errs = append(errs, fmt.Errorf("fleet.hub.endpoint %q: no host", e))
	} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		errs = append(errs, fmt.Errorf("fleet.hub.endpoint %q: port %q out of range", e, port))
	}
	if a := strings.TrimSpace(h.Address); a == "" {
		errs = append(errs, fmt.Errorf("fleet.hub.address must not be empty: it is where the hub answers inside the tunnel"))
	} else if ip, err := netip.ParseAddr(a); err != nil {
		errs = append(errs, fmt.Errorf("fleet.hub.address %q: %w", a, err))
	} else if !ip.Is4() {
		errs = append(errs, fmt.Errorf("fleet.hub.address %q: must be IPv4 (IPv6 inside the tunnel is not in M8)", a))
	}
	if k := strings.TrimSpace(h.PublicKey); k == "" {
		errs = append(errs, fmt.Errorf("fleet.hub.public_key must not be empty: it is what proves the hub is the one that was joined"))
	} else if key, err := vpn.ParseKey(k); err != nil {
		errs = append(errs, fmt.Errorf("fleet.hub.public_key: %w", err))
	} else if key.IsZero() {
		errs = append(errs, fmt.Errorf("fleet.hub.public_key is empty"))
	}
	return errs
}

func isSlug(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}
