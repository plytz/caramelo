package vpn

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

type loopbackStack struct {
	mu    sync.Mutex
	addrs map[netip.Addr]bool
	tcp   map[netip.AddrPort]net.Listener
	udp   map[netip.AddrPort]net.PacketConn
	fail  error
}

func newLoopbackStack() *loopbackStack {
	return &loopbackStack{
		addrs: map[netip.Addr]bool{},
		tcp:   map[netip.AddrPort]net.Listener{},
		udp:   map[netip.AddrPort]net.PacketConn{},
	}
}

func (l *loopbackStack) AddAddress(ip netip.Addr) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail != nil {
		return l.fail
	}
	l.addrs[ip] = true
	return nil
}

func (l *loopbackStack) RemoveAddress(ip netip.Addr) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail != nil {
		return l.fail
	}
	delete(l.addrs, ip)
	return nil
}

func (l *loopbackStack) hasAddress(ip netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.addrs[ip]
}

func (l *loopbackStack) ListenTCP(at netip.AddrPort) (net.Listener, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail != nil {
		return nil, l.fail
	}
	if _, taken := l.tcp[at]; taken {
		return nil, fmt.Errorf("vpn: listen tcp %s: address already in use", at)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	w := &fakeListener{Listener: ln, stack: l, at: at}
	l.tcp[at] = w
	return w, nil
}

type lingeringStack struct {
	*loopbackStack
	mu     sync.Mutex
	refuse map[netip.AddrPort]int
}

func newLingeringStack(l *loopbackStack) *lingeringStack {
	return &lingeringStack{loopbackStack: l, refuse: map[netip.AddrPort]int{}}
}

func (s *lingeringStack) holds(at netip.AddrPort, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuse[at] = n
}

func (s *lingeringStack) ListenTCP(at netip.AddrPort) (net.Listener, error) {
	s.mu.Lock()
	left := s.refuse[at]
	if left > 0 {
		s.refuse[at] = left - 1
	}
	s.mu.Unlock()
	if left > 0 {
		return nil, fmt.Errorf("vpn: listen tcp %s in the tunnel: bind tcp %s: port is in use", at, at)
	}
	return s.loopbackStack.ListenTCP(at)
}

func (l *loopbackStack) ListenUDP(at netip.AddrPort) (net.PacketConn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail != nil {
		return nil, l.fail
	}
	if _, taken := l.udp[at]; taken {
		return nil, fmt.Errorf("vpn: listen udp %s: address already in use", at)
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	w := &fakePacketConn{PacketConn: pc, stack: l, at: at}
	l.udp[at] = w
	return w, nil
}

func (l *loopbackStack) DialTCP(ctx context.Context, at netip.AddrPort) (net.Conn, error) {
	l.mu.Lock()
	ln, ok := l.tcp[at]
	fail := l.fail
	l.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	if !ok {
		return nil, fmt.Errorf("vpn: dial tcp %s in the tunnel: nothing listens there", at)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", ln.Addr().String())
}

func (l *loopbackStack) DialUDP(at netip.AddrPort) (net.Conn, error) {
	l.mu.Lock()
	pc, ok := l.udp[at]
	fail := l.fail
	l.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	if !ok {
		return nil, fmt.Errorf("vpn: dial udp %s in the tunnel: nothing listens there", at)
	}
	return net.Dial("udp", pc.LocalAddr().String())
}

type fakeListener struct {
	net.Listener
	stack *loopbackStack
	at    netip.AddrPort
}

func (l *fakeListener) Close() error {
	l.stack.mu.Lock()
	if l.stack.tcp[l.at] == net.Listener(l) {
		delete(l.stack.tcp, l.at)
	}
	l.stack.mu.Unlock()
	return l.Listener.Close()
}

type fakePacketConn struct {
	net.PacketConn
	stack *loopbackStack
	at    netip.AddrPort
}

func (c *fakePacketConn) Close() error {
	c.stack.mu.Lock()
	if c.stack.udp[c.at] == net.PacketConn(c) {
		delete(c.stack.udp, c.at)
	}
	c.stack.mu.Unlock()
	return c.PacketConn.Close()
}

func (l *loopbackStack) addrOf(t *testing.T, at netip.AddrPort, p Protocol) string {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if p == TCP {
		ln, ok := l.tcp[at]
		if !ok {
			t.Fatalf("nothing listens on %s/tcp", at)
		}
		return ln.Addr().String()
	}
	pc, ok := l.udp[at]
	if !ok {
		t.Fatalf("nothing listens on %s/udp", at)
	}
	return pc.LocalAddr().String()
}

func (l *loopbackStack) live(at netip.AddrPort, p Protocol) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p == TCP {
		_, ok := l.tcp[at]
		return ok
	}
	_, ok := l.udp[at]
	return ok
}

func discard(string, ...any) {}

func tcpEcho(t *testing.T, prefix string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						if _, err := c.Write([]byte(prefix + string(buf[:n]))); err != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func udpEcho(t *testing.T, prefix string) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, src, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if _, err := pc.WriteTo([]byte(prefix+string(buf[:n])), src); err != nil {
				return
			}
		}
	}()
	return pc.LocalAddr().String()
}

