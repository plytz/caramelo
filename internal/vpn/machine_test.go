package vpn

import (
	"context"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	fleetRange = netip.MustParsePrefix("10.80.0.0/12")
	hubSubnet  = netip.MustParsePrefix("10.86.0.0/16")
	m1Subnet   = netip.MustParsePrefix("10.87.0.0/16")
	m2Subnet   = netip.MustParsePrefix("10.88.0.0/16")
)

func newFleetDevice(t *testing.T, subnet netip.Prefix, relay bool) (*wgDevice, *loopbackStack) {
	t.Helper()
	d, err := newDevice(Options{
		Subnet:         subnet,
		Listen:         DefaultListen,
		PrivateKeyPath: filepath.Join(t.TempDir(), "private.key"),
		FleetRange:     fleetRange,
		Relay:          relay,
	})
	if err != nil {
		t.Fatalf("newDevice: %v", err)
	}
	l := newLoopbackStack()
	d.stk = l
	if err := d.AddAddress(context.Background(), Address{
		IP: d.machineIP, Kind: KindMachine, Owner: "hub", Names: []string{MachineHost("hub")},
	}); err != nil {
		t.Fatalf("AddAddress: %v", err)
	}
	return d, l
}

func aPublicKey(t *testing.T) string {
	t.Helper()
	priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := priv.Public()
	if err != nil {
		t.Fatal(err)
	}
	return pub.Base64()
}

func TestMemberPeerAndHubPeer(t *testing.T) {
	key := aPublicKey(t)

	m := MemberPeer("m1", key, m1Subnet)
	if m.Endpoint != "" || m.Keepalive != 0 {
		t.Errorf("the hub's entry for a member carries %q/%d, want neither: the hub never dials",
			m.Endpoint, m.Keepalive)
	}
	if len(m.AllowedIPs) != 1 || m.AllowedIPs[0] != m1Subnet {
		t.Errorf("allowed ips = %v, want just %s", m.AllowedIPs, m1Subnet)
	}
	if m.Address.String() != "10.87.0.1" {
		t.Errorf("address = %s, want 10.87.0.1", m.Address)
	}

	h := HubPeer("hub", key, "hub.example.com:4021", netip.MustParseAddr("10.86.0.1"), fleetRange)
	if h.Endpoint == "" || h.Keepalive != Keepalive {
		t.Errorf("a member's entry for its hub is %+v, want an endpoint and a keepalive", h)
	}
	if len(h.AllowedIPs) != 1 || h.AllowedIPs[0] != fleetRange {
		t.Errorf("allowed ips = %v, want the whole fleet range: a member reaches every other through the hub",
			h.AllowedIPs)
	}
	if !h.Holds(netip.MustParseAddr("10.88.1.4")) {
		t.Error("the hub does not hold another member's environment address")
	}
}

func TestAddMachinePeerAndFindIt(t *testing.T) {
	ctx := context.Background()
	d, _ := newFleetDevice(t, hubSubnet, true)

	m1 := MemberPeer("m1", aPublicKey(t), m1Subnet)
	m2 := MemberPeer("m2", aPublicKey(t), m2Subnet)
	for _, m := range []MachinePeer{m1, m2} {
		if err := d.AddMachinePeer(ctx, m); err != nil {
			t.Fatalf("AddMachinePeer(%s): %v", m.Name, err)
		}
	}

	if err := d.AddMachinePeer(ctx, m1); err != nil {
		t.Fatalf("AddMachinePeer twice: %v", err)
	}
	got, err := d.MachinePeers(ctx)
	if err != nil {
		t.Fatalf("MachinePeers: %v", err)
	}
	if len(got) != 2 || got[0].Name != "m1" || got[1].Name != "m2" {
		t.Fatalf("machines = %+v, want m1 and m2 in name order", got)
	}

	if m, ok := d.MachineAt(ctx, netip.MustParseAddr("10.87.1.9")); !ok || m.Name != "m1" {
		t.Errorf("MachineAt(10.87.1.9) = %+v, %v, want m1", m, ok)
	}
	if _, ok := d.MachineAt(ctx, netip.MustParseAddr("10.99.0.1")); ok {
		t.Error("an address outside every machine's range was claimed by one")
	}

	if err := d.RemoveMachinePeer(ctx, "m1"); err != nil {
		t.Fatalf("RemoveMachinePeer: %v", err)
	}
	if err := d.RemoveMachinePeer(ctx, "m1"); err != nil {
		t.Fatalf("RemoveMachinePeer twice: %v", err)
	}
	if got, _ := d.MachinePeers(ctx); len(got) != 1 {
		t.Fatalf("machines = %+v, want just m2", got)
	}
}

