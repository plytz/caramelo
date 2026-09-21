package sshapi

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/edge/certs"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/state"
)

func TestRefusedOverAPI(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		refused bool
	}{
		{[]string{"edge"}, true},
		{[]string{"edge", "--config-dir", "/etc/caramelo"}, true},
		{[]string{"edge", "status"}, false},
		{[]string{"edge", "status", "--json"}, false},
		{[]string{"edge", "ca"}, false},
		{[]string{"edge", "enable"}, false},
		{[]string{"fleet", "setup"}, true},
		{[]string{"fleet", "--progress", "text", "setup"}, true},
		{[]string{"fleet", "--config-dir", "/etc/caramelo", "setup"}, true},
		{[]string{"fleet", "--target", "you@box", "setup"}, true},
		{[]string{"fleet", "--name", "box", "setup", "--yes"}, true},
		{[]string{"fleet", "list"}, false},
		{[]string{"fleet", "--config-dir", "/etc/caramelo", "list"}, false},
		{[]string{"hub"}, true},
		{[]string{"commander", "init"}, true},
		{[]string{"commander"}, true},
		{[]string{"server", "setup"}, false},
		{[]string{"member", "list"}, false},
		{[]string{"env", "list"}, false},
		{nil, false},

		{[]string{"--json", "edge"}, true},
		{[]string{"--json", "fleet", "setup"}, true},
		{[]string{"--machine", "box", "edge"}, true},
		{[]string{"--machine=box", "edge"}, true},
		{[]string{"--json", "edge", "status"}, false},
		{[]string{"--json", "env", "list"}, false},
		{[]string{"--machine", "box", "env", "list"}, false},

		{[]string{"--machine", "edge", "env", "list"}, false},
	} {
		why, refused := refusedOverAPI(tc.args)
		if refused != tc.refused {
			t.Errorf("refusedOverAPI(%q) = %v, want %v", strings.Join(tc.args, " "), refused, tc.refused)
		}
		if refused && why == "" {
			t.Errorf("refusedOverAPI(%q) refused without saying why", strings.Join(tc.args, " "))
		}
	}
}

type fakeEdgeManager struct {
	exposeReq   env.ExposeRequest
	unexposeReq env.UnexposeRequest
	identity    string
	env         *env.Env
	routes      []edge.Route
	changed     []string
	err         error
}

func (f *fakeEdgeManager) Expose(ctx context.Context, req env.ExposeRequest, _ io.Writer) (*env.ExposeResult, error) {
	f.exposeReq, f.identity = req, env.IdentityFrom(ctx)
	return f.result(), f.err
}

func (f *fakeEdgeManager) Unexpose(ctx context.Context, req env.UnexposeRequest, _ io.Writer) (*env.ExposeResult, error) {
	f.unexposeReq, f.identity = req, env.IdentityFrom(ctx)
	return f.result(), f.err
}

func (f *fakeEdgeManager) result() *env.ExposeResult {
	if f.err != nil {
		return nil
	}
	res := &env.ExposeResult{Routes: f.routes, Changed: f.changed}
	if f.env != nil {
		res.Env = *f.env
	}
	return res
}

type fakeEdgeClient struct {
	status *edge.Status
	ca     *certs.CA

	counts *edge.Counts
	err    error

	pruned   *certs.PruneResult
	pruneReq *certs.PruneRequest
}

func (f *fakeEdgeClient) PushTable(context.Context, edge.Table) error { return f.err }

func (f *fakeEdgeClient) Status(context.Context) (*edge.Status, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.status, nil
}

func (f *fakeEdgeClient) Subscribe(context.Context, time.Time, func(edge.Event) error) error {
	return f.err
}

func (f *fakeEdgeClient) Counts(_ context.Context, since time.Time) (*edge.Counts, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.counts != nil {
		out := *f.counts
		out.Since = since
		return &out, nil
	}
	return &edge.Counts{Since: since}, nil
}

