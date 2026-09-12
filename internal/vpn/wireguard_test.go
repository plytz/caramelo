package vpn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"

	"github.com/plytz/caramelo/internal/vpn/netstack"
)

const (
	testTimeout = 15 * time.Second

	silenceTimeout = 2 * time.Second
)

type testBox struct {
	dev      *wgDevice
	pub      Key
	endpoint string
	envIP    netip.Addr
	tcpEcho  string
	udpEcho  string
}

func startBox(t *testing.T, peers ...Peer) *testBox {
	t.Helper()
	ctx := context.Background()

	priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := priv.Public()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "vpn", "private.key")
	if err := WritePrivateKey(keyPath, priv); err != nil {
		t.Fatal(err)
	}

	d, err := newDevice(Options{
		Subnet:         mustSubnet(t, DefaultSubnet),
		Listen:         "127.0.0.1:0",
		PrivateKeyPath: keyPath,
		Log:            testLog{t},
	})
	if err != nil {
		t.Fatalf("newDevice: %v", err)
	}

	envIP := netip.MustParseAddr("10.86.1.4")
	addrs := []Address{
		{IP: d.machineIP, Kind: KindMachine, Owner: "worker1", Names: []string{MachineHost("worker1")}},
		{IP: envIP, Kind: KindEnv, Owner: "shop/feat-x", Names: EnvHosts("shop", "feat-x", []string{"web", "db", "echo"})},
	}
	if err := d.SetAddresses(ctx, addrs); err != nil {
		t.Fatalf("SetAddresses: %v", err)
	}
	tcpTarget := tcpEcho(t, "web:")
	udpTarget := udpEcho(t, "echo:")
	routes := []Route{
		{IP: envIP, Port: 8000, Protocol: TCP, Target: tcpTarget, App: "shop", Env: "feat-x", Name: "web"},
		{IP: envIP, Port: 5432, Protocol: TCP, Target: tcpTarget, App: "shop", Env: "feat-x", Name: "db"},
		{IP: envIP, Port: 5533, Protocol: UDP, Target: udpTarget, App: "shop", Env: "feat-x", Name: "echo"},
	}
	if err := d.SetRoutes(ctx, routes); err != nil {
		t.Fatalf("SetRoutes: %v", err)
	}
	for _, p := range peers {
		if err := d.AddPeer(ctx, p); err != nil {
			t.Fatalf("AddPeer: %v", err)
		}
	}
	if err := d.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	t.Cleanup(func() { d.Down(context.Background()) })

	st, err := d.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Up {
		t.Fatal("the device came up but does not say so")
	}
	if st.PublicKey != pub.Base64() {
		t.Fatalf("status public key = %s, want %s", st.PublicKey, pub.Base64())
	}
	_, port, err := net.SplitHostPort(st.Listen)
	if err != nil || port == "0" {
		t.Fatalf("status listen = %q, want the port the socket actually got", st.Listen)
	}
	if st.Resolver != "10.86.0.1:53" {
		t.Fatalf("status resolver = %q, want 10.86.0.1:53", st.Resolver)
	}
	return &testBox{
		dev:      d,
		pub:      pub,
		endpoint: net.JoinHostPort("127.0.0.1", port),
		envIP:    envIP,
		tcpEcho:  tcpTarget,
		udpEcho:  udpTarget,
	}
}

type testLog struct{ t *testing.T }

