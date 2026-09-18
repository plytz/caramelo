package env

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/state"
)

type ExposeResult struct {
	Env Env `json:"env"`

	Routes []edge.Route `json:"routes"`

	Changed []string `json:"changed,omitempty"`

	URL string `json:"url,omitempty"`
}

const edgeTableLock = "\x00edge/table"

func PublicURL(host string) string {
	if host == "" {
		return ""
	}
	return "https://" + host
}

func errNoEdge() error {
	return errors.New("this machine has no edge: nothing is served on ports 80 and 443 " +
		"(run `caramelo hub setup --edge`, or `caramelo edge enable`)")
}

func (m *Manager) Expose(ctx context.Context, req ExposeRequest, progress io.Writer) (*ExposeResult, error) {
	if err := ValidateName("app", req.App); err != nil {
		return nil, err
	}
	if err := ValidateName("env", req.Name); err != nil {
		return nil, err
	}
	if m.Edge == nil {
		return nil, errNoEdge()
	}
	defer m.lockEnv(req.App, req.Name)()

	rec, err := m.env(ctx, req.App, req.Name)
	if err != nil {
		return nil, err
	}

	progress = m.feed(ctx, rec, progress)
	cfg, err := decodeConfig(rec)
	if err != nil {
		return nil, err
	}

	if _, err := m.setViaLocked(ctx, rec, req.Via, progress); err != nil {
		return nil, err
	}
	service, err := exposableService(cfg, req.Service)
	if err != nil {
		return nil, fmt.Errorf("expose env %q: %w", req.Name, err)
	}

	hosts := []string{req.Host}
	if req.Host == "" {
		if hosts, err = m.hostsFor(rec, cfg, service); err != nil {
			return nil, err
		}
	}
	var changed []string
	for _, h := range hosts {
		host := edge.NormalizeHost(h)
		if err := config.ValidateHostname(host); err != nil {
			return nil, fmt.Errorf("expose env %q: %w", req.Name, err)
		}

		added, err := m.addRoute(ctx, rec, service, host, req.Host == "")
		if err != nil {
			return nil, err
		}
		if added {
			changed = append(changed, host)
			m.event(ctx, rec.ID, "expose", "ok", host+" → "+service)
			progressf(progress, "changed", "route", "%s → %s", host, service)
		} else {
			progressf(progress, "ok", "route", "%s already routes to %s", host, service)
		}

		if err := m.publishRunning(ctx, rec, cfg, service, host, progress); err != nil {
			return nil, err
		}
	}
	if err := m.pushRoutes(ctx, progress); err != nil {
		return nil, err
	}
	return m.exposeResult(ctx, rec, changed)
}

func (m *Manager) publishRunning(ctx context.Context, rec *state.EnvRecord, cfg *config.App,
	service, host string, progress io.Writer) error {
	row, err := m.Store.RouteByHost(ctx, host)
	if err != nil {
		return fmt.Errorf("read route %s: %w", host, err)
	}
	l, err := m.layoutOf(rec, cfg)
	if err != nil {
		return err
	}
	live, err := m.liveReplicas(ctx, rec, service)
	if err != nil {
		return err
	}
	running := runningReplicas(numbered(live))
	replicas := m.replicasOf(rec, service, l, running, ReplicaActive)
	if err := m.recordTargets(ctx, route{id: row.ID, host: host}, replicas); err != nil {
		return err
	}
	if len(replicas) > 0 {
		progressf(progress, "changed", "route", "%s → %s", host, plural(len(replicas), "replica"))
	}
	return nil
}

