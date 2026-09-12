package env

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/config"
	cprogress "github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/state"
)

const DefaultStopTimeout = 10 * time.Second

const probeTries = 3

func (m *Manager) stopTimeout() time.Duration {
	if m.StopTimeout > 0 {
		return m.StopTimeout
	}
	return DefaultStopTimeout
}

func drainOf(cfg *config.App, service string) time.Duration {
	if cfg == nil {
		return config.DefaultDrain
	}
	s, ok := cfg.Service(service)
	if !ok {
		return config.DefaultDrain
	}
	return s.DrainTimeout()
}

type route struct {
	id   int64
	host string

	more []route
}

func (r route) all() []route {
	out := make([]route, 0, 1+len(r.more))
	out = append(out, route{id: r.id, host: r.host})
	out = append(out, r.more...)
	return out
}

func (m *Manager) rollService(ctx context.Context, rec *state.EnvRecord, p *upPlan, s svcPlan,
	svc *Service, r route, deadline time.Time, progress io.Writer) (*Rollout, error) {
	roll := &Rollout{Service: s.name, Host: r.host, URL: PublicURL(r.host)}
	want := replicaCount(p.cfg, s.name)
	drain := drainOf(p.cfg, s.name)

	if err := p.layout.fits(s.name, 2*want); err != nil {
		return roll, err
	}
	live, err := m.liveReplicas(ctx, rec, s.name)
	if err != nil {
		return roll, err
	}
	old := runningReplicas(numbered(live))

	legacy, upgrading := legacyOf(live)

	same, why, err := m.unchanged(ctx, rec, svc, s.health, want)
	if err != nil {
		return roll, err
	}

	if same {
		if stale := staleTree(old, p.tree); stale != "" {
			same, why = false, stale
		}
	}
	if same && len(old) == want && !upgrading {

		svc.Change, svc.Status = ChangeUnchanged, ServiceRunning
		svc.Health = HealthOK
		svc.Replicas = m.replicasOf(rec, s.name, p.layout, old, ReplicaActive)
		fillFromFirst(svc)

		emit(progress, Event{Action: "service", Status: cprogress.StatusOK, Service: s.name,
			Detail: fmt.Sprintf("%s unchanged (%s)", s.name, plural(len(old), "replica"))})
		if err := m.publishTargets(ctx, r, svc.Replicas, progress); err != nil {
			return roll, err
		}
		return roll, nil
	}
	if !same && why != "" {
		svc.Detail = why

		emit(progress, Event{Action: "rollout", Status: cprogress.StatusOK, Service: s.name,
			Detail: s.name + ": " + why})
	}

	indexes := freeIndexes(old, want)
	fresh := make([]Replica, 0, want)
	for _, index := range indexes {
		rep, err := m.bringUpReplica(ctx, rec, p, s, svc, roll, index, deadline, progress)
		if err != nil {

			svc.Status, svc.Health = ServiceFailed, HealthFailed
			svc.Replicas = append(fresh, m.replicasOf(rec, s.name, p.layout, old, ReplicaActive)...)
			if rep != nil {
				svc.Replicas = append([]Replica{*rep}, svc.Replicas...)
			}
			roll.Replicas = svc.Replicas
			return roll, err
		}
		fresh = append(fresh, *rep)
	}
	flippedAt, err := m.flipPool(ctx, rec, p, s, r, roll, fresh, old, progress)
	if err != nil {
		roll.Replicas = append(fresh, m.replicasOf(rec, s.name, p.layout, old, ReplicaActive)...)
		return roll, err
	}
	for i, o := range old {

		remaining := m.replicasOf(rec, s.name, p.layout, old[i+1:], ReplicaDraining)
		pool := append(append([]Replica{}, fresh...), remaining...)

		if err := m.drainAndStop(ctx, rec, r, roll, o, pool, drain, flippedAt, progress); err != nil {
			roll.Replicas = append(fresh, m.replicasOf(rec, s.name, p.layout, old[i:], ReplicaDraining)...)
			return roll, err
		}
	}
	if upgrading {
		if err := m.stopReplica(ctx, legacy.container); err != nil {
			return roll, err
		}
		m.step(ctx, rec, roll, progress, RolloutStep{Step: StepStop, Status: StepOK, State: ReplicaStopped,
			Detail: legacy.container + " stopped: it is the container this service had before it had replicas"})
	}
	svc.Replicas = fresh
	svc.Status, svc.Health = ServiceRunning, HealthOK
	fillFromFirst(svc)
	roll.Replicas = fresh
	return roll, nil
}

