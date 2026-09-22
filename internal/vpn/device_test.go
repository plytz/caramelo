package vpn

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestDevice(t *testing.T) (*wgDevice, *loopbackStack) {
	t.Helper()
	d, err := newDevice(Options{
		Subnet:         mustSubnet(t, DefaultSubnet),
		Listen:         DefaultListen,
		PrivateKeyPath: filepath.Join(t.TempDir(), "private.key"),
	})
	if err != nil {
		t.Fatalf("newDevice: %v", err)
	}
	l := newLoopbackStack()
	d.stk = l
	if err := d.AddAddress(context.Background(), Address{
		IP: d.machineIP, Kind: KindMachine, Owner: "worker1", Names: []string{MachineHost("worker1")},
	}); err != nil {
		t.Fatalf("AddAddress: %v", err)
	}
	return d, l
}

func TestNewDeviceChecksItsOptions(t *testing.T) {
	good := Options{Subnet: mustSubnet(t, DefaultSubnet), Listen: DefaultListen}
	if _, err := New(good); err != nil {
		t.Fatalf("New: %v", err)
	}
	for name, opts := range map[string]Options{
		"no subnet":     {Listen: DefaultListen},
		"tiny subnet":   {Subnet: netip.MustParsePrefix("10.86.0.0/28"), Listen: DefaultListen},
		"ipv6 subnet":   {Subnet: netip.MustParsePrefix("fd00::/64"), Listen: DefaultListen},
		"listen bare":   {Subnet: good.Subnet, Listen: "4021"},
		"listen noport": {Subnet: good.Subnet, Listen: "0.0.0.0:http-alt-alt"},
	} {
		if _, err := New(opts); err == nil {
			t.Errorf("New accepted %s", name)
		}
	}

	if _, err := New(Options{Subnet: good.Subnet}); err != nil {
		t.Errorf("New with no listen: %v", err)
	}
}

func TestADeviceReportsWhichTunnelItRuns(t *testing.T) {
	ctx := context.Background()

	down, _ := newTestDevice(t)
	down.stk = nil
	st, err := down.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Mode != ModeUserspace {
		t.Errorf("a device built with no mode reports %q while down, want %q", st.Mode, ModeUserspace)
	}

	priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "vpn", "private.key")
	if err := WritePrivateKey(keyPath, priv); err != nil {
		t.Fatal(err)
	}
	up, err := newDevice(Options{
		Subnet:         mustSubnet(t, DefaultSubnet),
		Listen:         "127.0.0.1:0",
		PrivateKeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("newDevice: %v", err)
	}
	if err := up.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	t.Cleanup(func() { _ = up.Down(context.Background()) })
	st, err = up.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Up {
		t.Fatalf("status = %+v, want the device up", st)
	}
	if st.Mode != ModeUserspace {
		t.Errorf("a device built with no mode reports %q while up, want %q", st.Mode, ModeUserspace)
	}
}

func TestNewRefusesAModeItCannotRun(t *testing.T) {
	_, err := New(Options{Subnet: mustSubnet(t, DefaultSubnet), Listen: DefaultListen, Mode: "kernel"})
	if err == nil {
		t.Fatal("New accepted a mode this build cannot run")
	}
	for _, want := range []string{`"kernel"`, string(ModeUserspace)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %s", err, want)
		}
	}
}

func TestStatusOfADeviceThatIsDown(t *testing.T) {
	d, _ := newTestDevice(t)
	d.stk = nil
	st, err := d.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Up {
		t.Error("a device that never came up reports itself up")
	}
	if st.IP.String() != "10.86.0.1" || st.Subnet.String() != "10.86.0.0/16" {
		t.Errorf("status = %+v, want the machine at 10.86.0.1 in 10.86.0.0/16", st)
	}
	if st.Listen != DefaultListen {
		t.Errorf("listen = %q, want %q", st.Listen, DefaultListen)
	}
}

func peerAt(t *testing.T, name, ip string) Peer {
	t.Helper()
	priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := priv.Public()
	if err != nil {
		t.Fatal(err)
	}
	return Peer{Name: name, PublicKey: pub.Base64(), IP: netip.MustParseAddr(ip)}
}

