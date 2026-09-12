package vpn

import (
	"context"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

type fleetBox struct {
	dev      *wgDevice
	pub      Key
	endpoint string
	subnet   netip.Prefix
	envIP    netip.Addr
	echo     string
}

func startFleetBox(t *testing.T, name string, subnet netip.Prefix, relay bool, forward netip.AddrPort) *fleetBox {
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
		Subnet:         subnet,
		Listen:         "127.0.0.1:0",
		PrivateKeyPath: keyPath,
		FleetRange:     fleetRange,
		Relay:          relay,
		Forward:        forward,
		Log:            testLog{t},
	})
	if err != nil {
		t.Fatalf("newDevice(%s): %v", name, err)
	}

	envIP := offset(subnet, envFirstOffset+3)
	echo := tcpEcho(t, name+":")
	addrs := []Address{
		{IP: d.machineIP, Kind: KindMachine, Owner: name, Names: []string{MachineHost(name)}},
		{IP: envIP, Kind: KindEnv, Owner: "shop/" + name, Names: []string{EnvHost("shop", name)}},
	}
	if err := d.SetAddresses(ctx, addrs); err != nil {
		t.Fatalf("SetAddresses(%s): %v", name, err)
	}
	if err := d.SetRoutes(ctx, []Route{
		{IP: envIP, Port: 8000, Protocol: TCP, Target: echo, App: "shop", Env: name, Name: "web"},
	}); err != nil {
		t.Fatalf("SetRoutes(%s): %v", name, err)
	}
	return &fleetBox{dev: d, pub: pub, subnet: subnet, envIP: envIP, echo: echo}
}

func (b *fleetBox) up(t *testing.T) {
	t.Helper()
	if err := b.dev.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	t.Cleanup(func() { b.dev.Down(context.Background()) })
	st, err := b.dev.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	b.endpoint = st.Listen
}

