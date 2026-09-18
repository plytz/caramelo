package serverconfig

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/plytz/caramelo/internal/vpn"
)

const (
	RoleHub = "hub"

	RoleMember = "member"

	RoleCommander = "commander"
)

var RoleValues = []string{RoleHub, RoleMember}

const DefaultFleetRange = "10.80.0.0/12"

const FleetSubnetBits = 16

type Hub struct {
	Fleet string `yaml:"fleet,omitempty"`

	Range string `yaml:"range,omitempty"`
}

func (h Hub) Empty() bool { return h.Fleet == "" && h.Range == "" }

type Member struct {
	Fleet string `yaml:"fleet,omitempty"`

	Subnet string `yaml:"subnet,omitempty"`

	Private bool `yaml:"private,omitempty"`

	Hub MemberHub `yaml:"hub,omitempty"`
}

func (m Member) Empty() bool {
	return m.Fleet == "" && m.Subnet == "" && !m.Private && m.Hub.Empty()
}

type MemberHub struct {
	Name string `yaml:"name,omitempty"`

	Endpoint string `yaml:"endpoint,omitempty"`

	Address string `yaml:"address,omitempty"`

	PublicKey string `yaml:"public_key,omitempty"`
}

func (h MemberHub) Empty() bool {
	return h.Name == "" && h.Endpoint == "" && h.Address == "" && h.PublicKey == ""
}

func (c Config) IsMember() bool { return c.FleetRole() == RoleMember }

func (c Config) IsHub() bool { return !c.IsMember() }

func (c Config) FleetRole() string {
	if strings.EqualFold(strings.TrimSpace(c.Role), RoleMember) {
		return RoleMember
	}
	return RoleHub
}

func (c Config) FleetName() string {
	if c.IsMember() {
		return strings.TrimSpace(c.Member.Fleet)
	}
	if f := strings.TrimSpace(c.Hub.Fleet); f != "" {
		return f
	}
	return strings.TrimSpace(c.Name)
}

func (c Config) HubName() string {
	if n := strings.TrimSpace(c.Member.Hub.Name); n != "" {
		return n
	}
	return c.FleetName()
}

func (c Config) FleetRangePrefix() (netip.Prefix, error) {
	s := strings.TrimSpace(c.Hub.Range)
	if s == "" {
		s = DefaultFleetRange
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("hub.range %q: %w", s, err)
	}
	return p.Masked(), nil
}

func (c Config) FleetSubnetPrefix() (netip.Prefix, error) {
	if s := strings.TrimSpace(c.Member.Subnet); s != "" {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("member.subnet %q: %w", s, err)
		}
		return p.Masked(), nil
	}
	return c.VPNSubnetPrefix()
}

func (c Config) HubAddress() (netip.Addr, error) {
	if !c.IsMember() {
		return netip.Addr{}, fmt.Errorf("this machine is a %s and has no hub", RoleHub)
	}
	s := strings.TrimSpace(c.Member.Hub.Address)
	if s == "" {
		return netip.Addr{}, fmt.Errorf("member.hub.address is empty: this member does not know where its hub answers inside the tunnel")
	}
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("member.hub.address %q: %w", s, err)
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

const retiredFleetBlock = "the fleet: block is retired: a machine says name: and role: at the top, " +
	"a hub its fleet under hub: (fleet, range), a member its own under member: (fleet, subnet, private, hub)"

func refuseRetiredKeys(b []byte) error {
	var probe struct {
		Fleet yaml.Node `yaml:"fleet"`
	}
	if err := yaml.Unmarshal(b, &probe); err != nil {
		return nil
	}
	if probe.Fleet.IsZero() {
		return nil
	}
	return fmt.Errorf("%s", retiredFleetBlock)
}

func validateRoles(c Config) []error {
	var errs []error
	role := strings.ToLower(strings.TrimSpace(c.Role))
	switch {
	case role == "":
		errs = append(errs, fmt.Errorf("role must say what this machine is: one of %s",
			strings.Join(RoleValues, ", ")))
	case role == RoleCommander:
		errs = append(errs, fmt.Errorf(
			"role %s: a commander is a person's machine and its config is the user's, not a machine's: want one of %s",
			RoleCommander, strings.Join(RoleValues, ", ")))
	case !slices.Contains(RoleValues, role):
		errs = append(errs, fmt.Errorf("role %q: want one of %s", c.Role, strings.Join(RoleValues, ", ")))
	}
	if n := strings.TrimSpace(c.Name); n != "" && !isSlug(n) {
		errs = append(errs, fmt.Errorf(
			"name %q: a machine's name is a slug — lowercase letters, digits and dashes", n))
	}
	switch role {
	case RoleHub:
		errs = append(errs, validateHubBlock(c)...)
	case RoleMember:
		errs = append(errs, validateMemberBlock(c)...)
	}
	return errs
}

func validateHubBlock(c Config) []error {
	var errs []error
	if !c.Member.Empty() {
		errs = append(errs, fmt.Errorf(
			"a member: block on a %s: only a %s joined somebody, and only `caramelo member join` writes it",
			RoleHub, RoleMember))
	}
	switch f := strings.TrimSpace(c.Hub.Fleet); {
	case f == "":
		errs = append(errs, fmt.Errorf(
			"hub.fleet must name the fleet this machine hubs: `caramelo hub setup --fleet NAME` sets it"))
	case !isSlug(f):
		errs = append(errs, fmt.Errorf(
			"hub.fleet %q: a fleet's name is a slug — lowercase letters, digits and dashes", f))
	}
	if s := strings.TrimSpace(c.Hub.Range); s != "" {
		p, err := netip.ParsePrefix(s)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("hub.range %q: %w", s, err))
		case !p.Addr().Is4():
			errs = append(errs, fmt.Errorf("hub.range %q: must be IPv4 (IPv6 inside the tunnel is not in M8)", s))
		case p.Bits() > FleetSubnetBits:
			errs = append(errs, fmt.Errorf("hub.range %q: /%d holds no /%d for a machine",
				s, p.Bits(), FleetSubnetBits))
		}
	}
	return errs
}

