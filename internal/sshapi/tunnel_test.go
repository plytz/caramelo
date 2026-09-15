package sshapi_test

import (
	"context"
	"errors"
	"github.com/plytz/caramelo/internal/testutil"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/plytz/caramelo/internal/cli"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/sshapi"
	"github.com/plytz/caramelo/internal/vpn"
)

type fakeTunnelListener struct {
	net.Listener
	once     sync.Once
	accepted chan struct{}
}

func newFakeTunnelListener(t *testing.T) *fakeTunnelListener {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return &fakeTunnelListener{Listener: ln, accepted: make(chan struct{})}
}

func (l *fakeTunnelListener) Accept() (net.Conn, error) {
	l.once.Do(func() { close(l.accepted) })
	return l.Listener.Accept()
}

type tunnelHarness struct {
	server *sshapi.Server
	ln     *fakeTunnelListener
	socket string
	log    *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func startTunnelServer(t *testing.T, apiListen string, peers map[string]string) *tunnelHarness {
	t.Helper()
	dir := t.TempDir()
	cfg := serverconfig.Default()
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.RunDir = filepath.Join(testutil.ShortDir(t), "run")
	cfg.Bind = "127.0.0.1"
	cfg.SSHPort = 0
	cfg.APIListen = apiListen

	ln := newFakeTunnelListener(t)
	log := &syncBuffer{}
	srv := &sshapi.Server{
		Config:      cfg,
		Service:     &fakeService{},
		Version:     "test",
		Hostname:    "testbox",
		Log:         log,
		VPNListener: ln,
		PeerLookup: sshapi.PeerLookupFunc(func(_ context.Context, ip netip.Addr) (string, bool) {
			name, ok := peers[ip.String()]
			return name, ok
		}),
		Exec: func(ctx context.Context, c sshapi.Command) int {
			return cli.RunWith(ctx, c.Args, c.Stdout, c.Stderr, cli.Options{Service: c.Service, Session: c.Session})
		},
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Serve did not stop within 10s of cancelling its context")
		}
	})
	return &tunnelHarness{server: srv, ln: ln, socket: srv.SocketPath(), log: log}
}

func (h *tunnelHarness) dial(t *testing.T) (*gossh.Client, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", h.ln.Addr().String(), 5*time.Second)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	c, chans, reqs, err := gossh.NewClientConn(conn, h.ln.Addr().String(), &gossh.ClientConfig{
		User:            serverconfig.DefaultUser,
		HostKeyCallback: gossh.FixedHostKey(h.server.HostKey.PublicKey()),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return gossh.NewClient(c, chans, reqs), nil
}

func TestTunnelSessionAuthenticatesByPeer(t *testing.T) {
	h := startTunnelServer(t, serverconfig.APIListenVPN, map[string]string{"127.0.0.1": "commander"})

	client, err := h.dial(t)
	if err != nil {
		t.Fatalf("dialling the tunnel listener: %v", err)
	}
	defer func() { _ = client.Close() }()

	code, stdout, stderr := run(t, client, "key", "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, `"name":"transport=tunnel"`) {
		t.Errorf("the service saw %q, want transport=tunnel", stdout)
	}
	if !strings.Contains(stdout, `"type":"identity=commander"`) {
		t.Errorf("the service saw %q, want identity=commander", stdout)
	}
	if got := h.log.String(); !strings.Contains(got, "tunnel commander:") {
		t.Errorf("the audit trail says %q, want a line naming the peer", got)
	}
}

func TestTunnelRefusesAnUnknownSource(t *testing.T) {
	h := startTunnelServer(t, serverconfig.APIListenVPN, nil)

	done := make(chan error, 1)
	go func() {
		c, err := h.dial(t)
		if c != nil {
			_ = c.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an unknown source completed the SSH handshake")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a refused connection hung instead of being closed")
	}
	if got := h.log.String(); !strings.Contains(got, "no peer owns 127.0.0.1") {
		t.Errorf("the log says %q, want it to say the source owned no peer", got)
	}
}

