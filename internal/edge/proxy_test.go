package edge

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type accessLogs struct {
	mu   sync.Mutex
	list []AccessLog
}

func (a *accessLogs) add(e AccessLog) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.list = append(a.list, e)
}

func (a *accessLogs) all() []AccessLog {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AccessLog(nil), a.list...)
}

func (a *accessLogs) last(t *testing.T) AccessLog {
	t.Helper()
	all := a.all()
	if len(all) == 0 {
		t.Fatal("nothing was written to the access log")
	}
	return all[len(all)-1]
}

type testProxy struct {
	proxy  *Proxy
	router *Router
	logs   *accessLogs
	front  *httptest.Server
	plain  *httptest.Server
	clock  *fakeClock
}

func newTestProxy(t *testing.T, clock *fakeClock, tbl Table) *testProxy {
	t.Helper()
	tp := &testProxy{logs: &accessLogs{}, clock: clock}
	tp.router = NewRouter(RouterOptions{
		Now:        clock.Now,
		Drain:      5 * time.Second,
		OnDraining: func(target Target) { tp.proxy.closeIdle(target) },
	})
	tp.proxy = NewProxy(proxyOptions{router: tp.router, now: clock.Now, access: tp.logs.add})
	if err := tp.router.Install(tbl); err != nil {
		t.Fatalf("Install: %v", err)
	}
	tp.front = httptest.NewServer(tp.proxy)
	tp.plain = httptest.NewServer(tp.proxy.HTTPHandler())
	t.Cleanup(func() {
		tp.front.Close()
		tp.plain.Close()
		tp.proxy.close()
	})
	return tp
}

func (tp *testProxy) get(t *testing.T, host, path string) *http.Response {
	t.Helper()
	return tp.do(t, http.MethodGet, host, path, nil)
}

func (tp *testProxy) do(t *testing.T, method, host, path string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, tp.front.URL+path, body)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Host = host
	res, err := noRedirect().Do(req)
	if err != nil {
		t.Fatalf("%s %s%s: %v", method, host, path, err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func noRedirect() *http.Client {
	return &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func bodyOf(t *testing.T, res *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return strings.TrimSpace(string(b))
}

func TestRequestsAreSharedRoundRobinBetweenReplicas(t *testing.T) {
	clock := newClock()
	one, two := newReplica(t, 1), newReplica(t, 2)
	tp := newTestProxy(t, clock, Table{Routes: []Route{route("feat-x.shop.test",
		one.target(TargetActive), two.target(TargetActive))}})

	seen := map[string]int{}
	for range 20 {
		res := tp.get(t, "feat-x.shop.test", "/")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", res.StatusCode)
		}
		seen[bodyOf(t, res)]++
	}
	if seen["replica 1"] != 10 || seen["replica 2"] != 10 {
		t.Errorf("replies = %v, want ten from each", seen)
	}

	e := tp.logs.last(t)
	if e.Host != "feat-x.shop.test" || e.Status != 200 || e.Env != "feat-x" || e.Service != "web" || e.App != "shop" {
		t.Errorf("access log = %+v", e)
	}
	if e.Replica == 0 || e.Target == "" || e.Proto != "http/1.1" || e.Client == "" {
		t.Errorf("access log lost the columns that make a rollout readable: %+v", e)
	}
	if e.Bytes == 0 {
		t.Errorf("access log bytes = 0 for a %d-byte body", len("replica 1"))
	}
}

func TestTheAppSeesWhoAskedAndWhatFor(t *testing.T) {
	clock := newClock()
	one := newReplica(t, 1)
	tp := newTestProxy(t, clock, Table{Routes: []Route{route("feat-x.shop.test", one.target(TargetActive))}})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tp.front.URL+"/", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Host = "feat-x.shop.test"

	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Header.Set("X-Real-IP", "203.0.113.9")
	req.Header.Set("True-Client-IP", "203.0.113.9")
	req.Header.Set("CF-Connecting-IP", "203.0.113.9")
	res, err := noRedirect().Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	res.Body.Close()

	got := one.requests()
	if len(got) != 1 {
		t.Fatalf("the replica saw %d requests", len(got))
	}
	if got[0].Host != "feat-x.shop.test" {
		t.Errorf("Host at the app = %q, want the public name and not the loopback port", got[0].Host)
	}
	if p := got[0].Header.Get("X-Forwarded-Proto"); p != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want https: the edge is what terminated TLS", p)
	}
	if h := got[0].Header.Get("X-Forwarded-Host"); h != "feat-x.shop.test" {
		t.Errorf("X-Forwarded-Host = %q", h)
	}
	if f := got[0].Header.Get("X-Forwarded-For"); f == "" || strings.Contains(f, "203.0.113.9") {
		t.Errorf("X-Forwarded-For = %q, want the address the edge saw and not the one the client claimed", f)
	}

	for _, h := range []string{"True-Client-IP", "CF-Connecting-IP"} {
		if v := got[0].Header.Get(h); v != "" {
			t.Errorf("%s = %q at the app, want it dropped: the edge is the only hop", h, v)
		}
	}
	if v := got[0].Header.Get("X-Real-IP"); v == "" || strings.Contains(v, "203.0.113.9") {
		t.Errorf("X-Real-IP = %q, want the address the edge saw and not the one the client claimed", v)
	}
}

func TestANameTheMachineDoesNotServeIsA404(t *testing.T) {
	clock := newClock()
	one := newReplica(t, 1)
	tp := newTestProxy(t, clock, Table{Routes: []Route{route("feat-x.shop.test", one.target(TargetActive))}})

	res := tp.get(t, "nope.shop.test", "/")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
	if e := tp.logs.last(t); e.Error == "" || e.Env != "" {
		t.Errorf("access log = %+v, want a reason and no environment", e)
	}
}

func TestARouteWithNoTargetTakingRequestsIsA502(t *testing.T) {
	clock := newClock()
	one := newReplica(t, 1)
	tp := newTestProxy(t, clock, Table{Routes: []Route{route("feat-x.shop.test", one.target(TargetDraining))}})

	res := tp.get(t, "feat-x.shop.test", "/")
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 while every replica is draining", res.StatusCode)
	}
}

