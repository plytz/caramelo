package env

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/fleet"
)

func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return p
}

func TestHandoffChangesTheOwnerAndSaysSoInTheFeed(t *testing.T) {
	h := newHarness(t)
	f, _ := h.onFleet("nx2")
	ctx := WithIdentity(context.Background(), "agent-a")
	if _, err := h.m.Create(ctx, CreateRequest{App: "shop", Name: "feat-x", From: "main"}, &h.out); err != nil {
		t.Fatalf("Create: %v\n%s", err, h.out.String())
	}

	e, err := h.m.Handoff(ctx, "shop", "feat-x", "agent-b")
	if err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	if e.Owner != "agent-b" {
		t.Fatalf("owner = %q, want agent-b", e.Owner)
	}
	if got := f.dir[[2]string{"shop", "feat-x"}].Owner; got != "agent-b" {
		t.Fatalf("the directory still says %q owns it", got)
	}
	_, _, events, err := h.m.Show(ctx, "shop", "feat-x")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	var line string
	for _, ev := range events {
		if ev.Action == "handoff" {
			line = ev.Detail
		}
	}
	if line != "agent-a → agent-b" {
		t.Fatalf("the feed says %q, want both names", line)
	}
}

func TestHandoffToTheOwnerItAlreadyHasChangesNothing(t *testing.T) {
	h := newHarness(t)
	_, rec := h.onFleet("nx2")
	ctx := WithIdentity(context.Background(), "agent-a")
	if _, err := h.m.Create(ctx, CreateRequest{App: "shop", Name: "feat-x", From: "main"}, &h.out); err != nil {
		t.Fatalf("Create: %v", err)
	}
	before := rec.count()
	if _, err := h.m.Handoff(ctx, "shop", "feat-x", "agent-a"); err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	if rec.count() != before {
		t.Fatal("a handoff that changed nothing announced something")
	}
	_, _, events, _ := h.m.Show(ctx, "shop", "feat-x")
	for _, ev := range events {
		if ev.Action == "handoff" {
			t.Fatal("a handoff that changed nothing wrote an event")
		}
	}
}

func TestHandoffNeedsAPeerAndAFleet(t *testing.T) {
	h := newHarness(t)
	h.onFleet("nx2")
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})

	if _, err := h.m.Handoff(context.Background(), "shop", "feat-x", "  "); err == nil ||
		!strings.Contains(err.Error(), "--to") {
		t.Fatalf("error = %v, want one naming the flag", err)
	}
	h.wire(func(w *FleetWiring) { w.Fleet = nil })
	if _, err := h.m.Handoff(context.Background(), "shop", "feat-x", "agent-b"); err == nil {
		t.Fatal("a machine that keeps no owner handed an environment over anyway")
	}
}

func TestListMineIsTheEnvironmentsOnePeerOwns(t *testing.T) {
	h := newHarness(t)
	h.onFleet("nx2")
	a := WithIdentity(context.Background(), "agent-a")
	b := WithIdentity(context.Background(), "agent-b")
	if _, err := h.m.Create(a, CreateRequest{App: "shop", Name: "feat-x", From: "main"}, &h.out); err != nil {
		t.Fatalf("Create feat-x: %v", err)
	}
	if _, err := h.m.Create(b, CreateRequest{App: "shop", Name: "feat-y", From: "main"}, &h.out); err != nil {
		t.Fatalf("Create feat-y: %v", err)
	}
	mine, err := h.m.Owned(a, "shop", "agent-a")
	if err != nil {
		t.Fatalf("Owned: %v", err)
	}
	if len(mine) != 1 || mine[0].Name != "feat-x" {
		t.Fatalf("agent-a owns %d environments: %+v", len(mine), mine)
	}
}

func TestExposeViaHubRecordsTheChoiceAndAnnouncesIt(t *testing.T) {
	h := newHarness(t)
	f, _ := h.onFleet("nx2", hub("hub", "amd64", 0, nil), member("nx2", "arm64", 0, nil))
	h.wire(func(w *FleetWiring) { w.Role = fleet.RoleMember })
	h.m.Edge = newEdge()
	h.cfg = serviceConfig()
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})

	if _, err := h.m.Expose(context.Background(), ExposeRequest{
		App: "shop", Name: "feat-x", Host: "feat-x.shop.test", Via: config.ViaHub,
	}, &h.out); err != nil {
		t.Fatalf("Expose: %v\n%s", err, h.out.String())
	}
	row, err := f.EnvFleetRow(context.Background(), h.envID("feat-x"))
	if err != nil {
		t.Fatalf("read the fleet row: %v", err)
	}
	if row.Via != config.ViaHub {
		t.Fatalf("via = %q, want hub", row.Via)
	}
	if got := f.dir[[2]string{"shop", "feat-x"}].Via; got != string(config.ViaHub) {
		t.Fatalf("the directory says via %q, want hub", got)
	}
}

