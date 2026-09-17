package env

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/edge/certs"
	"github.com/plytz/caramelo/internal/git"
	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/state"
)

type fakeStore struct {
	state.Store

	mu        sync.Mutex
	apps      map[string]state.App
	envs      []state.EnvRecord
	resources []state.EnvResource
	events    []state.EnvEvent
	services  []state.EnvService
	nextEnv   int64
	nextRes   int64
	nextSvc   int64

	peers []string

	routes    []state.Route
	targets   map[int64][]state.EdgeTarget
	nextRoute int64

	secrets []state.VaultEntry

	notifier progress.Notifier

	releases    []state.Release
	deploys     []state.Deploy
	nextRelease int64
	nextDeploy  int64

	createErr error
}

func newStore(apps ...state.App) *fakeStore {
	s := &fakeStore{apps: map[string]state.App{}, targets: map[int64][]state.EdgeTarget{}}
	for _, a := range apps {
		s.apps[a.Name] = a
	}
	return s
}

func (s *fakeStore) App(ctx context.Context, name string) (*state.App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[name]
	if !ok {
		return nil, state.ErrNotFound
	}
	return &a, nil
}

func (s *fakeStore) Envs(ctx context.Context, app string) ([]state.EnvRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []state.EnvRecord{}
	for _, e := range s.envs {
		if app == "" || e.App == app {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *fakeStore) Env(ctx context.Context, app, name string) (*state.EnvRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.envs {
		if s.envs[i].App == app && s.envs[i].Name == name {
			e := s.envs[i]
			return &e, nil
		}
	}
	return nil, state.ErrNotFound
}

func (s *fakeStore) CreateEnv(ctx context.Context, r state.EnvRecord) (*state.EnvRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.createErr; err != nil {
		s.createErr = nil
		return nil, err
	}
	for _, e := range s.envs {
		if (e.App == r.App && e.Name == r.Name) || e.PortBase == r.PortBase {
			return nil, state.ErrExists
		}

		if r.VPNIP != "" && e.VPNIP == r.VPNIP {
			return nil, state.ErrExists
		}
	}
	s.nextEnv++
	r.ID = s.nextEnv
	s.envs = append(s.envs, r)
	out := r
	return &out, nil
}

func (s *fakeStore) UpdateEnvStatus(ctx context.Context, id int64, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.envs {
		if s.envs[i].ID == id {
			s.envs[i].Status = status
			return nil
		}
	}
	return state.ErrNotFound
}

func (s *fakeStore) UpdateEnv(ctx context.Context, r state.EnvRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.envs {
		if s.envs[i].ID == r.ID {
			s.envs[i] = r
			return nil
		}
	}
	return state.ErrNotFound
}

func (s *fakeStore) RecordEnvPush(ctx context.Context, id int64, commit, sourceBranch, pushedBy string,
	at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.envs {
		if s.envs[i].ID == id {
			s.envs[i].Commit = commit
			s.envs[i].SourceBranch = sourceBranch
			s.envs[i].PushedBy = pushedBy
			s.envs[i].PushedAt = at
			return nil
		}
	}
	return state.ErrNotFound
}

func (s *fakeStore) DeleteEnv(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.envs[:0]
	for _, e := range s.envs {
		if e.ID != id {
			kept = append(kept, e)
		}
	}
	s.envs = kept
	return nil
}

func (s *fakeStore) AddResource(ctx context.Context, r state.EnvResource) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, have := range s.resources {
		if have.EnvID == r.EnvID && have.Kind == r.Kind && have.Name == r.Name &&
			have.Dep == r.Dep && have.Service == r.Service {
			return nil
		}
	}
	s.nextRes++
	r.ID = s.nextRes
	s.resources = append(s.resources, r)
	return nil
}

func (s *fakeStore) Resources(ctx context.Context, envID int64) ([]state.EnvResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []state.EnvResource{}
	for _, r := range s.resources {
		if r.EnvID == envID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeStore) DeleteResource(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.resources[:0]
	for _, r := range s.resources {
		if r.ID != id {
			kept = append(kept, r)
		}
	}
	s.resources = kept
	return nil
}

func (s *fakeStore) AddEvent(ctx context.Context, e state.EnvEvent) error {
	s.mu.Lock()
	e.ID = int64(len(s.events)) + 1
	s.events = append(s.events, e)
	notify := s.notifier
	s.mu.Unlock()

	if notify != nil {
		e.App, e.Env = s.namesOf(e.EnvID)
		notify.Notify(e.Progress())
	}
	return nil
}

func (s *fakeStore) SetNotifier(n progress.Notifier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifier = n
}

func (s *fakeStore) namesOf(envID int64) (app, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.envs {
		if rec.ID == envID {
			return rec.App, rec.Name
		}
	}
	return "", ""
}

func (s *fakeStore) QueryEvents(ctx context.Context, f state.EventFilter) ([]state.EnvEvent, error) {
	s.mu.Lock()
	rows := append([]state.EnvEvent(nil), s.events...)
	s.mu.Unlock()
	out := []state.EnvEvent{}
	for i := len(rows) - 1; i >= 0; i-- {
		e := rows[i]
		e.App, e.Env = s.namesOf(e.EnvID)
		switch {
		case f.EnvID != 0 && e.EnvID != f.EnvID:
			continue
		case f.EnvID == 0 && f.App != "" && e.App != f.App:
			continue
		}
		if !f.Since.IsZero() && e.At.Before(f.Since) {
			continue
		}
		out = append(out, e)
		if f.Limit > 0 && len(out) == f.Limit {
			break
		}
	}
	return out, nil
}

func (s *fakeStore) Events(ctx context.Context, envID int64, limit int) ([]state.EnvEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []state.EnvEvent{}
	for i := len(s.events) - 1; i >= 0; i-- {
		if s.events[i].EnvID != envID {
			continue
		}
		out = append(out, s.events[i])
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *fakeStore) Services(ctx context.Context, envID int64) ([]state.EnvService, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []state.EnvService{}
	for _, svc := range s.services {
		if svc.EnvID == envID {
			out = append(out, svc)
		}
	}
	return out, nil
}

func (s *fakeStore) PutService(ctx context.Context, svc state.EnvService) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.services {
		if s.services[i].EnvID == svc.EnvID && s.services[i].Name == svc.Name {
			svc.ID = s.services[i].ID
			s.services[i] = svc
			return nil
		}
	}
	s.nextSvc++
	svc.ID = s.nextSvc
	s.services = append(s.services, svc)
	return nil
}

func (s *fakeStore) DeleteService(ctx context.Context, envID int64, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.services[:0]
	for _, svc := range s.services {
		if svc.EnvID != envID || svc.Name != name {
			kept = append(kept, svc)
		}
	}
	s.services = kept
	return nil
}

func (s *fakeStore) service(envID int64, name string) (state.EnvService, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, svc := range s.services {
		if svc.EnvID == envID && svc.Name == name {
			return svc, true
		}
	}
	return state.EnvService{}, false
}

func (s *fakeStore) resourceNames(envID int64, kind string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{}
	for _, r := range s.resources {
		if r.EnvID == envID && r.Kind == kind {
			out = append(out, r.Name)
		}
	}
	sort.Strings(out)
	return out
}

func (s *fakeStore) Routes(ctx context.Context) ([]state.Route, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]state.Route{}, s.routes...), nil
}

