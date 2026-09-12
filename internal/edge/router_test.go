package edge

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInstallRefusesATableItCannotServeAndKeepsTheOldOne(t *testing.T) {
	clock := newClock()
	r := NewRouter(RouterOptions{Now: clock.Now})
	good := Table{Routes: []Route{route("feat-x.shop.test", Target{Replica: 1, Port: 20001, State: TargetActive})}}
	if err := r.Install(good); err != nil {
		t.Fatalf("Install: %v", err)
	}
	bad := Table{Routes: []Route{{Host: "feat-y.shop.test", Kind: KindTCP}}}
	if err := r.Install(bad); err == nil {
		t.Fatal("Install of a reserved kind = nil, want an error")
	}
	if !r.Has("feat-x.shop.test") || r.Has("feat-y.shop.test") {
		t.Errorf("a refused push half-applied: hosts = %v", r.Hosts())
	}
}

func TestRouterKeepsInflightAcrossAPush(t *testing.T) {
	clock := newClock()
	r := NewRouter(RouterOptions{Now: clock.Now})
	first := Table{Routes: []Route{route("feat-x.shop.test",
		Target{Replica: 1, Port: 20001, State: TargetActive},
	)}}
	if err := r.Install(first); err != nil {
		t.Fatalf("Install: %v", err)
	}
	_, pool, ok := r.Lookup("FEAT-X.shop.test.")
	if !ok {
		t.Fatal("Lookup did not normalise the hostname")
	}
	if _, err := pool.begin(1, func() {}); err != nil {
		t.Fatalf("begin: %v", err)
	}

	second := Table{Routes: []Route{route("feat-x.shop.test",
		Target{Replica: 1, Port: 20001, State: TargetActive},
		Target{Replica: 2, Port: 20002, State: TargetActive},
	)}}
	if err := r.Install(second); err != nil {
		t.Fatalf("Install: %v", err)
	}

	live := r.Table()
	if got := live.Routes[0].Inflight(); got != 1 {
		t.Errorf("in-flight after a push = %d, want the request that was already running", got)
	}
	if got := r.Pushed().Routes[0].Inflight(); got != 0 {
		t.Errorf("the pushed table carries in-flight %d; counts belong to the process, not to the table", got)
	}
}

func TestRouterReportsWhatARolloutDoes(t *testing.T) {
	clock := newClock()
	var drained []Target
	r := NewRouter(RouterOptions{Now: clock.Now, Drain: time.Second, OnDraining: func(t Target) { drained = append(drained, t) }})

	seen := make(chan Event, 32)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- r.Events().subscribe(ctx, time.Time{}, func(e Event) error { seen <- e; return nil })
	}()

	waitFor(t, func() bool { return subscribers(r.Events()) > 0 })

	if err := r.Install(Table{Routes: []Route{route("feat-x.shop.test",
		Target{Replica: 1, Port: 20001, State: TargetActive},
	)}}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if e := collect(t, seen, 1, time.Second)[0]; e.Kind != EventTable {
		t.Fatalf("first event = %+v, want the table acknowledgement", e)
	}

	if err := r.Install(Table{Routes: []Route{route("feat-x.shop.test",
		Target{Replica: 1, Port: 20001, State: TargetDraining},
		Target{Replica: 2, Port: 20002, State: TargetActive},
	)}}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	got := collect(t, seen, 4, time.Second)
	var kinds []EventKind
	for _, e := range got {
		kinds = append(kinds, e.Kind)
	}

	want := []EventKind{EventTarget, EventTarget, EventDrained, EventTable}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("events = %v, want %v", kinds, want)
		}
	}
	if len(drained) != 1 || drained[0].Replica != 1 {
		t.Errorf("onDraining = %+v, want replica 1", drained)
	}

	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("subscribe = %v", err)
	}
}

func TestSubscribeReplaysFromSince(t *testing.T) {
	clock := newClock()
	r := NewRouter(RouterOptions{Now: clock.Now})
	if err := r.Install(Table{Routes: []Route{route("feat-x.shop.test",
		Target{Replica: 1, Port: 20001, State: TargetActive})}}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	seen := make(chan Event, 8)
	go func() {
		_ = r.Events().subscribe(ctx, clock.Now().Add(-time.Hour), func(e Event) error { seen <- e; return nil })
	}()
	if e := collect(t, seen, 1, time.Second)[0]; e.Kind != EventTable {
		t.Errorf("replayed event = %+v, want the table that was pushed before the subscription", e)
	}
}

func TestASubscriberThatStopsReadingIsDropped(t *testing.T) {
	b := newBroker()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	block := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- b.subscribe(ctx, time.Time{}, func(Event) error {
			<-block
			return nil
		})
	}()
	waitFor(t, func() bool { return subscribers(b) > 0 })

	for range subscriberBuffer + 8 {
		b.publish(Event{Kind: EventTarget, At: time.Now()})
	}
	close(block)
	if err := <-done; !errors.Is(err, ErrSubscriberTooSlow) {
		t.Errorf("subscribe = %v, want %v: the proxy must never wait for a control-plane client", err, ErrSubscriberTooSlow)
	}
}

func TestSweepReachesEveryRoute(t *testing.T) {
	clock := newClock()
	r := NewRouter(RouterOptions{Now: clock.Now, Drain: 2 * time.Second})
	if err := r.Install(Table{Routes: []Route{
		route("a.shop.test", Target{Replica: 1, Port: 20001, State: TargetDraining}),
		route("b.shop.test", Target{Replica: 1, Port: 20002, State: TargetDraining}),
	}}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	seen := make(chan Event, 8)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = r.Events().subscribe(ctx, time.Time{}, func(e Event) error { seen <- e; return nil }) }()
	waitFor(t, func() bool { return subscribers(r.Events()) > 0 })

	r.Sweep(clock.advance(time.Hour))
	select {
	case e := <-seen:
		t.Fatalf("sweep reported %+v for a target that had already drained", e)
	case <-time.After(50 * time.Millisecond):
	}
}

func subscribers(b *broker) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("waited two seconds for something that never happened")
}