func (f *fakeEdgeClient) CA(context.Context) (*certs.CA, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.ca, nil
}

func (f *fakeEdgeClient) Prune(_ context.Context, req certs.PruneRequest) (*certs.PruneResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.pruneReq = &req
	if f.pruned != nil {
		return f.pruned, nil
	}
	return &certs.PruneResult{KeepFor: req.Keep(), DryRun: req.DryRun}, nil
}

func (f *fakeEdgeClient) Close() error { return nil }

func edgeDaemon(t *testing.T) *Daemon {
	t.Helper()
	cfg := serverconfig.Default()
	cfg.Edge, cfg.ACMEEmail = true, "ops@example.com"
	return &Daemon{Config: cfg}
}

func TestExposeHandsOverAndAnswersWithTheWholeTruth(t *testing.T) {
	d := edgeDaemon(t)
	m := &fakeEdgeManager{
		env: &env.Env{App: "shop", Name: "feat-x"},
		routes: []edge.Route{
			{Host: "feat-x.shop.test", Kind: edge.KindHTTPS, App: "shop", Env: "feat-x", Service: "web"},
			{Host: "api.feat-x.shop.test", Kind: edge.KindHTTPS, App: "shop", Env: "feat-x", Service: "api"},
		},
		changed: []string{"api.feat-x.shop.test"},
	}
	d.SetEdgeManager(m)

	ctx := WithSession(context.Background(), api.Session{Transport: "ssh", Identity: "commander"})
	res, err := d.Expose(ctx, env.ExposeRequest{App: "shop", Name: "feat-x", Service: "api"})
	if err != nil {
		t.Fatalf("Expose: %v", err)
	}
	if m.exposeReq.Service != "api" || m.exposeReq.Name != "feat-x" {
		t.Errorf("the request reached the route table as %+v", m.exposeReq)
	}
	if m.identity != "commander" {
		t.Errorf("identity = %q, want the session's key name on every route change", m.identity)
	}
	if res.Env.Name != "feat-x" || len(res.Routes) != 2 || len(res.Changed) != 1 {
		t.Fatalf("result = %+v, want the env, both routes and the one change", res)
	}

	if res.URL != "https://feat-x.shop.test" {
		t.Errorf("URL = %q, want the first route's", res.URL)
	}
}

func TestUnexposeAnswersWithWhatIsLeft(t *testing.T) {
	d := edgeDaemon(t)
	m := &fakeEdgeManager{env: &env.Env{App: "shop", Name: "feat-x"}, changed: []string{"feat-x.shop.test"}}
	d.SetEdgeManager(m)

	res, err := d.Unexpose(context.Background(), env.UnexposeRequest{App: "shop", Name: "feat-x"})
	if err != nil {
		t.Fatalf("Unexpose: %v", err)
	}
	if m.unexposeReq.App != "shop" {
		t.Errorf("the request reached the route table as %+v", m.unexposeReq)
	}
	if len(res.Routes) != 0 || res.URL != "" {
		t.Errorf("result = %+v, want no routes and no URL once the last name is gone", res)
	}
	if len(res.Changed) != 1 || res.Changed[0] != "feat-x.shop.test" {
		t.Errorf("changed = %v, want the name that was removed", res.Changed)
	}
}

func TestExposeWithoutARouteTableSaysSo(t *testing.T) {
	d := edgeDaemon(t)
	for name, err := range map[string]error{
		"expose":   second(d.Expose(context.Background(), env.ExposeRequest{App: "shop", Name: "feat-x"})),
		"unexpose": second(d.Unexpose(context.Background(), env.UnexposeRequest{App: "shop", Name: "feat-x"})),
	} {
		if err == nil || !strings.Contains(err.Error(), "not available") {
			t.Errorf("%s = %v, want it to say routes are not available here", name, err)
		}
	}
}