func (m *Manager) bringUpReplica(ctx context.Context, rec *state.EnvRecord, p *upPlan, s svcPlan, svc *Service,
	roll *Rollout, index int, deadline time.Time, progress io.Writer) (*Replica, error) {
	rep, err := m.startReplica(ctx, rec, p, s, svc, index, progress)
	if err != nil {
		m.step(ctx, rec, roll, progress, RolloutStep{Replica: index, Step: StepStart, Status: StepFailed,
			State: ReplicaFailed, Detail: err.Error()})
		return rep, err
	}
	m.step(ctx, rec, roll, progress, RolloutStep{Replica: index, Step: StepStart, Status: StepOK,
		State: ReplicaStarting, Detail: fmt.Sprintf("%s on %s:%d", rep.Container, DepHost, rep.Port)})

	if err := m.healthReplica(ctx, rec, s, rep, deadline, progress); err != nil {
		rep.State, rep.Health, rep.Detail = ReplicaFailed, HealthFailed, err.Error()
		m.step(ctx, rec, roll, progress, RolloutStep{Replica: index, Step: StepHealth, Status: StepFailed,
			State: ReplicaFailed, Detail: err.Error()})
		return rep, err
	}
	rep.State, rep.Health = ReplicaHealthy, HealthOK
	m.step(ctx, rec, roll, progress, RolloutStep{Replica: index, Step: StepHealth, Status: StepOK, State: ReplicaHealthy})

	if err := m.probeReplica(ctx, s, rep); err != nil {
		rep.State, rep.Detail = ReplicaFailed, err.Error()
		m.step(ctx, rec, roll, progress, RolloutStep{Replica: index, Step: StepProbe, Status: StepFailed,
			State: ReplicaFailed, Detail: err.Error()})
		return rep, err
	}
	m.step(ctx, rec, roll, progress, RolloutStep{Replica: index, Step: StepProbe, Status: StepOK, State: ReplicaHealthy})
	return rep, nil
}

func (m *Manager) flipPool(ctx context.Context, rec *state.EnvRecord, p *upPlan, s svcPlan,
	r route, roll *Rollout, fresh []Replica, old []replica, progress io.Writer) (time.Time, error) {
	pool, draining, now := m.flipTargets(rec, p, s, fresh, old)
	if err := m.recordFlip(ctx, r, pool); err != nil {
		m.flipFailed(ctx, rec, roll, r, fresh, err, progress)
		return now, err
	}
	if err := m.pushRoutes(ctx, progress); err != nil {
		m.flipFailed(ctx, rec, roll, r, fresh, err, progress)
		return now, err
	}
	m.flipSteps(ctx, rec, roll, r, fresh, draining, now, progress)
	return now, nil
}

func (m *Manager) flipTargets(rec *state.EnvRecord, p *upPlan, s svcPlan, fresh []Replica,
	old []replica) (pool, draining []Replica, at time.Time) {
	now := m.now()
	pool = make([]Replica, 0, len(fresh)+len(old))
	for i := range fresh {
		fresh[i].State, fresh[i].Since = ReplicaActive, now
		pool = append(pool, fresh[i])
	}
	draining = m.replicasOf(rec, s.name, p.layout, old, ReplicaDraining)
	for i := range draining {
		draining[i].Since = now
	}
	pool = append(pool, draining...)
	return pool, draining, now
}

func (m *Manager) recordFlip(ctx context.Context, r route, pool []Replica) error {
	return m.recordTargets(ctx, r, pool)
}

func (m *Manager) flipSteps(ctx context.Context, rec *state.EnvRecord, roll *Rollout, r route,
	fresh, draining []Replica, now time.Time, progress io.Writer) {
	detail := flipDetail(draining)
	for _, rep := range fresh {

		m.step(ctx, rec, roll, progress, RolloutStep{Replica: rep.Index, Step: StepFlip, Status: StepOK,
			State: ReplicaActive, Host: r.host, Detail: detail, At: now})
	}
	if len(draining) == 0 {

		for _, rep := range fresh {
			m.step(ctx, rec, roll, progress, RolloutStep{Replica: rep.Index, Step: StepDrain, Status: StepSkipped,
				Host: r.host, State: ReplicaActive, Detail: "nothing was replaced"})
		}
	}
}

func (m *Manager) flipFailed(ctx context.Context, rec *state.EnvRecord, roll *Rollout, r route,
	fresh []Replica, cause error, progress io.Writer) {
	for _, rep := range fresh {
		m.step(ctx, rec, roll, progress, RolloutStep{Replica: rep.Index, Step: StepFlip, Status: StepFailed,
			State: ReplicaHealthy, Host: r.host, Detail: cause.Error()})
	}
}

func staleTree(pool []replica, tree string) string {
	if tree == "" {
		return ""
	}
	for _, r := range pool {
		if r.tree != tree {
			return fmt.Sprintf("the code changed (%s → %s)", shortTree(r.tree), tree)
		}
	}
	return ""
}

func shortTree(tree string) string {
	if tree == "" {
		return "unknown"
	}
	return tree
}