func TestAddPeerIsIdempotentAndRotatesKeys(t *testing.T) {
	ctx := context.Background()
	d, _ := newTestDevice(t)
	p := peerAt(t, "laptop", "10.86.0.2")

	for i := 0; i < 3; i++ {
		if err := d.AddPeer(ctx, p); err != nil {
			t.Fatalf("AddPeer %d: %v", i, err)
		}
	}
	got, err := d.Peers(ctx)
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	if len(got) != 1 || got[0].Name != "laptop" || got[0].PublicKey != p.PublicKey {
		t.Fatalf("peers = %+v, want one laptop", got)
	}

	rotated := peerAt(t, "laptop", "10.86.0.2")
	if err := d.AddPeer(ctx, rotated); err != nil {
		t.Fatalf("AddPeer rotated: %v", err)
	}
	got, _ = d.Peers(ctx)
	if len(got) != 1 || got[0].PublicKey != rotated.PublicKey {
		t.Fatalf("peers = %+v, want the rotated key only", got)
	}
}

func TestPeersAreSortedByName(t *testing.T) {
	ctx := context.Background()
	d, _ := newTestDevice(t)
	for _, p := range []Peer{
		peerAt(t, "laptop", "10.86.0.3"),
		peerAt(t, "agent-7", "10.86.0.2"),
		peerAt(t, "ci", "10.86.0.4"),
	} {
		if err := d.AddPeer(ctx, p); err != nil {
			t.Fatalf("AddPeer: %v", err)
		}
	}
	got, err := d.Peers(ctx)
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "agent-7,ci,laptop" {
		t.Fatalf("peers = %v, want them sorted by name", names)
	}
}

func TestAddPeerRefusesWhatItCannotAdmit(t *testing.T) {
	ctx := context.Background()
	d, _ := newTestDevice(t)
	good := peerAt(t, "laptop", "10.86.0.2")
	bad := map[string]Peer{
		"no name":         {PublicKey: good.PublicKey, IP: good.IP},
		"no key":          {Name: "x", IP: good.IP},
		"rubbish key":     {Name: "x", PublicKey: "not-a-key", IP: good.IP},
		"zero key":        {Name: "x", PublicKey: Key{}.Base64(), IP: good.IP},
		"no address":      {Name: "x", PublicKey: good.PublicKey},
		"address outside": {Name: "x", PublicKey: good.PublicKey, IP: netip.MustParseAddr("10.99.0.2")},
	}
	for name, p := range bad {
		if err := d.AddPeer(ctx, p); err == nil {
			t.Errorf("AddPeer accepted %s", name)
		}
	}
	if got, _ := d.Peers(ctx); len(got) != 0 {
		t.Errorf("peers = %+v, want none", got)
	}
}

func TestRemovePeerIsIdempotent(t *testing.T) {
	ctx := context.Background()
	d, _ := newTestDevice(t)
	p := peerAt(t, "laptop", "10.86.0.2")
	if err := d.AddPeer(ctx, p); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := d.RemovePeer(ctx, "laptop"); err != nil {
			t.Fatalf("RemovePeer %d: %v", i, err)
		}
	}
	if err := d.RemovePeer(ctx, "never-existed"); err != nil {
		t.Fatalf("RemovePeer of an unknown peer: %v", err)
	}
	if got, _ := d.Peers(ctx); len(got) != 0 {
		t.Errorf("peers = %+v, want none", got)
	}
}

