package env

import (
	"context"
	"io"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/edge"
)

func TestExposeDerivesTheHostAndPushesIt(t *testing.T) {
	h, e := edgeHarness(t, 1)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	res, err := h.m.Expose(context.Background(), ExposeRequest{App: "shop", Name: "feat-x"}, &h.out)
	if err != nil {
		t.Fatalf("Expose: %v\n%s", err, h.out.String())
	}
	if len(res.Routes) != 1 || res.Routes[0].Host != webHost {
		t.Fatalf("routes = %+v, want the derived name", res.Routes)
	}
	if res.URL != "https://"+webHost {
		t.Errorf("URL = %q", res.URL)
	}
	if res.Routes[0].Service != "web" || res.Routes[0].Env != "feat-x" || res.Routes[0].App != "shop" {
		t.Errorf("route = %+v, want it to name the env and the service", res.Routes[0])
	}
	if !e.table().Has(webHost) {
		t.Error("the table the edge holds does not serve the name")
	}

	before := e.pushes()
	again, err := h.m.Expose(context.Background(), ExposeRequest{App: "shop", Name: "feat-x"}, &h.out)
	if err != nil {
		t.Fatalf("Expose twice: %v", err)
	}
	if len(again.Changed) != 0 {
		t.Errorf("the second expose changed %v, want nothing", again.Changed)
	}
	if len(h.store.hosts()) != 1 {
		t.Errorf("routes = %v, want one", h.store.hosts())
	}
	if e.pushes() != before+1 {
		t.Errorf("the second expose pushed %d tables, want exactly one", e.pushes()-before)
	}
}

func TestExposeWithAnExplicitHost(t *testing.T) {
	h, e := edgeHarness(t, 1)
	h.cfg.Domain = ""
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	if _, err := h.m.Expose(context.Background(), ExposeRequest{App: "shop", Name: "feat-x"}, &h.out); err == nil {
		t.Error("Expose without a domain and without --host = nil, want an error naming both ways out")
	}
	res, err := h.m.Expose(context.Background(),
		ExposeRequest{App: "shop", Name: "feat-x", Host: "Demo.Example.COM."}, &h.out)
	if err != nil {
		t.Fatalf("Expose --host: %v", err)
	}

	if len(res.Routes) != 1 || res.Routes[0].Host != "demo.example.com" {
		t.Fatalf("routes = %+v, want the normalised name", res.Routes)
	}
	if !e.table().Has("demo.example.com") {
		t.Error("the edge was not given the name")
	}

	if _, err := h.m.Expose(context.Background(),
		ExposeRequest{App: "shop", Name: "feat-x", Host: "other.example.com"}, &h.out); err != nil {
		t.Fatalf("Expose a second name: %v", err)
	}
	if got := h.store.hosts(); len(got) != 2 {
		t.Errorf("routes = %v, want both names", got)
	}
}

func TestARolloutMovesEveryNameOfAService(t *testing.T) {
	h, e := edgeHarness(t, 2)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	const second = "other.shop.test"
	if _, err := h.m.Expose(context.Background(),
		ExposeRequest{App: "shop", Name: "feat-x", Host: second}, &h.out); err != nil {
		t.Fatalf("Expose --host: %v\n%s", err, h.out.String())
	}

	h.setTree("bbbbbbbbbbbb")
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if roll := res.Rollouts[0]; roll.Failed != nil {
		t.Fatalf("rollout failed at %+v\n%s", roll.Failed, h.out.String())
	}
	want := map[int]bool{3: true, 4: true}
	for _, host := range []string{webHost, second} {
		r, ok := e.table().Route(host)
		if !ok {
			t.Fatalf("the edge does not serve %s at all", host)
		}
		got := map[int]bool{}
		for _, target := range r.Active() {
			got[target.Replica] = true
		}
		if len(got) != len(want) {
			t.Fatalf("%s is served by %v, want the replicas the rollout left running (%v)", host, got, want)
		}
		for replica := range want {
			if !got[replica] {
				t.Errorf("%s does not point at replica %d: it is still aimed at containers that were stopped",
					host, replica)
			}
		}
	}
}

