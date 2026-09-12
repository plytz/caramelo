package edge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"feat-x.shop.test":      "feat-x.shop.test",
		"FEAT-X.Shop.Test":      "feat-x.shop.test",
		"feat-x.shop.test.":     "feat-x.shop.test",
		"feat-x.shop.test:443":  "feat-x.shop.test",
		" feat-x.shop.test ":    "feat-x.shop.test",
		"":                      "",
		"api.feat-x.shop.test.": "api.feat-x.shop.test",
	} {
		if got := NormalizeHost(in); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTableLookupIsCaseAndDotInsensitive(t *testing.T) {
	table := Table{Routes: []Route{{Host: "feat-x.shop.test", Kind: KindHTTPS, Env: "feat-x", Service: "web"}}}
	for _, host := range []string{"feat-x.shop.test", "FEAT-X.SHOP.TEST", "feat-x.shop.test.", "feat-x.shop.test:443"} {
		if _, ok := table.Route(host); !ok {
			t.Errorf("Route(%q) not found", host)
		}
		if !table.Has(host) {
			t.Errorf("Has(%q) = false", host)
		}
	}
	if table.Has("other.shop.test") {
		t.Error("a hostname the machine does not serve was found; that is what would let anyone make it ask for a certificate")
	}
}

func TestTablePutRemoveAndClone(t *testing.T) {
	var table Table
	table.Put(Route{Host: "FEAT-X.shop.test", Kind: KindHTTPS, Env: "feat-x", Service: "web",
		Targets: []Target{{Replica: 1, Port: 20002, State: TargetActive}}})
	table.Put(Route{Host: "feat-y.shop.test", Kind: KindHTTPS, Env: "feat-y", Service: "web"})

	table.Put(Route{Host: "feat-x.shop.test", Kind: KindHTTPS, Env: "feat-x", Service: "web",
		Targets: []Target{{Replica: 2, Port: 20003, State: TargetActive}}})
	if len(table.Routes) != 2 {
		t.Fatalf("routes = %+v, want 2", table.Routes)
	}
	if got := table.Routes[0].Targets[0].Replica; got != 2 {
		t.Errorf("first route's replica = %d, want the pushed one (2)", got)
	}
	if got := strings.Join(table.Hosts(), " "); got != "feat-x.shop.test feat-y.shop.test" {
		t.Errorf("Hosts() = %q", got)
	}

	clone := table.Clone()
	table.Routes[0].Targets[0].State = TargetDraining
	if clone.Routes[0].Targets[0].State != TargetActive {
		t.Error("Clone shares its targets with the table it came from")
	}

	if !table.Remove("FEAT-X.shop.test.") {
		t.Error("Remove did not find the route it was given in another spelling")
	}
	if table.Remove("feat-x.shop.test") {
		t.Error("Remove found a route that was already gone")
	}
	if len(table.Routes) != 1 {
		t.Errorf("routes = %+v, want 1", table.Routes)
	}
}

func TestTableValidate(t *testing.T) {
	ok := Table{Routes: []Route{{
		Host: "feat-x.shop.test", Kind: KindHTTPS, Env: "feat-x", Service: "web",
		Targets: []Target{
			{Replica: 1, Port: 20002, State: TargetActive},
			{Replica: 2, Port: 20003, State: TargetDraining},
		},
	}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	for name, table := range map[string]Table{
		"no host":                {Routes: []Route{{Kind: KindHTTPS}}},
		"two of them":            {Routes: []Route{{Host: "a.test", Kind: KindHTTPS}, {Host: "A.test.", Kind: KindHTTPS}}},
		"a kind that is not one": {Routes: []Route{{Host: "a.test", Kind: Kind("gopher")}}},
		"a reserved kind":        {Routes: []Route{{Host: "a.test", Kind: KindTCP}}},
		"a replica index of zero": {Routes: []Route{{Host: "a.test", Kind: KindHTTPS,
			Targets: []Target{{Replica: 0, Port: 20002, State: TargetActive}}}}},
		"one replica twice": {Routes: []Route{{Host: "a.test", Kind: KindHTTPS,
			Targets: []Target{{Replica: 1, Port: 20002, State: TargetActive}, {Replica: 1, Port: 20003, State: TargetActive}}}}},
		"a port out of range": {Routes: []Route{{Host: "a.test", Kind: KindHTTPS,
			Targets: []Target{{Replica: 1, Port: 0, State: TargetActive}}}}},
		"a state that is not one": {Routes: []Route{{Host: "a.test", Kind: KindHTTPS,
			Targets: []Target{{Replica: 1, Port: 20002, State: TargetState("warming")}}}}},
	} {
		if err := table.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want an error", name)
		}
	}
}

func TestRouteTargets(t *testing.T) {
	r := Route{Host: "feat-x.shop.test", Kind: KindHTTPS, Targets: []Target{
		{Replica: 1, Port: 20002, State: TargetDraining, Inflight: 3},
		{Replica: 2, Port: 20003, State: TargetActive, Inflight: 1},
		{Replica: 3, Port: 20004, State: TargetStarting},
	}}
	active := r.Active()
	if len(active) != 1 || active[0].Replica != 2 {
		t.Errorf("Active() = %+v, want only replica 2: nothing is sent to a starting or draining target", active)
	}
	if got := r.Inflight(); got != 4 {
		t.Errorf("Inflight() = %d, want 4 (the draining replica's requests still count)", got)
	}
	if _, ok := r.Target(9); ok {
		t.Error("Target(9) found a replica that is not there")
	}
	if got := active[0].Addr(); got != "127.0.0.1:20003" {
		t.Errorf("Addr() = %q, want the loopback port the replica publishes on", got)
	}
}

func TestKinds(t *testing.T) {
	if !KindHTTPS.Supported() {
		t.Error("https is what M6 serves")
	}
	for _, k := range []Kind{KindTLS, KindTLSPassthrough, KindTCP, KindUDP} {
		if !k.Known() {
			t.Errorf("%s is reserved, so the table has to know the name", k)
		}
		if k.Supported() {
			t.Errorf("%s is reserved, not served, in M6", k)
		}
	}
	if Kind("gopher").Known() {
		t.Error("an invented kind must not be known")
	}
}

func TestTableRoundTripsThroughJSON(t *testing.T) {
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	table := Table{UpdatedAt: at, Routes: []Route{{
		Host: "feat-x.shop.test", Kind: KindHTTPS, App: "shop", Env: "feat-x", Service: "web",
		CreatedAt: at,
		Targets: []Target{
			{Replica: 1, Port: 20002, State: TargetActive, Inflight: 2, Since: at},
			{Replica: 2, Port: 20003, State: TargetDraining, Since: at, Detail: "replaced by 3"},
		},
	}}}
	b, err := json.Marshal(table)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Table
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Routes) != 1 || len(got.Routes[0].Targets) != 2 {
		t.Fatalf("round trip = %+v", got)
	}
	if got.Routes[0].Targets[1].Detail != "replaced by 3" || !got.UpdatedAt.Equal(at) {
		t.Errorf("round trip lost something: %+v", got)
	}
}

func TestPaths(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{SocketPath("/run/caramelo"), "/run/caramelo/edge.sock"},
		{RoutesPath("/var/lib/caramelo"), "/var/lib/caramelo/edge/routes.json"},
		{CertsPath("/var/lib/caramelo"), "/var/lib/caramelo/edge/certs"},
	} {
		if tc.got != tc.want {
			t.Errorf("path = %q, want %q", tc.got, tc.want)
		}
	}
}

