package edge

import (
	"sort"
	"sync/atomic"
	"time"
)

const maxCountMarks = 64

var incarnations atomic.Int64

func nextIncarnation() int64 { return incarnations.Add(1) }

type counter struct {
	requests        int
	status5xx       int
	connectFailures int
}

func (c counter) sub(o counter) counter {
	return counter{
		requests:        atLeastZero(c.requests - o.requests),
		status5xx:       atLeastZero(c.status5xx - o.status5xx),
		connectFailures: atLeastZero(c.connectFailures - o.connectFailures),
	}
}

func atLeastZero(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

type countMark struct {
	at time.Time

	unrouted counter

	per map[int]markEntry
}

type markEntry struct {
	incarnation int64
	counted     counter
}

func (p *RoutePool) snapshot(at time.Time) {
	m := countMark{at: at, unrouted: p.unrouted, per: make(map[int]markEntry, len(p.targets))}
	for _, ts := range p.targets {
		m.per[ts.t.Replica] = markEntry{incarnation: ts.incarnation, counted: ts.counted}
	}
	p.marks = append(p.marks, m)
	if len(p.marks) > maxCountMarks {
		p.marks = append(p.marks[:0], p.marks[1:]...)
	}
}

func (p *RoutePool) markAt(since time.Time) *countMark {
	if len(p.marks) == 0 {
		return nil
	}
	at := -1
	for i := range p.marks {
		if p.marks[i].at.After(since) {
			break
		}
		at = i
	}
	if at < 0 {
		return &p.marks[0]
	}
	return &p.marks[at]
}

func (p *RoutePool) countRequest(replica, status int, connectFailed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := &p.unrouted
	if ts := p.find(replica); ts != nil {
		c = &ts.counted
	}
	c.requests++
	switch {
	case connectFailed:

		c.connectFailures++
	case status >= 500:
		c.status5xx++
	}
}

func (p *RoutePool) countUnrouted() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unrouted.requests++
	p.unrouted.status5xx++
}

func (p *RoutePool) counts(since time.Time) HostCounts {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.markAt(since)

	own := p.unrouted
	if m != nil {
		own = own.sub(m.unrouted)
	}
	h := HostCounts{
		Host:            p.host,
		Requests:        own.requests,
		Status5xx:       own.status5xx,
		ConnectFailures: own.connectFailures,
		Unrouted: UnroutedCounts{
			Requests:        own.requests,
			Status5xx:       own.status5xx,
			ConnectFailures: own.connectFailures,
		},
	}
	for _, ts := range p.targets {
		c := ts.counted
		if m != nil {
			if e, ok := m.per[ts.t.Replica]; ok && e.incarnation == ts.incarnation {
				c = c.sub(e.counted)
			}
		}
		h.Targets = append(h.Targets, TargetCounts{
			Replica:         ts.t.Replica,
			Requests:        c.requests,
			Status5xx:       c.status5xx,
			ConnectFailures: c.connectFailures,
		})
		h.Requests += c.requests
		h.Status5xx += c.status5xx
		h.ConnectFailures += c.connectFailures
	}
	return h
}

func (r *Router) Counts(since time.Time) []HostCounts {
	r.mu.RLock()
	pools := make([]*RoutePool, 0, len(r.pools))
	for _, p := range r.pools {
		pools = append(pools, p)
	}
	r.mu.RUnlock()

	out := make([]HostCounts, 0, len(pools))
	for _, p := range pools {
		out = append(out, p.counts(since))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}
