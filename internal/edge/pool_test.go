package edge

import (
	"sync"
	"testing"
	"time"
)

type events struct {
	mu   sync.Mutex
	list []Event
}

func (e *events) add(ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, ev)
}

func (e *events) all() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Event(nil), e.list...)
}

func (e *events) kinds(k EventKind) []Event {
	var out []Event
	for _, ev := range e.all() {
		if ev.Kind == k {
			out = append(out, ev)
		}
	}
	return out
}

func newTestPool(r Route, clock *fakeClock, ev *events, drained ...func(Target)) *RoutePool {
	o := poolOptions{now: clock.Now, drain: 10 * time.Second, emit: ev.add}
	if len(drained) > 0 {
		o.onDraining = drained[0]
	}
	return NewPool(r, o)
}

func TestPickIsRoundRobinOverActiveTargetsOnly(t *testing.T) {
	clock, ev := newClock(), &events{}
	p := newTestPool(route("feat-x.shop.test",
		Target{Replica: 1, Port: 20001, State: TargetActive},
		Target{Replica: 2, Port: 20002, State: TargetActive},
		Target{Replica: 3, Port: 20003, State: TargetStarting},
		Target{Replica: 4, Port: 20004, State: TargetDraining},
	), clock, ev)

	var got []int
	for range 6 {
		tg, ok := p.Pick()
		if !ok {
			t.Fatal("Pick found nothing while two replicas are active")
		}
		got = append(got, tg.Replica)
	}
	want := []int{1, 2, 1, 2, 1, 2}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pick order = %v, want %v: a starting or draining replica must never be picked", got, want)
		}
	}
}

func TestPickSkipsTheReplicaARetryIsAvoiding(t *testing.T) {
	clock, ev := newClock(), &events{}
	p := newTestPool(route("feat-x.shop.test",
		Target{Replica: 1, Port: 20001, State: TargetActive},
		Target{Replica: 2, Port: 20002, State: TargetActive},
	), clock, ev)

	first, _ := p.Pick()
	second, ok := p.Pick(first.Replica)
	if !ok || second.Replica == first.Replica {
		t.Fatalf("Pick(skip %d) = %+v, %v: a retry must land on another replica", first.Replica, second, ok)
	}
	if _, ok := p.Pick(1, 2); ok {
		t.Error("Pick found a target with every replica skipped")
	}
}

