package sshapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/edge/certs"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/state"
)

type EdgeManager interface {
	Expose(ctx context.Context, req env.ExposeRequest, progress io.Writer) (*env.ExposeResult, error)
	Unexpose(ctx context.Context, req env.UnexposeRequest, progress io.Writer) (*env.ExposeResult, error)
}

var _ EdgeManager = (*env.Manager)(nil)

func (d *Daemon) SetEdgeManager(m EdgeManager) {
	d.edgeMu.Lock()
	defer d.edgeMu.Unlock()
	d.edgeManager = m
}

func (d *Daemon) SetEdgeClient(c edge.Client) {
	d.edgeMu.Lock()
	defer d.edgeMu.Unlock()
	d.edgeClient = c
}

func (d *Daemon) edgeRoutes() (EdgeManager, error) {
	d.edgeMu.Lock()
	defer d.edgeMu.Unlock()
	if d.edgeManager == nil {
		return nil, errors.New("routes are not available on this daemon")
	}
	return d.edgeManager, nil
}

func (d *Daemon) edgeControl() (edge.Client, error) {
	d.edgeMu.Lock()
	defer d.edgeMu.Unlock()
	if d.edgeClient != nil {
		return d.edgeClient, nil
	}
	if !d.Config.Edge {
		return nil, errors.New("this machine has no edge: run 'caramelo edge enable' on it " +
			"(or 'caramelo server setup --edge')")
	}
	d.edgeClient = edge.NewClient(d.Config.EdgeSocketPath())
	return d.edgeClient, nil
}

func (d *Daemon) Expose(ctx context.Context, req env.ExposeRequest) (*api.ExposeResult, error) {
	if err := d.forwardEnv(ctx, req.App, req.Name); err != nil {
		return nil, err
	}
	m, err := d.edgeRoutes()
	if err != nil {
		return nil, err
	}
	res, err := m.Expose(d.withIdentity(ctx), req, io.Discard)
	if err != nil {
		return nil, err
	}
	return exposeResult(res), nil
}

func (d *Daemon) Unexpose(ctx context.Context, req env.UnexposeRequest) (*api.ExposeResult, error) {
	if err := d.forwardEnv(ctx, req.App, req.Name); err != nil {
		return nil, err
	}
	m, err := d.edgeRoutes()
	if err != nil {
		return nil, err
	}
	res, err := m.Unexpose(d.withIdentity(ctx), req, io.Discard)
	if err != nil {
		return nil, err
	}
	return exposeResult(res), nil
}

func exposeResult(r *env.ExposeResult) *api.ExposeResult {
	if r == nil {
		return &api.ExposeResult{}
	}
	res := &api.ExposeResult{Env: r.Env, Routes: r.Routes, Changed: r.Changed, URL: r.URL}
	if res.URL == "" && len(res.Routes) > 0 {
		res.URL = publicURL(res.Routes[0].Host)
	}
	return res
}

func publicURL(host string) string {
	if host == "" {
		return ""
	}
	return "https://" + host
}

func (d *Daemon) EdgeStatus(ctx context.Context) (*edge.Status, error) {
	client, err := d.edgeControl()
	if err != nil {
		return d.edgeStatusOffline(ctx, err), nil
	}
	st, err := client.Status(ctx)
	if err != nil {
		return d.edgeStatusOffline(ctx, err), nil
	}
	if st == nil {
		return d.edgeStatusOffline(ctx, errors.New("the edge answered nothing")), nil
	}

	if st.TLS == "" {
		st.TLS = certs.Mode(d.Config.TLS)
	}
	if st.ACMEDirectory == "" && st.TLS == certs.ModeACME {
		st.ACMEDirectory = d.Config.ACMEDirectory()
	}
	st.Running = true
	return st, nil
}

func (d *Daemon) edgeStatusOffline(ctx context.Context, cause error) *edge.Status {
	st := &edge.Status{
		Running: false,
		Error:   cause.Error(),
		HTTP3:   d.Config.HTTP3,
		TLS:     certs.Mode(d.Config.TLS),
	}
	if st.TLS == certs.ModeACME {
		st.ACMEDirectory = d.Config.ACMEDirectory()
	}
	if !d.Config.Edge {
		return st
	}

	routes, err := d.recordedRoutes(ctx)
	if err == nil {
		st.Routes = routes
	}
	return st
}

