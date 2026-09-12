package edge

import (
	"fmt"
	"sync"
	"time"
)

type RouterOptions struct {
	Now func() time.Time

	Drain time.Duration

	OnDraining func(t Target)
}

type Router struct {
	opts   RouterOptions
	now    func() time.Time
	events *broker

	mu     sync.RWMutex
	table  Table
	pools  map[string]*RoutePool
	pushed time.Time
}

func NewRouter(o RouterOptions) *Router {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Router{opts: o, now: now, events: newBroker(), pools: map[string]*RoutePool{}}
}

func (r *Router) Install(t Table) error {
	if err := t.Validate(); err != nil {
		return err
	}
	t = t.Clone()
	for i := range t.Routes {
		t.Routes[i].Host = NormalizeHost(t.Routes[i].Host)
	}

	r.mu.Lock()
	pools := make(map[string]*RoutePool, len(t.Routes))
	var events []Event
	for _, route := range t.Routes {
		if p, ok := r.pools[route.Host]; ok {
			events = append(events, p.install(route)...)
			pools[route.Host] = p
			continue
		}
		pools[route.Host] = NewPool(route, poolOptions{
			now:        r.now,
			drain:      r.opts.Drain,
			emit:       r.events.publish,
			onDraining: r.opts.OnDraining,
		})
	}
	r.pools = pools
	r.table = t
	r.pushed = r.now()
	updated := t.UpdatedAt
	r.mu.Unlock()

	for _, e := range events {
		r.events.publish(e)
	}
	r.events.publish(Event{Kind: EventTable, At: r.now(), Detail: tableDetail(len(t.Routes), updated)})
	return nil
}

func tableDetail(routes int, updated time.Time) string {
	word := "routes"
	if routes == 1 {
		word = "route"
	}
	if updated.IsZero() {
		return fmt.Sprintf("%d %s installed", routes, word)
	}
	return fmt.Sprintf("%d %s installed, built %s", routes, word, updated.UTC().Format(time.RFC3339))
}

func (r *Router) Lookup(host string) (Route, *RoutePool, bool) {
	host = NormalizeHost(host)
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.pools[host]
	if !ok {
		return Route{}, nil, false
	}
	route, _ := r.table.Route(host)
	return route, p, true
}

func (r *Router) Has(host string) bool {
	host = NormalizeHost(host)
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.pools[host]
	return ok
}

func (r *Router) Hosts() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.table.Hosts()
}

func (r *Router) Pushed() Table {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.table.Clone()
}

func (r *Router) Table() Table {
	r.mu.RLock()
	out := r.table.Clone()
	pools := make([]*RoutePool, len(out.Routes))
	for i, route := range out.Routes {
		pools[i] = r.pools[route.Host]
	}
	r.mu.RUnlock()
	for i := range out.Routes {
		if pools[i] != nil {
			out.Routes[i].Targets = pools[i].Counts()
		}
	}
	return out
}

func (r *Router) Ingress() *Ingress {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.table.Ingress == nil {
		return nil
	}
	ing := *r.table.Ingress
	ing.Trusted = append([]string(nil), r.table.Ingress.Trusted...)
	return &ing
}

func (r *Router) TableUpdatedAt() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.table.UpdatedAt.IsZero() {
		return r.table.UpdatedAt
	}
	return r.pushed
}

func (r *Router) Sweep(now time.Time) {
	r.mu.RLock()
	pools := make([]*RoutePool, 0, len(r.pools))
	for _, p := range r.pools {
		pools = append(pools, p)
	}
	r.mu.RUnlock()
	for _, p := range pools {
		for _, e := range p.sweep(now) {
			r.events.publish(e)
		}
	}
}

func (r *Router) Events() *broker { return r.events }