func (l testLog) Write(b []byte) (int, error) {
	l.t.Logf("box: %s", strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

type testPeer struct {
	priv Key
	pub  Key
	ip   netip.Addr
	dev  *device.Device
	net  *netstack.Net
}

func newPeer(t *testing.T, name, ip string) (Peer, Key) {
	t.Helper()
	priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := priv.Public()
	if err != nil {
		t.Fatal(err)
	}
	return Peer{Name: name, PublicKey: pub.Base64(), IP: netip.MustParseAddr(ip)}, priv
}

func startPeer(t *testing.T, box *testBox, p Peer, priv Key) *testPeer {
	t.Helper()
	return startPeerWith(t, box, p, priv, mustSubnet(t, DefaultSubnet))
}

func startPeerWith(t *testing.T, box *testBox, p Peer, priv Key, allowed netip.Prefix) *testPeer {
	t.Helper()
	pub, err := priv.Public()
	if err != nil {
		t.Fatal(err)
	}
	tdev, tnet, err := netstack.CreateNetTUN(
		[]netip.Addr{p.IP},
		[]netip.Addr{MachineIP(mustSubnet(t, DefaultSubnet))},
		MTU,
	)
	if err != nil {
		t.Fatalf("create the peer's network stack: %v", err)
	}
	dev := device.NewDevice(tdev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "peer "))
	port := 0
	cfg := ipcConfig{
		PrivateKey: &priv,
		ListenPort: &port,
		Peers: []ipcPeer{{
			PublicKey: box.pub,
			Endpoint:  box.endpoint,

			AllowedIPs: []netip.Prefix{allowed},
			Keepalive:  25,
		}},
	}
	if err := dev.IpcSet(cfg.String()); err != nil {
		t.Fatalf("configure the peer: %v", err)
	}
	if err := dev.Up(); err != nil {
		t.Fatalf("bring the peer up: %v", err)
	}
	t.Cleanup(dev.Close)
	return &testPeer{priv: priv, pub: pub, ip: p.IP, dev: dev, net: tnet}
}

func (p *testPeer) say(t *testing.T, address, msg string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	c, err := p.net.DialContext(ctx, "tcp", address)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(testTimeout))
	if _, err := c.Write([]byte(msg)); err != nil {
		return "", err
	}
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

func TestTunnelRelaysTCPToTheLoopbackPort(t *testing.T) {
	peer, priv := newPeer(t, "laptop", "10.86.0.2")
	box := startBox(t, peer)
	lap := startPeer(t, box, peer, priv)

	for _, addr := range []string{"10.86.1.4:8000", "10.86.1.4:5432"} {
		got, err := lap.say(t, addr, "hello")
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		if got != "web:hello" {
			t.Fatalf("%s answered %q, want %q", addr, got, "web:hello")
		}
	}
}

func TestTunnelRelaysUDP(t *testing.T) {
	peer, priv := newPeer(t, "laptop", "10.86.0.2")
	box := startBox(t, peer)
	lap := startPeer(t, box, peer, priv)

	c, err := lap.net.DialUDPAddrPort(netip.AddrPort{}, netip.MustParseAddrPort("10.86.1.4:5533"))
	if err != nil {
		t.Fatalf("dial udp: %v", err)
	}
	defer c.Close()

	var got string
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if _, err := c.Write([]byte("ping")); err != nil {
			t.Fatalf("write: %v", err)
		}
		c.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 4096)
		n, err := c.Read(buf)
		if err == nil {
			got = string(buf[:n])
			break
		}
	}
	if got != "echo:ping" {
		t.Fatalf("udp answered %q, want %q", got, "echo:ping")
	}
}

func TestTunnelResolvesInternalNamesAndNothingElse(t *testing.T) {
	peer, priv := newPeer(t, "laptop", "10.86.0.2")
	box := startBox(t, peer)
	lap := startPeer(t, box, peer, priv)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	for _, name := range []string{"feat-x.shop.internal", "db.feat-x.shop.internal", "worker1.internal"} {
		addrs, err := lap.net.LookupContextHost(ctx, name)
		if err != nil {
			t.Fatalf("lookup %s: %v", name, err)
		}
		want := "10.86.1.4"
		if name == "worker1.internal" {
			want = "10.86.0.1"
		}
		if len(addrs) != 1 || addrs[0] != want {
			t.Fatalf("lookup %s = %v, want [%s]", name, addrs, want)
		}
	}

	got, err := lap.say(t, "db.feat-x.shop.internal:5432", "hello")
	if err != nil {
		t.Fatalf("dial by name: %v", err)
	}
	if got != "web:hello" {
		t.Fatalf("dial by name answered %q", got)
	}

	for _, name := range []string{"nope.internal", "example.com"} {
		if _, err := lap.net.LookupContextHost(ctx, name); err == nil {
			t.Errorf("lookup %s succeeded", name)
		}
	}
}

func TestAPortWithNoRouteIsRefusedNotSwallowed(t *testing.T) {
	peer, priv := newPeer(t, "laptop", "10.86.0.2")
	box := startBox(t, peer)
	lap := startPeer(t, box, peer, priv)

	if _, err := lap.say(t, "10.86.1.4:8000", "hello"); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), silenceTimeout)
	defer cancel()
	c, err := lap.net.DialContext(ctx, "tcp", "10.86.1.4:9999")
	if err == nil {
		c.Close()
		t.Fatal("a port with no route accepted a connection")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a port with no route timed out instead of refusing: %v", err)
	}
}