func TestAPIListenSelectsListeners(t *testing.T) {
	t.Run("vpn", func(t *testing.T) {
		h := startTunnelServer(t, serverconfig.APIListenVPN, map[string]string{"127.0.0.1": "commander"})
		if addr := h.server.Addr(); addr != nil {
			t.Errorf("api_listen: vpn still bound the public listener at %s", addr)
		}
		select {
		case <-h.ln.accepted:
		case <-time.After(5 * time.Second):
			t.Fatal("the tunnel listener was never accepted on")
		}

		conn, err := net.DialTimeout("unix", h.socket, 5*time.Second)
		if err != nil {
			t.Fatalf("api_listen: vpn closed the local socket too: %v", err)
		}
		_ = conn.Close()
	})

	t.Run("public", func(t *testing.T) {
		h := startTunnelServer(t, serverconfig.APIListenPublic, map[string]string{"127.0.0.1": "commander"})
		if h.server.Addr() == nil {
			t.Fatal("api_listen: public did not bind the public listener")
		}
		select {
		case <-h.ln.accepted:
			t.Error("api_listen: public served the tunnel listener anyway")
		case <-time.After(250 * time.Millisecond):
		}
	})

	t.Run("both", func(t *testing.T) {
		h := startTunnelServer(t, serverconfig.APIListenBoth, map[string]string{"127.0.0.1": "commander"})
		if h.server.Addr() == nil {
			t.Error("api_listen: both did not bind the public listener")
		}
		select {
		case <-h.ln.accepted:
		case <-time.After(5 * time.Second):
			t.Error("api_listen: both did not serve the tunnel listener")
		}
	})
}

func TestTunnelListenerRequiresAPeerLookup(t *testing.T) {
	dir := t.TempDir()
	cfg := serverconfig.Default()
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.RunDir = filepath.Join(testutil.ShortDir(t), "run")
	cfg.Bind = "127.0.0.1"
	cfg.SSHPort = 0
	ln := newFakeTunnelListener(t)
	t.Cleanup(func() { _ = ln.Close() })
	srv := &sshapi.Server{
		Config:      cfg,
		Service:     &fakeService{},
		VPNListener: ln,
		Exec:        func(context.Context, sshapi.Command) int { return 0 },
	}
	err := srv.Listen()
	if err == nil {
		t.Fatal("Listen accepted a tunnel listener with no PeerLookup")
	}
	if !strings.Contains(err.Error(), "PeerLookup") {
		t.Errorf("err = %v, want it to name PeerLookup", err)
	}
}

type fakeDevice struct {
	vpn.Device
	peers    []vpn.Peer
	machines []vpn.MachinePeer
	err      error
}

func (d *fakeDevice) Peers(context.Context) ([]vpn.Peer, error) { return d.peers, d.err }

func (d *fakeDevice) MachineAt(_ context.Context, ip netip.Addr) (vpn.MachinePeer, bool) {
	for _, m := range d.machines {
		for _, p := range m.AllowedIPs {
			if p.Contains(ip) {
				return m, true
			}
		}
	}
	return vpn.MachinePeer{}, false
}

type listeningDevice struct {
	*fakeDevice
	ln  net.Listener
	err error
	got netip.AddrPort
}

func (d *listeningDevice) Listen(_ context.Context, ap netip.AddrPort) (net.Listener, error) {
	d.got = ap
	if d.err != nil {
		return nil, d.err
	}
	return d.ln, nil
}

