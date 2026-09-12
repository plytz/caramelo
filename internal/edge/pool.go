package edge

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

const DefaultDrain = 30 * time.Second

type poolOptions struct {
	now func() time.Time

	drain time.Duration

	emit func(Event)

	onDraining func(Target)
}

func (o poolOptions) clock() func() time.Time {
	if o.now != nil {
		return o.now
	}
	return time.Now
}

type targetState struct {
	t           Target
	failedUntil time.Time

	conns map[int64]func()

	reported bool

	incarnation int64

	counted counter
}

type RoutePool struct {
	host  string
	opts  poolOptions
	now   func() time.Time
	drain time.Duration

	mu      sync.Mutex
	targets []*targetState
	next    int
	seq     int64

	unrouted counter

	marks []countMark
}

var _ Pool = (*RoutePool)(nil)

func NewPool(r Route, o poolOptions) *RoutePool {
	p := &RoutePool{host: NormalizeHost(r.Host), opts: o, now: o.clock()}
	p.install(r)
	return p
}

func (p *RoutePool) install(r Route) []Event {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.host = NormalizeHost(r.Host)
	p.drain = r.Drain
	if p.drain <= 0 {
		p.drain = p.opts.drain
	}
	if p.drain <= 0 {
		p.drain = DefaultDrain
	}

	old := make(map[int]*targetState, len(p.targets))
	for _, ts := range p.targets {
		old[ts.t.Replica] = ts
	}

	now := p.now()
	var events []Event
	var draining []Target
	next := make([]*targetState, 0, len(r.Targets))
	for _, want := range r.Targets {
		ts, ok := old[want.Replica]
		if !ok || ts.t.Port != want.Port {
			ts = &targetState{conns: map[int64]func(){}, incarnation: nextIncarnation()}
			ts.t = want
			ts.t.Inflight = 0
			if ts.t.Since.IsZero() {
				ts.t.Since = now
			}
			next = append(next, ts)
			events = append(events, targetEvent(p.host, ts.t, now))
			if ts.t.State == TargetDraining {
				draining = append(draining, ts.t)
			}
			continue
		}
		delete(old, want.Replica)
		changed := ts.t.State != want.State
		ts.t.Detail = want.Detail
		if changed {
			ts.t.State = want.State
			ts.t.Since = now
			if want.State == TargetDraining {
				ts.reported = false
				draining = append(draining, ts.t)
			}
			events = append(events, targetEvent(p.host, live(ts), now))
		}
		next = append(next, ts)
	}
	p.targets = next
	sort.Slice(p.targets, func(i, j int) bool { return p.targets[i].t.Replica < p.targets[j].t.Replica })
	if p.next >= len(p.targets) {
		p.next = 0
	}

	for i := range p.targets {
		ts := p.targets[i]
		if ts.t.State == TargetDraining && len(ts.conns) == 0 && !ts.reported {
			ts.reported = true
			events = append(events, drainedEvent(p.host, live(ts), false, 0, now))
		}
	}

	p.snapshot(now)

	if p.opts.onDraining != nil {
		for _, t := range draining {
			p.opts.onDraining(t)
		}
	}
	return events
}

func live(ts *targetState) Target {
	t := ts.t
	t.Inflight = len(ts.conns)
	return t
}

func (p *RoutePool) Pick(skip ...int) (Target, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.targets)
	if n == 0 {
		return Target{}, false
	}
	now := p.now()
	for i := 0; i < n; i++ {
		idx := (p.next + i) % n
		ts := p.targets[idx]
		if !ts.t.State.Routable() || contains(skip, ts.t.Replica) || now.Before(ts.failedUntil) {
			continue
		}
		p.next = (idx + 1) % n
		return live(ts), true
	}
	return Target{}, false
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (p *RoutePool) Mark(m Mark) error {
	events, draining, err := p.mark(m)
	if draining != nil && p.opts.onDraining != nil {
		p.opts.onDraining(*draining)
	}
	p.emit(events)
	return err
}