func TestExposeRefusesANameAnotherEnvHolds(t *testing.T) {
	h, _ := edgeHarness(t, 1)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-y"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-y"})

	_, err := h.m.Expose(context.Background(),
		ExposeRequest{App: "shop", Name: "feat-y", Host: webHost}, &h.out)
	if err == nil {
		t.Fatal("Expose of a name another env holds = nil, want an error")
	}
	if !strings.Contains(err.Error(), "feat-x") || !strings.Contains(err.Error(), "--host") {
		t.Errorf("err = %v, want it to name the holder and the way out", err)
	}
}

func TestExposeRefusesAnImpossibleHost(t *testing.T) {
	h, _ := edgeHarness(t, 1)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	for _, host := range []string{"https://demo.example.com", "demo", "demo..example.com", "-demo.example.com"} {
		if _, err := h.m.Expose(context.Background(),
			ExposeRequest{App: "shop", Name: "feat-x", Host: host}, &h.out); err == nil {
			t.Errorf("Expose --host %q = nil, want a refusal", host)
		}
	}
}

func TestUnexposeRemovesTheNameAndIsIdempotent(t *testing.T) {
	h, e := edgeHarness(t, 1)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	res, err := h.m.Unexpose(context.Background(), UnexposeRequest{App: "shop", Name: "feat-x"}, &h.out)
	if err != nil {
		t.Fatalf("Unexpose: %v", err)
	}
	if len(res.Changed) != 1 || res.Changed[0] != webHost {
		t.Errorf("changed = %v, want the name that was removed", res.Changed)
	}
	if e.table().Has(webHost) {
		t.Error("the edge still serves the name")
	}
	if !h.driver.hasContainer(ReplicaContainerName("shop", "feat-x", "web", 1)) {
		t.Error("unexpose stopped the environment: it must only remove the route")
	}
	again, err := h.m.Unexpose(context.Background(), UnexposeRequest{App: "shop", Name: "feat-x"}, &h.out)
	if err != nil {
		t.Fatalf("Unexpose twice: %v", err)
	}
	if len(again.Changed) != 0 || len(again.Routes) != 0 {
		t.Errorf("the second unexpose = %+v, want nothing left and no error", again)
	}
}

func TestUnexposeCanNameOneHost(t *testing.T) {
	h, _ := edgeHarness(t, 1)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if _, err := h.m.Expose(context.Background(),
		ExposeRequest{App: "shop", Name: "feat-x", Host: "demo.example.com"}, &h.out); err != nil {
		t.Fatalf("Expose: %v", err)
	}

	if _, err := h.m.Unexpose(context.Background(),
		UnexposeRequest{App: "shop", Name: "feat-x", Host: "demo.example.com"}, &h.out); err != nil {
		t.Fatalf("Unexpose --host: %v", err)
	}
	if got := h.store.hosts(); len(got) != 1 || got[0] != webHost {
		t.Errorf("routes = %v, want only the derived name left", got)
	}
}

func TestExposeUnderAutoDomain(t *testing.T) {
	h, _ := edgeHarness(t, 1)
	h.cfg.Domain = "auto"
	h.m.PublicIP = netip.MustParseAddr("192.0.2.10")
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	want := "feat-x.shop.192-0-2-10.sslip.io"
	if res.URL != "https://"+want {
		t.Errorf("URL = %q, want %q", res.URL, "https://"+want)
	}
}

func TestAutoDomainWithoutAnAddress(t *testing.T) {
	h, _ := edgeHarness(t, 1)
	h.cfg.Domain = "auto"
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if res.URL != "" {
		t.Errorf("URL = %q, want nothing exposed", res.URL)
	}
	if !strings.Contains(h.out.String(), "public IPv4") {
		t.Errorf("nothing explained why the name could not be built:\n%s", h.out.String())
	}

	if res.Services[0].Status != ServiceRunning {
		t.Errorf("web = %q, want it running", res.Services[0].Status)
	}
}

func TestExposeWithoutAnEdge(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	for _, err := range []error{
		exposeErr(h.m.Expose(context.Background(), ExposeRequest{App: "shop", Name: "feat-x"}, &h.out)),
		exposeErr(h.m.Unexpose(context.Background(), UnexposeRequest{App: "shop", Name: "feat-x"}, &h.out)),
	} {
		if err == nil || !strings.Contains(err.Error(), "--edge") {
			t.Errorf("err = %v, want it to name the setup step", err)
		}
	}
}

