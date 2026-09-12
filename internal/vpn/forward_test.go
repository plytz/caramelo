package vpn

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

func TestResolverWithNoForwarderIsM5(t *testing.T) {
	r := NewResolver()
	if err := r.SetZone(context.Background(), []Address{{
		IP: netip.MustParseAddr("10.86.1.1"), Names: []string{"feat-x.shop.internal"},
	}}); err != nil {
		t.Fatalf("SetZone: %v", err)
	}
	if _, err := r.Resolve(context.Background(), "feat-y.shop.internal"); !errors.Is(err, ErrNXDOMAIN) {
		t.Fatalf("err = %v, want ErrNXDOMAIN", err)
	}
}

func TestResolverForwardsOnlyInternalNamesItDoesNotHold(t *testing.T) {
	ctx := context.Background()
	r := NewResolver()
	if err := r.SetZone(ctx, []Address{{
		IP: netip.MustParseAddr("10.87.1.1"), Names: []string{"feat-x.shop.internal"},
	}}); err != nil {
		t.Fatalf("SetZone: %v", err)
	}

	var asked []string
	r.SetForwarder(func(_ context.Context, name string) ([]netip.Addr, error) {
		asked = append(asked, name)
		if name == "feat-y.shop.internal" {
			return []netip.Addr{netip.MustParseAddr("10.88.1.4")}, nil
		}
		return nil, nil
	})

	got, err := r.Resolve(ctx, "feat-x.shop.internal")
	if err != nil || len(got) != 1 || got[0].String() != "10.87.1.1" {
		t.Fatalf("own name = %v, %v", got, err)
	}
	if len(asked) != 0 {
		t.Fatalf("the hub was asked about a name this machine holds: %v", asked)
	}

	got, err = r.Resolve(ctx, "feat-y.shop.internal")
	if err != nil || len(got) != 1 || got[0].String() != "10.88.1.4" {
		t.Fatalf("forwarded name = %v, %v", got, err)
	}

	if _, err := r.Resolve(ctx, "nope.shop.internal"); !errors.Is(err, ErrNXDOMAIN) {
		t.Fatalf("err = %v, want ErrNXDOMAIN", err)
	}

	before := len(asked)
	if _, err := r.Resolve(ctx, "example.com"); !errors.Is(err, ErrNXDOMAIN) {
		t.Fatalf("err = %v, want ErrNXDOMAIN", err)
	}
	if len(asked) != before {
		t.Fatalf("a name outside %s was forwarded: %v", Suffix, asked)
	}

	r.SetForwarder(func(context.Context, string) ([]netip.Addr, error) {
		return nil, errors.New("the hub is not answering")
	})
	if _, err := r.Resolve(ctx, "feat-z.shop.internal"); err == nil || errors.Is(err, ErrNXDOMAIN) {
		t.Fatalf("err = %v, want the forwarder's own failure", err)
	}
}

func TestForwarderAsksAHubAndReadsTheAnswer(t *testing.T) {
	ctx := context.Background()

	hub := NewResolver()
	if err := hub.SetZone(ctx, []Address{{
		IP: netip.MustParseAddr("10.88.1.4"), Names: []string{"feat-y.shop.internal"},
	}}); err != nil {
		t.Fatalf("SetZone: %v", err)
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go serveDNS(pc, hub, func(string, ...any) {})

	at := netip.MustParseAddrPort(pc.LocalAddr().String())
	fwd := NewForwarder(plainDialer{}, at)

	got, err := fwd(ctx, "feat-y.shop.internal")
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if len(got) != 1 || got[0].String() != "10.88.1.4" {
		t.Fatalf("answer = %v, want 10.88.1.4", got)
	}

	got, err = fwd(ctx, "nope.shop.internal")
	if err != nil || len(got) != 0 {
		t.Fatalf("answer = %v, %v, want nothing and no error", got, err)
	}
}

func TestForwarderWhenTheHubIsNotThere(t *testing.T) {
	fwd := NewForwarder(refusingDialer{}, netip.MustParseAddrPort("10.86.0.1:53"))
	_, err := fwd(context.Background(), "feat-y.shop.internal")
	if err == nil {
		t.Fatal("a hub that cannot be dialled was not reported")
	}
	if !strings.Contains(err.Error(), "10.86.0.1:53") {
		t.Fatalf("error %q, want it to name the hub", err)
	}
}

type plainDialer struct{}

func (plainDialer) DialUDP(at netip.AddrPort) (net.Conn, error) {
	return net.Dial("udp", at.String())
}

type refusingDialer struct{}

func (refusingDialer) DialUDP(netip.AddrPort) (net.Conn, error) {
	return nil, errors.New("no route")
}