func TestRelayTCPCarriesAConnectionToTheLoopbackPort(t *testing.T) {
	target := tcpEcho(t, "web:")
	l := newLoopbackStack()
	at := netip.MustParseAddrPort("10.86.1.4:8000")
	e, err := startRelay(l, netDial, Route{IP: at.Addr(), Port: int(at.Port()), Protocol: TCP, Target: target}, discard)
	if err != nil {
		t.Fatalf("startRelay: %v", err)
	}
	defer e.closer.Close()

	c, err := net.Dial("tcp", l.addrOf(t, at, TCP))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "web:hello" {
		t.Fatalf("got %q, want %q", got, "web:hello")
	}
}

func TestRelayTCPCarriesABulkStreamBothWays(t *testing.T) {

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()

	l := newLoopbackStack()
	at := netip.MustParseAddrPort("10.86.1.4:5432")
	e, err := startRelay(l, netDial, Route{IP: at.Addr(), Port: int(at.Port()), Protocol: TCP, Target: ln.Addr().String()}, discard)
	if err != nil {
		t.Fatalf("startRelay: %v", err)
	}
	defer e.closer.Close()

	c, err := net.Dial("tcp", l.addrOf(t, at, TCP))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	payload := strings.Repeat("caramelo", 32*1024)
	go func() { c.Write([]byte(payload)) }()
	c.SetReadDeadline(time.Now().Add(20 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != payload {
		t.Fatal("the stream came back changed")
	}
}

func TestRelayUDPCarriesDatagramsAndKeepsFlowsApart(t *testing.T) {
	target := udpEcho(t, "echo:")
	l := newLoopbackStack()
	at := netip.MustParseAddrPort("10.86.1.4:5533")
	e, err := startRelay(l, netDial, Route{IP: at.Addr(), Port: int(at.Port()), Protocol: UDP, Target: target}, discard)
	if err != nil {
		t.Fatalf("startRelay: %v", err)
	}
	defer e.closer.Close()

	via := l.addrOf(t, at, UDP)

	for i, want := range []string{"one", "two"} {
		c, err := net.Dial("udp", via)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		defer c.Close()
		if _, err := c.Write([]byte(want)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 64)
		n, err := c.Read(buf)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if got := string(buf[:n]); got != "echo:"+want {
			t.Fatalf("source %d got %q, want %q", i, got, "echo:"+want)
		}
	}
}

func TestRelayRefusesARouteItCouldNotServe(t *testing.T) {
	l := newLoopbackStack()
	ip := netip.MustParseAddr("10.86.1.4")
	for name, r := range map[string]Route{
		"no address":   {Port: 80, Protocol: TCP, Target: "127.0.0.1:1"},
		"port zero":    {IP: ip, Port: 0, Protocol: TCP, Target: "127.0.0.1:1"},
		"port too big": {IP: ip, Port: 70000, Protocol: TCP, Target: "127.0.0.1:1"},
		"no protocol":  {IP: ip, Port: 80, Target: "127.0.0.1:1"},
		"icmp":         {IP: ip, Port: 80, Protocol: Protocol("icmp"), Target: "127.0.0.1:1"},
		"no target":    {IP: ip, Port: 80, Protocol: TCP},
		"bad target":   {IP: ip, Port: 80, Protocol: TCP, Target: "not a target"},
	} {
		if _, err := startRelay(l, netDial, r, discard); err == nil {
			t.Errorf("startRelay accepted the %s route", name)
		}
	}
}

func TestRelayDropsTheConnectionWhenTheLoopbackPortIsDead(t *testing.T) {

	l := newLoopbackStack()
	at := netip.MustParseAddrPort("10.86.1.4:8000")
	dead := deadPort(t)
	e, err := startRelay(l, netDial, Route{IP: at.Addr(), Port: int(at.Port()), Protocol: TCP, Target: dead}, discard)
	if err != nil {
		t.Fatalf("startRelay: %v", err)
	}
	defer e.closer.Close()

	c, err := net.Dial("tcp", l.addrOf(t, at, TCP))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadAll(c); err != nil {
		t.Fatalf("the connection was not closed: %v", err)
	}
}

func TestRelayDialErrorsAreLoggedNotFatal(t *testing.T) {
	l := newLoopbackStack()
	at := netip.MustParseAddrPort("10.86.1.4:8000")
	var (
		mu   sync.Mutex
		logs []string
	)
	logf := func(format string, args ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, fmt.Errorf("connection refused")
	}
	e, err := startRelay(l, dial, Route{IP: at.Addr(), Port: int(at.Port()), Protocol: TCP, Target: "127.0.0.1:1"}, logf)
	if err != nil {
		t.Fatalf("startRelay: %v", err)
	}
	defer e.closer.Close()

	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", l.addrOf(t, at, TCP))
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		io.ReadAll(c)
		c.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(logs)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) < 2 {
		t.Fatalf("logged %v, want a line per failed connection", logs)
	}
	if !strings.Contains(logs[0], "connection refused") {
		t.Errorf("log line %q does not name the cause", logs[0])
	}
}

func deadPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestRelayTCPCarriesTheAnswerAfterAHalfClose(t *testing.T) {

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		req, err := io.ReadAll(c)
		if err != nil {
			return
		}
		_, _ = c.Write([]byte("answer to " + string(req)))
	}()

	l := newLoopbackStack()
	at := netip.MustParseAddrPort("10.86.1.4:8000")
	e, err := startRelay(l, netDial, Route{IP: at.Addr(), Port: int(at.Port()), Protocol: TCP, Target: ln.Addr().String()}, discard)
	if err != nil {
		t.Fatalf("startRelay: %v", err)
	}
	defer e.closer.Close()

	c, err := net.Dial("tcp", l.addrOf(t, at, TCP))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("a request")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("half-close: %v", err)
	}
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := "answer to a request"; string(got) != want {
		t.Fatalf("got %q, want %q: the relay closed the connection before the answer came back", got, want)
	}
}