func (b *fleetBox) say(t *testing.T, at netip.AddrPort, msg string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	c, err := b.dev.stk.DialTCP(ctx, at)
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

func startFleet(t *testing.T) (hub, member *fleetBox) {
	t.Helper()
	ctx := context.Background()

	hub = startFleetBox(t, "hub", hubSubnet, true, netip.AddrPort{})
	hub.up(t)

	member = startFleetBox(t, "m1", m1Subnet, false,
		netip.AddrPortFrom(MachineIP(hubSubnet), ResolverPort))

	if err := hub.dev.AddMachinePeer(ctx, MemberPeer("m1", member.pub.Base64(), m1Subnet)); err != nil {
		t.Fatalf("hub AddMachinePeer: %v", err)
	}
	if err := member.dev.AddMachinePeer(ctx, HubPeer("hub", hub.pub.Base64(), hub.endpoint,
		MachineIP(hubSubnet), fleetRange)); err != nil {
		t.Fatalf("member AddMachinePeer: %v", err)
	}
	member.up(t)
	return hub, member
}

func TestFleetMemberDialsAndTheHubLearnsWhereFrom(t *testing.T) {
	hub, member := startFleet(t)

	got, err := member.say(t, netip.AddrPortFrom(hub.envIP, 8000), "hello")
	if err != nil {
		t.Fatalf("member -> hub: %v", err)
	}
	if got != "hub:hello" {
		t.Fatalf("member -> hub answered %q, want %q", got, "hub:hello")
	}

	machines, err := hub.dev.MachinePeers(context.Background())
	if err != nil {
		t.Fatalf("MachinePeers: %v", err)
	}
	if len(machines) != 1 {
		t.Fatalf("machines = %+v, want one", machines)
	}
	if machines[0].LastHandshake.IsZero() {
		t.Fatal("the hub records no handshake with the member")
	}
	if machines[0].SeenAt == "" {
		t.Fatal("the hub did not learn the member's endpoint from the handshake")
	}
	if machines[0].Endpoint != "" {
		t.Errorf("the hub stored an endpoint %q for a member it never dials", machines[0].Endpoint)
	}

	got, err = hub.say(t, netip.AddrPortFrom(member.envIP, 8000), "back")
	if err != nil {
		t.Fatalf("hub -> member: %v", err)
	}
	if got != "m1:back" {
		t.Fatalf("hub -> member answered %q, want %q", got, "m1:back")
	}
}

func TestFleetHubRelaysALaptopToAMember(t *testing.T) {
	hub, member := startFleet(t)

	if _, err := member.say(t, netip.AddrPortFrom(hub.envIP, 8000), "hello"); err != nil {
		t.Fatalf("member -> hub: %v", err)
	}

	peer, priv := newPeer(t, "laptop", "10.86.0.2")
	if err := hub.dev.AddPeer(context.Background(), peer); err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	lap := startFleetPeer(t, hub, peer, priv)

	got, err := lap.say(t, netip.AddrPortFrom(member.envIP, 8000).String(), "hello")
	if err != nil {
		t.Fatalf("laptop -> member: %v", err)
	}
	if got != "m1:hello" {
		t.Fatalf("laptop -> member answered %q, want %q", got, "m1:hello")
	}
}

func TestFleetMemberResolverForwardsToTheHub(t *testing.T) {
	hub, member := startFleet(t)
	if _, err := member.say(t, netip.AddrPortFrom(hub.envIP, 8000), "hello"); err != nil {
		t.Fatalf("member -> hub: %v", err)
	}

	addrs, err := askOverTunnel(t, hub, netip.AddrPortFrom(member.dev.machineIP, ResolverPort),
		EnvHost("shop", "m1"))
	if err != nil {
		t.Fatalf("ask the member about its own name: %v", err)
	}
	if len(addrs) != 1 || addrs[0] != member.envIP {
		t.Fatalf("m1.shop.internal = %v, want %s", addrs, member.envIP)
	}

	addrs, err = askOverTunnel(t, hub, netip.AddrPortFrom(member.dev.machineIP, ResolverPort),
		EnvHost("shop", "hub"))
	if err != nil {
		t.Fatalf("ask the member about the hub's name: %v", err)
	}
	if len(addrs) != 1 || addrs[0] != hub.envIP {
		t.Fatalf("hub.shop.internal = %v, want %s (the member should have asked its hub)", addrs, hub.envIP)
	}

	if addrs, err := askOverTunnel(t, hub, netip.AddrPortFrom(member.dev.machineIP, ResolverPort),
		EnvHost("shop", "nope")); err != nil || len(addrs) != 0 {
		t.Fatalf("an unknown name answered %v (%v)", addrs, err)
	}
	if addrs, err := askOverTunnel(t, hub, netip.AddrPortFrom(member.dev.machineIP, ResolverPort),
		"example.com"); err != nil || len(addrs) != 0 {
		t.Fatalf("a name outside the tunnel answered %v (%v)", addrs, err)
	}
}

func askOverTunnel(t *testing.T, from *fleetBox, server netip.AddrPort, name string) ([]netip.Addr, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	return NewForwarder(from.dev.stk, server)(ctx, name)
}

func startFleetPeer(t *testing.T, hub *fleetBox, p Peer, priv Key) *testPeer {
	t.Helper()
	box := &testBox{pub: hub.pub, endpoint: hub.endpoint}
	return startPeerWith(t, box, p, priv, fleetRange)
}

func TestFleetPrivateIngressAnswersTheHubAlone(t *testing.T) {
	ctx := context.Background()
	hub, member := startFleet(t)
	if _, err := member.say(t, netip.AddrPortFrom(hub.envIP, 8000), "hello"); err != nil {
		t.Fatalf("member -> hub: %v", err)
	}

	edge := tcpEcho(t, "edge:")
	_, port, err := net.SplitHostPort(edge)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	route := IngressRoute(member.dev.machineIP, p, hubSubnet)
	if err := member.dev.SetAddressRoutes(ctx, member.dev.machineIP, []Route{route}); err != nil {
		t.Fatalf("SetAddressRoutes: %v", err)
	}

	ingress := netip.AddrPortFrom(member.dev.machineIP, IngressPort)
	got, err := hub.say(t, ingress, "GET /")
	if err != nil {
		t.Fatalf("hub -> the member's ingress: %v", err)
	}
	if got != "edge:GET /" {
		t.Fatalf("the ingress answered %q, want %q", got, "edge:GET /")
	}

	peer, priv := newPeer(t, "laptop", "10.87.0.2")
	if err := member.dev.AddPeer(ctx, peer); err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	lap := startPeerWith(t, &testBox{pub: member.pub, endpoint: member.endpoint}, peer, priv, m1Subnet)
	if _, err := lap.say(t, ingress.String(), "GET /"); err == nil {
		t.Fatal("a laptop on the member came through the hub's private ingress")
	}
}
