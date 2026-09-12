package env

import (
	"context"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/plytz/caramelo/internal/ports"
	"github.com/plytz/caramelo/internal/vpn"
)

type fakeNet struct {
	mu     sync.Mutex
	addrs  map[netip.Addr]vpn.Address
	routes map[netip.Addr][]vpn.Route

	setCount map[netip.Addr]int

	addErr error

	removeErr error
	removed   []netip.Addr
}

func newNet() *fakeNet {
	return &fakeNet{addrs: map[netip.Addr]vpn.Address{}, routes: map[netip.Addr][]vpn.Route{}}
}

func (n *fakeNet) AddAddress(ctx context.Context, a vpn.Address) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.addErr; err != nil {
		n.addErr = nil
		return err
	}
	n.addrs[a.IP] = a
	return nil
}

func (n *fakeNet) RemoveAddress(ctx context.Context, ip netip.Addr) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.removeErr != nil {
		return n.removeErr
	}
	n.removed = append(n.removed, ip)
	delete(n.addrs, ip)
	delete(n.routes, ip)
	return nil
}

func (n *fakeNet) SetAddressRoutes(ctx context.Context, ip netip.Addr, routes []vpn.Route) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.routes[ip] = routes
	if n.setCount == nil {
		n.setCount = map[netip.Addr]int{}
	}
	n.setCount[ip]++
	return nil
}

func (n *fakeNet) Addresses(ctx context.Context) ([]vpn.Address, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]vpn.Address, 0, len(n.addrs))
	for _, a := range n.addrs {
		out = append(out, a)
	}
	return out, nil
}

func (n *fakeNet) Routes(ctx context.Context) ([]vpn.Route, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []vpn.Route
	for _, rs := range n.routes {
		out = append(out, rs...)
	}
	return out, nil
}

func (n *fakeNet) sets(ip string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.setCount[netip.MustParseAddr(ip)]
}

func (n *fakeNet) address(ip string) (vpn.Address, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	a, ok := n.addrs[netip.MustParseAddr(ip)]
	return a, ok
}

func (n *fakeNet) routesOf(ip string) []vpn.Route {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.routes[netip.MustParseAddr(ip)]
}

func (h *harness) withNetwork() *fakeNet {
	h.t.Helper()
	net := newNet()
	h.m.VPNAlloc = vpn.NewAllocator(netip.MustParsePrefix(vpn.DefaultSubnet), h.store)
	h.m.Net = net
	return net
}

func TestCreateAllocatesAnAddressAndPublishesIt(t *testing.T) {
	h := newHarness(t)
	net := h.withNetwork()

	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	if e.VPNIP != "10.86.1.1" {
		t.Fatalf("address = %q, want 10.86.1.1", e.VPNIP)
	}
	rec, err := h.store.Env(context.Background(), "shop", "feat-x")
	if err != nil || rec.VPNIP != e.VPNIP {
		t.Fatalf("the row does not carry the address: %+v, %v", rec, err)
	}

	addr, ok := net.address("10.86.1.1")
	if !ok {
		t.Fatal("the address was not installed on the device")
	}
	want := []string{"feat-x.shop.internal", "db.feat-x.shop.internal", "cache.feat-x.shop.internal"}
	if strings.Join(addr.Names, ",") != strings.Join(want, ",") {
		t.Errorf("names = %v, want %v", addr.Names, want)
	}
	if addr.Kind != vpn.KindEnv || addr.Owner != "shop/feat-x" {
		t.Errorf("address = %+v", addr)
	}

	routes := net.routesOf("10.86.1.1")
	if len(routes) != 2 {
		t.Fatalf("routes = %+v", routes)
	}
	base := rec.PortBase
	for i, want := range []vpn.Route{
		{IP: netip.MustParseAddr("10.86.1.1"), Port: 5432, Protocol: vpn.TCP,
			Target: "127.0.0.1:" + strconv.Itoa(base+1), App: "shop", Env: "feat-x", Name: "db"},
		{IP: netip.MustParseAddr("10.86.1.1"), Port: 6379, Protocol: vpn.TCP,
			Target: "127.0.0.1:" + strconv.Itoa(base+2), App: "shop", Env: "feat-x", Name: "cache"},
	} {
		if routes[i] != want {
			t.Errorf("route %d = %+v, want %+v", i, routes[i], want)
		}
	}
	if !strings.Contains(h.out.String(), "feat-x.shop.internal") {
		t.Errorf("the progress stream never mentions the env's name:\n%s", h.out.String())
	}
}