func TestMachineAtTakesTheLongestPrefix(t *testing.T) {
	ctx := context.Background()
	d, _ := newFleetDevice(t, hubSubnet, true)
	if err := d.AddMachinePeer(ctx, HubPeer("hub", aPublicKey(t), "h:4021", netip.MustParseAddr("10.86.0.1"), fleetRange)); err != nil {
		t.Fatalf("AddMachinePeer(hub): %v", err)
	}
	if err := d.AddMachinePeer(ctx, MemberPeer("m1", aPublicKey(t), m1Subnet)); err != nil {
		t.Fatalf("AddMachinePeer(m1): %v", err)
	}
	m, ok := d.MachineAt(ctx, netip.MustParseAddr("10.87.1.1"))
	if !ok || m.Name != "m1" {
		t.Fatalf("MachineAt = %+v, %v, want m1 and not the hub's /12", m, ok)
	}
}

func TestAddMachinePeerRefusesWhatItCannotRoute(t *testing.T) {
	ctx := context.Background()
	d, _ := newFleetDevice(t, hubSubnet, true)
	key := aPublicKey(t)
	if err := d.AddMachinePeer(ctx, MemberPeer("m1", key, m1Subnet)); err != nil {
		t.Fatalf("AddMachinePeer: %v", err)
	}

	cases := map[string]struct {
		m    MachinePeer
		want string
	}{
		"no name": {MachinePeer{PublicKey: key, AllowedIPs: []netip.Prefix{m2Subnet}}, "no name"},
		"no key":  {MachinePeer{Name: "m2", AllowedIPs: []netip.Prefix{m2Subnet}}, "parse key"},
		"nothing allowed": {
			MachinePeer{Name: "m2", PublicKey: key}, "no allowed ips",
		},
		"the whole internet": {
			MachinePeer{Name: "m2", PublicKey: key, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}},
			"outside the fleet's range",
		},
		"somebody else's range": {
			MachinePeer{Name: "m2", PublicKey: key, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("192.168.0.0/16")}},
			"outside the fleet's range",
		},
		"a range another machine holds": {
			MemberPeer("m2", key, m1Subnet), "overlaps",
		},
		"this machine's own range": {
			MemberPeer("m2", key, hubSubnet), "this machine's own range",
		},
		"an address it does not hold": {
			MachinePeer{Name: "m2", PublicKey: key, Address: netip.MustParseAddr("10.99.0.1"),
				AllowedIPs: []netip.Prefix{m2Subnet}},
			"which is not in",
		},
		"an endpoint that is not one": {
			MachinePeer{Name: "m2", PublicKey: key, AllowedIPs: []netip.Prefix{m2Subnet}, Endpoint: "somewhere"},
			"host:port",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := d.AddMachinePeer(ctx, tc.m)
			if err == nil {
				t.Fatalf("AddMachinePeer accepted %+v", tc.m)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestAMachineOfOneHoldsNoMachines(t *testing.T) {
	d, _ := newTestDevice(t)
	err := d.AddMachinePeer(context.Background(), MemberPeer("m1", aPublicKey(t), m1Subnet))
	if err == nil || !strings.Contains(err.Error(), "no fleet range") {
		t.Fatalf("err = %v, want a refusal naming the missing range", err)
	}
}

func TestForwardCarriesALoopbackPortIntoTheTunnel(t *testing.T) {
	ctx := context.Background()
	d, l := newFleetDevice(t, hubSubnet, true)

	ingress := netip.AddrPortFrom(netip.MustParseAddr("10.87.0.1"), IngressPort)
	ln, err := l.ListenTCP(ingress)
	if err != nil {
		t.Fatalf("ListenTCP: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 64)
				n, err := c.Read(buf)
				if err != nil {
					return
				}
				c.Write([]byte("m1:" + string(buf[:n])))
			}()
		}
	}()

	f, err := d.AddForward(ctx, IngressForward("m1", netip.MustParseAddr("10.87.0.1")))
	if err != nil {
		t.Fatalf("AddForward: %v", err)
	}
	if f.Port == 0 {
		t.Fatal("the forward got no port, so the edge would have nothing to dial")
	}

	c, err := net.DialTimeout("tcp", f.Addr(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the forward: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "m1:hello" {
		t.Fatalf("the forward answered %q, want %q", got, "m1:hello")
	}

	again, err := d.AddForward(ctx, IngressForward("m1", netip.MustParseAddr("10.87.0.1")))
	if err != nil || again.Port != f.Port {
		t.Fatalf("AddForward again = %+v, %v, want the same port %d", again, err, f.Port)
	}

	if err := d.RemoveForward(ctx, f.Name); err != nil {
		t.Fatalf("RemoveForward: %v", err)
	}
	if err := d.RemoveForward(ctx, f.Name); err != nil {
		t.Fatalf("RemoveForward twice: %v", err)
	}
	if got, _ := d.Forwards(ctx); len(got) != 0 {
		t.Fatalf("forwards = %+v, want none", got)
	}
	if _, err := net.DialTimeout("tcp", f.Addr(), time.Second); err == nil {
		t.Error("the loopback listener outlived the forward")
	}
}

func TestForwardThatMoves(t *testing.T) {
	ctx := context.Background()
	d, _ := newFleetDevice(t, hubSubnet, true)
	first, err := d.AddForward(ctx, IngressForward("m1", netip.MustParseAddr("10.87.0.1")))
	if err != nil {
		t.Fatalf("AddForward: %v", err)
	}
	moved := IngressForward("m1", netip.MustParseAddr("10.88.0.1"))
	second, err := d.AddForward(ctx, moved)
	if err != nil {
		t.Fatalf("AddForward moved: %v", err)
	}
	if second.To == first.To {
		t.Fatalf("the forward did not move: %+v", second)
	}
	if _, err := net.DialTimeout("tcp", first.Addr(), time.Second); err == nil && first.Port != second.Port {
		t.Error("the old listener is still open")
	}
}

func TestForwardRefusesWhatItCannotCarry(t *testing.T) {
	ctx := context.Background()
	d, _ := newFleetDevice(t, hubSubnet, true)
	for name, f := range map[string]Forward{
		"no name":   {To: netip.MustParseAddrPort("10.87.0.1:8443")},
		"no target": {Name: "m1/ingress"},
		"no port":   {Name: "m1/ingress", To: netip.AddrPortFrom(netip.MustParseAddr("10.87.0.1"), 0)},
	} {
		if _, err := d.AddForward(ctx, f); err == nil {
			t.Errorf("AddForward accepted %s", name)
		}
	}
}

func TestForwardOnADeviceThatIsDown(t *testing.T) {
	d, _ := newFleetDevice(t, hubSubnet, true)
	d.stk = nil
	_, err := d.AddForward(context.Background(), IngressForward("m1", netip.MustParseAddr("10.87.0.1")))
	if err == nil || !strings.Contains(err.Error(), "not up") {
		t.Fatalf("err = %v, want a refusal naming the device", err)
	}
}

func TestIngressRoute(t *testing.T) {
	ctx := context.Background()
	d, _ := newFleetDevice(t, m1Subnet, false)
	r := IngressRoute(d.machineIP, 43210, netip.Prefix{})
	if r.Port != IngressPort || r.Target != "127.0.0.1:43210" || r.Protocol != TCP {
		t.Fatalf("route = %+v", r)
	}
	if err := d.SetAddressRoutes(ctx, d.machineIP, []Route{r}); err != nil {
		t.Fatalf("SetAddressRoutes: %v", err)
	}
	envIP := netip.MustParseAddr("10.87.1.1")
	if err := d.AddAddress(ctx, Address{IP: envIP, Kind: KindEnv, Owner: "shop/feat-x"}); err != nil {
		t.Fatalf("AddAddress: %v", err)
	}
	if err := d.SetAddressRoutes(ctx, envIP, []Route{{IP: envIP, Port: 8000, Protocol: TCP, Target: "127.0.0.1:32001"}}); err != nil {
		t.Fatalf("SetAddressRoutes(env): %v", err)
	}
	routes, err := d.Routes(ctx)
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	var found bool
	for _, got := range routes {
		if got.IP == d.machineIP && got.Port == IngressPort {
			found = true
		}
	}
	if !found {
		t.Fatalf("the ingress route did not survive a rollout: %+v", routes)
	}
}

func TestRouteAllow(t *testing.T) {
	open := Route{IP: netip.MustParseAddr("10.87.1.1"), Port: 8000, Protocol: TCP, Target: "127.0.0.1:1"}
	if !open.Allows(netip.MustParseAddr("10.87.0.2")) || !open.Allows(netip.MustParseAddr("10.86.0.1")) {
		t.Error("a route with no Allow refused somebody the tunnel admitted")
	}
	ingress := IngressRoute(netip.MustParseAddr("10.87.0.1"), 43210, netip.MustParsePrefix("10.86.0.0/16"))
	ingress.Allow = hubSubnet
	if !ingress.Allows(netip.MustParseAddr("10.86.0.1")) {
		t.Error("the ingress refused its own hub")
	}
	if ingress.Allows(netip.MustParseAddr("10.87.0.2")) {
		t.Error("the ingress answered a laptop on the member: the hub is the only thing that may come through it")
	}
	if !allowed(ingress, tcpAddr("10.86.0.1:5000")) {
		t.Error("allowed() refused the hub")
	}
	if allowed(ingress, tcpAddr("10.87.0.2:5000")) {
		t.Error("allowed() admitted a laptop")
	}

	if allowed(ingress, unreadableAddr{}) {
		t.Error("allowed() admitted an address it could not read")
	}
	if !allowed(open, unreadableAddr{}) {
		t.Error("allowed() refused an unrestricted route")
	}
}

func tcpAddr(s string) net.Addr {
	a, err := net.ResolveTCPAddr("tcp", s)
	if err != nil {
		panic(err)
	}
	return a
}

type unreadableAddr struct{}

func (unreadableAddr) Network() string { return "tcp" }
func (unreadableAddr) String() string  { return "somewhere" }

func TestDialIntoTheTunnel(t *testing.T) {
	d, l := newTestDevice(t)
	ctx := context.Background()

	at := netip.MustParseAddrPort("10.87.0.1:4022")
	ln, err := l.ListenTCP(at)
	if err != nil {
		t.Fatalf("ListenTCP: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = c.Write([]byte("member"))
		_ = c.Close()
	}()

	c, err := d.DialContext(ctx, "tcp", at.String())
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	got, err := io.ReadAll(c)
	_ = c.Close()
	if err != nil || string(got) != "member" {
		t.Fatalf("read = %q, %v; want the member's own bytes", got, err)
	}

	if _, err := d.DialContext(ctx, "sctp", at.String()); err == nil ||
		!strings.Contains(err.Error(), "tcp and udp only") {
		t.Fatalf("dial of an unknown protocol = %v, want it refused by name", err)
	}

	d.mu.Lock()
	d.stk = nil
	d.mu.Unlock()
	if _, err := d.DialContext(ctx, "tcp", at.String()); err == nil ||
		!strings.Contains(err.Error(), "the tunnel is not up") {
		t.Fatalf("dial with the device down = %v, want it to say the tunnel is not up", err)
	}
}
