package netstack

import (
	"fmt"
	"net/netip"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

const nicID tcpip.NICID = 1

func (net *Net) Stack() *stack.Stack { return net.stack }

func (net *Net) AddAddress(ip netip.Addr) error {
	pa := tcpip.ProtocolAddress{
		Protocol:          protocolOf(ip),
		AddressWithPrefix: tcpip.AddrFromSlice(ip.AsSlice()).WithPrefix(),
	}
	if err := net.stack.AddProtocolAddress(nicID, pa, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("netstack: add address %s: %v", ip, err)
	}
	return nil
}

func (net *Net) RemoveAddress(ip netip.Addr) error {
	err := net.stack.RemoveAddress(nicID, tcpip.AddrFromSlice(ip.AsSlice()))
	if err == nil {
		return nil
	}
	if _, ok := err.(*tcpip.ErrBadLocalAddress); ok {
		return nil
	}
	return fmt.Errorf("netstack: remove address %s: %v", ip, err)
}

func (net *Net) SetForwarding(on bool) error {
	if err := net.stack.SetForwardingDefaultAndAllNICs(ipv4.ProtocolNumber, on); err != nil {
		return fmt.Errorf("netstack: set ipv4 forwarding to %v: %v", on, err)
	}
	return nil
}

func (net *Net) Forwarding() (bool, error) {
	on, err := net.stack.NICForwarding(nicID, ipv4.ProtocolNumber)
	if err != nil {
		return false, fmt.Errorf("netstack: read ipv4 forwarding: %v", err)
	}
	return on, nil
}

func protocolOf(ip netip.Addr) tcpip.NetworkProtocolNumber {
	if ip.Is4() {
		return ipv4.ProtocolNumber
	}
	return ipv6.ProtocolNumber
}
