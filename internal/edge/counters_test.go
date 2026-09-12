package edge

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func statusBackend(t *testing.T, index, status int) Target {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, fmt.Sprintf("replica %d is broken", index), status)
	}))
	t.Cleanup(srv.Close)
	return Target{Replica: index, Port: portOf(t, srv), State: TargetActive}
}

func portOf(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("replica address: %v", err)
	}
	var n int
	if _, err := fmt.Sscanf(port, "%d", &n); err != nil {
		t.Fatalf("replica port %q: %v", port, err)
	}
	return n
}

func countsOf(t *testing.T, tp *testProxy, host string, since time.Time) HostCounts {
	t.Helper()
	c := &Counts{Hosts: tp.router.Counts(since)}
	h, ok := c.Host(host)
	if !ok {
		t.Fatalf("no counts for %s: %+v", host, c.Hosts)
	}
	return h
}

func TestEveryRequestIsCountedOnItsReplica(t *testing.T) {
	clock := newClock()
	one, two := newReplica(t, 1), newReplica(t, 2)
	tp := newTestProxy(t, clock, Table{Routes: []Route{route("feat-x.shop.test",
		one.target(TargetActive), two.target(TargetActive))}})

	for range 10 {
		res := tp.get(t, "feat-x.shop.test", "/")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", res.StatusCode)
		}
	}

	h := countsOf(t, tp, "feat-x.shop.test", time.Time{})
	if h.Requests != 10 || h.Errors() != 0 {
		t.Errorf("host = %+v, want 10 requests and no errors", h)
	}
	if h.Rate() != 0 {
		t.Errorf("rate = %v on a clean pool", h.Rate())
	}
	if len(h.Targets) != 2 {
		t.Fatalf("%d replicas counted, want 2: %+v", len(h.Targets), h.Targets)
	}
	for _, tc := range h.Targets {
		if tc.Requests != 5 {
			t.Errorf("replica %d served %d of 10 round-robin requests", tc.Replica, tc.Requests)
		}
	}

	if got := h.Replicas(1, 2); got.Requests != 10 {
		t.Errorf("Replicas(1,2) = %+v", got)
	}
}

func TestA5xxIsCountedOnTheReplicaThatAnsweredIt(t *testing.T) {
	clock := newClock()
	good := newReplica(t, 1)
	bad := statusBackend(t, 2, http.StatusInternalServerError)
	tp := newTestProxy(t, clock, Table{Routes: []Route{route("feat-x.shop.test",
		good.target(TargetActive), bad)}})

	for range 10 {
		tp.get(t, "feat-x.shop.test", "/")
	}

	h := countsOf(t, tp, "feat-x.shop.test", time.Time{})
	if h.Requests != 10 || h.Status5xx != 5 || h.ConnectFailures != 0 {
		t.Errorf("host = %+v, want 10 requests and 5 of them 5xx", h)
	}
	if h.Rate() != 0.5 {
		t.Errorf("rate = %v, want 0.5", h.Rate())
	}
	one, _ := h.Target(1)
	two, _ := h.Target(2)
	if one.Errors() != 0 {
		t.Errorf("the healthy replica = %+v", one)
	}
	if two.Status5xx != 5 || two.Rate() != 1 {
		t.Errorf("the broken replica = %+v, want every request a 5xx", two)
	}
}

func TestAConnectionFailureIsCountedWithTheRetryThatHidItFromTheClient(t *testing.T) {
	clock := newClock()
	good := newReplica(t, 1)
	dead := Target{Replica: 2, Port: deadPort(t), State: TargetActive}
	tp := newTestProxy(t, clock, Table{Routes: []Route{route("feat-x.shop.test",
		good.target(TargetActive), dead)}})

	for range 2 {
		res := tp.get(t, "feat-x.shop.test", "/")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: the retry did not hide the dead replica", res.StatusCode)
		}
	}

	h := countsOf(t, tp, "feat-x.shop.test", time.Time{})
	if h.ConnectFailures != 1 {
		t.Errorf("host = %+v, want one connection failure", h)
	}
	if h.Status5xx != 0 {
		t.Errorf("host = %+v: the client saw no error, so nothing is a 5xx", h)
	}

	if h.Requests != 3 {
		t.Errorf("host requests = %d, want 3 attempts", h.Requests)
	}
	two, _ := h.Target(2)
	if two.Requests != 1 || two.ConnectFailures != 1 || two.Rate() != 1 {
		t.Errorf("the dead replica = %+v", two)
	}
}