func (s *fakeStore) RoutesOfEnv(ctx context.Context, envID int64) ([]state.Route, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []state.Route{}
	for _, r := range s.routes {
		if r.EnvID == envID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeStore) RouteByHost(ctx context.Context, host string) (*state.Route, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.routes {
		if s.routes[i].Host == host {
			r := s.routes[i]
			return &r, nil
		}
	}
	return nil, state.ErrNotFound
}

func (s *fakeStore) AddRoute(ctx context.Context, r state.Route) (*state.Route, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, have := range s.routes {
		if have.Host == r.Host {
			return nil, state.ErrExists
		}
	}
	s.nextRoute++
	r.ID = s.nextRoute
	s.routes = append(s.routes, r)
	out := r
	return &out, nil
}

func (s *fakeStore) DeleteRoute(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.routes[:0]
	for _, r := range s.routes {
		if r.ID != id {
			kept = append(kept, r)
		}
	}
	s.routes = kept
	delete(s.targets, id)
	return nil
}

func (s *fakeStore) Targets(ctx context.Context, routeID int64) ([]state.EdgeTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]state.EdgeTarget{}, s.targets[routeID]...), nil
}

func (s *fakeStore) setTargetStates(from, to string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id := range s.targets {
		for i := range s.targets[id] {
			if s.targets[id][i].State == from {
				s.targets[id][i].State = to
				n++
			}
		}
	}
	return n
}