func (d *Daemon) envEdge(ctx context.Context, app, name string) ([]edge.Route, []certs.Certificate) {
	if d.Store == nil {
		return nil, nil
	}
	rec, err := d.Store.Env(ctx, app, name)
	if err != nil || rec == nil {
		return nil, nil
	}
	rows, err := d.Store.RoutesOfEnv(ctx, rec.ID)
	if err != nil || len(rows) == 0 {
		return nil, nil
	}
	routes := make([]edge.Route, 0, len(rows))
	hosts := make(map[string]bool, len(rows))
	for _, row := range rows {
		r := edge.Route{
			Host: row.Host, Kind: edge.Kind(row.Kind), App: rec.App, Env: rec.Name,
			Service: row.Service, CreatedAt: row.CreatedAt,
		}
		if targets, err := d.Store.Targets(ctx, row.ID); err == nil {
			for _, t := range targets {
				r.Targets = append(r.Targets, edge.Target{
					Replica: t.Replica, Port: t.Port,
					State: edge.TargetState(t.State), Since: t.UpdatedAt,
				})
			}
		}
		routes = append(routes, r)
		hosts[row.Host] = true
	}

	st, err := d.EdgeStatus(ctx)
	if err != nil || st == nil || !st.Running {
		return routes, nil
	}
	for i, r := range routes {
		if live, ok := (edge.Table{Routes: st.Routes}).Route(r.Host); ok {
			live.App, live.Env = rec.App, rec.Name
			if live.CreatedAt.IsZero() {
				live.CreatedAt = r.CreatedAt
			}
			routes[i] = live
		}
	}
	var held []certs.Certificate
	for _, c := range st.Certificates {
		if hosts[edge.NormalizeHost(c.Host)] {
			held = append(held, c)
		}
	}
	return routes, held
}

var edgeKeeperRetry = 5 * time.Second

func (d *Daemon) keepEdgeTable(ctx context.Context, logf func(string, ...any)) {
	pushed := false
	for ctx.Err() == nil {
		client, err := d.edgeControl()
		if err != nil {

			return
		}
		if err := d.pushRecordedTable(ctx); err != nil {
			if pushed {
				logf("edge: the route table could not be pushed: %v", err)
			}
			pushed = false
		} else if !pushed {
			logf("edge: the route table is what the database says")
			pushed = true
		}

		_ = client.Subscribe(ctx, time.Time{}, func(edge.Event) error { return nil })
		select {
		case <-ctx.Done():
			return
		case <-time.After(edgeKeeperRetry):
		}
	}
}

func (d *Daemon) pushRecordedTable(ctx context.Context) error {
	if d.EnvManager == nil {
		return errors.New("this daemon has no environment lifecycle")
	}
	return d.EnvManager.PushRoutes(ctx)
}

func overlayInflight(services []env.Service, routes []edge.Route) {
	if len(services) == 0 || len(routes) == 0 {
		return
	}

	inflight := map[string]map[int]int{}
	for _, r := range routes {
		if r.Service == "" {
			continue
		}
		per, ok := inflight[r.Service]
		if !ok {
			per = map[int]int{}
			inflight[r.Service] = per
		}
		for _, t := range r.Targets {
			per[t.Replica] += t.Inflight
		}
	}
	for i := range services {
		per, ok := inflight[services[i].Name]
		if !ok {
			continue
		}
		for j := range services[i].Replicas {
			if n, ok := per[services[i].Replicas[j].Index]; ok {
				services[i].Replicas[j].Inflight = n
			}
		}
	}
}

func (d *Daemon) recordedRoutes(ctx context.Context) ([]edge.Route, error) {
	if d.Store == nil {
		return nil, errors.New("no state store")
	}
	rows, err := d.Store.Routes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list routes: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}

	envs, err := d.Store.Envs(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	names := make(map[int64]state.EnvRecord, len(envs))
	for _, e := range envs {
		names[e.ID] = e
	}
	out := make([]edge.Route, 0, len(rows))
	for _, row := range rows {
		r := edge.Route{
			Host:      row.Host,
			Kind:      edge.Kind(row.Kind),
			Service:   row.Service,
			CreatedAt: row.CreatedAt,
		}

		if e, ok := names[row.EnvID]; ok {
			r.Env, r.App = e.Name, e.App
		}
		targets, err := d.Store.Targets(ctx, row.ID)
		if err != nil {
			return nil, fmt.Errorf("list the targets of %s: %w", row.Host, err)
		}
		for _, t := range targets {
			r.Targets = append(r.Targets, edge.Target{
				Replica: t.Replica,
				Port:    t.Port,
				State:   edge.TargetState(t.State),
				Since:   t.UpdatedAt,
			})
		}
		out = append(out, r)
	}
	return out, nil
}

func (d *Daemon) EdgeCA(ctx context.Context) (*certs.CA, error) {
	mode, err := d.Config.TLSMode()
	if err != nil {
		return nil, err
	}
	if mode != certs.ModeInternal {
		return nil, fmt.Errorf("this machine issues certificates from %s, so there is nothing to "+
			"trust by hand; 'tls: internal' is what generates a CA of its own",
			d.Config.ACMEDirectory())
	}
	client, err := d.edgeControl()
	if err != nil {
		return nil, err
	}
	ca, err := client.CA(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the machine's internal CA: %w", err)
	}
	if ca == nil || ca.PEM == "" {
		return nil, errors.New("the edge has no internal CA yet: it is generated with the first " +
			"certificate, so expose something and try again")
	}
	return ca, nil
}

func (d *Daemon) EdgeCounts(ctx context.Context, since time.Time) (*edge.Counts, error) {
	client, err := d.edgeControl()
	if err != nil {
		return nil, err
	}
	counts, err := client.Counts(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("read the edge's counts: %w", err)
	}
	if counts == nil {
		return nil, errors.New("the edge answered no counts")
	}
	return counts, nil
}