func TestEdgeStatusIsTheEdgesOwnAnswer(t *testing.T) {
	d := edgeDaemon(t)
	d.SetEdgeClient(&fakeEdgeClient{status: &edge.Status{
		Version: "test", HTTP3: true,
		Routes: []edge.Route{{Host: "feat-x.shop.test", Kind: edge.KindHTTPS}},
	}})

	st, err := d.EdgeStatus(context.Background())
	if err != nil {
		t.Fatalf("EdgeStatus: %v", err)
	}
	switch {
	case !st.Running:
		t.Error("an edge that answered is not running")
	case len(st.Routes) != 1:
		t.Errorf("routes = %+v, want the live table", st.Routes)
	case st.TLS != certs.ModeACME:
		t.Errorf("tls = %q, want the machine's mode filled in", st.TLS)
	case st.ACMEDirectory == "":
		t.Error("the ACME directory was not filled in")
	}
}

func TestEdgeStatusReportsAnEdgeThatIsNotThere(t *testing.T) {
	for name, d := range map[string]*Daemon{
		"a machine with no edge at all": {Config: serverconfig.Default()},
		"an edge that is not answering": edgeDaemon(t),
	} {
		if name == "an edge that is not answering" {
			d.SetEdgeClient(&fakeEdgeClient{err: errors.New("dial edge.sock: connection refused")})
		}
		st, err := d.EdgeStatus(context.Background())
		if err != nil {
			t.Fatalf("%s: EdgeStatus = %v, want a report", name, err)
		}
		if st.Running || st.Error == "" {
			t.Errorf("%s: status = %+v, want running=false and a reason", name, st)
		}
	}
}

func TestEdgeStatusShowsTheRecordedRoutesWhenNothingIsServing(t *testing.T) {
	store, envID := edgeStore(t)
	d := edgeDaemon(t)
	d.Store = store
	d.SetEdgeClient(&fakeEdgeClient{err: errors.New("connection refused")})

	ctx := context.Background()
	r, err := store.AddRoute(ctx, state.Route{EnvID: envID, Service: "web", Host: "feat-x.shop.test"})
	if err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := store.PutTarget(ctx, state.EdgeTarget{RouteID: r.ID, Replica: 2, Port: 20005,
		State: state.TargetActive}); err != nil {
		t.Fatalf("PutTarget: %v", err)
	}

	st, err := d.EdgeStatus(ctx)
	if err != nil {
		t.Fatalf("EdgeStatus: %v", err)
	}
	if len(st.Routes) != 1 {
		t.Fatalf("routes = %+v, want the one caramelod recorded", st.Routes)
	}
	got := st.Routes[0]
	if got.Host != "feat-x.shop.test" || got.Env != "feat-x" || got.App != "shop" || got.Service != "web" {
		t.Errorf("route = %+v, want the environment named", got)
	}
	if len(got.Targets) != 1 || got.Targets[0].Replica != 2 || got.Targets[0].Port != 20005 {
		t.Errorf("targets = %+v, want the recorded replica", got.Targets)
	}
}

func TestEdgeCARefusesOnAPublicCA(t *testing.T) {
	d := edgeDaemon(t)
	d.SetEdgeClient(&fakeEdgeClient{ca: &certs.CA{PEM: "-----BEGIN CERTIFICATE-----"}})

	_, err := d.EdgeCA(context.Background())
	if err == nil || !strings.Contains(err.Error(), "internal") {
		t.Fatalf("EdgeCA = %v, want it to point at 'tls: internal'", err)
	}
	if !strings.Contains(err.Error(), certs.LetsEncryptProduction) {
		t.Errorf("EdgeCA = %v, want it to name the CA this machine uses", err)
	}
}