func (s *fakeStore) PutTarget(ctx context.Context, t state.EdgeTarget) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, have := range s.targets[t.RouteID] {
		if have.Replica == t.Replica {
			s.targets[t.RouteID][i] = t
			return nil
		}
	}
	s.targets[t.RouteID] = append(s.targets[t.RouteID], t)
	return nil
}

func (s *fakeStore) SetTargets(ctx context.Context, routeID int64, targets []state.EdgeTarget) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.targets == nil {
		s.targets = map[int64][]state.EdgeTarget{}
	}
	s.targets[routeID] = append([]state.EdgeTarget{}, targets...)
	return nil
}

func (s *fakeStore) DeleteTarget(ctx context.Context, routeID int64, replica int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.targets[routeID][:0]
	for _, t := range s.targets[routeID] {
		if t.Replica != replica {
			kept = append(kept, t)
		}
	}
	s.targets[routeID] = kept
	return nil
}

func (s *fakeStore) hosts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{}
	for _, r := range s.routes {
		out = append(out, r.Host)
	}
	sort.Strings(out)
	return out
}

type fakeEdge struct {
	mu     sync.Mutex
	tables []edge.Table
	subs   int

	counts    *edge.Counts
	countsErr error

	drained  []edge.Event
	reported map[string]bool

	sinces []time.Time

	PushErr error

	OnPush func(t edge.Table)

	Drain func(ctx context.Context, fn func(edge.Event) error) error
}

func newEdge() *fakeEdge { return &fakeEdge{} }

func (e *fakeEdge) PushTable(ctx context.Context, t edge.Table) error {
	if e.OnPush != nil {
		e.OnPush(t.Clone())
	}
	if e.PushErr != nil {
		return e.PushErr
	}
	if err := t.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tables = append(e.tables, t.Clone())
	if e.reported == nil {
		e.reported = map[string]bool{}
	}
	now := time.Now()
	for _, r := range t.Routes {
		for _, tg := range r.Targets {
			key := edge.NormalizeHost(r.Host) + "/" + strconv.Itoa(tg.Replica)
			if tg.State != edge.TargetDraining {

				e.reported[key] = false
				continue
			}
			if e.reported[key] {
				continue
			}
			e.reported[key] = true
			target := tg
			target.Inflight = 0
			e.drained = append(e.drained, edge.Event{
				Kind: edge.EventDrained, Host: r.Host, Target: &target, At: now})
		}
	}
	return nil
}

func (e *fakeEdge) Status(ctx context.Context) (*edge.Status, error) {
	t := e.table()
	return &edge.Status{Running: true, Routes: t.Routes}, nil
}

func (e *fakeEdge) Subscribe(ctx context.Context, since time.Time, fn func(edge.Event) error) error {
	e.mu.Lock()
	e.subs++
	e.sinces = append(e.sinces, since)
	drain := e.Drain
	replay := make([]edge.Event, 0, len(e.drained))
	for _, ev := range e.drained {
		if since.IsZero() || !ev.At.Before(since) {
			replay = append(replay, ev)
		}
	}
	e.mu.Unlock()
	if drain != nil {
		return drain(ctx, fn)
	}
	for _, ev := range replay {
		if err := fn(ev); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func (e *fakeEdge) CA(ctx context.Context) (*certs.CA, error) { return nil, nil }

func (e *fakeEdge) Prune(_ context.Context, req certs.PruneRequest) (*certs.PruneResult, error) {
	return &certs.PruneResult{KeepFor: req.Keep(), DryRun: req.DryRun}, nil
}

func (e *fakeEdge) Counts(_ context.Context, since time.Time) (*edge.Counts, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.countsErr != nil {
		return nil, e.countsErr
	}
	if e.counts != nil {
		out := *e.counts
		if out.Since.IsZero() {

			out.Since = since
		}
		return &out, nil
	}
	out := &edge.Counts{Since: since}
	if len(e.tables) > 0 {
		for _, r := range e.tables[len(e.tables)-1].Routes {
			h := edge.HostCounts{Host: r.Host}
			for _, t := range r.Targets {
				h.Targets = append(h.Targets, edge.TargetCounts{Replica: t.Replica})
			}
			out.Hosts = append(out.Hosts, h)
		}
	}
	return out, nil
}

func (e *fakeEdge) Close() error { return nil }

func (e *fakeEdge) table() edge.Table {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.tables) == 0 {
		return edge.Table{}
	}
	return e.tables[len(e.tables)-1].Clone()
}

func (e *fakeEdge) pushes() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.tables)
}

