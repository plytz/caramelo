package env

import (
	"context"
	"testing"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/ports"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vpn"
)

func envRecord(ip string) *state.EnvRecord {
	return &state.EnvRecord{App: "shop", Name: "feat-x", PortBase: ports.Base, PortCount: 16, VPNIP: ip}
}

func TestEndpointsCarryBothViews(t *testing.T) {
	got, err := Endpoints(envRecord("10.86.1.4"), serviceConfig())
	if err != nil {
		t.Fatalf("Endpoints: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("endpoints = %+v, want web, echo, db and cache", got)
	}

	web, ok := FindEndpoint(got, "web")
	if !ok || web.Kind != KindService {
		t.Fatalf("web = %+v", web)
	}
	if web.URL != "http://127.0.0.1:20000" || web.InternalURL != "http://web.feat-x.shop.internal:20000" {
		t.Errorf("web urls = %q / %q", web.URL, web.InternalURL)
	}
	if web.Port != ports.Base || web.InternalPort != ports.Base {
		t.Errorf("web ports = %d / %d", web.Port, web.InternalPort)
	}
	if web.InternalAddress != "10.86.1.4:20000" {
		t.Errorf("web address = %q", web.InternalAddress)
	}

	echo, _ := FindEndpoint(got, "echo")
	if echo.Protocol != string(config.ProtocolUDP) || echo.InternalPort != 9999 || echo.Port != ports.Base+3 {
		t.Errorf("echo = %+v", echo)
	}
	if echo.InternalURL != "udp://echo.feat-x.shop.internal:9999" {
		t.Errorf("echo internal url = %q", echo.InternalURL)
	}

	db, ok := FindEndpoint(got, "db")
	if !ok || db.Kind != KindDep {
		t.Fatalf("db = %+v", db)
	}
	if db.Port != ports.Base+1 || db.InternalPort != 5432 {
		t.Errorf("db ports = %d / %d, want %d / 5432", db.Port, db.InternalPort, ports.Base+1)
	}
	if db.InternalHost != "db.feat-x.shop.internal" || db.InternalAddress != "10.86.1.4:5432" {
		t.Errorf("db endpoint = %+v", db)
	}
	if db.InternalURL != "tcp://db.feat-x.shop.internal:5432" || db.URL != "tcp://127.0.0.1:20001" {
		t.Errorf("db urls = %q / %q", db.InternalURL, db.URL)
	}
}

func TestEndpointsWithoutAnAddressHaveNoNames(t *testing.T) {
	got, err := Endpoints(envRecord(""), serviceConfig())
	if err != nil {
		t.Fatalf("Endpoints: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no endpoints")
	}
	for _, e := range got {
		if e.InternalHost != "" || e.InternalURL != "" || e.InternalAddress != "" || e.InternalPort != 0 {
			t.Errorf("%s: %+v", e.Name, e)
		}
		if e.URL == "" || e.Host != DepHost {
			t.Errorf("%s has no usable local address: %+v", e.Name, e)
		}
	}
}

func TestEndpointsSkipAPortlessService(t *testing.T) {
	cfg := serviceConfig()
	cfg.Services = append(cfg.Services, config.Service{Name: "worker", Run: "python worker.py", Port: config.PortNone})
	got, err := Endpoints(envRecord("10.86.1.4"), cfg)
	if err != nil {
		t.Fatalf("Endpoints: %v", err)
	}
	if _, ok := FindEndpoint(got, "worker"); ok {
		t.Errorf("a portless service was given an address: %+v", got)
	}
}

func TestURLsPicksOneOrAll(t *testing.T) {
	h := upHarness(t)
	h.withNetwork()
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	ctx := context.Background()

	all, err := h.m.URLs(ctx, "shop", "feat-x", "")
	if err != nil || len(all) != 4 {
		t.Fatalf("URLs = %+v, %v", all, err)
	}

	if all[0].Kind != KindService {
		t.Errorf("URLs starts with %+v, want a service", all[0])
	}
	first, ok := EnvURL(all)
	if !ok || first.Name != "web" {
		t.Errorf("EnvURL = %+v, %v", first, ok)
	}
	one, err := h.m.URLs(ctx, "shop", "feat-x", "cache")
	if err != nil || len(one) != 1 || one[0].Name != "cache" {
		t.Fatalf("URLs(cache) = %+v, %v", one, err)
	}
	if one[0].InternalURL != "tcp://cache.feat-x.shop.internal:6379" {
		t.Errorf("cache internal url = %q", one[0].InternalURL)
	}
	if _, err := h.m.URLs(ctx, "shop", "feat-x", "nope"); err == nil {
		t.Error("an unknown service was accepted")
	}
	if _, err := h.m.URLs(ctx, "shop", "gone", ""); err == nil {
		t.Error("an unknown env was accepted")
	}
}

func TestRoutesCoverEveryServiceAndDependency(t *testing.T) {
	routes := Routes(envRecord("10.86.1.4"), serviceConfig())
	if len(routes) != 4 {
		t.Fatalf("routes = %+v, want one per service and dependency", routes)
	}
	byName := map[string]vpn.Route{}
	for _, r := range routes {
		byName[r.Name] = r
	}
	if r := byName["web"]; r.Port != ports.Base || r.Protocol != vpn.TCP || r.Target != "127.0.0.1:20000" {
		t.Errorf("web route = %+v", r)
	}
	if r := byName["echo"]; r.Port != 9999 || r.Protocol != vpn.UDP || r.Target != "127.0.0.1:20003" {
		t.Errorf("echo route = %+v", r)
	}
	if r := byName["db"]; r.Port != 5432 || r.Protocol != vpn.TCP || r.Target != "127.0.0.1:20001" {
		t.Errorf("db route = %+v", r)
	}
	if r := byName["cache"]; r.Port != 6379 || r.Target != "127.0.0.1:20002" {
		t.Errorf("cache route = %+v", r)
	}
	for _, r := range routes {
		if r.App != "shop" || r.Env != "feat-x" || r.IP.String() != "10.86.1.4" {
			t.Errorf("route %s = %+v", r.Name, r)
		}
	}
}

func TestRoutesGiveAContestedPortToTheService(t *testing.T) {
	cfg := serviceConfig()
	cfg.Services[0].Port = 5432
	routes := Routes(envRecord("10.86.1.4"), cfg)
	var tcp5432 []vpn.Route
	for _, r := range routes {
		if r.Port == 5432 && r.Protocol == vpn.TCP {
			tcp5432 = append(tcp5432, r)
		}
	}
	if len(tcp5432) != 1 || tcp5432[0].Name != "web" {
		t.Fatalf("routes on 5432/tcp = %+v, want just the service", tcp5432)
	}
	if len(routes) != 3 {
		t.Errorf("routes = %+v, want the dependency dropped, not duplicated", routes)
	}
}

func TestRoutesKeepTcpAndUdpApartOnOnePort(t *testing.T) {
	cfg := serviceConfig()
	cfg.Services[1].Port = 5432
	routes := Routes(envRecord("10.86.1.4"), cfg)
	n := 0
	for _, r := range routes {
		if r.Port == 5432 {
			n++
		}
	}
	if n != 2 {
		t.Errorf("routes on 5432 = %d, want a tcp one and a udp one: %+v", n, routes)
	}
}

func TestRoutesAndAddressAreEmptyWithoutAnAddress(t *testing.T) {
	if got := Routes(envRecord(""), serviceConfig()); got != nil {
		t.Errorf("routes without an address = %+v", got)
	}
}

func TestAddressNamesCoverServicesAndDeps(t *testing.T) {
	addr := Address(envRecord("10.86.1.4"), serviceConfig())
	want := []string{
		"feat-x.shop.internal",
		"web.feat-x.shop.internal",
		"echo.feat-x.shop.internal",
		"db.feat-x.shop.internal",
		"cache.feat-x.shop.internal",
	}
	if len(addr.Names) != len(want) {
		t.Fatalf("names = %q, want %q", addr.Names, want)
	}
	for i, w := range want {
		if addr.Names[i] != w {
			t.Errorf("name %d = %q, want %q", i, addr.Names[i], w)
		}
	}
	if addr.IP.String() != "10.86.1.4" || addr.Owner != "shop/feat-x" {
		t.Errorf("address = %+v", addr)
	}
}