func TestViaRouteAndIngress(t *testing.T) {
	if !KindVia.Known() {
		t.Error("via is a kind the table has to know the name of")
	}
	if !KindVia.Supported() {
		t.Error("via is served: its targets are loopback ports like any other's")
	}

	err := Table{Routes: []Route{{Host: "feat-p.shop.test", Kind: KindVia}}}.Validate()
	if err == nil {
		t.Error("a via route with no machine was accepted")
	}

	err = Table{Routes: []Route{{Host: "feat-x.shop.test", Kind: KindHTTPS, Via: "m1"}}}.Validate()
	if err == nil {
		t.Error("an https route served by another machine was accepted")
	}
	if err := (Table{Routes: []Route{{Host: "feat-x.shop.test", Kind: KindHTTPS}}}).Validate(); err != nil {
		t.Errorf("M6's own route stopped validating: %v", err)
	}

	table := Table{
		Routes:  []Route{{Host: "feat-x.shop.test", Kind: KindHTTPS}},
		Ingress: &Ingress{Enabled: true, Port: IngressPort, Trusted: []string{"10.86.0.1"}},
	}
	b, err := json.Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	var back Table
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Ingress == nil || !back.Ingress.Enabled || back.Ingress.Port != IngressPort {
		t.Errorf("ingress did not survive the round trip: %s", b)
	}

	b, err = json.Marshal(Table{Routes: []Route{{Host: "a.test", Kind: KindHTTPS}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ingress") || strings.Contains(string(b), "via") {
		t.Errorf("a machine that serves its own names pushed %s", b)
	}
}