func TestDevicePeersNamesTheSource(t *testing.T) {
	dev := &fakeDevice{peers: []vpn.Peer{
		{Name: "laptop", IP: netip.MustParseAddr("10.86.0.2")},
		{Name: "agent-7", IP: netip.MustParseAddr("10.86.0.3")},
	}}
	lookup := sshapi.DevicePeers{Device: dev}
	ctx := context.Background()

	if name, ok := lookup.PeerAt(ctx, netip.MustParseAddr("10.86.0.3")); !ok || name != "agent-7" {
		t.Errorf("PeerAt(10.86.0.3) = %q, %v; want agent-7, true", name, ok)
	}

	if name, ok := lookup.PeerAt(ctx, netip.MustParseAddr("10.86.1.4")); ok {
		t.Errorf("PeerAt(10.86.1.4) = %q, true; want no peer", name)
	}
	if _, ok := lookup.PeerAt(ctx, netip.Addr{}); ok {
		t.Error("PeerAt(invalid address) found a peer")
	}

	dev.machines = []vpn.MachinePeer{vpn.MemberPeer("m1", "m1-key", netip.MustParsePrefix("10.87.0.0/16"))}
	if name, ok := lookup.PeerAt(ctx, netip.MustParseAddr("10.87.0.1")); !ok || name != "m1" {
		t.Errorf("PeerAt(10.87.0.1) = %q, %v; want m1, true", name, ok)
	}

	if name, ok := lookup.PeerAt(ctx, netip.MustParseAddr("10.87.1.9")); ok {
		t.Errorf("PeerAt(10.87.1.9) = %q, true; want an environment to be nobody", name)
	}
	if name, ok := lookup.PeerAt(ctx, netip.MustParseAddr("10.86.0.3")); !ok || name != "agent-7" {
		t.Errorf("PeerAt(10.86.0.3) = %q, %v; want the peer to win, not a machine", name, ok)
	}
	dev.machines = nil

	hub := netip.MustParseAddr("10.86.0.1")
	dev.machines = []vpn.MachinePeer{vpn.HubPeer("hub", "hub-key", "hub.example:4021", hub,
		netip.MustParsePrefix("10.80.0.0/12"))}
	if name, ok := lookup.PeerAt(ctx, hub); !ok || name != "hub" {
		t.Errorf("PeerAt(10.86.0.1) = %q, %v; want hub, true", name, ok)
	}
	for _, ip := range []string{"10.86.0.7", "10.88.0.1", "10.87.1.4"} {
		if name, ok := lookup.PeerAt(ctx, netip.MustParseAddr(ip)); ok {
			t.Errorf("PeerAt(%s) = %q, true; want no identity on a member", ip, name)
		}
	}
	dev.machines = nil

	dev.err = errors.New("device is down")
	if _, ok := lookup.PeerAt(ctx, netip.MustParseAddr("10.86.0.3")); ok {
		t.Error("PeerAt trusted a device that returned an error")
	}
	if _, ok := (sshapi.DevicePeers{}).PeerAt(ctx, netip.MustParseAddr("10.86.0.3")); ok {
		t.Error("PeerAt with no device found a peer")
	}
}

func TestListenTunnel(t *testing.T) {
	ctx := context.Background()
	addr := netip.MustParseAddrPort("10.86.0.1:4022")

	if _, err := sshapi.ListenTunnel(ctx, nil, addr); err == nil {
		t.Error("ListenTunnel accepted a nil device")
	}

	broken := &listeningDevice{fakeDevice: &fakeDevice{}, err: errors.New("device is down")}
	if _, err := sshapi.ListenTunnel(ctx, broken, addr); err == nil {
		t.Error("ListenTunnel accepted a device that cannot listen")
	} else if !strings.Contains(err.Error(), "10.86.0.1:4022") {
		t.Errorf("err = %v, want it to name the address", err)
	}

	ln := newFakeTunnelListener(t)
	t.Cleanup(func() { _ = ln.Close() })
	dev := &listeningDevice{fakeDevice: &fakeDevice{}, ln: ln}
	got, err := sshapi.ListenTunnel(ctx, dev, addr)
	if err != nil {
		t.Fatal(err)
	}
	if got != ln {
		t.Error("ListenTunnel did not return the device's listener")
	}
	if dev.got != addr {
		t.Errorf("the device was asked for %s, want %s", dev.got, addr)
	}
}