func (e *fakeEdge) states(host string) map[int]edge.TargetState {
	out := map[int]edge.TargetState{}
	r, ok := e.table().Route(host)
	if !ok {
		return out
	}
	for _, t := range r.Targets {
		out[t.Replica] = t.State
	}
	return out
}

func (e *fakeEdge) subscriptions() []time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]time.Time(nil), e.sinces...)
}

func blockedDrain(ctx context.Context, fn func(edge.Event) error) error {
	<-ctx.Done()
	return ctx.Err()
}

type fakeDriver struct {
	mu         sync.Mutex
	containers map[string]runtime.ContainerState
	volumes    map[string]map[string]string
	pulled     []string
	specs      []runtime.ContainerSpec
	removed    []string
	stopped    []stopped
	restarted  []stopped
	logs       map[string]string

	networks map[string]map[string][]string
	images   map[string]bool
	builds   []runtime.BuildSpec
	attached []runtime.ContainerSpec
	logsErr  map[string]string
	logOpts  []string

	RunErr func(spec runtime.ContainerSpec) error

	ExecResult func(name string, argv []string) (runner.Result, error)

	OnInspect func(name string, c *runtime.ContainerState)

	ListErr error

	BuildErr error

	Attached func(spec runtime.ContainerSpec, streams runtime.Streams) (int, error)
}

type stopped struct {
	name    string
	timeout time.Duration
}

func newDriver() *fakeDriver {
	return &fakeDriver{
		containers: map[string]runtime.ContainerState{},
		volumes:    map[string]map[string]string{},
		logs:       map[string]string{},
		networks:   map[string]map[string][]string{},
		images:     map[string]bool{},
		logsErr:    map[string]string{},
	}
}

func (d *fakeDriver) Pull(ctx context.Context, image string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pulled = append(d.pulled, image)
	return nil
}

func (d *fakeDriver) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.volumes[name] = labels
	return nil
}

func (d *fakeDriver) RemoveVolume(ctx context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.volumes, name)
	d.removed = append(d.removed, "volume "+name)
	return nil
}

func (d *fakeDriver) Run(ctx context.Context, spec runtime.ContainerSpec) (string, error) {
	if d.RunErr != nil {
		if err := d.RunErr(spec); err != nil {

			d.mu.Lock()
			d.containers[spec.Name] = runtime.ContainerState{Name: spec.Name, ID: "id-" + spec.Name, Status: runtime.StatusCreated, Image: spec.Image, Labels: spec.Labels, Ports: spec.Publish}
			d.mu.Unlock()
			return "", err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.specs = append(d.specs, spec)
	d.containers[spec.Name] = runtime.ContainerState{Name: spec.Name, ID: "id-" + spec.Name, Status: runtime.StatusRunning, Image: spec.Image, Labels: spec.Labels, Ports: spec.Publish}
	return "id-" + spec.Name, nil
}

func (d *fakeDriver) Inspect(ctx context.Context, name string) (runtime.ContainerState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.containers[name]
	if !ok {
		return runtime.ContainerState{}, runtime.ErrNotFound
	}
	if d.OnInspect != nil {
		d.OnInspect(name, &c)
		d.containers[name] = c
	}
	return c, nil
}

func (d *fakeDriver) Exec(ctx context.Context, name string, argv []string) (runner.Result, error) {
	if d.ExecResult != nil {
		return d.ExecResult(name, argv)
	}
	return runner.Result{}, nil
}

func (d *fakeDriver) LogTail(ctx context.Context, name string, tail int) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out, ok := d.logs[name]
	if !ok {
		return "", runtime.ErrNotFound
	}
	return out, nil
}

func (d *fakeDriver) Stop(ctx context.Context, name string, timeout time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopped = append(d.stopped, stopped{name: name, timeout: timeout})
	c, ok := d.containers[name]
	if !ok {
		return nil
	}
	c.Status = runtime.StatusExited
	d.containers[name] = c
	return nil
}

func (d *fakeDriver) Restart(ctx context.Context, name string, timeout time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.restarted = append(d.restarted, stopped{name: name, timeout: timeout})
	c, ok := d.containers[name]
	if !ok {
		return runtime.ErrNotFound
	}
	c.Status = runtime.StatusRunning
	c.Restarts++
	d.containers[name] = c
	return nil
}

func (d *fakeDriver) Remove(ctx context.Context, name string, force bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.containers, name)
	d.removed = append(d.removed, "container "+name)
	return nil
}