func TestAConnectionThatFailsIsRetriedOnAnotherReplicaOnce(t *testing.T) {
	clock := newClock()
	dead := deadPort(t)
	two := newReplica(t, 2)
	tp := newTestProxy(t, clock, Table{Routes: []Route{route("feat-x.shop.test",
		Target{Replica: 1, Port: dead, State: TargetActive},
		two.target(TargetActive),
	)}})

	res := tp.get(t, "feat-x.shop.test", "/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the retry to have landed on the replica that is up", res.StatusCode)
	}
	if got := bodyOf(t, res); got != "replica 2" {
		t.Errorf("body = %q", got)
	}

	for range 4 {
		if got := bodyOf(t, tp.get(t, "feat-x.shop.test", "/")); got != "replica 2" {
			t.Fatalf("body = %q, want the failed replica to be skipped for %s", got, FailureWindow)
		}
	}
	if len(two.requests()) != 5 {
		t.Errorf("the healthy replica saw %d requests, want 5", len(two.requests()))
	}
}

func TestARequestWithABodyIsNeverSentTwice(t *testing.T) {
	clock := newClock()
	dead := deadPort(t)
	two := newReplica(t, 2)
	tp := newTestProxy(t, clock, Table{Routes: []Route{route("feat-x.shop.test",
		Target{Replica: 1, Port: dead, State: TargetActive},
		two.target(TargetActive),
	)}})

	res := tp.do(t, http.MethodPost, "feat-x.shop.test", "/", strings.NewReader("order=1"))
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502: a POST that may have been half-sent must not be replayed", res.StatusCode)
	}
	if n := len(two.requests()); n != 0 {
		t.Errorf("the second replica saw %d requests; the POST was retried", n)
	}
}

func TestARequestIsCountedInFlightWhileItRuns(t *testing.T) {
	clock := newClock()
	one := newReplica(t, 1)
	tp := newTestProxy(t, clock, Table{Routes: []Route{route("feat-x.shop.test", one.target(TargetActive))}})
	_, pool, _ := tp.router.Lookup("feat-x.shop.test")

	done := make(chan struct{})
	go func() {
		defer close(done)
		res := tp.get(t, "feat-x.shop.test", "/hold")
		_ = bodyOf(t, res)
	}()
	waitFor(t, func() bool { return pool.inflight() == 1 })

	one.release()
	<-done
	waitFor(t, func() bool { return pool.inflight() == 0 })
}

func TestAWebSocketIsOneInFlightRequestUntilTheDrainDeadlineCutsIt(t *testing.T) {
	clock := newClock()
	one := newReplica(t, 1)
	r := route("feat-x.shop.test", one.target(TargetActive))
	r.Drain = 5 * time.Second
	tp := newTestProxy(t, clock, Table{Routes: []Route{r}})
	_, pool, _ := tp.router.Lookup("feat-x.shop.test")

	conn, err := net.Dial("tcp", tp.front.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
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
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if strings.TrimSpace(line) != "ping" {
		t.Fatalf("echo = %q", line)
	}
	if got := pool.inflight(); got != 1 {
		t.Fatalf("in-flight = %d, want the WebSocket to count as one until it closes", got)
	}

	if err := pool.Mark(Mark{Replica: 1, Kind: MarkState, State: TargetDraining, At: clock.Now()}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	clock.advance(4 * time.Second)
	tp.router.Sweep(clock.Now())
	fmt.Fprint(conn, "still-here\n")
	if line, err := br.ReadString('\n'); err != nil || strings.TrimSpace(line) != "still-here" {
		t.Fatalf("the WebSocket was cut before the drain deadline: %q, %v", line, err)
	}

	clock.advance(2 * time.Second)
	tp.router.Sweep(clock.Now())
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := br.ReadString('\n'); err == nil {
		t.Fatal("the WebSocket outlived the drain deadline")
	}
	waitFor(t, func() bool { return pool.inflight() == 0 })

	waitFor(t, func() bool { return len(tp.logs.all()) > 0 })
	e := tp.logs.last(t)
	if !e.WebSocket || e.Status != http.StatusSwitchingProtocols {
		t.Errorf("access log = %+v, want the upgrade recorded as a 101", e)
	}
}

func TestPortEightyRedirectsWhatWeServeAndRefusesTheRest(t *testing.T) {
	clock := newClock()
	one := newReplica(t, 1)
	tp := newTestProxy(t, clock, Table{Routes: []Route{route("feat-x.shop.test", one.target(TargetActive))}})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tp.plain.URL+"/deep/path?a=b", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Host = "feat-x.shop.test"
	res, err := noRedirect().Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != RedirectStatus {
		t.Errorf("status = %d, want %d", res.StatusCode, RedirectStatus)
	}
	if got := res.Header.Get("Location"); got != "https://feat-x.shop.test/deep/path?a=b" {
		t.Errorf("Location = %q", got)
	}

	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, tp.plain.URL+"/", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Host = "nope.shop.test"
	res2, err := noRedirect().Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a name nothing is exposed under", res2.StatusCode)
	}
}
