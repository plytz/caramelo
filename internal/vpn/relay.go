package vpn

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/plytz/caramelo/internal/vpn/netstack"
)

const (
	dialTimeout = 5 * time.Second

	udpIdle = 30 * time.Second

	udpBuffer = 64 * 1024
)

type stack interface {
	AddAddress(ip netip.Addr) error

	RemoveAddress(ip netip.Addr) error
	ListenTCP(at netip.AddrPort) (net.Listener, error)
	ListenUDP(at netip.AddrPort) (net.PacketConn, error)

	DialTCP(ctx context.Context, at netip.AddrPort) (net.Conn, error)

	DialUDP(at netip.AddrPort) (net.Conn, error)
}

type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

func netDial(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}

type netStack struct{ net *netstack.Net }

func (s netStack) AddAddress(ip netip.Addr) error    { return s.net.AddAddress(ip) }
func (s netStack) RemoveAddress(ip netip.Addr) error { return s.net.RemoveAddress(ip) }

func (s netStack) ListenTCP(at netip.AddrPort) (net.Listener, error) {
	ln, err := s.net.ListenTCPAddrPort(at)
	if err != nil {
		return nil, fmt.Errorf("vpn: listen tcp %s in the tunnel: %w", at, err)
	}
	return ln, nil
}

func (s netStack) ListenUDP(at netip.AddrPort) (net.PacketConn, error) {
	pc, err := s.net.ListenUDPAddrPort(at)
	if err != nil {
		return nil, fmt.Errorf("vpn: listen udp %s in the tunnel: %w", at, err)
	}
	return pc, nil
}

func (s netStack) DialTCP(ctx context.Context, at netip.AddrPort) (net.Conn, error) {
	c, err := s.net.DialContextTCPAddrPort(ctx, at)
	if err != nil {
		return nil, fmt.Errorf("vpn: dial tcp %s in the tunnel: %w", at, err)
	}
	return c, nil
}

func (s netStack) DialUDP(at netip.AddrPort) (net.Conn, error) {
	c, err := s.net.DialUDPAddrPort(netip.AddrPort{}, at)
	if err != nil {
		return nil, fmt.Errorf("vpn: dial udp %s in the tunnel: %w", at, err)
	}
	return c, nil
}

type relayKey struct {
	IP       netip.Addr
	Port     int
	Protocol Protocol
}

func keyOf(r Route) relayKey { return relayKey{IP: r.IP, Port: r.Port, Protocol: r.Protocol} }

func (k relayKey) String() string {
	return fmt.Sprintf("%s/%s", netip.AddrPortFrom(k.IP, uint16(k.Port)), k.Protocol)
}

type relayEntry struct {
	mu     sync.Mutex
	route  Route
	closer io.Closer
}

func (e *relayEntry) Route() Route {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.route
}

func (e *relayEntry) setRoute(r Route) {
	e.mu.Lock()
	e.route = r
	e.mu.Unlock()
}

func startRelay(l stack, dial dialFunc, r Route, logf func(string, ...any)) (*relayEntry, error) {
	if err := checkRoute(r); err != nil {
		return nil, err
	}
	at := r.AddrPort()
	switch r.Protocol {
	case TCP:
		ln, err := l.ListenTCP(at)
		if err != nil {
			return nil, err
		}
		e := &relayEntry{route: r, closer: ln}
		go relayTCP(ln, e, dial, logf)
		return e, nil
	case UDP:
		pc, err := l.ListenUDP(at)
		if err != nil {
			return nil, err
		}
		e := &relayEntry{route: r, closer: pc}
		go relayUDP(pc, e, dial, logf)
		return e, nil
	default:
		return nil, fmt.Errorf("vpn: route %s: unknown protocol %q", at, r.Protocol)
	}
}

const (
	rebindWait  = 5 * time.Second
	rebindPause = 20 * time.Millisecond
)

