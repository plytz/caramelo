package edge

import (
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func ingressPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func askIngress(t *testing.T, te *testEdge, host, path string, headers map[string]string) *http.Response {
	t.Helper()
	addr := te.e.ingressAddr()
	if addr == "" {
		t.Fatal("the private ingress is not listening")
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatalf("ingress request: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestPrivateIngressServesWithoutRedirectingAndKeepsTheHubsClient(t *testing.T) {
	te := newTestEdge(t, nil)
	r1 := newReplica(t, 1)
	port := ingressPort(t)

	te.push(t, Table{
		Routes: []Route{{
			Host: "feat-p.shop.test", Kind: KindHTTPS, App: "shop", Env: "feat-p", Service: "web",
			Targets: []Target{r1.target(TargetActive)},
		}},
		Ingress: &Ingress{Enabled: true, Port: port, Trusted: []string{"10.86.0.1"}},
	})

	resp := askIngress(t, te, "feat-p.shop.test", "/", map[string]string{
		"X-Forwarded-For":   "203.0.113.7",
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "feat-p.shop.test",
		"X-Real-IP":         "203.0.113.7",
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the ingress must not redirect)", resp.StatusCode)
	}

	reqs := r1.requests()
	if len(reqs) != 1 {
		t.Fatalf("the replica saw %d requests, want 1", len(reqs))
	}
	got := reqs[0]

	if xff := got.Header.Get("X-Forwarded-For"); xff != "203.0.113.7" {
		t.Errorf("X-Forwarded-For = %q, want the client the hub saw", xff)
	}
	if p := got.Header.Get("X-Forwarded-Proto"); p != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want https", p)
	}
	if h := got.Header.Get("X-Forwarded-Host"); h != "feat-p.shop.test" {
		t.Errorf("X-Forwarded-Host = %q, want the public name", h)
	}
	if ip := got.Header.Get("X-Real-IP"); ip != "203.0.113.7" {
		t.Errorf("X-Real-IP = %q, want the client the hub measured, not the relay's loopback", ip)
	}
	if got.Host != "feat-p.shop.test" {
		t.Errorf("Host = %q, want the name the client asked for", got.Host)
	}

	st, err := te.e.Status(t.Context())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Ingress == nil || !st.Ingress.Enabled || st.Ingress.Requests != 1 {
		t.Fatalf("ingress status = %+v, want one request through it", st.Ingress)
	}
	if st.Ingress.Addr == "" {
		t.Error("the ingress reports no address, so caramelod could not point its relay at it")
	}
}

func TestPrivateIngressThatTrustsNobodyBelievesNothing(t *testing.T) {
	te := newTestEdge(t, nil)
	r1 := newReplica(t, 1)
	te.push(t, Table{
		Routes: []Route{{
			Host: "feat-p.shop.test", Kind: KindHTTPS, App: "shop", Env: "feat-p", Service: "web",
			Targets: []Target{r1.target(TargetActive)},
		}},
		Ingress: &Ingress{Enabled: true, Port: ingressPort(t)},
	})
	askIngress(t, te, "feat-p.shop.test", "/", map[string]string{
		"X-Forwarded-For": "203.0.113.7",
		"X-Real-IP":       "203.0.113.7",
	})
	reqs := r1.requests()
	if len(reqs) != 1 {
		t.Fatalf("the replica saw %d requests, want 1", len(reqs))
	}
	if ip := reqs[0].Header.Get("X-Real-IP"); ip == "203.0.113.7" {
		t.Errorf("X-Real-IP = %q: an ingress that trusts nobody believed the caller", ip)
	}
	if xff := reqs[0].Header.Get("X-Forwarded-For"); xff == "203.0.113.7" {
		t.Errorf("X-Forwarded-For = %q: an ingress that trusts nobody believed the caller", xff)
	}
}

func TestPrivateIngressFollowsTheTable(t *testing.T) {
	te := newTestEdge(t, nil)
	r1 := newReplica(t, 1)
	route := Route{Host: "feat-p.shop.test", Kind: KindHTTPS, App: "shop", Env: "feat-p", Service: "web",
		Targets: []Target{r1.target(TargetActive)}}

	te.push(t, Table{Routes: []Route{route}})
	if addr := te.e.ingressAddr(); addr != "" {
		t.Fatalf("an edge with no ingress in its table is listening on %s", addr)
	}
	st, _ := te.e.Status(t.Context())
	if st.Ingress != nil {
		t.Errorf("status carries an ingress %+v on a machine that has none", st.Ingress)
	}

	first := ingressPort(t)
	te.push(t, Table{Routes: []Route{route}, Ingress: &Ingress{Enabled: true, Port: first}})
	addr := te.e.ingressAddr()
	if addr == "" {
		t.Fatal("the ingress did not open")
	}

	te.push(t, Table{Routes: []Route{route}, Ingress: &Ingress{Enabled: true, Port: first}})
	if again := te.e.ingressAddr(); again != addr {
		t.Errorf("the ingress moved from %s to %s on an unchanged table", addr, again)
	}

	te.push(t, Table{Routes: []Route{route},
		Ingress: &Ingress{Enabled: true, Port: first, Trusted: []string{"10.86.0.1"}}})
	if again := te.e.ingressAddr(); again != addr {
		t.Errorf("the ingress moved from %s to %s when only Trusted changed", addr, again)
	}

	second := ingressPort(t)
	te.push(t, Table{Routes: []Route{route}, Ingress: &Ingress{Enabled: true, Port: second}})
	moved := te.e.ingressAddr()
	if moved == addr {
		t.Errorf("the ingress stayed on %s when the table asked for port %d", addr, second)
	}
	if _, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		t.Errorf("the old ingress on %s is still open", addr)
	}

	te.push(t, Table{Routes: []Route{route}, Ingress: &Ingress{Enabled: false}})
	if addr := te.e.ingressAddr(); addr != "" {
		t.Fatalf("a disabled ingress is still listening on %s", addr)
	}
	if _, err := net.DialTimeout("tcp", moved, time.Second); err == nil {
		t.Errorf("the ingress on %s is still open after being disabled", moved)
	}
}

func TestViaRouteIsServedLikeAnyOther(t *testing.T) {
	te := newTestEdge(t, nil)

	relay := newReplica(t, 1)

	te.push(t, Table{Routes: []Route{{
		Host: "feat-p.shop.test", Kind: KindVia, Via: "m1",
		App: "shop", Env: "feat-p", Service: "web",
		Targets: []Target{relay.target(TargetActive)},
	}}})

	resp := te.get(t, te.client(t), "https://feat-p.shop.test/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	st, err := te.e.Status(t.Context())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(st.Routes) != 1 {
		t.Fatalf("routes = %+v, want one", st.Routes)
	}

	if st.Routes[0].Via != "m1" {
		t.Errorf("route via = %q, want m1", st.Routes[0].Via)
	}
	if st.Routes[0].Kind != KindVia {
		t.Errorf("route kind = %q, want %q", st.Routes[0].Kind, KindVia)
	}
	if len(relay.requests()) != 1 {
		t.Fatalf("the relay port saw %d requests, want 1", len(relay.requests()))
	}
}