func TestPeerAtIsTheIdentityBehindAnAddress(t *testing.T) {
	ctx := context.Background()
	d, _ := newTestDevice(t)
	p := peerAt(t, "agent-7", "10.86.0.5")
	if err := d.AddPeer(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, ok := d.PeerAt(ctx, netip.MustParseAddr("10.86.0.5"))
	if !ok || got.Name != "agent-7" {
		t.Fatalf("PeerAt = %+v, %v; want agent-7", got, ok)
	}
	if _, ok := d.PeerAt(ctx, netip.MustParseAddr("10.86.0.6")); ok {
		t.Error("an address no peer holds resolved to an identity")
	}
	if err := d.RemovePeer(ctx, "agent-7"); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.PeerAt(ctx, netip.MustParseAddr("10.86.0.5")); ok {
		t.Error("a revoked peer still has an identity")
	}
}

func envAddress(t *testing.T, ip, app, env string, names ...string) Address {
	t.Helper()
	return Address{
		IP:    netip.MustParseAddr(ip),
		Kind:  KindEnv,
		Owner: app + "/" + env,
		Names: EnvHosts(app, env, names),
	}
}

func TestAddressesReachTheStackAndTheZone(t *testing.T) {
	ctx := context.Background()
	d, l := newTestDevice(t)
	a := envAddress(t, "10.86.1.4", "shop", "feat-x", "web", "db")
	if err := d.AddAddress(ctx, a); err != nil {
		t.Fatalf("AddAddress: %v", err)
	}
	if !l.hasAddress(a.IP) {
		t.Error("the address never reached the network stack")
	}
	for _, name := range []string{"feat-x.shop.internal", "web.feat-x.shop.internal", "db.feat-x.shop.internal"} {
		got, err := d.resolver.Resolve(ctx, name)
		if err != nil {
			t.Errorf("Resolve(%s): %v", name, err)
			continue
		}
		if got[0] != a.IP {
			t.Errorf("Resolve(%s) = %v, want %s", name, got, a.IP)
		}
	}

	if err := d.AddAddress(ctx, a); err != nil {
		t.Fatalf("AddAddress twice: %v", err)
	}
	if got, _ := d.Addresses(ctx); len(got) != 2 {
		t.Fatalf("addresses = %+v, want the machine and the env", got)
	}
}

func TestRemoveAddressTakesItsNamesAndRoutesWithIt(t *testing.T) {
	ctx := context.Background()
	d, l := newTestDevice(t)
	a := envAddress(t, "10.86.1.4", "shop", "feat-x", "web")
	if err := d.AddAddress(ctx, a); err != nil {
		t.Fatal(err)
	}
	route := Route{IP: a.IP, Port: 8000, Protocol: TCP, Target: "127.0.0.1:20000", App: "shop", Env: "feat-x", Name: "web"}
	if err := d.SetRoutes(ctx, []Route{route}); err != nil {
		t.Fatalf("SetRoutes: %v", err)
	}
	if !l.live(route.AddrPort(), TCP) {
		t.Fatal("the route never opened a listener")
	}

	if err := d.RemoveAddress(ctx, a.IP); err != nil {
		t.Fatalf("RemoveAddress: %v", err)
	}
	if l.hasAddress(a.IP) {
		t.Error("the address is still on the network stack")
	}
	if _, err := d.resolver.Resolve(ctx, "feat-x.shop.internal"); !errors.Is(err, ErrNXDOMAIN) {
		t.Errorf("the name still resolves: %v", err)
	}
	if got, _ := d.Routes(ctx); len(got) != 0 {
		t.Errorf("routes = %+v, want none", got)
	}

	if err := d.RemoveAddress(ctx, a.IP); err != nil {
		t.Fatalf("RemoveAddress twice: %v", err)
	}
}

func TestTheMachinesOwnAddressCannotBeRemoved(t *testing.T) {
	d, _ := newTestDevice(t)
	if err := d.RemoveAddress(context.Background(), d.machineIP); err == nil {
		t.Fatal("the machine's own address was removed, which would take the API with it")
	}
}

func TestSetAddressesReplacesTheTable(t *testing.T) {
	ctx := context.Background()
	d, l := newTestDevice(t)
	machine := Address{IP: d.machineIP, Kind: KindMachine, Owner: "worker1", Names: []string{MachineHost("worker1")}}
	one := envAddress(t, "10.86.1.1", "shop", "feat-x", "web")
	two := envAddress(t, "10.86.1.2", "shop", "feat-y", "web")
	if err := d.SetAddresses(ctx, []Address{machine, one, two}); err != nil {
		t.Fatalf("SetAddresses: %v", err)
	}
	if got, _ := d.Addresses(ctx); len(got) != 3 {
		t.Fatalf("addresses = %+v, want three", got)
	}
	if err := d.SetAddresses(ctx, []Address{machine, two}); err != nil {
		t.Fatalf("SetAddresses: %v", err)
	}
	got, _ := d.Addresses(ctx)
	if len(got) != 2 || got[1].IP != two.IP {
		t.Fatalf("addresses = %+v, want the machine and feat-y", got)
	}
	if l.hasAddress(one.IP) {
		t.Error("the dropped address is still on the network stack")
	}
	if _, err := d.resolver.Resolve(ctx, "feat-x.shop.internal"); !errors.Is(err, ErrNXDOMAIN) {
		t.Errorf("the dropped env still resolves: %v", err)
	}
}

func TestAddressesAreCheckedBeforeAnythingChanges(t *testing.T) {
	ctx := context.Background()
	d, _ := newTestDevice(t)
	for name, a := range map[string]Address{
		"no address":   {Kind: KindEnv, Owner: "shop/x"},
		"outside":      {IP: netip.MustParseAddr("10.99.1.1"), Kind: KindEnv},
		"foreign name": {IP: netip.MustParseAddr("10.86.1.1"), Kind: KindEnv, Names: []string{"feat-x.shop.example.com"}},
	} {
		if err := d.AddAddress(ctx, a); err == nil {
			t.Errorf("AddAddress accepted %s", name)
		}
		if err := d.SetAddresses(ctx, []Address{a}); err == nil {
			t.Errorf("SetAddresses accepted %s", name)
		}
	}

	if got, _ := d.Addresses(ctx); len(got) != 1 {
		t.Fatalf("addresses = %+v, want just the machine", got)
	}
}

func TestSetRoutesOpensAndClosesListeners(t *testing.T) {
	ctx := context.Background()
	d, l := newTestDevice(t)
	a := envAddress(t, "10.86.1.4", "shop", "feat-x", "web", "db", "echo")
	if err := d.AddAddress(ctx, a); err != nil {
		t.Fatal(err)
	}
	web := Route{IP: a.IP, Port: 8000, Protocol: TCP, Target: "127.0.0.1:20000", Name: "web"}
	db := Route{IP: a.IP, Port: 5432, Protocol: TCP, Target: "127.0.0.1:20001", Name: "db"}
	echo := Route{IP: a.IP, Port: 5533, Protocol: UDP, Target: "127.0.0.1:20002", Name: "echo"}
	if err := d.SetRoutes(ctx, []Route{web, db, echo}); err != nil {
		t.Fatalf("SetRoutes: %v", err)
	}
	got, _ := d.Routes(ctx)
	if len(got) != 3 {
		t.Fatalf("routes = %+v, want three", got)
	}

	if got[0].Port != 5432 || got[1].Port != 5533 || got[2].Port != 8000 {
		t.Errorf("routes are not sorted: %+v", got)
	}

	before := l.addrOf(t, web.AddrPort(), TCP)
	if err := d.SetRoutes(ctx, []Route{web, db, echo}); err != nil {
		t.Fatalf("SetRoutes again: %v", err)
	}
	if after := l.addrOf(t, web.AddrPort(), TCP); after != before {
		t.Errorf("the listener was restarted (%s -> %s) for an unchanged route", before, after)
	}

	moved := web
	moved.Target = "127.0.0.1:20009"
	if err := d.SetRoutes(ctx, []Route{moved}); err != nil {
		t.Fatalf("SetRoutes moved: %v", err)
	}
	if l.live(db.AddrPort(), TCP) || l.live(echo.AddrPort(), UDP) {
		t.Error("a route that is gone still has a listener")
	}
	got, _ = d.Routes(ctx)
	if len(got) != 1 || got[0].Target != "127.0.0.1:20009" {
		t.Fatalf("routes = %+v, want only the moved web route", got)
	}
}

func TestSetAddressRoutesLeavesOtherAddressesAlone(t *testing.T) {
	ctx := context.Background()
	d, l := newTestDevice(t)
	x := envAddress(t, "10.86.1.1", "shop", "feat-x", "web")
	y := envAddress(t, "10.86.1.2", "shop", "feat-y", "web")
	if err := d.SetAddresses(ctx, []Address{
		{IP: d.machineIP, Kind: KindMachine, Names: []string{MachineHost("worker1")}}, x, y,
	}); err != nil {
		t.Fatal(err)
	}
	rx := Route{IP: x.IP, Port: 8000, Protocol: TCP, Target: "127.0.0.1:20000"}
	ry := Route{IP: y.IP, Port: 8000, Protocol: TCP, Target: "127.0.0.1:20016"}
	if err := d.SetRoutes(ctx, []Route{rx, ry}); err != nil {
		t.Fatal(err)
	}
	yAddr := l.addrOf(t, ry.AddrPort(), TCP)

	if err := d.SetAddressRoutes(ctx, x.IP, nil); err != nil {
		t.Fatalf("SetAddressRoutes: %v", err)
	}
	if l.live(rx.AddrPort(), TCP) {
		t.Error("feat-x still has a listener")
	}
	if !l.live(ry.AddrPort(), TCP) {
		t.Fatal("feat-y lost its listener")
	}
	if after := l.addrOf(t, ry.AddrPort(), TCP); after != yAddr {
		t.Errorf("feat-y's listener was restarted (%s -> %s)", yAddr, after)
	}
	got, _ := d.Routes(ctx)
	if len(got) != 1 || got[0].IP != y.IP {
		t.Fatalf("routes = %+v, want feat-y's only", got)
	}
}

func TestSetAddressRoutesRefusesAForeignRoute(t *testing.T) {
	ctx := context.Background()
	d, _ := newTestDevice(t)
	x := envAddress(t, "10.86.1.1", "shop", "feat-x", "web")
	if err := d.AddAddress(ctx, x); err != nil {
		t.Fatal(err)
	}
	err := d.SetAddressRoutes(ctx, x.IP, []Route{{
		IP: netip.MustParseAddr("10.86.1.2"), Port: 80, Protocol: TCP, Target: "127.0.0.1:20000",
	}})
	if err == nil {
		t.Fatal("a route belonging to another address was accepted")
	}
}

func TestRoutesAreCheckedAgainstTheAddressTable(t *testing.T) {
	ctx := context.Background()
	d, _ := newTestDevice(t)
	err := d.SetRoutes(ctx, []Route{{
		IP: netip.MustParseAddr("10.86.1.9"), Port: 80, Protocol: TCP, Target: "127.0.0.1:20000",
	}})
	if err == nil {
		t.Fatal("a route on an address the machine does not hold was accepted")
	}
	if !strings.Contains(err.Error(), "10.86.1.9") {
		t.Errorf("error %q does not name the address", err)
	}

	a := envAddress(t, "10.86.1.4", "shop", "feat-x", "web")
	if err := d.AddAddress(ctx, a); err != nil {
		t.Fatal(err)
	}
	err = d.SetRoutes(ctx, []Route{
		{IP: a.IP, Port: 8000, Protocol: TCP, Target: "127.0.0.1:20000"},
		{IP: a.IP, Port: 8000, Protocol: TCP, Target: "127.0.0.1:20001"},
	})
	if err == nil {
		t.Fatal("the same port was accepted twice with different targets")
	}
}

func TestRoutesSurviveDownAndComeBackUp(t *testing.T) {

	ctx := context.Background()
	d, l := newTestDevice(t)
	a := envAddress(t, "10.86.1.4", "shop", "feat-x", "web")
	if err := d.AddAddress(ctx, a); err != nil {
		t.Fatal(err)
	}
	r := Route{IP: a.IP, Port: 8000, Protocol: TCP, Target: "127.0.0.1:20000"}
	if err := d.SetRoutes(ctx, []Route{r}); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.downLocked()
	d.mu.Unlock()
	if l.live(r.AddrPort(), TCP) {
		t.Error("a listener survived the device going down")
	}
	got, _ := d.Routes(ctx)
	if len(got) != 1 {
		t.Fatalf("routes = %+v, want the table to be remembered", got)
	}

	if err := d.SetRoutes(ctx, []Route{r, {IP: a.IP, Port: 5432, Protocol: TCP, Target: "127.0.0.1:20001"}}); err != nil {
		t.Fatalf("SetRoutes while down: %v", err)
	}
	if got, _ := d.Routes(ctx); len(got) != 2 {
		t.Fatalf("routes = %+v, want two", got)
	}
}

func TestListenNeedsADeviceThatIsUpAndAnAddressItHolds(t *testing.T) {
	ctx := context.Background()
	d, _ := newTestDevice(t)
	if _, err := d.Listen(ctx, netip.MustParseAddrPort("10.86.9.9:4022")); err == nil {
		t.Error("listening on an address the machine does not hold succeeded")
	}
	ln, err := d.Listen(ctx, netip.AddrPortFrom(d.machineIP, 4022))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ln.Close()

	d.mu.Lock()
	d.downLocked()
	d.mu.Unlock()
	if _, err := d.Listen(ctx, netip.AddrPortFrom(d.machineIP, 4022)); err == nil {
		t.Error("listening on a device that is down succeeded")
	}
}

func TestSetAddressesKeepsTheMachineOnItsOwnNetwork(t *testing.T) {
	ctx := context.Background()
	d, l := newTestDevice(t)
	if err := d.SetAddresses(ctx, []Address{envAddress(t, "10.86.1.1", "shop", "feat-x", "web")}); err != nil {
		t.Fatalf("SetAddresses: %v", err)
	}
	got, _ := d.Addresses(ctx)
	if len(got) != 2 || got[0].IP != d.machineIP {
		t.Fatalf("addresses = %+v, want the machine kept", got)
	}
	if _, err := d.resolver.Resolve(ctx, "worker1.internal"); err != nil {
		t.Errorf("the machine lost its name: %v", err)
	}
	if !l.hasAddress(netip.MustParseAddr("10.86.1.1")) {
		t.Error("the new address never reached the network stack")
	}
}

func TestARouteWhoseTargetMovedKeepsItsListener(t *testing.T) {
	ctx := context.Background()
	d, l := newTestDevice(t)
	lingering := newLingeringStack(l)
	d.stk = lingering
	x := envAddress(t, "10.86.1.1", "shop", "feat-x", "web")
	if err := d.AddAddress(ctx, x); err != nil {
		t.Fatal(err)
	}
	one, two := tcpEcho(t, "one:"), tcpEcho(t, "two:")
	before := Route{IP: x.IP, Port: 8000, Protocol: TCP, Target: one}
	if err := d.SetAddressRoutes(ctx, x.IP, []Route{before}); err != nil {
		t.Fatalf("SetAddressRoutes: %v", err)
	}
	listener := l.addrOf(t, before.AddrPort(), TCP)

	held, err := net.Dial("tcp", listener)
	if err != nil {
		t.Fatalf("dial through the relay: %v", err)
	}
	defer held.Close()
	if got := exchange(t, held, "a"); got != "one:a" {
		t.Fatalf("the relay answered %q, want the first target", got)
	}

	lingering.holds(before.AddrPort(), 10)
	after := before
	after.Target = two
	if err := d.SetAddressRoutes(ctx, x.IP, []Route{after}); err != nil {
		t.Fatalf("the route could not move: %v", err)
	}
	if got := l.addrOf(t, after.AddrPort(), TCP); got != listener {
		t.Errorf("the listener was reopened (%s -> %s) for a route that only moved", listener, got)
	}

	fresh, err := net.Dial("tcp", listener)
	if err != nil {
		t.Fatalf("dial after the move: %v", err)
	}
	defer fresh.Close()
	if got := exchange(t, fresh, "b"); got != "two:b" {
		t.Errorf("the relay answered %q after the move, want the new target", got)
	}
	if got := exchange(t, held, "c"); got != "one:c" {
		t.Errorf("a connection open through the move answered %q, want the target it was made against", got)
	}
	got, _ := d.Routes(ctx)
	if len(got) != 1 || got[0].Target != two {
		t.Fatalf("routes = %+v, want the one that moved to %s", got, two)
	}
}

func TestAPortThatWentAwayAndCameBackWaitsForTheStack(t *testing.T) {
	ctx := context.Background()
	d, l := newTestDevice(t)
	lingering := newLingeringStack(l)
	d.stk = lingering
	x := envAddress(t, "10.86.1.1", "shop", "feat-x", "web")
	if err := d.AddAddress(ctx, x); err != nil {
		t.Fatal(err)
	}
	r := Route{IP: x.IP, Port: 8000, Protocol: TCP, Target: "127.0.0.1:20000"}
	if err := d.SetAddressRoutes(ctx, x.IP, []Route{r}); err != nil {
		t.Fatalf("SetAddressRoutes: %v", err)
	}
	if err := d.SetAddressRoutes(ctx, x.IP, nil); err != nil {
		t.Fatalf("SetAddressRoutes down: %v", err)
	}
	lingering.holds(r.AddrPort(), 3)
	if err := d.SetAddressRoutes(ctx, x.IP, []Route{r}); err != nil {
		t.Fatalf("the route did not wait for its own port: %v", err)
	}
	if !l.live(r.AddrPort(), TCP) {
		t.Fatal("the port is not listened on after it came back")
	}
}

func exchange(t *testing.T, c net.Conn, line string) string {
	t.Helper()
	if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte(line)); err != nil {
		t.Fatalf("write to the relay: %v", err)
	}
	buf := make([]byte, 256)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read from the relay: %v", err)
	}
	return string(buf[:n])
}