func (m *Manager) Unexpose(ctx context.Context, req UnexposeRequest, progress io.Writer) (*ExposeResult, error) {
	if err := ValidateName("app", req.App); err != nil {
		return nil, err
	}
	if err := ValidateName("env", req.Name); err != nil {
		return nil, err
	}
	if m.Edge == nil {
		return nil, errNoEdge()
	}
	defer m.lockEnv(req.App, req.Name)()

	rec, err := m.env(ctx, req.App, req.Name)
	if err != nil {
		return nil, err
	}
	progress = m.feed(ctx, rec, progress)
	host := edge.NormalizeHost(req.Host)

	if rec.Protected && !req.Force && host == "" {
		return nil, fmt.Errorf("env %q is protected: `caramelo env unexpose %s --force` takes every name "+
			"it serves off the edge. Removing one of them (--host NAME) needs no force",
			req.Name, req.Name)
	}
	rows, err := m.Store.RoutesOfEnv(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("read the routes of env %q: %w", req.Name, err)
	}
	var changed []string
	for _, r := range rows {
		if (req.Service != "" && r.Service != req.Service) || (host != "" && r.Host != host) {
			continue
		}
		if err := m.Store.DeleteRoute(ctx, r.ID); err != nil {
			return nil, fmt.Errorf("remove route %s: %w", r.Host, err)
		}
		changed = append(changed, r.Host)
		m.event(ctx, rec.ID, "unexpose", "ok", r.Host)
		progressf(progress, "changed", "route", "%s removed", r.Host)

		if r.Managed && m.fileAsksFor(rec, r.Service, r.Host) {
			progressf(progress, "warning", "route",
				"%s comes from %s, so the next `caramelo up %s` exposes it again: "+
					"take it out of the file (or out of envs.%s.hosts) to keep it down",
				r.Host, config.FileName, rec.Name, rec.Name)
		}
	}
	if len(changed) == 0 {
		progressf(progress, "ok", "route", "env %q serves no such name", req.Name)
	}

	if err := m.pushRoutes(ctx, progress); err != nil {
		return nil, err
	}
	return m.exposeResult(ctx, rec, changed)
}

func (m *Manager) exposeResult(ctx context.Context, rec *state.EnvRecord, changed []string) (*ExposeResult, error) {
	e, err := recordToEnv(rec)
	if err != nil {
		return nil, err
	}
	routes, err := m.envRoutes(ctx, rec)
	if err != nil {
		return nil, err
	}
	res := &ExposeResult{Env: *e, Routes: routes, Changed: changed}
	if len(routes) > 0 {
		res.URL = PublicURL(routes[0].Host)
	}
	return res, nil
}

func (m *Manager) addRoute(ctx context.Context, rec *state.EnvRecord, service, host string,
	managed bool) (bool, error) {
	_, err := m.Store.AddRoute(ctx, state.Route{
		EnvID:   rec.ID,
		Service: service,
		Host:    host,
		Kind:    state.RouteKindHTTPS,

		Managed:   managed,
		CreatedAt: m.now(),
	})
	switch {
	case err == nil:
		return true, nil
	case !errors.Is(err, state.ErrExists):
		return false, fmt.Errorf("record route %s: %w", host, err)
	}
	have, err := m.Store.RouteByHost(ctx, host)
	if err != nil {
		return false, fmt.Errorf("read route %s: %w", host, err)
	}
	if have.EnvID != rec.ID {
		owner, oerr := m.envOf(ctx, have.EnvID)
		if oerr != nil {
			return false, fmt.Errorf("the hostname %s is already served by another environment", host)
		}
		return false, fmt.Errorf("the hostname %s is already served by env %q of app %q: "+
			"unexpose it there, or give this one another name with --host", host, owner.Name, owner.App)
	}
	if have.Service != service {
		return false, fmt.Errorf("the hostname %s already routes to service %q of this environment: "+
			"unexpose it first, or give %q a name of its own with --host", host, have.Service, service)
	}
	return false, nil
}

func (m *Manager) envOf(ctx context.Context, id int64) (*state.EnvRecord, error) {
	recs, err := m.Store.Envs(ctx, "")
	if err != nil {
		return nil, err
	}
	for i := range recs {
		if recs[i].ID == id {
			return &recs[i], nil
		}
	}
	return nil, state.ErrNotFound
}

func exposableService(cfg *config.App, name string) (string, error) {
	if cfg == nil || len(cfg.Services) == 0 {
		return "", errors.New("this environment has no services to expose (has `caramelo up` run?)")
	}
	if name != "" {
		s, ok := cfg.Service(name)
		if !ok {
			return "", fmt.Errorf("no such service %q: this app has %s", name, quoteNames(configServiceNames(cfg)))
		}
		if !s.HasPort() {
			return "", fmt.Errorf("service %q publishes no port, so there is nothing to route to it", name)
		}
		return name, nil
	}
	for _, s := range cfg.Services {
		if s.HasPort() {
			return s.Name, nil
		}
	}
	return "", errors.New("no service of this environment publishes a port, so there is nothing to expose")
}