func TestAFailedTargetIsSkippedForTheFailureWindow(t *testing.T) {
	clock, ev := newClock(), &events{}
	p := newTestPool(route("feat-x.shop.test",
		Target{Replica: 1, Port: 20001, State: TargetActive},
		Target{Replica: 2, Port: 20002, State: TargetActive},
	), clock, ev)

	if err := p.Mark(Mark{Replica: 1, Kind: MarkFail, At: clock.Now(), Detail: "connection refused"}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	for range 4 {
		tg, ok := p.Pick()
		if !ok || tg.Replica != 2 {
			t.Fatalf("Pick = %+v, %v: the replica that refused a connection is skipped for %s", tg, ok, FailureWindow)
		}
	}

	clock.advance(FailureWindow + time.Millisecond)
	seen := map[int]bool{}
	for range 4 {
		tg, _ := p.Pick()
		seen[tg.Replica] = true
	}
	if !seen[1] {
		t.Errorf("replica 1 is still skipped after %s", FailureWindow)
	}
}

func TestMarkRefusesAReplicaTheRouteDoesNotHave(t *testing.T) {
	clock, ev := newClock(), &events{}
	p := newTestPool(route("feat-x.shop.test", Target{Replica: 1, Port: 20001, State: TargetActive}), clock, ev)
	if err := p.Mark(Mark{Replica: 7, Kind: MarkDone}); err == nil {
		t.Error("Mark on an unknown replica = nil: the pool and its caller disagreeing must be loud")
	}
	if err := p.Mark(Mark{Replica: 1, Kind: MarkState, State: TargetState("warming")}); err == nil {
		t.Error("Mark with a state that is not one = nil")
	}
	if err := p.Mark(Mark{Replica: 1, Kind: MarkKind("wat")}); err == nil {
		t.Error("Mark with an unknown kind = nil")
	}
}

func TestADrainEndsWhenTheLastRequestFinishes(t *testing.T) {
	clock, ev := newClock(), &events{}
	var draining []Target
	p := newTestPool(route("feat-x.shop.test",
		Target{Replica: 1, Port: 20001, State: TargetActive},
		Target{Replica: 2, Port: 20002, State: TargetActive},
	), clock, ev, func(t Target) { draining = append(draining, t) })

	first, err := p.begin(1, func() {})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	second, err := p.begin(1, func() {})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	if err := p.Mark(Mark{Replica: 1, Kind: MarkState, State: TargetDraining}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if len(draining) != 1 || draining[0].Replica != 1 {
		t.Errorf("onDraining = %+v, want replica 1: its idle connections have to be closed", draining)
	}
	if got := ev.kinds(EventDrained); len(got) != 0 {
		t.Fatalf("EventDrained with two requests still on the replica: %+v", got)
	}
	if _, ok := p.Pick(); !ok {
		t.Error("the other replica stopped taking requests when this one started draining")
	}

	p.end(1, first)
	if got := ev.kinds(EventDrained); len(got) != 0 {
		t.Fatalf("EventDrained with one request still on the replica: %+v", got)
	}
	p.end(1, second)
	drained := ev.kinds(EventDrained)
	if len(drained) != 1 {
		t.Fatalf("EventDrained = %+v, want exactly one", drained)
	}
	if drained[0].Deadline {
		t.Error("the drain ended because the last request finished, not because the deadline passed")
	}
	if drained[0].Target == nil || drained[0].Target.Replica != 1 || drained[0].Target.Inflight != 0 {
		t.Errorf("drained event = %+v", drained[0].Target)
	}

	p.end(1, second)
	if got := ev.kinds(EventDrained); len(got) != 1 {
		t.Errorf("EventDrained = %d, want 1", len(got))
	}
}

func TestADrainWithNothingInFlightIsReportedAtOnce(t *testing.T) {
	clock, ev := newClock(), &events{}
	p := newTestPool(route("feat-x.shop.test", Target{Replica: 1, Port: 20001, State: TargetActive}), clock, ev)
	if err := p.Mark(Mark{Replica: 1, Kind: MarkState, State: TargetDraining}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	drained := ev.kinds(EventDrained)
	if len(drained) != 1 || drained[0].Deadline {
		t.Fatalf("EventDrained = %+v, want one immediate drain", drained)
	}
}

func TestTheDrainDeadlineCutsWhatIsLeft(t *testing.T) {
	clock, ev := newClock(), &events{}
	r := route("feat-x.shop.test", Target{Replica: 1, Port: 20001, State: TargetActive})
	r.Drain = 5 * time.Second
	p := newTestPool(r, clock, ev)

	var cut int
	var mu sync.Mutex
	if _, err := p.begin(1, func() { mu.Lock(); cut++; mu.Unlock() }); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := p.Mark(Mark{Replica: 1, Kind: MarkState, State: TargetDraining, At: clock.Now()}); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	clock.advance(4 * time.Second)
	if got := p.sweep(clock.Now()); len(got) != 0 {
		t.Fatalf("sweep before the deadline = %+v", got)
	}
	mu.Lock()
	early := cut
	mu.Unlock()
	if early != 0 {
		t.Fatal("a request was cut before the drain deadline")
	}

	clock.advance(2 * time.Second)
	got := p.sweep(clock.Now())
	if len(got) != 1 || !got[0].Deadline {
		t.Fatalf("sweep at the deadline = %+v, want one drained event marked deadline", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if cut != 1 {
		t.Errorf("cut %d request(s), want 1", cut)
	}
	if p.inflight() != 0 {
		t.Errorf("in-flight = %d after the deadline, want 0", p.inflight())
	}

	if got := p.sweep(clock.Now().Add(time.Minute)); len(got) != 0 {
		t.Errorf("second sweep = %+v, want nothing", got)
	}
}

func TestInstallKeepsWhatTheEdgeKnows(t *testing.T) {
	clock, ev := newClock(), &events{}
	p := newTestPool(route("feat-x.shop.test",
		Target{Replica: 1, Port: 20001, State: TargetActive},
	), clock, ev)

	id, err := p.begin(1, func() {})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	p.install(route("feat-x.shop.test",
		Target{Replica: 1, Port: 20001, State: TargetActive},
		Target{Replica: 2, Port: 20002, State: TargetStarting},
	))
	if p.inflight() != 1 {
		t.Fatalf("in-flight = %d after a push, want the request that is still running", p.inflight())
	}

	p.install(route("feat-x.shop.test",
		Target{Replica: 1, Port: 20009, State: TargetActive},
	))
	if p.inflight() != 0 {
		t.Errorf("in-flight = %d, want 0: replica 1 answers on another port now", p.inflight())
	}
	p.end(1, id)

	counts := p.Counts()
	if len(counts) != 1 || counts[0].Port != 20009 {
		t.Errorf("Counts() = %+v", counts)
	}
}

func TestInstallReportsATargetThatArrivesAlreadyDrained(t *testing.T) {
	clock, ev := newClock(), &events{}
	p := newTestPool(route("feat-x.shop.test", Target{Replica: 1, Port: 20001, State: TargetActive}), clock, ev)
	p.install(route("feat-x.shop.test", Target{Replica: 1, Port: 20001, State: TargetDraining}))

	got := p.install(route("feat-x.shop.test", Target{Replica: 1, Port: 20001, State: TargetDraining}))
	if len(got) != 0 {
		t.Fatalf("re-pushing the same table produced %+v: it must disturb nothing", got)
	}
}

func TestAMarkedRequestNeverReleasesAProxiedOne(t *testing.T) {
	clock, ev := newClock(), &events{}
	p := newTestPool(route("feat-x.shop.test", Target{Replica: 1, Port: 20001, State: TargetActive}), clock, ev)

	var cut int
	if _, err := p.begin(1, func() { cut++ }); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := p.Mark(Mark{Replica: 1, Kind: MarkStart}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if err := p.Mark(Mark{Replica: 1, Kind: MarkDone}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if got := p.inflight(); got != 1 {
		t.Fatalf("in-flight = %d, want the proxied request still counted", got)
	}

	if err := p.Mark(Mark{Replica: 1, Kind: MarkState, State: TargetDraining, At: clock.Now()}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	clock.advance(time.Minute)
	if got := p.sweep(clock.Now()); len(got) != 1 || !got[0].Deadline {
		t.Fatalf("sweep = %+v", got)
	}
	if cut != 1 {
		t.Errorf("cut %d request(s), want the one the proxy registered", cut)
	}
}

func TestCountsCarryTheLiveInflight(t *testing.T) {
	clock, ev := newClock(), &events{}
	p := newTestPool(route("feat-x.shop.test",
		Target{Replica: 2, Port: 20002, State: TargetActive},
		Target{Replica: 1, Port: 20001, State: TargetActive},
	), clock, ev)
	if _, err := p.begin(1, nil); err != nil {
		t.Fatalf("begin: %v", err)
	}
	counts := p.Counts()
	if len(counts) != 2 || counts[0].Replica != 1 || counts[1].Replica != 2 {
		t.Fatalf("Counts() = %+v, want replica order", counts)
	}
	if counts[0].Inflight != 1 || counts[1].Inflight != 0 {
		t.Errorf("Counts() in-flight = %d, %d, want 1, 0", counts[0].Inflight, counts[1].Inflight)
	}
}

func TestTheDeadlineReportsWhatItCut(t *testing.T) {
	clock, ev := newClock(), &events{}
	p := newTestPool(route("feat-x.shop.test",
		Target{Replica: 1, Port: 20001, State: TargetActive},
		Target{Replica: 2, Port: 20002, State: TargetActive},
	), clock, ev)

	var cancelled int
	if _, err := p.begin(1, func() { cancelled++ }); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := p.begin(1, func() { cancelled++ }); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := p.Mark(Mark{Replica: 1, Kind: MarkState, State: TargetDraining, At: clock.Now()}); err != nil {
		t.Fatalf("mark draining: %v", err)
	}

	now := clock.advance(11 * time.Second)
	events := p.sweep(now)
	if len(events) != 1 {
		t.Fatalf("%d events at the deadline, want one", len(events))
	}
	e := events[0]
	if !e.Deadline {
		t.Error("the event does not say the deadline passed")
	}
	if e.Target == nil || e.Target.Inflight != 2 {
		t.Errorf("the event reports %+v, want the two requests it cut", e.Target)
	}
	if cancelled != 2 {
		t.Errorf("%d requests were cancelled, want 2", cancelled)
	}
}
