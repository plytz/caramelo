package vpn

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

const Keepalive = 25

type MachinePeer struct {
	Name string `json:"name"`

	PublicKey string `json:"public_key"`

	Address netip.Addr `json:"address"`

	AllowedIPs []netip.Prefix `json:"allowed_ips"`

	Endpoint string `json:"endpoint,omitempty"`

	Keepalive int `json:"keepalive,omitempty"`

	LastHandshake time.Time `json:"last_handshake,omitempty"`

	SeenAt string `json:"seen_at,omitempty"`
}

func MemberPeer(name, publicKey string, subnet netip.Prefix) MachinePeer {
	return MachinePeer{
		Name:       name,
		PublicKey:  publicKey,
		Address:    MachineIP(subnet.Masked()),
		AllowedIPs: []netip.Prefix{subnet.Masked()},
	}
}

func HubPeer(name, publicKey, endpoint string, address netip.Addr, fleetRange netip.Prefix) MachinePeer {
	return MachinePeer{
		Name:       name,
		PublicKey:  publicKey,
		Address:    address,
		AllowedIPs: []netip.Prefix{fleetRange.Masked()},
		Endpoint:   endpoint,
		Keepalive:  Keepalive,
	}
}

func (m MachinePeer) Holds(ip netip.Addr) bool {
	for _, p := range m.AllowedIPs {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

const IngressPort = 8443

func IngressRoute(machineIP netip.Addr, edgePort int, allow netip.Prefix) Route {
	return Route{
		IP:       machineIP,
		Port:     IngressPort,
		Protocol: TCP,
		Target:   net.JoinHostPort("127.0.0.1", strconv.Itoa(edgePort)),
		Name:     "ingress",
		Allow:    allow,
	}
}

func IngressForward(machine string, address netip.Addr) Forward {
	return Forward{
		Name:    machine + "/ingress",
		Machine: machine,
		To:      netip.AddrPortFrom(address, IngressPort),
	}
}

func checkMachinePeer(m MachinePeer, fleetRange netip.Prefix) (Key, error) {
	if strings.TrimSpace(m.Name) == "" {
		return Key{}, fmt.Errorf("vpn: machine peer: no name")
	}
	key, err := ParseKey(m.PublicKey)
	if err != nil {
		return Key{}, fmt.Errorf("vpn: machine %s: %w", m.Name, err)
	}
	if key.IsZero() {
		return Key{}, fmt.Errorf("vpn: machine %s: the public key is empty", m.Name)
	}
	if !fleetRange.IsValid() {
		return Key{}, fmt.Errorf("vpn: machine %s: this device has no fleet range, so it can hold no machine peers", m.Name)
	}
	if len(m.AllowedIPs) == 0 {
		return Key{}, fmt.Errorf("vpn: machine %s: no allowed ips, so nothing would ever reach it", m.Name)
	}
	for _, p := range m.AllowedIPs {
		switch {
		case !p.IsValid():
			return Key{}, fmt.Errorf("vpn: machine %s: %q is not a prefix", m.Name, p)
		case !p.Addr().Is4():
			return Key{}, fmt.Errorf("vpn: machine %s: %s must be IPv4", m.Name, p)
		case !fleetRange.Contains(p.Masked().Addr()) || p.Bits() < fleetRange.Bits():
			return Key{}, fmt.Errorf("vpn: machine %s: %s is outside the fleet's range %s", m.Name, p, fleetRange)
		}
	}
	if m.Address.IsValid() && !m.Holds(m.Address) {
		return Key{}, fmt.Errorf("vpn: machine %s: it answers at %s, which is not in %s",
			m.Name, m.Address, prefixList(m.AllowedIPs))
	}
	if m.Endpoint != "" {
		if _, port, err := net.SplitHostPort(m.Endpoint); err != nil {
			return Key{}, fmt.Errorf("vpn: machine %s: endpoint %q is not host:port: %w", m.Name, m.Endpoint, err)
		} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return Key{}, fmt.Errorf("vpn: machine %s: endpoint %q: port %q out of range", m.Name, m.Endpoint, port)
		}
	}
	return key, nil
}

func machineSection(key Key, m MachinePeer) ipcPeer {
	return ipcPeer{
		PublicKey:         key,
		ReplaceAllowedIPs: true,
		AllowedIPs:        append([]netip.Prefix(nil), m.AllowedIPs...),
		Endpoint:          m.Endpoint,
		Keepalive:         m.Keepalive,
	}
}

func prefixList(ps []netip.Prefix) string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.String())
	}
	return strings.Join(out, ", ")
}
