package vpn

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	forwardTimeout = 3 * time.Second

	forwardBuffer = 4096
)

type dialer interface {
	DialUDP(at netip.AddrPort) (net.Conn, error)
}

func NewForwarder(d dialer, server netip.AddrPort) Forwarder {
	return func(ctx context.Context, name string) ([]netip.Addr, error) {
		return askA(ctx, d, server, name)
	}
}

func askA(ctx context.Context, d dialer, server netip.AddrPort, name string) ([]netip.Addr, error) {
	q, err := queryA(name)
	if err != nil {
		return nil, err
	}
	c, err := d.DialUDP(server)
	if err != nil {
		return nil, fmt.Errorf("vpn: ask %s about %q: %w", server, name, err)
	}
	defer c.Close()

	deadline := time.Now().Add(forwardTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := c.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("vpn: ask %s about %q: %w", server, name, err)
	}
	if _, err := c.Write(q); err != nil {
		return nil, fmt.Errorf("vpn: ask %s about %q: %w", server, name, err)
	}
	buf := make([]byte, forwardBuffer)
	n, err := c.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("vpn: ask %s about %q: %w", server, name, err)
	}
	return answersOf(buf[:n], name, server)
}

func queryA(name string) ([]byte, error) {
	n, err := dnsmessage.NewName(Normalize(name) + ".")
	if err != nil {
		return nil, fmt.Errorf("vpn: %q is not a name that can be asked about: %w", name, err)
	}
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{RecursionDesired: false},
		Questions: []dnsmessage.Question{{Name: n, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}
	b, err := msg.Pack()
	if err != nil {
		return nil, fmt.Errorf("vpn: build a query for %q: %w", name, err)
	}
	return b, nil
}

func answersOf(reply []byte, name string, server netip.AddrPort) ([]netip.Addr, error) {
	var msg dnsmessage.Message
	if err := msg.Unpack(reply); err != nil {
		return nil, fmt.Errorf("vpn: %s answered about %q with something that is not a dns message: %w", server, name, err)
	}
	var out []netip.Addr
	for _, a := range msg.Answers {
		r, ok := a.Body.(*dnsmessage.AResource)
		if !ok {
			continue
		}
		out = append(out, netip.AddrFrom4(r.A))
	}
	return out, nil
}
