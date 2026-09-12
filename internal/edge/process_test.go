package edge

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"github.com/plytz/caramelo/internal/testutil"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/plytz/caramelo/internal/edge/certs"
)

type testEdge struct {
	e       *Edge
	issuer  *testIssuer
	dir     string
	log     *syncBuffer
	done    chan error
	cancel  context.CancelFunc
	stopped sync.Once
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) accessLines(t *testing.T) []AccessLog {
	t.Helper()
	var out []AccessLog
	for _, line := range strings.Split(s.String(), "\n") {
		rest, ok := strings.CutPrefix(line, AccessLogPrefix+" ")
		if !ok {
			continue
		}
		var e AccessLog
		if err := json.Unmarshal([]byte(rest), &e); err != nil {
			t.Fatalf("access log line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

func newTestEdge(t *testing.T, tweak func(*Options)) *testEdge {
	t.Helper()
	te := &testEdge{issuer: newTestIssuer(t), dir: testutil.ShortDir(t), log: &syncBuffer{}}
	o := Options{
		StateDir: te.dir,
		RunDir:   te.dir,
		HTTP3:    true,
		Version:  "test",
		Log:      te.log,
		Issuer:   te.issuer,
		Bind:     BindOptions{HTTP: "127.0.0.1:0", HTTPS: "127.0.0.1:0"},
		Drain:    5 * time.Second,
	}
	if tweak != nil {
		tweak(&o)
	}
	e, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	te.e = e

	ctx, cancel := context.WithCancel(context.Background())
	te.cancel = cancel
	te.done = make(chan error, 1)
	go func() { te.done <- e.Serve(ctx) }()
	t.Cleanup(func() { te.stop(t) })
	return te
}

func (te *testEdge) stop(t *testing.T) {
	t.Helper()
	te.stopped.Do(func() {
		te.cancel()
		select {
		case err := <-te.done:
			if err != nil {
				t.Errorf("Serve = %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the edge did not stop within ten seconds of its context being cancelled")
		}
	})
}

func (te *testEdge) httpsAddr() string { return te.e.listeners.HTTPS.Addr().String() }
func (te *testEdge) httpAddr() string  { return te.e.listeners.HTTP.Addr().String() }
func (te *testEdge) quicAddr() string  { return te.e.listeners.QUIC.LocalAddr().String() }

func (te *testEdge) client(t *testing.T) *http.Client { return te.clientWithHTTP2(t, true) }

func (te *testEdge) http11Client(t *testing.T) *http.Client { return te.clientWithHTTP2(t, false) }

func (te *testEdge) clientWithHTTP2(t *testing.T, h2 bool) *http.Client {
	t.Helper()
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: te.issuer.pool(t), MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: h2,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", te.httpsAddr())
		},
	}
	if !h2 {

		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	return &http.Client{
		Timeout:       15 * time.Second,
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (te *testEdge) h3Client(t *testing.T) (*http.Client, func()) {
	t.Helper()
	tr := &http3.Transport{
		TLSClientConfig: &tls.Config{RootCAs: te.issuer.pool(t), MinVersion: tls.VersionTLS12},
		QUICConfig:      &quic.Config{HandshakeIdleTimeout: 10 * time.Second},
		Dial: func(ctx context.Context, _ string, tlsConf *tls.Config, qconf *quic.Config) (*quic.Conn, error) {
			ua, err := net.ResolveUDPAddr("udp", te.quicAddr())
			if err != nil {
				return nil, err
			}
			pc, err := net.ListenUDP("udp", nil)
			if err != nil {
				return nil, err
			}
			return quic.Dial(ctx, pc, ua, tlsConf, qconf)
		},
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: tr}, func() { tr.Close() }
}

func (te *testEdge) push(t *testing.T, table Table) {
	t.Helper()
	c := NewClient(te.e.opts.socketPath())
	defer c.Close()

	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err = c.PushTable(t.Context(), table); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("PushTable: %v", err)
}

func (te *testEdge) get(t *testing.T, c *http.Client, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestTheEdgeServesOneNameOverEveryProtocolItSpeaks(t *testing.T) {
	te := newTestEdge(t, nil)
	one := newReplica(t, 1)
	te.push(t, Table{Routes: []Route{route("feat-x.shop.test", one.target(TargetActive))}})

	h3, closeH3 := te.h3Client(t)
	defer closeH3()
	res := te.get(t, h3, "https://feat-x.shop.test/")
	if res.StatusCode != http.StatusOK || res.ProtoMajor != 3 {
		t.Fatalf("http/3: status %d proto %s", res.StatusCode, res.Proto)
	}
	if got := bodyOf(t, res); got != "replica 1" {
		t.Errorf("http/3 body = %q", got)
	}

	res = te.get(t, te.client(t), "https://feat-x.shop.test/")
	if res.StatusCode != http.StatusOK || res.ProtoMajor != 2 {
		t.Fatalf("http/2: status %d proto %s", res.StatusCode, res.Proto)
	}
	if alt := res.Header.Get("Alt-Svc"); !strings.Contains(alt, "h3=") {
		t.Errorf("Alt-Svc = %q, want the h3 advertisement", alt)
	}

	res = te.get(t, te.http11Client(t), "https://feat-x.shop.test/")
	if res.StatusCode != http.StatusOK || res.ProtoMajor != 1 {
		t.Fatalf("http/1.1: status %d proto %s", res.StatusCode, res.Proto)
	}

	protos := map[string]bool{}
	for _, e := range te.log.accessLines(t) {
		protos[e.Proto] = true
		if e.Host != "feat-x.shop.test" || e.Replica != 1 {
			t.Errorf("access line = %+v", e)
		}
	}
	for _, want := range []string{"h3", "h2", "http/1.1"} {
		if !protos[want] {
			t.Errorf("the access log has no %s request: %v", want, protos)
		}
	}
}

func TestAHandshakeForANameTheMachineDoesNotServeIsRefused(t *testing.T) {
	te := newTestEdge(t, func(o *Options) { o.HTTP3 = false })
	one := newReplica(t, 1)
	te.push(t, Table{Routes: []Route{route("feat-x.shop.test", one.target(TargetActive))}})

	conn, err := tls.Dial("tcp", te.httpsAddr(), &tls.Config{
		ServerName: "nope.shop.test",
		RootCAs:    te.issuer.pool(t),
	})
	if err == nil {
		conn.Close()
		t.Fatal("the handshake for an unrouted name succeeded")
	}

	conn, err = tls.Dial("tcp", te.httpsAddr(), &tls.Config{InsecureSkipVerify: true})
	if err == nil {
		conn.Close()
		t.Error("the handshake with no server name succeeded; this machine serves names")
	}

	res := te.get(t, te.client(t), "https://feat-x.shop.test/")
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d", res.StatusCode)
	}
}

func TestUnexposeStopsTheNameAnsweringAtOnce(t *testing.T) {
	te := newTestEdge(t, func(o *Options) { o.HTTP3 = false })
	one := newReplica(t, 1)
	te.push(t, Table{Routes: []Route{route("feat-x.shop.test", one.target(TargetActive))}})
	if res := te.get(t, te.client(t), "https://feat-x.shop.test/"); res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}

	te.push(t, Table{})
	conn, err := tls.Dial("tcp", te.httpsAddr(), &tls.Config{
		ServerName: "feat-x.shop.test",
		RootCAs:    te.issuer.pool(t),
	})
	if err == nil {
		conn.Close()
		t.Fatal("the name still answers after it was taken out of the table")
	}

	before := te.issuer.issuedFor("feat-x.shop.test")
	te.push(t, Table{Routes: []Route{route("feat-x.shop.test", one.target(TargetActive))}})
	if res := te.get(t, te.client(t), "https://feat-x.shop.test/"); res.StatusCode != http.StatusOK {
		t.Errorf("status = %d after being exposed again", res.StatusCode)
	}
	if got := te.issuer.issuedFor("feat-x.shop.test"); got != before {
		t.Errorf("the certificate was issued again (%d -> %d); exposing a name it already has must not order one", before, got)
	}
}

func TestTheEdgeAsksForACertificateOnlyForANameItRoutes(t *testing.T) {
	te := newTestEdge(t, func(o *Options) { o.HTTP3 = false })
	one := newReplica(t, 1)
	te.push(t, Table{Routes: []Route{route("feat-x.shop.test", one.target(TargetActive))}})

	if err := te.e.ask(t.Context(), "FEAT-X.shop.test."); err != nil {
		t.Errorf("ask for a routed name = %v, want nil", err)
	}
	err := te.e.ask(t.Context(), "nope.shop.test")
	if err == nil || !strings.Contains(err.Error(), "nope.shop.test") {
		t.Errorf("ask for a name the machine does not route = %v, want a refusal naming it", err)
	}
}

func TestPortEightyRedirectsOnTheRealListener(t *testing.T) {
	te := newTestEdge(t, func(o *Options) { o.HTTP3 = false })
	one := newReplica(t, 1)
	te.push(t, Table{Routes: []Route{route("feat-x.shop.test", one.target(TargetActive))}})

	c := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", te.httpAddr())
		}},
	}
	res := te.get(t, c, "http://feat-x.shop.test/x")
	if res.StatusCode != RedirectStatus || res.Header.Get("Location") != "https://feat-x.shop.test/x" {
		t.Errorf("status %d location %q", res.StatusCode, res.Header.Get("Location"))
	}
	res = te.get(t, c, "http://nope.shop.test/x")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

func TestAPushedTableIsPersistedAndServedAgainAfterARestart(t *testing.T) {
	te := newTestEdge(t, func(o *Options) { o.HTTP3 = false })
	one := newReplica(t, 1)
	te.push(t, Table{Routes: []Route{route("feat-x.shop.test", one.target(TargetActive))}})

	saved, err := LoadTable(RoutesPath(te.dir))
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}
	if len(saved.Routes) != 1 || saved.Routes[0].Host != "feat-x.shop.test" {
		t.Fatalf("routes.json = %+v", saved.Routes)
	}

	te.stop(t)

	again := &testEdge{issuer: te.issuer, dir: te.dir, log: &syncBuffer{}}
	e, err := New(Options{
		StateDir: te.dir, RunDir: te.dir, Version: "test",
		Log: again.log, Issuer: te.issuer,
		Bind: BindOptions{HTTP: "127.0.0.1:0", HTTPS: "127.0.0.1:0"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	again.e = e
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Serve(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	res := again.get(t, again.client(t), "https://feat-x.shop.test/")
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d: the edge must serve its own table without a control plane", res.StatusCode)
	}
	if got := bodyOf(t, res); got != "replica 1" {
		t.Errorf("body = %q", got)
	}
}

func TestStatusIsWhatEdgeStatusPrints(t *testing.T) {
	te := newTestEdge(t, nil)
	one, two := newReplica(t, 1), newReplica(t, 2)
	te.push(t, Table{UpdatedAt: time.Now().UTC(), Routes: []Route{
		route("feat-x.shop.test", one.target(TargetActive), two.target(TargetDraining)),
	}})

	te.get(t, te.client(t), "https://feat-x.shop.test/")

	c := NewClient(te.e.opts.socketPath())
	defer c.Close()
	st, err := c.Status(t.Context())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Running || st.Version != "test" || !st.HTTP3 {
		t.Errorf("status = %+v", st)
	}
	if len(st.Listeners) != 3 {
		t.Errorf("listeners = %v, want tcp 80, tcp 443 and udp 443", st.Listeners)
	}
	if len(st.Routes) != 1 || len(st.Routes[0].Targets) != 2 {
		t.Fatalf("routes = %+v", st.Routes)
	}
	if st.Routes[0].Targets[1].State != TargetDraining {
		t.Errorf("target = %+v, want the draining one reported as such", st.Routes[0].Targets[1])
	}
	if len(st.Certificates) != 1 || st.Certificates[0].Host != "feat-x.shop.test" {
		t.Errorf("certificates = %+v", st.Certificates)
	}
	if st.StartedAt.IsZero() || st.TableUpdatedAt.IsZero() {
		t.Errorf("status = %+v, want when it started and when its table was built", st)
	}
}

func TestSubscribersSeeARolloutThroughTheSocket(t *testing.T) {
	te := newTestEdge(t, func(o *Options) { o.HTTP3 = false })
	one, two := newReplica(t, 1), newReplica(t, 2)

	c := NewClient(te.e.opts.socketPath())
	defer c.Close()

	te.push(t, Table{Routes: []Route{route("feat-x.shop.test", one.target(TargetActive))}})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	seen := make(chan Event, 16)
	go func() {
		_ = c.Subscribe(ctx, time.Time{}, func(e Event) error { seen <- e; return nil })
	}()
	waitFor(t, func() bool { return subscribers(te.e.router.Events()) > 0 })

	te.push(t, Table{Routes: []Route{route("feat-x.shop.test",
		one.target(TargetDraining), two.target(TargetActive))}})

	var drained *Event
	deadline := time.After(5 * time.Second)
	for drained == nil {
		select {
		case e := <-seen:
			if e.Kind == EventDrained {
				ev := e
				drained = &ev
			}
		case <-deadline:
			t.Fatal("no drain was reported over the socket")
		}
	}
	if drained.Host != "feat-x.shop.test" || drained.Target == nil || drained.Target.Replica != 1 || drained.Deadline {
		t.Errorf("drained = %+v", drained)
	}

	res := te.get(t, te.client(t), "https://feat-x.shop.test/")
	if got := bodyOf(t, res); got != "replica 2" {
		t.Errorf("body = %q, want the replica that was flipped in", got)
	}
}

func TestTheSocketIsRemovedWhenTheProcessStops(t *testing.T) {
	te := newTestEdge(t, func(o *Options) { o.HTTP3 = false })
	te.push(t, Table{})
	path := te.e.opts.socketPath()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	te.stop(t)
	if _, err := os.Stat(path); err == nil {
		t.Errorf("%s is still there after the edge stopped", path)
	}
}

func TestACertificateChangeIsAnEvent(t *testing.T) {
	e, err := New(Options{
		StateDir: t.TempDir(),
		RunDir:   testutil.ShortDir(t),
		Issuer:   newTestIssuer(t),
		Bind:     BindOptions{HTTP: "127.0.0.1:0", HTTPS: "127.0.0.1:0"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	at := time.Now().Add(-time.Second)
	e.certificateChanged(certs.Obtained, certs.Certificate{
		Host: "FEAT-X.shop.test.", Serial: "ABC", NotAfter: at.Add(time.Hour),
	}, "")

	events := e.router.Events().since(at)
	var got *Event
	for i := range events {
		if events[i].Kind == EventCertificate {
			got = &events[i]
		}
	}
	if got == nil {
		t.Fatalf("no certificate event among %d", len(events))
	}
	if got.Host != "feat-x.shop.test" {
		t.Errorf("event host = %q, want the normalized name", got.Host)
	}
	if got.Certificate == nil || got.Certificate.Serial != "ABC" {
		t.Errorf("event carries %+v, want the certificate", got.Certificate)
	}
	if got.Detail != string(certs.Obtained) {
		t.Errorf("event detail = %q, want %q", got.Detail, certs.Obtained)
	}
}

func TestAPushedTableUnmanagesTheNamesItDrops(t *testing.T) {
	issuer := newTestIssuer(t)
	e, err := New(Options{
		StateDir: t.TempDir(),
		RunDir:   testutil.ShortDir(t),
		Issuer:   issuer,
		Bind:     BindOptions{HTTP: "127.0.0.1:0", HTTPS: "127.0.0.1:0"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	both := Table{Routes: []Route{
		{Host: "feat-x.shop.test", Kind: KindHTTPS, Env: "feat-x", Service: "web"},
		{Host: "feat-y.shop.test", Kind: KindHTTPS, Env: "feat-y", Service: "web"},
	}}
	if err := e.PushTable(context.Background(), both); err != nil {
		t.Fatalf("push both: %v", err)
	}
	if got := issuer.unmanaged(); len(got) != 0 {
		t.Fatalf("a push that dropped nothing unmanaged %v", got)
	}

	one := Table{Routes: both.Routes[:1]}
	if err := e.PushTable(context.Background(), one); err != nil {
		t.Fatalf("push one: %v", err)
	}
	got := issuer.unmanaged()
	if len(got) != 1 || got[0] != "feat-y.shop.test" {
		t.Errorf("unmanaged %v, want just the host that was dropped", got)
	}
	if _, ok := e.router.Table().Route("feat-x.shop.test"); !ok {
		t.Error("the surviving route is gone")
	}
}