func (d *fakeDriver) ListByLabel(ctx context.Context, labels map[string]string) ([]runtime.ContainerState, error) {
	if d.ListErr != nil {
		return nil, d.ListErr
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []runtime.ContainerState{}
	for _, c := range d.containers {
		if matches(c.Labels, labels) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (d *fakeDriver) ListVolumesByLabel(ctx context.Context, labels map[string]string) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []string{}
	for name, l := range d.volumes {
		if matches(l, labels) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (d *fakeDriver) Build(ctx context.Context, spec runtime.BuildSpec) (string, error) {
	if d.BuildErr != nil {
		return "", d.BuildErr
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.builds = append(d.builds, spec)
	d.images[spec.Tag] = true
	if spec.Progress != nil {
		fmt.Fprintf(spec.Progress, "building %s\n", spec.Tag)
	}
	return "sha256:" + spec.Tag, nil
}

func (d *fakeDriver) ImageExists(ctx context.Context, ref string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.images[ref], nil
}

func (d *fakeDriver) RemoveImage(ctx context.Context, ref string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.images, ref)
	return nil
}

func (d *fakeDriver) CreateNetwork(ctx context.Context, name string, labels map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.networks[name]; !ok {
		d.networks[name] = map[string][]string{}
	}
	return nil
}

func (d *fakeDriver) RemoveNetwork(ctx context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.networks, name)
	d.removed = append(d.removed, "network "+name)
	return nil
}

func (d *fakeDriver) Connect(ctx context.Context, network, container string, aliases []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.networks[network] == nil {
		return fmt.Errorf("no such network %q", network)
	}
	d.networks[network][container] = aliases
	return nil
}

func (d *fakeDriver) RunAttached(ctx context.Context, spec runtime.ContainerSpec, streams runtime.Streams) (int, error) {
	d.mu.Lock()
	d.attached = append(d.attached, spec)
	hook := d.Attached
	d.mu.Unlock()
	if hook != nil {
		return hook(spec, streams)
	}
	return 0, nil
}

func (d *fakeDriver) Logs(ctx context.Context, name string, opts runtime.LogOptions, stdout, stderr io.Writer) error {
	d.mu.Lock()
	d.logOpts = append(d.logOpts, name)
	out, ok := d.logs[name]
	errOut := d.logsErr[name]
	d.mu.Unlock()
	if !ok && errOut == "" {
		return runtime.ErrNotFound
	}
	if out != "" {
		if _, err := io.WriteString(stdout, out); err != nil {
			return err
		}
	}
	if errOut != "" {
		if _, err := io.WriteString(stderr, errOut); err != nil {
			return err
		}
	}
	if opts.Follow {
		<-ctx.Done()
	}
	return nil
}

func (d *fakeDriver) setStatus(name, status string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.containers[name]
	c.Status = status
	d.containers[name] = c
}

func (d *fakeDriver) hasContainer(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.containers[name]
	return ok
}

func (d *fakeDriver) imageOf(name string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := ""
	for _, spec := range d.specs {
		if spec.Name == name {
			out = spec.Image
		}
	}
	return out
}

func (d *fakeDriver) hasNetwork(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.networks[name]
	return ok
}

func (d *fakeDriver) networkOf(network, container string) ([]string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	aliases, ok := d.networks[network][container]
	return aliases, ok
}

func (d *fakeDriver) containerNames() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []string{}
	for n := range d.containers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (d *fakeDriver) volumeNames() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []string{}
	for n := range d.volumes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (d *fakeDriver) spec(name string) (runtime.ContainerSpec, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.specs {
		if s.Name == name {
			return s, true
		}
	}
	return runtime.ContainerSpec{}, false
}

func matches(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

type fakeRepo struct {
	mu        sync.Mutex
	branches  map[string]string
	head      string
	worktrees map[string]string

	commits map[string]bool
	calls   []string

	WorktreeAddErr error
}

func newRepo(head string, branches map[string]string) *fakeRepo {
	commits := make(map[string]bool, len(branches))
	for _, c := range branches {
		commits[c] = true
	}
	return &fakeRepo{branches: branches, head: head, worktrees: map[string]string{}, commits: commits}
}

func (r *fakeRepo) record(format string, args ...any) {
	r.calls = append(r.calls, fmt.Sprintf(format, args...))
}

func (r *fakeRepo) InitBare(ctx context.Context, path string) error { return nil }

func (r *fakeRepo) Exists(ctx context.Context, path string) (bool, error) { return true, nil }

func (r *fakeRepo) BranchExists(ctx context.Context, repo, branch string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.branches[branch]
	return ok, nil
}

func (r *fakeRepo) CreateBranch(ctx context.Context, repo, branch, from string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.branches[branch]; ok {
		return fmt.Errorf("branch %q exists", branch)
	}
	commit, ok := r.branches[from]
	if !ok {

		if !r.commits[from] {
			return fmt.Errorf("no such ref %q", from)
		}
		commit = from
	}
	r.branches[branch] = commit
	r.commits[commit] = true
	r.record("create-branch %s %s", branch, from)
	return nil
}

func (r *fakeRepo) DeleteBranch(ctx context.Context, repo, branch string, force bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.branches, branch)
	r.record("delete-branch %s", branch)
	return nil
}

func (r *fakeRepo) WorktreeAdd(ctx context.Context, repo, path, branch string) error {
	if r.WorktreeAddErr != nil {
		return r.WorktreeAddErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.MkdirAll(path, 0o750); err != nil {
		return err
	}
	r.worktrees[path] = branch
	r.record("worktree-add %s %s", path, branch)
	return nil
}

func (r *fakeRepo) WorktreeRemove(ctx context.Context, repo, path string, force bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.worktrees, path)
	r.record("worktree-remove %s", path)
	return nil
}

func (r *fakeRepo) WorktreePrune(ctx context.Context, repo string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("worktree-prune")
	return nil
}

func (r *fakeRepo) RevParse(ctx context.Context, repo, ref string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if commit, ok := r.branches[ref]; ok {
		return commit, nil
	}

	if r.commits[ref] {
		return ref, nil
	}
	return "", git.ErrNotFound
}

func (r *fakeRepo) SymbolicRefHEAD(ctx context.Context, repo string) (string, error) {
	if r.head == "" {
		return "", git.ErrNotFound
	}
	return "refs/heads/" + r.head, nil
}

func (r *fakeRepo) SetHEAD(ctx context.Context, repo, branch string) error {
	r.head = branch
	return nil
}

func (r *fakeRepo) Branches(ctx context.Context, repo string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []string{}
	for b := range r.branches {
		out = append(out, b)
	}
	sort.Strings(out)
	return out, nil
}

func (r *fakeRepo) Refs(ctx context.Context, repo string) ([]git.Ref, error) {
	names, _ := r.Branches(ctx, repo)
	out := []git.Ref{}
	for _, n := range names {
		out = append(out, git.Ref{Name: "refs/heads/" + n, Commit: r.branches[n]})
	}
	return out, nil
}

func sampleConfig() *config.App {
	return &config.App{
		Name: "shop",
		Deps: []config.Dep{
			{Name: "db", Image: "postgres:16-alpine", Port: 5432, Ready: []string{"pg_isready", "-U", "postgres"}, Env: map[string]string{"POSTGRES_PASSWORD": "caramelo", "POSTGRES_DB": "${app.name}_${env.name}"}, Data: "/var/lib/postgresql/data"},
			{Name: "cache", Image: "redis:7-alpine", Port: 6379, Ready: []string{"redis-cli", "ping"}, Data: "/data"},
		},
		Env: map[string]string{
			"DATABASE_URL": "postgres://postgres:caramelo@${deps.db.host}:${deps.db.port}/postgres",
			"REDIS_URL":    "redis://${deps.cache.host}:${deps.cache.port}",
			"SELF":         "${app.name}/${env.name} on ${port}",
		},
		Path: "/mnt/caramelo/apps/shop/envs/feat-x/src/caramelo.yaml",
	}
}

func (s *fakeStore) TakenVPNIPs(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{}
	for _, p := range s.peers {
		out = append(out, p)
	}
	for _, e := range s.envs {
		if e.VPNIP != "" {
			out = append(out, e.VPNIP)
		}
	}
	sort.Strings(out)
	return out, nil
}