func TestEdgeCAInInternalMode(t *testing.T) {
	d := edgeDaemon(t)
	d.Config.TLS = string(certs.ModeInternal)
	want := &certs.CA{Subject: "Caramelo Internal CA", PEM: "-----BEGIN CERTIFICATE-----\n"}
	d.SetEdgeClient(&fakeEdgeClient{ca: want})

	ca, err := d.EdgeCA(context.Background())
	if err != nil {
		t.Fatalf("EdgeCA: %v", err)
	}
	if ca.PEM != want.PEM {
		t.Errorf("ca = %+v, want the edge's root", ca)
	}

	d.SetEdgeClient(&fakeEdgeClient{ca: &certs.CA{}})
	if _, err := d.EdgeCA(context.Background()); err == nil || !strings.Contains(err.Error(), "expose") {
		t.Errorf("EdgeCA with no CA yet = %v, want it to say how one is made", err)
	}
}

func TestEnvEdgeReadsTheRecordedRoutes(t *testing.T) {
	store, envID := edgeStore(t)
	d := edgeDaemon(t)
	d.Store = store
	d.SetEdgeClient(&fakeEdgeClient{err: errors.New("connection refused")})

	ctx := context.Background()
	r, err := store.AddRoute(ctx, state.Route{EnvID: envID, Service: "web", Host: "feat-x.shop.test"})
	if err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := store.PutTarget(ctx, state.EdgeTarget{RouteID: r.ID, Replica: 1, Port: 20002,
		State: state.TargetActive}); err != nil {
		t.Fatalf("PutTarget: %v", err)
	}

	routes, held := d.envEdge(ctx, "shop", "feat-x")
	if len(routes) != 1 || routes[0].Host != "feat-x.shop.test" || routes[0].Env != "feat-x" {
		t.Fatalf("routes = %+v", routes)
	}
	if len(routes[0].Targets) != 1 || routes[0].Targets[0].Port != 20002 {
		t.Errorf("targets = %+v, want the recorded replica", routes[0].Targets)
	}
	if held != nil {
		t.Errorf("certificates = %+v, want none from an edge that is not answering", held)
	}

	if routes, held := d.envEdge(ctx, "shop", "feat-y"); routes != nil || held != nil {
		t.Errorf("an unexposed env reported %+v / %+v", routes, held)
	}
}

func TestEnvEdgePrefersTheLiveTable(t *testing.T) {
	store, envID := edgeStore(t)
	d := edgeDaemon(t)
	d.Store = store
	ctx := context.Background()
	if _, err := store.AddRoute(ctx, state.Route{EnvID: envID, Service: "web", Host: "feat-x.shop.test"}); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	d.SetEdgeClient(&fakeEdgeClient{status: &edge.Status{
		Running: true,
		Routes: []edge.Route{{Host: "feat-x.shop.test", Service: "web", Targets: []edge.Target{
			{Replica: 7, Port: 20009, State: edge.TargetActive, Inflight: 4},
		}}},
		Certificates: []certs.Certificate{
			{Host: "feat-x.shop.test", Issuer: "Pebble", State: certs.Live, Serial: "42C5"},
			{Host: "feat-x.shop.test", Issuer: "Caramelo Internal CA", State: certs.Stale,
				IssuerKey: "internal"},
			{Host: "someone-else.test", Issuer: "Pebble", State: certs.Live},
		},
	}})

	routes, held := d.envEdge(ctx, "shop", "feat-x")
	if len(routes) != 1 || len(routes[0].Targets) != 1 || routes[0].Targets[0].Inflight != 4 {
		t.Fatalf("routes = %+v, want the live targets", routes)
	}
	if routes[0].Env != "feat-x" || routes[0].App != "shop" {
		t.Errorf("route = %+v, want the environment named even on the live path", routes[0])
	}
	if len(held) != 1 || held[0].Host != "feat-x.shop.test" {
		t.Errorf("certificates = %+v, want only this environment's", held)
	}
	if held[0].State == certs.Stale || held[0].Serial != "42C5" {
		t.Errorf("certificate = %+v, want the live one: `env show` and `up` show what is served, "+
			"not what the store still holds", held[0])
	}
}

