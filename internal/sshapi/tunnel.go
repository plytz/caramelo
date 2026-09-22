package sshapi

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"charm.land/ssh"

	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/vpn"
)

const peerLookupTimeout = 5 * time.Second

type PeerLookup interface {
	PeerAt(ctx context.Context, ip netip.Addr) (string, bool)
}

type PeerLookupFunc func(ctx context.Context, ip netip.Addr) (string, bool)

func (f PeerLookupFunc) PeerAt(ctx context.Context, ip netip.Addr) (string, bool) {
	return f(ctx, ip)
}

type DevicePeers struct{ Device vpn.Device }

var _ PeerLookup = DevicePeers{}

func (d DevicePeers) PeerAt(ctx context.Context, ip netip.Addr) (string, bool) {
	if d.Device == nil || !ip.IsValid() {
		return "", false
	}
	peers, err := d.Device.Peers(ctx)
	if err != nil {
		return "", false
	}
	for _, p := range peers {
		if p.IP == ip && p.Name != "" {
			return p.Name, true
		}
	}
	if m, ok := d.Device.MachineAt(ctx, ip); ok && m.Name != "" && m.Address == ip {
		return m.Name, true
	}
	return "", false
}

func ListenTunnel(ctx context.Context, dev vpn.Device, ap netip.AddrPort) (net.Listener, error) {
	if dev == nil {
		return nil, fmt.Errorf("listen on %s inside the tunnel: no device", ap)
	}
	ln, err := dev.Listen(ctx, ap)
	if err != nil {
		return nil, fmt.Errorf("listen on %s inside the tunnel: %w", ap, err)
	}
	return ln, nil
}

func (s *Server) tunnelConn(ctx ssh.Context, conn net.Conn) net.Conn {
	ip, ok := connIP(conn)
	if !ok {
		s.logf("tunnel: connection from %v has no usable address; refused", conn.RemoteAddr())
		return nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, peerLookupTimeout)
	defer cancel()
	name, ok := s.PeerLookup.PeerAt(lookupCtx, ip)
	if !ok {
		s.logf("tunnel: no peer owns %s; refused", ip)
		return nil
	}
	ctx.SetValue(contextKeyIdentity, name)
	return conn
}

func connIP(conn net.Conn) (netip.Addr, bool) {
	if conn == nil || conn.RemoteAddr() == nil {
		return netip.Addr{}, false
	}
	switch a := conn.RemoteAddr().(type) {
	case *net.TCPAddr:
		ip, ok := netip.AddrFromSlice(a.IP)
		return ip.Unmap(), ok
	default:
		ap, err := netip.ParseAddrPort(conn.RemoteAddr().String())
		if err != nil {
			return netip.Addr{}, false
		}
		return ap.Addr().Unmap(), true
	}
}

type Tunnel struct {
	Device   vpn.Device
	Listener net.Listener
	Peers    PeerLookup

	Addr netip.AddrPort
}

func (t *Tunnel) Close(ctx context.Context) error {
	if t == nil {
		return nil
	}
	if t.Listener != nil {
		_ = t.Listener.Close()
	}
	if t.Device != nil {
		return t.Device.Down(ctx)
	}
	return nil
}

func startTunnel(ctx context.Context, cfg serverconfig.Config, logw io.Writer, start func(context.Context, vpn.Device) error) (*Tunnel, error) {
	subnet, err := cfg.VPNSubnetPrefix()
	if err != nil {
		return nil, err
	}
	addr, err := cfg.VPNAPIAddrPort()
	if err != nil {
		return nil, err
	}
	opts := vpn.Options{
		Subnet:         subnet,
		Listen:         cfg.VPNListen,
		Mode:           vpn.Mode(cfg.VPNMode),
		PrivateKeyPath: cfg.VPNKeyPath(),
		Log:            logw,
	}

	if rng, err := cfg.FleetRangePrefix(); err == nil {
		opts.FleetRange = rng
	}
	if cfg.IsMember() {
		opts.Listen = memberListen(cfg.VPNListen)
		if at, err := cfg.HubResolverAddrPort(); err == nil {
			opts.Forward = at
		}
	} else {
		opts.Relay = true
	}
	dev, err := vpn.New(opts)
	if err != nil {
		return nil, fmt.Errorf("build the network device: %w", err)
	}
	if err := start(ctx, dev); err != nil {
		_ = dev.Down(ctx)
		return nil, fmt.Errorf("start the network device on %s: %w", cfg.VPNListen, err)
	}
	ln, err := ListenTunnel(ctx, dev, addr)
	if err != nil {
		_ = dev.Down(ctx)
		return nil, err
	}
	return &Tunnel{Device: dev, Listener: ln, Peers: DevicePeers{dev}, Addr: addr}, nil
}

func memberListen(configured string) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(configured))
	if err != nil {
		return "0.0.0.0:0"
	}
	return net.JoinHostPort(host, "0")
}