func TestIdentityIsTheSourceAddress(t *testing.T) {

	peer, priv := newPeer(t, "agent-7", "10.86.0.9")
	box := startBox(t, peer)
	lap := startPeer(t, box, peer, priv)
	ctx := context.Background()

	ln, err := box.dev.Listen(ctx, netip.AddrPortFrom(box.dev.machineIP, 4022))
	if err != nil {
		t.Fatalf("Listen in the tunnel: %v", err)
	}
	defer ln.Close()

	type seen struct {
		remote string
		name   string
	}
	got := make(chan seen, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
		ip, _ := netip.ParseAddr(host)
		name := ""
		if p, ok := box.dev.PeerAt(ctx, ip); ok {
			name = p.Name
		}
		got <- seen{remote: host, name: name}
		c.Write([]byte("ok"))
	}()

	if _, err := lap.say(t, "10.86.0.1:4022", "hello"); err != nil {
		t.Fatalf("session through the tunnel: %v", err)
	}
	select {
	case s := <-got:
		if s.remote != "10.86.0.9" {
			t.Errorf("the connection came from %s, want the peer's tunnel address 10.86.0.9", s.remote)
		}
		if s.name != "agent-7" {
			t.Errorf("identity = %q, want agent-7", s.name)
		}
	case <-time.After(testTimeout):
		t.Fatal("the listener inside the tunnel never accepted")
	}
}

func TestAnEnvAddressCanBeAddedAndRemovedOnALiveDevice(t *testing.T) {

	peer, priv := newPeer(t, "laptop", "10.86.0.2")
	box := startBox(t, peer)
	lap := startPeer(t, box, peer, priv)
	ctx := context.Background()

	if _, err := lap.say(t, "10.86.1.4:8000", "hello"); err != nil {
		t.Fatalf("the first env: %v", err)
	}

	target := tcpEcho(t, "feat-y:")
	newIP := netip.MustParseAddr("10.86.1.5")
	a := Address{IP: newIP, Kind: KindEnv, Owner: "shop/feat-y", Names: EnvHosts("shop", "feat-y", []string{"web"})}
	if err := box.dev.AddAddress(ctx, a); err != nil {
		t.Fatalf("AddAddress: %v", err)
	}
	err := box.dev.SetAddressRoutes(ctx, newIP, []Route{
		{IP: newIP, Port: 8000, Protocol: TCP, Target: target, App: "shop", Env: "feat-y", Name: "web"},
	})
	if err != nil {
		t.Fatalf("SetAddressRoutes: %v", err)
	}

	if got, err := lap.say(t, "10.86.1.5:8000", "hello"); err != nil || got != "feat-y:hello" {
		t.Fatalf("the new env answered %q, %v; want %q", got, err, "feat-y:hello")
	}
	lookupCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	if addrs, err := lap.net.LookupContextHost(lookupCtx, "feat-y.shop.internal"); err != nil || addrs[0] != "10.86.1.5" {
		t.Fatalf("lookup feat-y.shop.internal = %v, %v", addrs, err)
	}

	if got, err := lap.say(t, "10.86.1.4:8000", "hello"); err != nil || got != "web:hello" {
		t.Fatalf("the first env broke: %q, %v", got, err)
	}

	if err := box.dev.RemoveAddress(ctx, newIP); err != nil {
		t.Fatalf("RemoveAddress: %v", err)
	}
	gone, cancelGone := context.WithTimeout(ctx, silenceTimeout)
	defer cancelGone()
	if c, err := lap.net.DialContext(gone, "tcp", "10.86.1.5:8000"); err == nil {
		c.Close()
		t.Fatal("a destroyed env's address still answers")
	}
	nx, cancelNX := context.WithTimeout(ctx, testTimeout)
	defer cancelNX()
	if _, err := lap.net.LookupContextHost(nx, "feat-y.shop.internal"); err == nil {
		t.Error("a destroyed env's name still resolves")
	}
	if got, err := lap.say(t, "10.86.1.4:8000", "hello"); err != nil || got != "web:hello" {
		t.Fatalf("the surviving env broke: %q, %v", got, err)
	}
}

func TestRevokingAPeerLeavesItWithSilence(t *testing.T) {
	peer, priv := newPeer(t, "laptop", "10.86.0.2")
	box := startBox(t, peer)
	lap := startPeer(t, box, peer, priv)
	ctx := context.Background()

	if _, err := lap.say(t, "10.86.1.4:8000", "hello"); err != nil {
		t.Fatalf("before the revocation: %v", err)
	}

	peers, err := box.dev.Peers(ctx)
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	if len(peers) != 1 || peers[0].LastHandshake.IsZero() {
		t.Fatalf("peers = %+v, want laptop with a handshake", peers)
	}

	if err := box.dev.RemovePeer(ctx, "laptop"); err != nil {
		t.Fatalf("RemovePeer: %v", err)
	}
	dead, cancel := context.WithTimeout(ctx, silenceTimeout)
	defer cancel()
	c, err := lap.net.DialContext(dead, "tcp", "10.86.1.4:8000")
	if err == nil {
		c.Close()
		t.Fatal("a revoked peer still got through")
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a revoked peer got %v, want its own deadline to expire", err)
	}
}