func TestEveryReplicaReportsWhatItIsHolding(t *testing.T) {
	services := []env.Service{
		{Name: "web", Replicas: []env.Replica{{Service: "web", Index: 1}, {Service: "web", Index: 2}}},
		{Name: "worker", Replicas: []env.Replica{{Service: "worker", Index: 1}}},
	}
	routes := []edge.Route{
		{Host: "feat-x.shop.test", Service: "web", Targets: []edge.Target{
			{Replica: 1, Port: 20002, State: edge.TargetActive, Inflight: 3},
			{Replica: 2, Port: 20006, State: edge.TargetDraining, Inflight: 1},
		}},
		{Host: "other.shop.test", Service: "web", Targets: []edge.Target{
			{Replica: 1, Port: 20002, State: edge.TargetActive, Inflight: 2},
		}},
	}
	overlayInflight(services, routes)
	if got := services[0].Replicas[0].Inflight; got != 5 {
		t.Errorf("web/1 holds %d, want 5: the requests on it, whichever name they arrived at", got)
	}
	if got := services[0].Replicas[1].Inflight; got != 1 {
		t.Errorf("web/2 holds %d, want the one request the drain is waiting for", got)
	}
	if got := services[1].Replicas[0].Inflight; got != 0 {
		t.Errorf("worker/1 holds %d, want 0: nothing routes to it", got)
	}
}

func TestTheRouteTableIsPushedAgainWhenTheEdgeComesBack(t *testing.T) {
	store, envID := edgeStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := store.AddRoute(ctx, state.Route{EnvID: envID, Service: "web", Host: "feat-x.shop.test"}); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	client := &restartingEdge{gone: make(chan struct{}, 8)}
	d := edgeDaemon(t)
	d.Store = store
	d.SetEdgeClient(client)
	d.EnvManager = &env.Manager{Store: store, Edge: client}

	prev := edgeKeeperRetry
	edgeKeeperRetry = time.Millisecond

	t.Cleanup(func() { edgeKeeperRetry = prev })
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.keepEdgeTable(ctx, func(string, ...any) {})
	}()
	t.Cleanup(func() { cancel(); <-done })

	for i := 0; i < 3; i++ {
		select {
		case <-client.gone:
		case <-time.After(5 * time.Second):
			t.Fatalf("the keeper stopped subscribing after %d restarts", i)
		}
	}
	cancel()
	if got := client.pushed(); got < 3 {
		t.Errorf("%d tables were pushed across three restarts of the edge, want one each", got)
	}
	if hosts := client.hosts(); len(hosts) != 1 || hosts[0] != "feat-x.shop.test" {
		t.Errorf("the pushed table serves %v, want what the rows say", hosts)
	}
}

type restartingEdge struct {
	mu     sync.Mutex
	tables []edge.Table
	gone   chan struct{}
}

func (f *restartingEdge) PushTable(_ context.Context, t edge.Table) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tables = append(f.tables, t.Clone())
	return nil
}

func (f *restartingEdge) Status(context.Context) (*edge.Status, error) {
	return &edge.Status{Running: true}, nil
}

func (f *restartingEdge) Subscribe(ctx context.Context, _ time.Time, _ func(edge.Event) error) error {
	select {
	case f.gone <- struct{}{}:
	default:
	}
	return errors.New("the edge went away")
}

func (f *restartingEdge) Counts(_ context.Context, since time.Time) (*edge.Counts, error) {
	return &edge.Counts{Since: since}, nil
}

func (f *restartingEdge) Prune(_ context.Context, req certs.PruneRequest) (*certs.PruneResult, error) {
	return &certs.PruneResult{KeepFor: req.Keep(), DryRun: req.DryRun}, nil
}

func (f *restartingEdge) CA(context.Context) (*certs.CA, error) { return nil, nil }

func (f *restartingEdge) Close() error { return nil }

func (f *restartingEdge) pushed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tables)
}