func TestAPrivateMemberRefusesViaNodeNamingTheFlagItWasJoinedWith(t *testing.T) {
	h := newHarness(t)
	h.onFleet("nx2", hub("hub", "amd64", 0, nil), member("nx2", "arm64", 0, nil))
	h.wire(func(w *FleetWiring) { w.Role = fleet.RoleMember })
	h.wire(func(w *FleetWiring) { w.Private = true })
	h.m.Edge = newEdge()
	h.cfg = serviceConfig()
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})

	_, err := h.m.Expose(context.Background(), ExposeRequest{
		App: "shop", Name: "feat-x", Host: "feat-x.shop.test", Via: config.ViaNode,
	}, &h.out)
	if err == nil || !strings.Contains(err.Error(), "--private") {
		t.Fatalf("error = %v, want one naming the flag the machine was joined with", err)
	}
}

func TestTheIngressIsOpenOnAMemberThatServesSomethingViaTheHub(t *testing.T) {
	h := newHarness(t)
	f, _ := h.onFleet("nx2", hub("hub", "amd64", 0, nil), member("nx2", "arm64", 0, nil))
	h.wire(func(w *FleetWiring) { w.Role = fleet.RoleMember })

	f.machines[0].Subnet = mustPrefix(t, "10.86.0.0/16")
	h.m.Edge = newEdge()
	h.cfg = serviceConfig()
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})

	if ing := h.m.ingress(context.Background()); ing == nil || ing.Enabled {
		t.Fatalf("ingress = %+v, want one that is not enabled", ing)
	}
	if _, err := h.m.Expose(context.Background(), ExposeRequest{
		App: "shop", Name: "feat-x", Host: "feat-x.shop.test", Via: config.ViaHub,
	}, &h.out); err != nil {
		t.Fatalf("Expose: %v\n%s", err, h.out.String())
	}
	ing := h.m.ingress(context.Background())
	if ing == nil || !ing.Enabled || ing.Port == 0 {
		t.Fatalf("ingress = %+v, want it open on a port", ing)
	}
	if len(ing.Trusted) != 1 || ing.Trusted[0] != "10.86.0.1" {
		t.Fatalf("trusted = %v, want the hub's tunnel address and nothing else", ing.Trusted)
	}
}

func TestAHubAndAMachineOfOnePushNoIngressAtAll(t *testing.T) {
	h := newHarness(t)
	if ing := h.m.ingress(context.Background()); ing != nil {
		t.Fatalf("a machine of one has an ingress: %+v", ing)
	}
	h.onFleet("hub", hub("hub", "amd64", 0, nil))
	h.wire(func(w *FleetWiring) { w.Role = fleet.RoleHub })
	if ing := h.m.ingress(context.Background()); ing != nil {
		t.Fatalf("the hub has an ingress of its own: %+v", ing)
	}
}

func TestViaRouteIsAKindOfItsOwnWithTheMachineNamed(t *testing.T) {
	r := ViaRoute("Shop.Example.COM", "m1", "shop", "production", "web", []int{18487, 18488}, 0)
	if r.Host != "shop.example.com" {
		t.Fatalf("host = %q, want it normalised", r.Host)
	}
	if r.Kind != "via" || r.Via != "m1" {
		t.Fatalf("route = %+v, want a via route naming m1", r)
	}
	if len(r.Targets) != 2 || r.Targets[0].Port != 18487 || !r.Targets[0].State.Routable() {
		t.Fatalf("targets = %+v, want two active relay ports", r.Targets)
	}

	if err := (edge.Table{Routes: []edge.Route{r}}).Validate(); err != nil {
		t.Fatalf("validate = %v, want the route this package builds to be accepted", err)
	}
	anonymous := r
	anonymous.Via = ""
	err := (edge.Table{Routes: []edge.Route{anonymous}}).Validate()
	if err == nil || !strings.Contains(err.Error(), "must name the machine") {
		t.Fatalf("validate of a via route with no machine = %v, want it refused for that", err)
	}
}

func TestOneRelayPortPerMachine(t *testing.T) {
	m1 := fleet.Machine{Name: "m1", Subnet: mustPrefix(t, "10.87.0.0/16")}
	m2 := fleet.Machine{Name: "m2", Subnet: mustPrefix(t, "10.88.0.0/16")}
	p1, ok1 := ViaRelayPortFor(m1)
	p2, ok2 := ViaRelayPortFor(m2)
	if !ok1 || !ok2 || p1 == p2 {
		t.Fatalf("ports = %d and %d (%v, %v), want two different ones", p1, p2, ok1, ok2)
	}
	again, _ := ViaRelayPortFor(m1)
	if again != p1 {
		t.Fatalf("the port of one machine moved between two calls: %d then %d", p1, again)
	}
	if _, ok := ViaRelayPortFor(fleet.Machine{Name: "joining"}); ok {
		t.Fatal("a machine with no subnet was given a relay port")
	}
}