func TestAFailedConnectionThatCannotBeRetriedIsOneError(t *testing.T) {
	clock := newClock()
	dead := Target{Replica: 1, Port: deadPort(t), State: TargetActive}
	tp := newTestProxy(t, clock, Table{Routes: []Route{route("feat-x.shop.test", dead)}})

	res := tp.do(t, http.MethodPost, "feat-x.shop.test", "/", strings.NewReader("x=1"))
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.StatusCode)
	}

	h := countsOf(t, tp, "feat-x.shop.test", time.Time{})
	if h.Requests != 1 || h.ConnectFailures != 1 || h.Status5xx != 0 {
		t.Errorf("host = %+v, want one request counted once, as a connection failure", h)
	}
	if h.Errors() > h.Requests || h.Rate() != 1 {
		t.Errorf("host = %+v: %d errors in %d requests, rate %v", h, h.Errors(), h.Requests, h.Rate())
	}
}

func TestA502WithNoTargetIsCountedOnTheRoute(t *testing.T) {
	clock := newClock()
	one := newReplica(t, 1)
	tp := newTestProxy(t, clock, Table{Routes: []Route{route("feat-x.shop.test",
		one.target(TargetHeld))}})

	res := tp.get(t, "feat-x.shop.test", "/")
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.StatusCode)
	}

	h := countsOf(t, tp, "feat-x.shop.test", time.Time{})
	if h.Requests != 1 || h.Status5xx != 1 || h.Rate() != 1 {
		t.Errorf("host = %+v, want one request and one 5xx", h)
	}

	if h.Unrouted.Requests != 1 || h.Unrouted.Errors() != 1 {
		t.Errorf("unrouted = %+v, want one request and one error", h.Unrouted)
	}
	if got := h.Replicas(1); got.Requests != 0 {
		t.Errorf("Replicas(1) = %+v: the 502 must not be attributed to a replica", got)
	}

	held, ok := h.Target(1)
	if !ok {
		t.Fatalf("the held replica is not listed: %+v", h.Targets)
	}
	if held.Requests != 0 {
		t.Errorf("the held replica = %+v", held)
	}
}

func TestCountsAreTakenFromTheGenerationAtTheFlip(t *testing.T) {
	clock := newClock()
	old := statusBackend(t, 1, http.StatusInternalServerError)
	fresh := newReplica(t, 2)
	tp := newTestProxy(t, clock, Table{Routes: []Route{route("feat-x.shop.test", old)}})

	for range 4 {
		tp.get(t, "feat-x.shop.test", "/")
	}
	before := countsOf(t, tp, "feat-x.shop.test", time.Time{})
	if before.Status5xx != 4 {
		t.Fatalf("before the flip = %+v", before)
	}

	flip := clock.advance(time.Second)
	old.State = TargetHeld
	if err := tp.router.Install(Table{Routes: []Route{route("feat-x.shop.test",
		old, fresh.target(TargetActive))}}); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		if res := tp.get(t, "feat-x.shop.test", "/"); res.StatusCode != http.StatusOK {
			t.Fatalf("status after the flip = %d", res.StatusCode)
		}
	}

	since := countsOf(t, tp, "feat-x.shop.test", flip)
	if since.Requests != 6 || since.Errors() != 0 {
		t.Errorf("since the flip = %+v, want the new pool's six clean requests", since)
	}
	if got, _ := since.Target(1); got.Requests != 0 {
		t.Errorf("the held replica served %d requests after the flip", got.Requests)
	}
	if got, _ := since.Target(2); got.Requests != 6 {
		t.Errorf("the new replica = %+v", got)
	}

	all := countsOf(t, tp, "feat-x.shop.test", time.Time{})
	if all.Requests != 10 || all.Status5xx != 4 {
		t.Errorf("since the beginning = %+v", all)
	}
}