func openRelay(l stack, dial dialFunc, r Route, logf func(string, ...any)) (*relayEntry, error) {
	e, err := startRelay(l, dial, r, logf)
	if err == nil {
		return e, nil
	}
	for deadline := time.Now().Add(rebindWait); time.Now().Before(deadline); {
		time.Sleep(rebindPause)
		if e, err = startRelay(l, dial, r, logf); err == nil {
			return e, nil
		}
	}
	return nil, fmt.Errorf("%w (still held %s after the listener on it was closed)", err, rebindWait)
}

func checkRoute(r Route) error {
	if !r.IP.IsValid() {
		return fmt.Errorf("vpn: route: no address")
	}
	if r.Port < 1 || r.Port > 65535 {
		return fmt.Errorf("vpn: route %s: port %d is out of range", r.IP, r.Port)
	}
	if !r.Protocol.Valid() {
		return fmt.Errorf("vpn: route %s:%d: protocol must be tcp or udp, got %q", r.IP, r.Port, r.Protocol)
	}
	if _, _, err := net.SplitHostPort(r.Target); err != nil {
		return fmt.Errorf("vpn: route %s:%d: target %q is not host:port: %w", r.IP, r.Port, r.Target, err)
	}
	return nil
}

func relayTCP(ln net.Listener, e *relayEntry, dial dialFunc, logf func(string, ...any)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}

		r := e.Route()
		target := r.Target
		if !allowed(r, c.RemoteAddr()) {
			logf("relay tcp %s: refused %s, which is not allowed on this route", ln.Addr(), c.RemoteAddr())
			c.Close()
			continue
		}
		go func() {
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
			up, err := dial(ctx, "tcp", target)
			cancel()
			if err != nil {
				logf("relay tcp %s -> %s: %v", ln.Addr(), target, err)
				return
			}
			defer up.Close()
			splice(c, up)
		}()
	}
}

func allowed(r Route, addr net.Addr) bool {
	if !r.Allow.IsValid() {
		return true
	}
	ap, err := netip.ParseAddrPort(addr.String())
	if err != nil {
		return false
	}
	return r.Allows(ap.Addr())
}

func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(b, a); closeWrite(b) }()
	go func() { defer wg.Done(); io.Copy(a, b); closeWrite(a) }()
	wg.Wait()
}

type halfCloser interface{ CloseWrite() error }

func closeWrite(c net.Conn) {
	if hc, ok := c.(halfCloser); ok {
		if err := hc.CloseWrite(); err == nil {
			return
		}
	}
	_ = c.Close()
}

func relayUDP(pc net.PacketConn, e *relayEntry, dial dialFunc, logf func(string, ...any)) {
	var (
		mu    sync.Mutex
		flows = map[string]net.Conn{}
	)
	defer func() {
		mu.Lock()
		for _, up := range flows {
			up.Close()
		}
		flows = nil
		mu.Unlock()
	}()

	drop := func(key string, up net.Conn) {
		mu.Lock()
		if flows != nil && flows[key] == up {
			delete(flows, key)
		}
		mu.Unlock()
		up.Close()
	}

	buf := make([]byte, udpBuffer)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		r := e.Route()
		target := r.Target
		if !allowed(r, src) {
			logf("relay udp %s: refused %s, which is not allowed on this route", pc.LocalAddr(), src)
			continue
		}

		key := src.String() + " -> " + target
		mu.Lock()
		up, ok := flows[key]
		if !ok {
			ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
			up, err = dial(ctx, "udp", target)
			cancel()
			if err != nil {
				mu.Unlock()
				logf("relay udp %s -> %s: %v", pc.LocalAddr(), target, err)
				continue
			}
			flows[key] = up
			go func(key string, src net.Addr, up net.Conn) {
				defer drop(key, up)
				rbuf := make([]byte, udpBuffer)
				for {

					up.SetReadDeadline(time.Now().Add(udpIdle))
					n, err := up.Read(rbuf)
					if err != nil {
						return
					}
					if _, err := pc.WriteTo(rbuf[:n], src); err != nil {
						return
					}
				}
			}(key, src, up)
		}
		mu.Unlock()
		if _, err := up.Write(buf[:n]); err != nil {
			logf("relay udp %s -> %s: %v", pc.LocalAddr(), target, err)
		}
	}
}