func (m *Manager) drainAndStop(ctx context.Context, rec *state.EnvRecord, r route, roll *Rollout,
	old replica, pool []Replica, drain time.Duration, since time.Time, progress io.Writer) error {
	started := m.now()
	inflight, byDeadline, err := m.waitDrain(ctx, r.host, old.index, drain, since)
	switch {
	case err != nil:

		m.step(ctx, rec, roll, progress, RolloutStep{Replica: old.index, Step: StepDrain, Status: StepSkipped,
			State: ReplicaDraining, Host: r.host, Detail: err.Error()})
	case byDeadline:
		m.step(ctx, rec, roll, progress, RolloutStep{Replica: old.index, Step: StepDrain, Status: StepOK,
			State: ReplicaDraining, Host: r.host, Inflight: inflight,
			Detail:   fmt.Sprintf("the %s drain deadline passed with %d still in flight", drain, inflight),
			Duration: m.now().Sub(started)})
	default:
		m.step(ctx, rec, roll, progress, RolloutStep{Replica: old.index, Step: StepDrain, Status: StepOK,
			State: ReplicaDraining, Host: r.host, Detail: "the last request finished",
			Duration: m.now().Sub(started)})
	}

	if err := m.stopReplica(ctx, old.container); err != nil {
		m.step(ctx, rec, roll, progress, RolloutStep{Replica: old.index, Step: StepStop, Status: StepFailed,
			State: ReplicaDraining, Detail: err.Error()})
		return err
	}
	m.step(ctx, rec, roll, progress, RolloutStep{Replica: old.index, Step: StepStop, Status: StepOK,
		State: ReplicaStopped, Detail: old.container + " stopped"})

	if err := m.publishTargets(ctx, r, pool, progress); err != nil {
		return err
	}
	return nil
}

func (m *Manager) stopReplica(ctx context.Context, container string) error {
	if err := m.Driver.Stop(ctx, container, m.stopTimeout()); err != nil {
		return fmt.Errorf("stop %s: %w", container, err)
	}
	if err := m.Driver.Remove(ctx, container, true); err != nil {
		return fmt.Errorf("remove %s: %w", container, err)
	}
	return nil
}

func (m *Manager) startReplica(ctx context.Context, rec *state.EnvRecord, p *upPlan, s svcPlan, svc *Service,
	index int, progress io.Writer) (*Replica, error) {
	port, err := p.layout.replicaPort(s.name, index)
	if err != nil {
		return nil, err
	}
	spec, err := m.serviceSpec(rec, p, s, svc.Image, index, port)
	if err != nil {
		return nil, err
	}
	rep := &Replica{
		Service:   s.name,
		Index:     index,
		Container: spec.Name,
		Port:      port,
		State:     ReplicaStarting,
		Status:    ServiceStarting,
		Health:    healthOf(s, port),
		Since:     m.now(),
	}

	if err := m.Driver.Remove(ctx, spec.Name, true); err != nil {
		return rep, fmt.Errorf("replace %s: %w", spec.Name, err)
	}
	secret, err := m.secretVarsOf(rec, p, s)
	if err != nil {
		return rep, err
	}
	id, err := m.runContainer(ctx, spec, secret)
	if err != nil {
		return rep, fmt.Errorf("start %s: %w", spec.Name, err)
	}
	rep.ID, rep.Status = id, ServiceRunning
	m.addReplicaResource(ctx, rec.ID, spec.Name, s.name, index, port)
	emit(progress, Event{Action: "replica", Status: cprogress.StatusChanged, Service: s.name, Replica: index,
		Detail: fmt.Sprintf("%s on %s:%d", spec.Name, DepHost, port)})
	return rep, nil
}

func (m *Manager) healthReplica(ctx context.Context, rec *state.EnvRecord, s svcPlan, rep *Replica,
	deadline time.Time, progress io.Writer) error {
	if rep.Health == HealthNone {
		return nil
	}
	probe := &Service{Name: replicaName(rep.Service, rep.Index), Container: rep.Container, Port: rep.Port}
	if err := m.waitService(ctx, probe, s.health, deadline); err != nil {
		if out, lerr := m.Driver.LogTail(context.WithoutCancel(ctx), rep.Container, serviceLogTail); lerr == nil && out != "" {
			progressf(progress, "warning", "logs", "last %d lines of %s:\n%s", serviceLogTail, rep.Container, out)
		}
		return fmt.Errorf("replica %d of service %q was not healthy: %w", rep.Index, rep.Service, err)
	}
	return nil
}