func TestAPushForAnotherHostDoesNotDisturbAWatch(t *testing.T) {
	clock := newClock()
	prod := newReplica(t, 1)
	preview := newReplica(t, 2)
	prodRoute := func() Route {
		r := route("shop.test", prod.target(TargetActive))
		r.Env = "production"
		return r
	}
	tp := newTestProxy(t, clock, Table{Routes: []Route{prodRoute()}})

	flip := clock.Now()
	for range 5 {
		tp.get(t, "shop.test", "/")
	}

	clock.advance(time.Second)
	pr := route("pr-41.shop.test", preview.target(TargetActive))
	pr.Env = "pr-41"
	if err := tp.router.Install(Table{Routes: []Route{prodRoute(), pr}}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		tp.get(t, "shop.test", "/")
	}

	h := countsOf(t, tp, "shop.test", flip)
	if h.Requests != 8 {
		t.Errorf("production counted %d requests since its flip, want 8: another "+
			"environment's push must not shorten the window", h.Requests)
	}

	if got := countsOf(t, tp, "pr-41.shop.test", flip); got.Requests != 0 {
		t.Errorf("the preview = %+v", got)
	}
}

func TestAReplacedContainerStartsFromZero(t *testing.T) {
	clock := newClock()
	first := newReplica(t, 1)
	tp := newTestProxy(t, clock, Table{Routes: []Route{route("feat-x.shop.test", first.target(TargetActive))}})
	start := clock.Now()
	for range 3 {
		tp.get(t, "feat-x.shop.test", "/")
	}

	second := newReplica(t, 1)
	clock.advance(time.Second)
	if err := tp.router.Install(Table{Routes: []Route{route("feat-x.shop.test",
		second.target(TargetActive))}}); err != nil {
		t.Fatal(err)
	}
	tp.get(t, "feat-x.shop.test", "/")

	h := countsOf(t, tp, "feat-x.shop.test", start)
	if got, _ := h.Target(1); got.Requests != 1 {
		t.Errorf("replica 1 = %+v, want only what the new container served", got)
	}
	if h.Requests != 1 {
		t.Errorf("host = %+v", h)
	}
}

func TestAWebSocketIsCountedOnceWhenItCloses(t *testing.T) {
	clock := newClock()
	one := newReplica(t, 1)
	tp := newTestProxy(t, clock, Table{Routes: []Route{route("feat-x.shop.test", one.target(TargetActive))}})
	_, pool, _ := tp.router.Lookup("feat-x.shop.test")

	conn, err := net.Dial("tcp", tp.front.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	fmt.Fprint(conn, "GET /ws HTTP/1.1\r\nHost: feat-x.shop.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", res.StatusCode)
	}
	fmt.Fprint(conn, "ping\n")
	if line, err := br.ReadString('\n'); err != nil || strings.TrimSpace(line) != "ping" {
		t.Fatalf("echo = %q, %v", line, err)
	}

	if h := countsOf(t, tp, "feat-x.shop.test", time.Time{}); h.Requests != 0 {
		t.Errorf("an open WebSocket was counted: %+v", h)
	}
	conn.Close()
	waitFor(t, func() bool { return pool.inflight() == 0 })

	h := countsOf(t, tp, "feat-x.shop.test", time.Time{})
	if h.Requests != 1 {
		t.Errorf("host = %+v, want the closed WebSocket counted once", h)
	}

	if h.Errors() != 0 {
		t.Errorf("the WebSocket was counted as an error: %+v", h)
	}
}

func TestTheHTTPRedirectIsNotCounted(t *testing.T) {
	clock := newClock()
	one := newReplica(t, 1)
	tp := newTestProxy(t, clock, Table{Routes: []Route{route("feat-x.shop.test", one.target(TargetActive))}})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tp.plain.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "feat-x.shop.test"
	res, err := noRedirect().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != RedirectStatus {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if h := countsOf(t, tp, "feat-x.shop.test", time.Time{}); h.Requests != 0 {
		t.Errorf("the redirect was counted: %+v", h)
	}
}

func TestEdgeCountsAnswersWhatTheProxyServed(t *testing.T) {
	clock := newClock()
	one := newReplica(t, 1)
	tp := newTestProxy(t, clock, Table{Routes: []Route{route("feat-x.shop.test", one.target(TargetActive))}})
	for range 3 {
		tp.get(t, "feat-x.shop.test", "/")
	}

	e := &Edge{now: clock.Now, router: tp.router}
	e.started = clock.Now().Add(-time.Minute)

	got, err := e.Counts(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Since.Equal(e.started) {
		t.Errorf("Since = %v, want the edge's start %v", got.Since, e.started)
	}
	if got.At.IsZero() {
		t.Error("At is zero")
	}
	h, ok := got.Host("feat-x.shop.test")
	if !ok {
		t.Fatalf("no counts for the host: %+v", got)
	}
	if h.Requests != 3 {
		t.Errorf("host = %+v", h)
	}
}