func TestRelayUDPForgetsAFlowWhoseReplyCannotBeWritten(t *testing.T) {
	target := udpEcho(t, "echo:")
	l := newLoopbackStack()
	at := netip.MustParseAddrPort("10.86.1.4:9000")
	pc, err := l.ListenUDP(at)
	if err != nil {
		t.Fatal(err)
	}

	failing := &failingPacketConn{PacketConn: pc}
	dialed := make(chan net.Conn, 4)
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := netDial(ctx, network, address)
		if err == nil {
			dialed <- c
		}
		return c, err
	}
	go relayUDP(failing, &relayEntry{route: Route{Target: target}}, dial, discard)
	defer failing.Close()

	client, err := net.Dial("udp", l.addrOf(t, at, UDP))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	failing.setFail(true)
	if _, err := client.Write([]byte("one")); err != nil {
		t.Fatal(err)
	}

	first := <-dialed
	waitFor(t, func() bool { return connClosed(first) })

	failing.setFail(false)
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("two")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("the flow was not reopened after a failed reply: %v", err)
	}
	if got, want := string(buf[:n]), "echo:two"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

type failingPacketConn struct {
	net.PacketConn
	mu   sync.Mutex
	fail bool
}

func (c *failingPacketConn) setFail(v bool) {
	c.mu.Lock()
	c.fail = v
	c.mu.Unlock()
}

func (c *failingPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	c.mu.Lock()
	fail := c.fail
	c.mu.Unlock()
	if fail {
		return 0, fmt.Errorf("write into the tunnel: broken")
	}
	return c.PacketConn.WriteTo(b, addr)
}

func connClosed(c net.Conn) bool {
	err := c.SetDeadline(time.Now())
	return err != nil && strings.Contains(err.Error(), "closed")
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the condition never held")
}