func (f *restartingEdge) hosts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tables) == 0 {
		return nil
	}
	var out []string
	for _, r := range f.tables[len(f.tables)-1].Routes {
		out = append(out, r.Host)
	}
	return out
}

func edgeStore(t *testing.T) (state.Store, int64) {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "caramelo.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.AddApp(ctx, state.App{Name: "shop", RepoPath: "/repo"}); err != nil {
		t.Fatalf("AddApp: %v", err)
	}
	e, err := store.CreateEnv(ctx, state.EnvRecord{App: "shop", Name: "feat-x", Branch: "feat-x",
		Worktree: "/wt", PortBase: 20000, PortCount: 32, Status: state.EnvReady})
	if err != nil {
		t.Fatalf("CreateEnv: %v", err)
	}
	return store, e.ID
}

func second[T any](_ T, err error) error { return err }

func TestEdgePruneResolvesTheMachinesRetention(t *testing.T) {
	d := edgeDaemon(t)
	d.Config.CertsKeep = "48h"
	client := &fakeEdgeClient{}
	d.SetEdgeClient(client)

	if _, err := d.EdgePrune(context.Background(), certs.PruneRequest{}); err != nil {
		t.Fatalf("EdgePrune: %v", err)
	}
	if client.pruneReq == nil || client.pruneReq.KeepFor == nil {
		t.Fatalf("the edge was asked %+v, want the machine's certs_keep filled in", client.pruneReq)
	}
	if *client.pruneReq.KeepFor != 48*time.Hour {
		t.Errorf("the edge was asked to keep for %s, want the 48h in config.yaml", *client.pruneReq.KeepFor)
	}

	asked := 2 * time.Hour
	if _, err := d.EdgePrune(context.Background(), certs.PruneRequest{KeepFor: &asked, DryRun: true}); err != nil {
		t.Fatalf("EdgePrune --older-than: %v", err)
	}
	if *client.pruneReq.KeepFor != asked || !client.pruneReq.DryRun {
		t.Errorf("the edge was asked %+v, want the caller's retention and dry run kept",
			client.pruneReq)
	}

	zero := time.Duration(0)
	if _, err := d.EdgePrune(context.Background(), certs.PruneRequest{KeepFor: &zero}); err != nil {
		t.Fatalf("EdgePrune --older-than 0: %v", err)
	}
	if *client.pruneReq.KeepFor != 0 {
		t.Errorf("--older-than 0 reached the edge as %s; the escape must not be read as unset",
			*client.pruneReq.KeepFor)
	}
}

func TestEdgePruneRefusesWhatItCannotRun(t *testing.T) {
	d := edgeDaemon(t)
	d.SetEdgeClient(&fakeEdgeClient{err: errors.New("the edge is not answering")})
	if _, err := d.EdgePrune(context.Background(), certs.PruneRequest{}); err == nil ||
		!strings.Contains(err.Error(), "not answering") {
		t.Errorf("EdgePrune with the edge down = %v, want the reason: a prune errors where "+
			"`edge status` reports", err)
	}

	off := edgeDaemon(t)
	off.Config.Edge = false
	if _, err := off.EdgePrune(context.Background(), certs.PruneRequest{}); err == nil {
		t.Error("a machine with no edge pruned anyway")
	}

	bad := edgeDaemon(t)
	bad.Config.CertsKeep = "a fortnight"
	bad.SetEdgeClient(&fakeEdgeClient{})
	if _, err := bad.EdgePrune(context.Background(), certs.PruneRequest{}); err == nil ||
		!strings.Contains(err.Error(), "certs_keep") {
		t.Errorf("EdgePrune with an unreadable certs_keep = %v, want it to name the key", err)
	}

	negative := time.Duration(-time.Hour)
	if _, err := d.EdgePrune(context.Background(), certs.PruneRequest{KeepFor: &negative}); err == nil {
		t.Error("a negative retention reached the edge")
	}
}
