package env

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/state"
)

func (m *Manager) Services(ctx context.Context, app, name string) ([]Service, error) {
	rec, err := m.env(ctx, app, name)
	if err != nil {
		return nil, err
	}
	rows, err := m.serviceRows(ctx, rec)
	if err != nil {
		return nil, err
	}
	cfg, err := decodeConfig(rec)
	if err != nil {
		return nil, err
	}
	l, err := m.layoutOf(rec, cfg)
	if err != nil {
		return nil, err
	}

	routes, err := m.routesByService(ctx, rec)
	if err != nil {
		return nil, err
	}
	out := make([]Service, 0, len(rows))
	for _, row := range rows {
		svc := serviceFromRow(row)
		r, exposed := routes[row.Name]
		if exposed {
			svc.Expose, svc.Host, svc.PublicURL = string(config.ExposeHTTPS), r.Host, PublicURL(r.Host)
		}
		targets := make(map[int]state.EdgeTarget)
		if exposed {
			if targets, err = m.targetRows(ctx, r); err != nil {
				return nil, err
			}
		}
		if svc.Replicas, err = m.replicaViews(ctx, rec, l, row, targets, exposed); err != nil {
			return nil, err
		}
		svc.Status = aggregateStatus(svc.Replicas, row)
		fillFromFirst(&svc)
		svc.URL = serviceURL(svc.Port, svc.Protocol)
		out = append(out, svc)
	}
	return out, nil
}

func aggregateStatus(replicas []Replica, row state.EnvService) ServiceStatus {
	if len(replicas) == 0 {
		return ServiceMissing
	}
	worst := ServiceRunning
	rank := map[ServiceStatus]int{ServiceRunning: 0, ServiceStarting: 1, ServiceExited: 2, ServiceFailed: 3, ServiceMissing: 4}
	for _, r := range replicas {
		if rank[r.Status] > rank[worst] {
			worst = r.Status
		}
	}
	if worst == ServiceRunning && (row.Status == state.ServiceStarting || row.Status == state.ServiceFailed) {
		return ServiceStatus(row.Status)
	}
	return worst
}

func (m *Manager) routesByService(ctx context.Context, rec *state.EnvRecord) (map[string]state.Route, error) {
	out := map[string]state.Route{}
	if m.Edge == nil {
		return out, nil
	}
	rows, err := m.Store.RoutesOfEnv(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("read the routes of env %q: %w", rec.Name, err)
	}
	for _, r := range rows {
		if _, ok := out[r.Service]; !ok {
			out[r.Service] = r
		}
	}
	return out, nil
}

func (m *Manager) targetRows(ctx context.Context, r state.Route) (map[int]state.EdgeTarget, error) {
	rows, err := m.Store.Targets(ctx, r.ID)
	if err != nil {
		return nil, fmt.Errorf("read the targets of route %s: %w", r.Host, err)
	}
	return targetsByReplica(rows), nil
}

func (m *Manager) URLs(ctx context.Context, app, name, target string) ([]URL, error) {
	rec, err := m.env(ctx, app, name)
	if err != nil {
		return nil, err
	}
	cfg, err := decodeConfig(rec)
	if err != nil {
		return nil, err
	}
	out, err := EndpointsAt(rec, cfg, m.frontPorts(ctx, rec, cfg))
	if err != nil {
		return nil, err
	}
	if target == "" {
		return out, nil
	}
	if one, ok := FindEndpoint(out, target); ok {
		return []URL{one}, nil
	}
	return nil, fmt.Errorf("no such service or dependency %q in env %q: it has %s",
		target, name, quoteNames(urlNames(out)))
}

func urlNames(urls []URL) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		out = append(out, u.Name)
	}
	sort.Strings(out)
	return out
}

func (m *Manager) ExportView(ctx context.Context, app, name string, format ExportFormat, view config.View,
	reveal bool) (string, error) {
	if view == config.ViewHost || view == "" {
		if !reveal {
			return m.Export(ctx, app, name, format)
		}
	}
	vars, err := m.VarsIn(ctx, app, name, view, reveal)
	if err != nil {
		return "", err
	}
	return render(vars, format)
}

func (m *Manager) VarsIn(ctx context.Context, app, name string, view config.View, reveal bool) (map[string]string, error) {
	rec, err := m.env(ctx, app, name)
	if err != nil {
		return nil, err
	}
	if (view == config.ViewHost || view == "") && !reveal {
		e, err := recordToEnv(rec)
		if err != nil {
			return nil, err
		}
		return e.Vars, nil
	}
	cfg, err := decodeConfig(rec)
	if err != nil {
		return nil, err
	}
	l, err := m.layoutOf(rec, cfg)
	if err != nil {
		return nil, err
	}
	views := l.views(rec, cfg.Deps)
	if hostExpanded(cfg, views) {

		fresh, err := m.readConfig(rec.Worktree, rec.App, nil)
		if err != nil {
			return nil, fmt.Errorf("env %q was created before %s existed and its stored variables are already "+
				"expanded for the machine; re-reading its checkout failed (run `caramelo up %s` to refresh them): %w",
				name, config.FileName, name, err)
		}
		cfg = fresh
		if l, err = m.layoutOf(rec, cfg); err != nil {
			return nil, err
		}
		views = l.views(rec, cfg.Deps)
	}

	secrets, err := m.secretsOf(ctx, rec.App, rec.Name)
	if err != nil {
		return nil, err
	}
	secrets.withDepDefaults(cfg.Deps, Mode(rec.Mode).IsRelease())
	lookups := secrets.redacted()
	if reveal {
		lookups = secrets.lookups()

		m.RecordReveal(ctx, rec.App, name, sortedSecretNames(secrets.values))
	}
	asked := view
	if asked == "" {
		asked = config.ViewHost
	}
	resolved, err := cfg.ResolveWith(asked, views, lookups)
	if err != nil {
		return nil, fmt.Errorf("expand the variables of env %q in the %s view: %w", name, asked, err)
	}
	return managedVars(rec, resolved.Env, ""), nil
}

func hostExpanded(cfg *config.App, views config.Views) bool {
	if cfg == nil || len(cfg.Env) == 0 || len(cfg.Deps) == 0 {
		return false
	}
	addrs := make([]string, 0, len(cfg.Deps))
	for _, d := range cfg.Deps {
		a, ok := views.Host.Deps[d.Name]
		if !ok || a.Port == 0 {
			continue
		}
		addrs = append(addrs, a.Host+":"+strconv.Itoa(a.Port))
	}
	found := false
	for _, v := range cfg.Env {
		if strings.Contains(v, "${") {
			return false
		}
		for _, a := range addrs {
			if strings.Contains(v, a) {
				found = true
			}
		}
	}
	return found
}