func TestDestroyDropsTheRoutes(t *testing.T) {
	h, e := edgeHarness(t, 2)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	if err := h.m.Destroy(context.Background(), DestroyRequest{App: "shop", Name: "feat-x", DeleteBranch: false}, &h.out); err != nil {
		t.Fatalf("Destroy: %v\n%s", err, h.out.String())
	}
	if len(h.store.hosts()) != 0 {
		t.Errorf("routes = %v, want none after the env is gone", h.store.hosts())
	}
	if e.table().Has(webHost) {
		t.Error("the edge still serves a destroyed env's name")
	}
	for _, index := range []int{1, 2} {
		if h.driver.hasContainer(ReplicaContainerName("shop", "feat-x", "web", index)) {
			t.Errorf("replica %d survived destroy", index)
		}
	}
}

func TestTableCarriesEveryEnvsRoutes(t *testing.T) {
	h, e := edgeHarness(t, 1)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-y"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-y"})

	table := e.table()
	if len(table.Routes) != 2 {
		t.Fatalf("the edge holds %d routes, want both envs'", len(table.Routes))
	}
	for _, host := range []string{webHost, "feat-y.shop.test"} {
		r, ok := table.Route(host)
		if !ok {
			t.Fatalf("the table does not serve %s", host)
		}
		if r.Kind != edge.KindHTTPS {
			t.Errorf("%s is kind %q, want https", host, r.Kind)
		}
		if len(r.Active()) != 1 {
			t.Errorf("%s has %d active targets, want one", host, len(r.Active()))
		}
	}

	if err := h.m.PushRoutes(context.Background()); err != nil {
		t.Fatalf("PushRoutes: %v", err)
	}
	if len(e.table().Routes) != 2 {
		t.Errorf("the rebuilt table holds %d routes, want 2", len(e.table().Routes))
	}
}

func TestDownEmptiesThePoolAndKeepsTheRoute(t *testing.T) {
	h, e := edgeHarness(t, 2)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	if _, err := h.m.Down(context.Background(), DownRequest{App: "shop", Name: "feat-x"}, &h.out); err != nil {
		t.Fatalf("Down: %v", err)
	}
	r, ok := e.table().Route(webHost)
	if !ok {
		t.Fatal("down removed the route: it is not unexpose")
	}
	if len(r.Targets) != 0 {
		t.Errorf("the pool still holds %+v, want nothing behind a stopped env", r.Targets)
	}

	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if len(res.Routes) != 1 || len(res.Routes[0].Targets) != 2 {
		t.Errorf("up after down = %+v, want both replicas back behind the name", res.Routes)
	}
}

func TestConcurrentUpsAllReachTheEdge(t *testing.T) {
	h, e := edgeHarness(t, 1)
	names := []string{"feat-a", "feat-b", "feat-c", "feat-d"}
	for _, name := range names {
		h.mustCreate(CreateRequest{App: "shop", Name: name})
	}

	var wg sync.WaitGroup
	errs := make([]error, len(names))
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			_, errs[i] = h.m.Up(context.Background(), UpRequest{App: "shop", Name: name}, io.Discard)
		}(i, name)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("up %s: %v", names[i], err)
		}
	}
	table := e.table()
	for _, name := range names {
		host := name + ".shop.test"
		r, ok := table.Route(host)
		if !ok {
			t.Errorf("the edge does not serve %s", host)
			continue
		}
		if r.Env != name || len(r.Active()) != 1 {
			t.Errorf("%s = %+v, want one active target of its own env", host, r)
		}
	}
}

func exposeErr(_ *ExposeResult, err error) error { return err }

func TestThePushedTableCarriesTheServicesDrain(t *testing.T) {
	h, e := edgeHarness(t, 1)
	h.cfg.Services[0].Drain = 7 * time.Second
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	r, ok := e.table().Route(webHost)
	if !ok {
		t.Fatalf("the edge holds no route for %s\n%s", webHost, h.out.String())
	}
	if r.Drain != 7*time.Second {
		t.Errorf("route drain = %s, want the service's 7s", r.Drain)
	}
}