func (m *Manager) probeReplica(ctx context.Context, s svcPlan, rep *Replica) error {
	if rep.Port <= 0 {
		return nil
	}
	url := fmt.Sprintf("http://%s:%d/", DepHost, rep.Port)
	if s.health != nil && s.health.Path != "" {
		url = fmt.Sprintf("http://%s:%d%s", DepHost, rep.Port, s.health.Path)
	}
	var last error
	for try := 0; try < probeTries; try++ {
		if try > 0 {
			if err := sleep(ctx, m.readyInterval()); err != nil {
				return err
			}
		}
		_, err := m.httpStatus(ctx, url)
		if err == nil {
			return nil
		}
		last = err
	}
	return fmt.Errorf("the edge's probe of %s never answered: %w", url, last)
}

func (m *Manager) publishTargets(ctx context.Context, r route, replicas []Replica, progress io.Writer) error {
	if m.Edge == nil || r.id == 0 {
		return nil
	}
	if err := m.recordTargets(ctx, r, replicas); err != nil {
		return err
	}
	return m.pushRoutes(ctx, progress)
}

func (m *Manager) recordTargets(ctx context.Context, r route, replicas []Replica) error {
	if m.Edge == nil || r.id == 0 {
		return nil
	}
	for _, one := range r.all() {
		rows := make([]state.EdgeTarget, 0, len(replicas))
		for _, rep := range replicas {
			if rep.Port <= 0 {
				continue
			}
			rows = append(rows, state.EdgeTarget{
				RouteID:   one.id,
				Replica:   rep.Index,
				Port:      rep.Port,
				State:     string(rep.State.TargetState()),
				UpdatedAt: m.now(),
			})
		}
		if err := m.Store.SetTargets(ctx, one.id, rows); err != nil {
			return fmt.Errorf("record the targets of %s: %w", one.host, err)
		}
	}
	return nil
}

func (m *Manager) step(ctx context.Context, rec *state.EnvRecord, roll *Rollout, progress io.Writer, s RolloutStep) *RolloutStep {
	if s.At.IsZero() {
		s.At = m.now()
	}
	step := roll.Add(s)
	what := replicaName(roll.Service, step.Replica) + " " + string(step.Step)
	detail := step.Detail
	if detail == "" {
		detail = string(step.State)
	}

	emit(progress, Event{
		Action:  "rollout",
		Service: roll.Service,
		Replica: step.Replica,
		Step:    string(step.Step),
		Status:  rolloutStatus(step.Status),
		Detail:  what + ": " + detail,
		At:      step.At,
	})
	return step
}

func rolloutStatus(s StepStatus) string {
	switch s {
	case StepFailed:
		return cprogress.StatusFailed
	case StepSkipped:
		return cprogress.StatusSkipped
	default:
		return cprogress.StatusChanged
	}
}

func (m *Manager) addReplicaResource(ctx context.Context, envID int64, container, service string, index, port int) {

	_ = m.Store.AddResource(ctx, state.EnvResource{
		EnvID:     envID,
		Kind:      state.ResourceContainer,
		Name:      container,
		Service:   service,
		Replica:   index,
		Port:      port,
		CreatedAt: m.now(),
	})
}

func (m *Manager) replicasOf(rec *state.EnvRecord, service string, l portLayout, live []replica, state ReplicaState) []Replica {
	out := make([]Replica, 0, len(live))
	for _, r := range live {
		out = append(out, m.replicaOf(rec, service, l, r, state))
	}
	return out
}

func (m *Manager) replicaOf(rec *state.EnvRecord, service string, l portLayout, r replica, st ReplicaState) Replica {
	port, err := l.replicaPort(service, r.index)
	if err != nil {
		port = 0
	}
	return Replica{
		Service:   service,
		Index:     r.index,
		Container: r.container,
		ID:        r.id,
		Port:      port,
		State:     st,
		Status:    replicaStatus(r),
	}
}

func runningReplicas(live []replica) []replica {
	out := make([]replica, 0, len(live))
	for _, r := range live {
		if r.running {
			out = append(out, r)
		}
	}
	return out
}

func fillFromFirst(svc *Service) {
	if len(svc.Replicas) == 0 {
		return
	}
	first := svc.Replicas[0]
	svc.Container, svc.ID = first.Container, first.ID
	if first.Port > 0 {
		svc.Port = first.Port
	}
}

func replicaName(service string, index int) string {
	if index <= 0 {
		return service
	}
	return service + "/" + itoa(index)
}

func flipDetail(draining []Replica) string {
	if len(draining) == 0 {
		return "in the pool"
	}
	indexes := make([]string, 0, len(draining))
	for _, d := range draining {
		indexes = append(indexes, strconv.Itoa(d.Index))
	}
	return "in the pool, replica " + strings.Join(indexes, " and ") + " draining"
}

func errRolloutFailed(service string, roll *Rollout, cause error) error {
	if roll == nil || roll.Failed == nil {
		return cause
	}
	return fmt.Errorf("rollout of service %q stopped at %s of replica %d: %w",
		service, roll.Failed.Step, roll.Failed.Replica, cause)
}

func itoa(n int) string { return strconv.Itoa(n) }
