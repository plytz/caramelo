package edge

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTheNewStatesAreValidAndNotRoutable(t *testing.T) {
	for _, s := range []TargetState{TargetUnhealthy, TargetHeld} {
		if !s.Valid() {
			t.Errorf("%s is not a valid state", s)
		}
		if s.Routable() {
			t.Errorf("%s is routable", s)
		}
	}
	if !TargetActive.Routable() {
		t.Error("active is not routable")
	}
	for _, s := range []TargetState{TargetStarting, TargetDraining, TargetStopped} {
		if s.Routable() {
			t.Errorf("%s is routable", s)
		}
	}
	if len(TargetStates) != 6 {
		t.Errorf("%d states, want 6", len(TargetStates))
	}
}

func TestValidateAcceptsHeldAndUnhealthy(t *testing.T) {
	tb := Table{Routes: []Route{{
		Host: "shop.test", Kind: KindHTTPS, Env: "production", Service: "web",
		Targets: []Target{
			{Replica: 1, Port: 20001, State: TargetHeld},
			{Replica: 2, Port: 20002, State: TargetUnhealthy},
			{Replica: 3, Port: 20003, State: TargetActive},
		},
	}}}
	if err := tb.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := tb.Routes[0].Active(); len(got) != 1 || got[0].Replica != 3 {
		t.Errorf("Active = %v, want only replica 3", got)
	}
}

func TestPoolSkipsHeldAndUnhealthy(t *testing.T) {
	p := NewPool(Route{Host: "shop.test", Targets: []Target{
		{Replica: 1, Port: 20001, State: TargetHeld},
		{Replica: 2, Port: 20002, State: TargetUnhealthy},
		{Replica: 3, Port: 20003, State: TargetActive},
	}}, poolOptions{})
	for i := 0; i < 6; i++ {
		got, ok := p.Pick()
		if !ok {
			t.Fatalf("pick %d: nothing active", i)
		}
		if got.Replica != 3 {
			t.Fatalf("pick %d chose replica %d", i, got.Replica)
		}
	}

	none := NewPool(Route{Host: "shop.test", Targets: []Target{
		{Replica: 1, Port: 20001, State: TargetHeld},
		{Replica: 2, Port: 20002, State: TargetUnhealthy},
	}}, poolOptions{})
	if got, ok := none.Pick(); ok {
		t.Errorf("a pool of held and unhealthy targets offered %v", got)
	}
}

func TestCountsArithmetic(t *testing.T) {
	c := &Counts{Hosts: []HostCounts{{
		Host: "Shop.Test.", Requests: 100, Status5xx: 3, ConnectFailures: 2,
		Targets: []TargetCounts{
			{Replica: 1, Requests: 60, Status5xx: 0, ConnectFailures: 0},
			{Replica: 2, Requests: 40, Status5xx: 3, ConnectFailures: 2},
		},
	}}}

	h, ok := c.Host("shop.test")
	if !ok {
		t.Fatal("Host did not find shop.test")
	}
	if h.Errors() != 5 {
		t.Errorf("Errors = %d, want 5", h.Errors())
	}
	if h.Rate() != 0.05 {
		t.Errorf("Rate = %v, want 0.05", h.Rate())
	}
	if _, ok := c.Host("other.test"); ok {
		t.Error("Host found a name the edge does not serve")
	}
	if _, ok := (*Counts)(nil).Host("shop.test"); ok {
		t.Error("Host on a nil Counts found something")
	}

	if got := (HostCounts{}).Rate(); got != 0 {
		t.Errorf("the rate of an idle host = %v", got)
	}

	two, ok := h.Target(2)
	if !ok {
		t.Fatal("Target(2) not found")
	}
	if two.Errors() != 5 || two.Rate() != 0.125 {
		t.Errorf("replica 2 = %d errors, rate %v", two.Errors(), two.Rate())
	}
	if _, ok := h.Target(9); ok {
		t.Error("Target found a replica that is not there")
	}

	if got := h.Replicas(1, 2); !reflect.DeepEqual(got, TargetCounts{Requests: 100, Status5xx: 3, ConnectFailures: 2}) {
		t.Errorf("Replicas(1,2) = %+v", got)
	}
	if got := h.Replicas(1); got.Errors() != 0 || got.Requests != 60 {
		t.Errorf("Replicas(1) = %+v", got)
	}

	if got := h.Replicas(1, 9); got.Requests != 60 {
		t.Errorf("Replicas(1,9) = %+v", got)
	}
	if got := h.Replicas(); got != (TargetCounts{}) {
		t.Errorf("Replicas() = %+v", got)
	}
}

func TestEdgeCountsAnswersZerosForWhatItServes(t *testing.T) {
	started := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	e := &Edge{now: func() time.Time { return started.Add(time.Minute) }}
	e.started = started
	e.router = NewRouter(RouterOptions{})
	if err := e.router.Install(Table{Routes: []Route{{
		Host: "shop.test", Kind: KindHTTPS, Env: "production", Service: "web",
		Targets: []Target{
			{Replica: 1, Port: 20001, State: TargetHeld},
			{Replica: 2, Port: 20002, State: TargetActive},
		},
	}}}); err != nil {
		t.Fatal(err)
	}

	since := started.Add(30 * time.Second)
	got, err := e.Counts(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Since.Equal(since) {
		t.Errorf("Since = %v, want %v", got.Since, since)
	}
	if got.At.IsZero() {
		t.Error("At is zero")
	}
	h, ok := got.Host("shop.test")
	if !ok {
		t.Fatalf("no counts for shop.test: %+v", got)
	}
	if h.Requests != 0 || h.Errors() != 0 {
		t.Errorf("the stub counted %+v", h)
	}
	if len(h.Targets) != 2 {
		t.Errorf("%d replicas, want 2: %+v", len(h.Targets), h.Targets)
	}

	old, err := e.Counts(context.Background(), started.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !old.Since.Equal(started) {
		t.Errorf("Since = %v, want the edge's start %v", old.Since, started)
	}

	zero, err := e.Counts(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !zero.Since.Equal(started) {
		t.Errorf("Since = %v, want the edge's start", zero.Since)
	}
}

func TestCountsOverTheControlSocket(t *testing.T) {
	h := newFakeHandler()
	h.counts = &Counts{Hosts: []HostCounts{{
		Host: "shop.test", Requests: 412, Status5xx: 7, ConnectFailures: 1,
		Targets: []TargetCounts{
			{Replica: 3, Requests: 206, Status5xx: 7, ConnectFailures: 1},
			{Replica: 4, Requests: 206},
		},
	}}}
	c, _ := serveControl(t, h)

	since := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	got, err := c.Counts(t.Context(), since)
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if !got.Since.Equal(since) {
		t.Errorf("Since = %v, want %v", got.Since, since)
	}
	host, ok := got.Host("shop.test")
	if !ok {
		t.Fatalf("no counts for shop.test: %+v", got)
	}
	if host.Requests != 412 || host.Errors() != 8 {
		t.Errorf("host = %+v", host)
	}

	if newPool := host.Replicas(3, 4); newPool.Requests != 412 || newPool.Errors() != 8 {
		t.Errorf("the new pool = %+v", newPool)
	}

	h.countsErr = errors.New("the counters are gone")
	if _, err := c.Counts(t.Context(), since); err == nil {
		t.Error("a failing Counts was reported as success")
	} else if !strings.Contains(err.Error(), "counters are gone") {
		t.Errorf("error = %v", err)
	}
}