func (p *RoutePool) mark(m Mark) ([]Event, *Target, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ts := p.find(m.Replica)
	if ts == nil {
		return nil, nil, fmt.Errorf("edge: %s: no replica %d behind this route", p.host, m.Replica)
	}
	at := m.At
	if at.IsZero() {
		at = p.now()
	}
	switch m.Kind {
	case MarkStart:
		p.seq++
		ts.conns[p.seq] = nil
		return nil, nil, nil
	case MarkDone:

		if id, ok := anyConn(ts, false); ok {
			return p.release(ts, id, at), nil, nil
		}
		if id, ok := anyConn(ts, true); ok {
			return p.release(ts, id, at), nil, nil
		}
		return nil, nil, nil
	case MarkFail:
		ts.failedUntil = at.Add(FailureWindow)
		if m.Detail != "" {
			ts.t.Detail = m.Detail
		}
		return nil, nil, nil
	case MarkState:
		if !m.State.Valid() {
			return nil, nil, fmt.Errorf("edge: %s: replica %d: unknown state %q", p.host, m.Replica, m.State)
		}
		if ts.t.State == m.State {
			return nil, nil, nil
		}
		ts.t.State = m.State
		ts.t.Since = at
		if m.Detail != "" {
			ts.t.Detail = m.Detail
		}
		events := []Event{targetEvent(p.host, live(ts), at)}
		var draining *Target
		if m.State == TargetDraining {
			ts.reported = false
			t := live(ts)
			draining = &t
			if len(ts.conns) == 0 {
				ts.reported = true
				events = append(events, drainedEvent(p.host, live(ts), false, 0, at))
			}
		}
		return events, draining, nil
	default:
		return nil, nil, fmt.Errorf("edge: %s: unknown mark %q", p.host, m.Kind)
	}
}

func (p *RoutePool) begin(replica int, cancel func()) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ts := p.find(replica)
	if ts == nil {
		return 0, fmt.Errorf("edge: %s: no replica %d behind this route", p.host, replica)
	}
	p.seq++
	id := p.seq
	ts.conns[id] = cancel
	return id, nil
}

func (p *RoutePool) end(replica int, id int64) {
	p.mu.Lock()
	ts := p.find(replica)
	var events []Event
	if ts != nil {
		events = p.release(ts, id, p.now())
	}
	p.mu.Unlock()
	p.emit(events)
}

func anyConn(ts *targetState, withCancel bool) (int64, bool) {
	for id, cancel := range ts.conns {
		if (cancel != nil) == withCancel {
			return id, true
		}
	}
	return 0, false
}

func (p *RoutePool) release(ts *targetState, id int64, at time.Time) []Event {
	if _, ok := ts.conns[id]; !ok {
		return nil
	}
	delete(ts.conns, id)
	if ts.t.State != TargetDraining || len(ts.conns) != 0 || ts.reported {
		return nil
	}
	ts.reported = true
	return []Event{drainedEvent(p.host, live(ts), false, 0, at)}
}

func (p *RoutePool) sweep(now time.Time) []Event {
	p.mu.Lock()
	var events []Event
	var cancels []func()
	for _, ts := range p.targets {
		if ts.t.State != TargetDraining || ts.reported {
			continue
		}
		if now.Sub(ts.t.Since) < p.drain {
			continue
		}
		cut := len(ts.conns)
		for id, cancel := range ts.conns {
			if cancel != nil {
				cancels = append(cancels, cancel)
			}
			delete(ts.conns, id)
		}
		ts.reported = true
		events = append(events, drainedEvent(p.host, live(ts), true, cut, now))
	}
	p.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return events
}

func (p *RoutePool) Counts() []Target {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Target, 0, len(p.targets))
	for _, ts := range p.targets {
		out = append(out, live(ts))
	}
	return out
}

func (p *RoutePool) inflight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, ts := range p.targets {
		n += len(ts.conns)
	}
	return n
}

func (p *RoutePool) find(replica int) *targetState {
	for _, ts := range p.targets {
		if ts.t.Replica == replica {
			return ts
		}
	}
	return nil
}

func (p *RoutePool) emit(events []Event) {
	if p.opts.emit == nil {
		return
	}
	for _, e := range events {
		p.opts.emit(e)
	}
}

func targetEvent(host string, t Target, at time.Time) Event {
	return Event{Kind: EventTarget, Host: host, Target: &t, At: at, Detail: t.Detail}
}

func drainedEvent(host string, t Target, deadline bool, cut int, at time.Time) Event {
	detail := fmt.Sprintf("replica %d drained", t.Replica)
	if deadline {

		t.Inflight = cut
		detail = fmt.Sprintf("replica %d cut at the drain deadline with %d request(s) still open", t.Replica, cut)
	}
	return Event{Kind: EventDrained, Host: host, Target: &t, Deadline: deadline, At: at, Detail: detail}
}