func TestAnUnregisteredPeerGetsNothing(t *testing.T) {
	box := startBox(t)
	stranger, priv := newPeer(t, "stranger", "10.86.0.3")
	lap := startPeer(t, box, stranger, priv)

	ctx, cancel := context.WithTimeout(context.Background(), silenceTimeout)
	defer cancel()
	c, err := lap.net.DialContext(ctx, "tcp", "10.86.1.4:8000")
	if err == nil {
		c.Close()
		t.Fatal("an unregistered key got through")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an unregistered key got %v, want silence", err)
	}
}

func TestAPeerCannotUseAnotherPeersAddress(t *testing.T) {

	registered, priv := newPeer(t, "laptop", "10.86.0.2")
	box := startBox(t, registered)

	spoofing := registered
	spoofing.IP = netip.MustParseAddr("10.86.0.3")
	lap := startPeer(t, box, spoofing, priv)

	ctx, cancel := context.WithTimeout(context.Background(), silenceTimeout)
	defer cancel()
	c, err := lap.net.DialContext(ctx, "tcp", "10.86.1.4:8000")
	if err == nil {
		c.Close()
		t.Fatal("a peer sending from an address outside its allowed IPs got through")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the packets to be dropped in silence", err)
	}
}

func TestTunnelCarriesABulkStream(t *testing.T) {
	peer, priv := newPeer(t, "laptop", "10.86.0.2")
	box := startBox(t, peer)
	lap := startPeer(t, box, peer, priv)

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
	ctx := context.Background()
	err = box.dev.SetAddressRoutes(ctx, box.envIP, []Route{
		{IP: box.envIP, Port: 9000, Protocol: TCP, Target: ln.Addr().String()},
	})
	if err != nil {
		t.Fatalf("SetAddressRoutes: %v", err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	c, err := lap.net.DialContext(dialCtx, "tcp", "10.86.1.4:9000")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	payload := strings.Repeat("caramelo", 64*1024)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.Write([]byte(payload))
	}()
	c.SetReadDeadline(time.Now().Add(testTimeout))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	wg.Wait()
	if string(got) != payload {
		t.Fatal("the stream came back changed")
	}
}

func TestUpIsIdempotentAndDownIsSafe(t *testing.T) {
	peer, priv := newPeer(t, "laptop", "10.86.0.2")
	box := startBox(t, peer)
	lap := startPeer(t, box, peer, priv)
	ctx := context.Background()

	if err := box.dev.Up(ctx); err != nil {
		t.Fatalf("Up twice: %v", err)
	}
	if got, err := lap.say(t, "10.86.1.4:8000", "hello"); err != nil || got != "web:hello" {
		t.Fatalf("the session broke after a second Up: %q, %v", got, err)
	}

	if err := box.dev.Down(ctx); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if err := box.dev.Down(ctx); err != nil {
		t.Fatalf("Down twice: %v", err)
	}
	st, err := box.dev.Status(ctx)
	if err != nil {
		t.Fatalf("Status after down: %v", err)
	}
	if st.Up {
		t.Error("the device says it is up after Down")
	}

	if st.Peers != 1 || st.Routes != 3 {
		t.Errorf("status = %+v, want the tables remembered", st)
	}
	if st.PublicKey != box.pub.Base64() {
		t.Errorf("public key = %q after down, want it still readable from the key file", st.PublicKey)
	}
}

func TestUpNeedsAKey(t *testing.T) {
	t.Parallel()
	d, err := newDevice(Options{
		Subnet:         mustSubnet(t, DefaultSubnet),
		Listen:         "127.0.0.1:0",
		PrivateKeyPath: filepath.Join(t.TempDir(), "missing.key"),
	})
	if err != nil {
		t.Fatalf("newDevice: %v", err)
	}
	err = d.Up(context.Background())
	if err == nil {
		t.Fatal("a device with no key came up")
	}
	if !strings.Contains(err.Error(), "missing.key") {
		t.Errorf("error %q does not name the key file", err)
	}
}

func TestTwoDevicesCannotShareAUDPPort(t *testing.T) {
	t.Parallel()

	priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	held, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	_, port, _ := net.SplitHostPort(held.LocalAddr().String())

	keyPath := filepath.Join(t.TempDir(), "private.key")
	if err := WritePrivateKey(keyPath, priv); err != nil {
		t.Fatal(err)
	}
	d, err := newDevice(Options{
		Subnet:         mustSubnet(t, DefaultSubnet),
		Listen:         fmt.Sprintf("0.0.0.0:%s", port),
		PrivateKeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("newDevice: %v", err)
	}
	err = d.Up(context.Background())
	if err == nil {
		d.Down(context.Background())
		t.Skip("the port was not held exclusively on this system")
	}
	if !strings.Contains(err.Error(), "already in use") || !strings.Contains(err.Error(), port) {
		t.Errorf("error = %q, want it to name the busy udp port %s", err, port)
	}
}