func validateMemberBlock(c Config) []error {
	var errs []error
	if !c.Hub.Empty() {
		errs = append(errs, fmt.Errorf(
			"a hub: block on a %s: a machine hubs its own fleet or joined somebody else's, not both", RoleMember))
	}
	if strings.TrimSpace(c.Name) == "" {
		errs = append(errs, fmt.Errorf("role %s: name must say what the hub calls this machine", RoleMember))
	}
	switch f := strings.TrimSpace(c.Member.Fleet); {
	case f == "":
		errs = append(errs, fmt.Errorf(
			"member.fleet must name the fleet this machine joined: `caramelo member join` copies it from the hub"))
	case !isSlug(f):
		errs = append(errs, fmt.Errorf(
			"member.fleet %q: a fleet's name is a slug — lowercase letters, digits and dashes", f))
	}
	if c.Member.Hub.Empty() {
		errs = append(errs, fmt.Errorf("role %s: member.hub must say which machine this one joined", RoleMember))
	} else {
		errs = append(errs, validateMemberHub(c.Member.Hub)...)
	}
	errs = append(errs, validateMemberSubnet(c)...)
	return errs
}

func validateMemberSubnet(c Config) []error {
	s := strings.TrimSpace(c.Member.Subnet)
	if s == "" {
		return nil
	}
	p, err := netip.ParsePrefix(s)
	switch {
	case err != nil:
		return []error{fmt.Errorf("member.subnet %q: %w", s, err)}
	case !p.Addr().Is4():
		return []error{fmt.Errorf("member.subnet %q: must be IPv4 (IPv6 inside the tunnel is not in M8)", s)}
	case p.Bits() != FleetSubnetBits:
		return []error{fmt.Errorf("member.subnet %q: a machine's range is a /%d", s, FleetSubnetBits)}
	}
	var errs []error
	if r, err := c.FleetRangePrefix(); err == nil && !r.Contains(p.Masked().Addr()) {
		errs = append(errs, fmt.Errorf("member.subnet %q is outside the fleet's range %s", s, r))
	}
	if v, err := netip.ParsePrefix(strings.TrimSpace(c.VPNSubnet)); err == nil && v.Masked() != p.Masked() {
		errs = append(errs, fmt.Errorf("member.subnet %q and vpn_subnet %q are the same range said twice and they disagree",
			s, c.VPNSubnet))
	}
	return errs
}

func validateMemberHub(h MemberHub) []error {
	var errs []error
	switch n := strings.TrimSpace(h.Name); {
	case n == "":
		errs = append(errs, fmt.Errorf(
			"member.hub.name must name the machine this one joined: `caramelo member join` copies it from the hub"))
	case !isSlug(n):
		errs = append(errs, fmt.Errorf(
			"member.hub.name %q: a machine's name is a slug — lowercase letters, digits and dashes", n))
	}
	if e := strings.TrimSpace(h.Endpoint); e == "" {
		errs = append(errs, fmt.Errorf("member.hub.endpoint must not be empty: a member dials, so it needs somewhere to dial"))
	} else if host, port, err := net.SplitHostPort(e); err != nil {
		errs = append(errs, fmt.Errorf("member.hub.endpoint %q: want host:port: %w", e, err))
	} else if host == "" {
		errs = append(errs, fmt.Errorf("member.hub.endpoint %q: no host", e))
	} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		errs = append(errs, fmt.Errorf("member.hub.endpoint %q: port %q out of range", e, port))
	}
	if a := strings.TrimSpace(h.Address); a == "" {
		errs = append(errs, fmt.Errorf("member.hub.address must not be empty: it is where the hub answers inside the tunnel"))
	} else if ip, err := netip.ParseAddr(a); err != nil {
		errs = append(errs, fmt.Errorf("member.hub.address %q: %w", a, err))
	} else if !ip.Is4() {
		errs = append(errs, fmt.Errorf("member.hub.address %q: must be IPv4 (IPv6 inside the tunnel is not in M8)", a))
	}
	if k := strings.TrimSpace(h.PublicKey); k == "" {
		errs = append(errs, fmt.Errorf("member.hub.public_key must not be empty: it is what proves the hub is the one that was joined"))
	} else if key, err := vpn.ParseKey(k); err != nil {
		errs = append(errs, fmt.Errorf("member.hub.public_key: %w", err))
	} else if key.IsZero() {
		errs = append(errs, fmt.Errorf("member.hub.public_key is empty"))
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