func configServiceNames(cfg *config.App) []string {
	out := make([]string, 0, len(cfg.Services))
	for _, s := range cfg.Services {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

func (m *Manager) hostFor(rec *state.EnvRecord, cfg *config.App, service string) (string, error) {
	domain, err := m.domainOf(cfg)
	if err != nil {
		return "", err
	}
	if domain == "" {
		return "", fmt.Errorf("app %q has no `domain:` in %s, so there is no hostname to derive: "+
			"set one, or name the hostname with --host", rec.App, config.FileName)
	}
	first := true
	if exposed := cfg.ExposedServices(); len(exposed) > 0 {
		first = exposed[0].Name == service
	} else if s, ok := cfg.FirstService(); ok {
		first = s.Name == service
	}
	return config.DerivedHost(domain, rec.Name, service, first), nil
}

func (m *Manager) hostsFor(rec *state.EnvRecord, cfg *config.App, service string) ([]string, error) {
	if hosts := cfg.EnvHosts(rec.Name); len(hosts) > 0 && firstExposed(cfg) == service {
		out := make([]string, 0, len(hosts))
		for _, h := range hosts {
			host := edge.NormalizeHost(h)
			if err := config.ValidateHostname(host); err != nil {
				return nil, fmt.Errorf("env %q of app %q: `envs.%s.hosts`: %w", rec.Name, rec.App, rec.Name, err)
			}
			out = append(out, host)
		}
		return out, nil
	}
	host, err := m.hostFor(rec, cfg, service)
	if err != nil {
		return nil, err
	}
	return []string{host}, nil
}

func firstExposed(cfg *config.App) string {
	if cfg == nil {
		return ""
	}
	if exposed := cfg.ExposedServices(); len(exposed) > 0 {
		return exposed[0].Name
	}
	if s, ok := cfg.FirstService(); ok {
		return s.Name
	}
	return ""
}

func (m *Manager) domainOf(cfg *config.App) (string, error) {
	if cfg == nil || cfg.Domain == "" {
		return "", nil
	}
	if cfg.Domain != config.DomainAuto {
		return cfg.Domain, nil
	}
	if !m.PublicIP.IsValid() {
		return "", fmt.Errorf("`domain: %s` needs this machine's public IPv4 address and it does not know one: "+
			"set `domain:` to a name you pointed at it, or expose the environment with --host", config.DomainAuto)
	}
	return config.AutoDomain(cfg.Name, m.PublicIP)
}

func (m *Manager) planRoutes(ctx context.Context, rec *state.EnvRecord, p *upPlan, wanted []svcPlan, progress io.Writer) error {
	p.routes = map[string]route{}
	exposed := p.cfg.ExposedServices()
	if m.Edge == nil {
		if len(exposed) > 0 {
			progressf(progress, "warning", "edge", "%s asks for a public name and this machine has no edge "+
				"(run `caramelo edge enable`)", config.FileName)
		}
		return nil
	}
	rows, err := m.Store.RoutesOfEnv(ctx, rec.ID)
	if err != nil {
		return fmt.Errorf("read the routes of env %q: %w", rec.Name, err)
	}
	have := make(map[string]bool, len(rows))
	for _, r := range rows {
		have[r.Host] = true
	}

	primary := map[string]string{}
	for _, s := range exposed {
		hosts, err := m.hostsFor(rec, p.cfg, s.Name)
		if err != nil {

			progressf(progress, "warning", "edge", "service %q is not exposed: %v", s.Name, err)
			continue
		}
		primary[s.Name] = hosts[0]
		for _, host := range hosts {
			if have[host] {
				continue
			}
			if _, err := m.addRoute(ctx, rec, s.Name, host, true); err != nil {
				return err
			}
			have[host] = true
			m.event(ctx, rec.ID, "expose", "ok", host+" → "+s.Name)
			progressf(progress, "changed", "route", "%s → %s", host, s.Name)
		}
	}

	if rows, err = m.Store.RoutesOfEnv(ctx, rec.ID); err != nil {
		return fmt.Errorf("read the routes of env %q: %w", rec.Name, err)
	}
	for _, r := range rows {

		one := route{id: r.ID, host: r.Host}
		cur, ok := p.routes[r.Service]
		switch {
		case !ok:
			p.routes[r.Service] = one
		case r.Host == primary[r.Service]:

			one.more = append(append([]route{}, cur.more...), route{id: cur.id, host: cur.host})
			p.routes[r.Service] = one
		default:
			cur.more = append(cur.more, one)
			p.routes[r.Service] = cur
		}
	}

	if len(wanted) > 0 {
		keep := make(map[string]route, len(wanted))
		for _, s := range wanted {
			if r, ok := p.routes[s.name]; ok {
				keep[s.name] = r
			}
		}
		p.routes = keep
	}
	return nil
}

func (m *Manager) Routes(ctx context.Context, app, name string) ([]edge.Route, error) {
	rec, err := m.env(ctx, app, name)
	if err != nil {
		return nil, err
	}
	return m.envRoutes(ctx, rec)
}

func (m *Manager) envRoutes(ctx context.Context, rec *state.EnvRecord) ([]edge.Route, error) {
	rows, err := m.Store.RoutesOfEnv(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("read the routes of env %q: %w", rec.Name, err)
	}
	cfg := m.recordedConfig(rec)
	out := make([]edge.Route, 0, len(rows))
	for _, r := range rows {
		route, err := m.routeOf(ctx, *rec, cfg, r)
		if err != nil {
			return nil, err
		}
		out = append(out, route)
	}
	return out, nil
}

func (m *Manager) routeOf(ctx context.Context, rec state.EnvRecord, cfg *config.App, r state.Route) (edge.Route, error) {
	targets, err := m.Store.Targets(ctx, r.ID)
	if err != nil {
		return edge.Route{}, fmt.Errorf("read the targets of route %s: %w", r.Host, err)
	}
	route := edge.Route{
		Host:      r.Host,
		Kind:      edge.Kind(r.Kind),
		App:       rec.App,
		Env:       rec.Name,
		Service:   r.Service,
		Drain:     drainOf(cfg, r.Service),
		CreatedAt: r.CreatedAt,
	}
	if route.Kind == "" {
		route.Kind = edge.KindHTTPS
	}
	for _, t := range targets {
		route.Targets = append(route.Targets, edge.Target{
			Replica: t.Replica,
			Port:    t.Port,
			State:   edge.TargetState(t.State),
			Since:   t.UpdatedAt,
		})
	}
	sort.Slice(route.Targets, func(i, j int) bool { return route.Targets[i].Replica < route.Targets[j].Replica })
	return route, nil
}

func (m *Manager) table(ctx context.Context) (edge.Table, error) {
	rows, err := m.Store.Routes(ctx)
	if err != nil {
		return edge.Table{}, fmt.Errorf("read the machine's routes: %w", err)
	}
	envs, err := m.Store.Envs(ctx, "")
	if err != nil {
		return edge.Table{}, fmt.Errorf("list envs: %w", err)
	}
	byID := make(map[int64]state.EnvRecord, len(envs))
	for _, e := range envs {
		byID[e.ID] = e
	}
	configs := make(map[int64]*config.App, len(envs))
	t := edge.Table{UpdatedAt: m.now()}
	for _, r := range rows {
		e, ok := byID[r.EnvID]
		if !ok {

			continue
		}
		cfg, seen := configs[r.EnvID]
		if !seen {
			cfg = m.recordedConfig(&e)
			configs[r.EnvID] = cfg
		}
		route, err := m.routeOf(ctx, e, cfg, r)
		if err != nil {
			return edge.Table{}, err
		}
		t.Routes = append(t.Routes, route)
	}

	t.Ingress = m.ingress(ctx)

	taken := make(map[string]string, len(t.Routes))
	for _, r := range t.Routes {
		taken[edge.NormalizeHost(r.Host)] = "this machine"
	}
	t.Routes = append(t.Routes, m.viaRoutes(ctx, taken)...)
	return t, nil
}

func (m *Manager) viaRoutes(ctx context.Context, taken map[string]string) []edge.Route {
	if m.fw().Fleet == nil || !m.fw().Role.IsHub() {
		return nil
	}
	if taken == nil {
		taken = map[string]string{}
	}
	entries, err := m.fw().Fleet.DirectoryEntries(ctx, "")
	if err != nil {
		m.logf("read the directory for the hub's routes: %v", err)
		return nil
	}
	machines, err := m.fw().Fleet.Machines(ctx)
	if err != nil {
		m.logf("read the fleet's machines for the hub's routes: %v", err)
		return nil
	}
	var out []edge.Route
	for _, e := range entries {
		if e.Machine == "" || e.Machine == m.fw().Machine || len(e.Hosts) == 0 {
			continue
		}
		mach, ok := fleet.Find(machines, e.Machine)
		if !ok {
			continue
		}
		port, ok := ViaRelayPortFor(mach)
		if !ok {
			m.logf("machine %s has no address, so %s/%s cannot be served through this one", e.Machine, e.App, e.Env)
			continue
		}
		for _, h := range e.Hosts {
			host := edge.NormalizeHost(h.Host)
			if host == "" {
				m.logf("machine %s announced %s/%s with a nameless host; ignored", e.Machine, e.App, e.Env)
				continue
			}
			if by, dup := taken[host]; dup {
				m.logf("machine %s announced %s for %s/%s, which is already served by %s; ignored",
					e.Machine, host, e.App, e.Env, by)
				continue
			}
			taken[host] = e.Machine
			out = append(out, ViaRoute(h.Host, e.Machine, e.App, e.Env, h.Service, []int{port}, h.Drain))
		}
	}
	return out
}

func (m *Manager) pushRoutes(ctx context.Context, progress io.Writer) error {
	if m.Edge == nil {
		return nil
	}
	if err := m.pushTable(ctx, progress); err != nil {
		return err
	}

	m.announceServed(ctx)
	return nil
}

func (m *Manager) pushTable(ctx context.Context, progress io.Writer) error {
	defer m.lockKey(edgeTableLock)()
	t, err := m.table(ctx)
	if err != nil {
		return err
	}
	if err := m.Edge.PushTable(ctx, t); err != nil {
		return fmt.Errorf("push the route table to the edge: %w", err)
	}

	if m.fw().OpenIngress != nil {
		if err := m.fw().OpenIngress(ctx, t.Ingress); err != nil {
			return fmt.Errorf("open the private ingress: %w", err)
		}
	}
	progressf(progress, "ok", "edge", "%s", plural(len(t.Routes), "route"))
	return nil
}

func (m *Manager) announceServed(ctx context.Context) {
	if m.fw().Announce == nil || m.fw().Role.IsHub() {
		return
	}
	if err := m.AnnounceAll(ctx); err != nil {
		m.logf("announce what this machine serves: %v", err)
	}
}

func (m *Manager) dropRoutes(ctx context.Context, rec *state.EnvRecord, progress io.Writer) error {
	if m.Edge == nil || rec == nil {
		return nil
	}
	rows, err := m.Store.RoutesOfEnv(ctx, rec.ID)
	if err != nil {
		return fmt.Errorf("read the routes of env %q: %w", rec.Name, err)
	}
	for _, r := range rows {
		if err := m.Store.DeleteRoute(ctx, r.ID); err != nil {
			return fmt.Errorf("remove route %s: %w", r.Host, err)
		}
		progressf(progress, "changed", "route", "%s removed", r.Host)
	}
	return m.pushRoutes(ctx, progress)
}

func (m *Manager) clearTargets(ctx context.Context, rec *state.EnvRecord, stopped []Service, progress io.Writer) error {
	if m.Edge == nil || len(stopped) == 0 {
		return nil
	}
	rows, err := m.Store.RoutesOfEnv(ctx, rec.ID)
	if err != nil {
		return fmt.Errorf("read the routes of env %q: %w", rec.Name, err)
	}
	touched := false
	for _, r := range rows {
		for _, svc := range stopped {
			if svc.Name != r.Service {
				continue
			}
			if err := m.Store.SetTargets(ctx, r.ID, nil); err != nil {
				return fmt.Errorf("empty the pool of %s: %w", r.Host, err)
			}
			touched = true
		}
	}
	if !touched {
		return nil
	}
	return m.pushRoutes(ctx, progress)
}

func (m *Manager) PushRoutes(ctx context.Context) error {
	return m.pushRoutes(ctx, nil)
}

var errDrained = errors.New("drained")

func (m *Manager) waitDrain(ctx context.Context, host string, replica int, drain time.Duration,
	since time.Time) (inflight int, deadline bool, err error) {
	if m.Edge == nil {
		return 0, false, nil
	}
	if since.IsZero() {
		since = m.now().Add(-time.Second)
	}
	c, cancel := context.WithTimeout(ctx, drain)
	defer cancel()

	serr := m.Edge.Subscribe(c, since, func(ev edge.Event) error {
		if ev.Kind != edge.EventDrained || edge.NormalizeHost(ev.Host) != edge.NormalizeHost(host) {
			return nil
		}
		if ev.Target == nil || ev.Target.Replica != replica {
			return nil
		}
		inflight, deadline = ev.Target.Inflight, ev.Deadline
		return errDrained
	})
	switch {
	case errors.Is(serr, errDrained):
		return inflight, deadline, nil
	case errors.Is(serr, context.DeadlineExceeded), errors.Is(c.Err(), context.DeadlineExceeded):

		return inflight, true, nil
	case serr != nil:
		return 0, false, fmt.Errorf("watch the edge while %s drains: %w", host, serr)
	}
	return inflight, deadline, nil
}

func (m *Manager) fileAsksFor(rec *state.EnvRecord, service, host string) bool {
	cfg, err := decodeConfig(rec)
	if err != nil {
		return false
	}
	hosts, err := m.hostsFor(rec, cfg, service)
	if err != nil {
		return false
	}
	for _, h := range hosts {
		if edge.NormalizeHost(h) == host {
			return true
		}
	}
	return false
}
