package vpnclient

import (
	"context"
	"net"
	"net/netip"
	"time"

	"github.com/plytz/caramelo/internal/vpn"
)

type fixedLookup []Target

var _ Lookup = fixedLookup{}

func (l fixedLookup) EnvTargets(context.Context, string, string, string) ([]Target, error) {
	return []Target(l), nil
}

type stubDialer struct{}

var _ Dialer = stubDialer{}

func (stubDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, vpn.ErrNotImplemented
}

func (stubDialer) Resolve(context.Context, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("10.86.1.1")}, nil
}

func (stubDialer) Close() error { return nil }

func dialLocalOnce(address string) (net.Conn, error) {
	return net.DialTimeout("tcp", address, 200*time.Millisecond)
}