func TestUnexposeForgetsTheDrain(t *testing.T) {
	h, e := edgeHarness(t, 1)
	h.cfg.Services[0].Drain = 7 * time.Second
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	if _, err := h.m.Unexpose(context.Background(), UnexposeRequest{App: "shop", Name: "feat-x"}, &h.out); err != nil {
		t.Fatalf("Unexpose: %v", err)
	}
	if d := h.m.drainFor(webHost); d != 0 {
		t.Errorf("drain of an unexposed host = %s, want nothing remembered", d)
	}
	if _, ok := e.table().Route(webHost); ok {
		t.Error("the edge still holds the route that was unexposed")
	}
}

func TestExposeRecordsWhoOwnsTheRoute(t *testing.T) {
	h, _ := edgeHarness(t, 1)
	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	ctx := context.Background()

	if _, err := h.m.Expose(ctx, ExposeRequest{App: "shop", Name: e.Name}, &h.out); err != nil {
		t.Fatalf("Expose: %v", err)
	}
	if _, err := h.m.Expose(ctx, ExposeRequest{App: "shop", Name: e.Name,
		Host: "pinned.shop.test"}, &h.out); err != nil {
		t.Fatalf("Expose --host: %v", err)
	}
	rec, err := h.store.Env(ctx, "shop", e.Name)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := h.store.RoutesOfEnv(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Host] = r.Managed
	}
	if len(got) != 2 {
		t.Fatalf("routes = %v, want two", got)
	}
	for host, wantManaged := range map[string]bool{
		"feat-x.shop.test": true,
		"pinned.shop.test": false,
	} {
		managed, ok := got[host]
		if !ok {
			t.Errorf("no route for %s: %v", host, got)
			continue
		}
		if managed != wantManaged {
			t.Errorf("%s managed = %v, want %v", host, managed, wantManaged)
		}
	}

	h.out.Reset()
	if _, err := h.m.Unexpose(ctx, UnexposeRequest{App: "shop", Name: e.Name,
		Host: webHost}, &h.out); err != nil {
		t.Fatalf("Unexpose: %v", err)
	}
	if out := h.out.String(); !strings.Contains(out, "exposes it again") {
		t.Errorf("unexpose of a derived name did not say it comes back:\n%s", out)
	}
	h.out.Reset()
	if _, err := h.m.Unexpose(ctx, UnexposeRequest{App: "shop", Name: e.Name,
		Host: "pinned.shop.test"}, &h.out); err != nil {
		t.Fatalf("Unexpose --host: %v", err)
	}
	if out := h.out.String(); strings.Contains(out, "exposes it again") {
		t.Errorf("unexpose of a pinned name claimed it comes back:\n%s", out)
	}
}

func TestUnexposeOfAProtectedEnvNeedsForce(t *testing.T) {
	h, e := edgeHarness(t, 1)
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})

	for _, host := range []string{"shop.test", "www.shop.test"} {
		if _, err := h.m.Expose(context.Background(),
			ExposeRequest{App: "shop", Name: "production", Host: host}, &h.out); err != nil {
			t.Fatalf("Expose %s: %v", host, err)
		}
	}
	before := h.store.hosts()
	if len(before) != 2 {
		t.Fatalf("routes = %v, want the two names the file gives production", before)
	}

	_, err := h.m.Unexpose(context.Background(), UnexposeRequest{App: "shop", Name: "production"}, &h.out)
	if err == nil {
		t.Fatal("a protected environment was taken off the edge with no --force")
	}
	for _, want := range []string{"protected", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
	if got := h.store.hosts(); len(got) != len(before) {
		t.Fatalf("routes = %v, want the refusal to have changed nothing", got)
	}
	if got := len(e.table().Routes); got != len(before) {
		t.Errorf("the edge serves %d names, want %d: the refusal pushed a table", got, len(before))
	}

	if _, err := h.m.Unexpose(context.Background(),
		UnexposeRequest{App: "shop", Name: "production", Host: "www.shop.test"}, &h.out); err != nil {
		t.Fatalf("Unexpose --host of a protected env: %v", err)
	}
	if got := h.store.hosts(); len(got) != 1 {
		t.Errorf("routes = %v, want the apex left", got)
	}

	if _, err := h.m.Unexpose(context.Background(),
		UnexposeRequest{App: "shop", Name: "production", Force: true}, &h.out); err != nil {
		t.Fatalf("Unexpose --force: %v", err)
	}
	if got := h.store.hosts(); len(got) != 0 {
		t.Errorf("routes = %v, want none", got)
	}
}