func TestCreateWithoutANetworkIsUnchanged(t *testing.T) {
	h := newHarness(t)
	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	if e.VPNIP != "" {
		t.Errorf("a machine with no network gave the env address %q", e.VPNIP)
	}
	rec, err := h.store.Env(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatal(err)
	}

	eps, err := Endpoints(rec, sampleConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range eps {
		if ep.InternalHost != "" || ep.InternalURL != "" || ep.InternalAddress != "" {
			t.Errorf("endpoint %s claims a tunnel address: %+v", ep.Name, ep)
		}
		if ep.URL == "" {
			t.Errorf("endpoint %s has no address on the box", ep.Name)
		}
	}
	if err := h.m.Destroy(context.Background(), DestroyRequest{App: "shop", Name: "feat-x", DeleteBranch: false}, &h.out); err != nil {
		t.Fatalf("Destroy without a network: %v", err)
	}
}

func TestTwoEnvsGetDistinctAddresses(t *testing.T) {
	h := newHarness(t)
	h.withNetwork()
	x := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	y := h.mustCreate(CreateRequest{App: "shop", Name: "feat-y"})
	if x.VPNIP == y.VPNIP {
		t.Fatalf("both envs got %s", x.VPNIP)
	}
	if x.VPNIP != "10.86.1.1" || y.VPNIP != "10.86.1.2" {
		t.Errorf("addresses = %s, %s", x.VPNIP, y.VPNIP)
	}
}

func TestCreateSkipsAnAddressAPeerAlreadyHolds(t *testing.T) {
	h := newHarness(t)
	h.withNetwork()

	h.store.peers = []string{"10.86.0.2", "10.86.1.1"}
	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	if e.VPNIP != "10.86.1.2" {
		t.Errorf("address = %q, want 10.86.1.2 (10.86.1.1 is taken)", e.VPNIP)
	}
}

func TestCreateRetriesWhenTheAddressWasTakenBetweenTheTwoSteps(t *testing.T) {
	h := newHarness(t)
	net := newNet()
	h.m.Net = net

	alloc := &fixedAddrs{addrs: []string{"10.86.1.1", "10.86.1.1", "10.86.1.7"}}
	h.m.VPNAlloc = alloc

	x := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	y := h.mustCreate(CreateRequest{App: "shop", Name: "feat-y"})
	if x.VPNIP != "10.86.1.1" || y.VPNIP != "10.86.1.7" {
		t.Fatalf("addresses = %s, %s", x.VPNIP, y.VPNIP)
	}
	if alloc.calls != 3 {
		t.Errorf("the allocator was asked %d times, want 3 (the second create lost once)", alloc.calls)
	}
}

type fixedAddrs struct {
	addrs []string
	calls int
}

func (a *fixedAddrs) AllocatePeer(context.Context, string) (netip.Addr, error) {
	return netip.Addr{}, vpn.ErrExhausted
}

func (a *fixedAddrs) AllocateEnv(context.Context, int64) (netip.Addr, error) {
	if a.calls >= len(a.addrs) {
		return netip.Addr{}, vpn.ErrExhausted
	}
	ip := netip.MustParseAddr(a.addrs[a.calls])
	a.calls++
	return ip, nil
}

func (a *fixedAddrs) Release(context.Context, netip.Addr) error { return nil }

func TestDestroyTakesTheAddressOffTheDeviceAndFreesIt(t *testing.T) {
	h := newHarness(t)
	net := h.withNetwork()
	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	if err := h.m.Destroy(context.Background(), DestroyRequest{App: "shop", Name: "feat-x", DeleteBranch: false}, &h.out); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, ok := net.address(e.VPNIP); ok {
		t.Error("the address is still on the device: its names would still resolve")
	}
	if len(net.removed) != 1 || net.removed[0].String() != e.VPNIP {
		t.Errorf("removed = %v", net.removed)
	}

	again := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	if again.VPNIP != e.VPNIP {
		t.Errorf("the freed address was not reused: %s, want %s", again.VPNIP, e.VPNIP)
	}
}

func TestDestroyReportsADeviceThatWillNotReleaseTheAddress(t *testing.T) {
	h := newHarness(t)
	net := h.withNetwork()
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	net.removeErr = errors.New("device is down")

	err := h.m.Destroy(context.Background(), DestroyRequest{App: "shop", Name: "feat-x", DeleteBranch: false}, &h.out)
	if err == nil || !strings.Contains(err.Error(), "device is down") {
		t.Fatalf("err = %v, want it to name the device failure", err)
	}

	if got := h.status("feat-x"); got != "destroying" {
		t.Errorf("status = %q, want destroying", got)
	}
}

func TestADeviceThatRefusesTheAddressDoesNotFailTheCreate(t *testing.T) {
	h := newHarness(t)
	net := h.withNetwork()
	net.addErr = errors.New("device is down")

	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	if e.Status != StatusReady {
		t.Fatalf("status = %q, want ready: the env itself is fine", e.Status)
	}
	if !strings.Contains(h.out.String(), "warning") || !strings.Contains(h.out.String(), "device is down") {
		t.Errorf("the failure was not reported:\n%s", h.out.String())
	}

	if e.VPNIP == "" {
		t.Error("the env lost its address because the device was down")
	}
}

func TestRepublishRebuildsTheTablesFromTheRows(t *testing.T) {
	h := newHarness(t)
	net := h.withNetwork()
	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	fresh := newNet()
	h.m.Net = fresh
	if err := h.m.Republish(context.Background()); err != nil {
		t.Fatalf("Republish: %v", err)
	}
	addr, ok := fresh.address(e.VPNIP)
	if !ok || len(addr.Names) != 3 {
		t.Fatalf("address after a restart: %+v, %v", addr, ok)
	}
	if len(fresh.routesOf(e.VPNIP)) != 2 {
		t.Errorf("routes after a restart: %+v", fresh.routesOf(e.VPNIP))
	}
	if _, ok := net.address(e.VPNIP); !ok {
		t.Error("the first device lost the address it was given")
	}
}

func TestUpPublishesTheServicesOnTheEnvsAddress(t *testing.T) {
	h := upHarness(t)
	net := h.withNetwork()
	h.cfg = sampleConfig()
	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	if got := len(net.routesOf(e.VPNIP)); got != 2 {
		t.Fatalf("create relayed %d ports, want the two dependencies", got)
	}

	h.cfg = serviceConfig()
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	routes := net.routesOf(e.VPNIP)
	if len(routes) != 4 {
		t.Fatalf("routes = %+v, want one per service and dependency", routes)
	}
	byName := map[string]vpn.Route{}
	for _, r := range routes {
		byName[r.Name] = r
	}
	if r := byName["web"]; r.Port != ports.Base || r.Protocol != vpn.TCP {
		t.Errorf("web route = %+v", r)
	}
	if r := byName["echo"]; r.Port != 9999 || r.Protocol != vpn.UDP {
		t.Errorf("echo route = %+v, want its own port on udp", r)
	}
	addr, ok := net.address(e.VPNIP)
	if !ok {
		t.Fatal("the env has no address entry")
	}
	if len(addr.Names) != 5 {
		t.Errorf("names = %q, want the env, both services and both dependencies", addr.Names)
	}
	if !strings.Contains(h.out.String(), "feat-x.shop.internal") {
		t.Errorf("up said nothing about the network:\n%s", h.out.String())
	}
}

func TestUpLeavesAnUnchangedTableAlone(t *testing.T) {
	h := upHarness(t)
	net := h.withNetwork()
	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	before := net.sets(e.VPNIP)

	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if got := net.sets(e.VPNIP); got != before {
		t.Errorf("the relay table was rewritten %d time(s) by an up that changed nothing", got-before)
	}
}

func TestDownLeavesTheEnvsPortsInPlace(t *testing.T) {
	h := upHarness(t)
	net := h.withNetwork()
	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	before, sets := net.routesOf(e.VPNIP), net.sets(e.VPNIP)

	if _, err := h.m.Down(context.Background(), DownRequest{App: "shop", Name: "feat-x"}, &h.out); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if after := net.routesOf(e.VPNIP); len(after) != len(before) {
		t.Errorf("down changed the relay table: %+v, was %+v", after, before)
	}
	if got := net.sets(e.VPNIP); got != sets {
		t.Errorf("down rewrote the relay table %d time(s); nothing about the env changed", got-sets)
	}
}

func TestDownRestoresATableTheDeviceLost(t *testing.T) {
	h := upHarness(t)
	net := h.withNetwork()
	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	if err := net.RemoveAddress(context.Background(), netip.MustParseAddr(e.VPNIP)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Down(context.Background(), DownRequest{App: "shop", Name: "feat-x"}, &h.out); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if got := len(net.routesOf(e.VPNIP)); got != 4 {
		t.Errorf("down relayed %d ports, want the env's four back", got)
	}
}

func TestUpWithoutANetworkIsUnchanged(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if len(res.Services) != 2 {
		t.Errorf("up started %d service(s) without a network", len(res.Services))
	}
	if strings.Contains(h.out.String(), "internal") {
		t.Errorf("up mentioned a network this machine has not got:\n%s", h.out.String())
	}
}
