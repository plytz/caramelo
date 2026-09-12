package env

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/state"
)

const DefaultHealthInterval = 10 * time.Second

const DefaultHealthFailures = 3

type HealthLoop struct {
	Manager *Manager

	Interval time.Duration

	Failures int

	Log io.Writer

	mu sync.Mutex

	fails    map[string]int
	restarts map[string]int

	down map[string]bool
}

func NewHealthLoop(m *Manager) *HealthLoop {
	return &HealthLoop{Manager: m}
}

func (l *HealthLoop) interval() time.Duration {
	if l != nil && l.Interval > 0 {
		return l.Interval
	}
	return DefaultHealthInterval
}

func (l *HealthLoop) failures() int {
	if l != nil && l.Failures > 0 {
		return l.Failures
	}
	return DefaultHealthFailures
}

func (l *HealthLoop) Run(ctx context.Context) error {
	if l == nil || l.Manager == nil {
		return errors.New("supervise the machine's environments: no manager")
	}
	t := time.NewTicker(l.interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		if err := l.Once(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			l.logf("health loop: %v", err)
		}
	}
}

func (l *HealthLoop) Once(ctx context.Context) error {
	if l == nil || l.Manager == nil {
		return errors.New("probe the machine's environments: no manager")
	}
	m := l.Manager
	recs, err := m.Store.Envs(ctx, "")
	if err != nil {
		return fmt.Errorf("list envs: %w", err)
	}
	for i := range recs {
		rec := &recs[i]
		if rec.Status != state.EnvReady {

			continue
		}
		if m.envBusy(rec.App, rec.Name) {

			continue
		}
		if l.deploying(ctx, rec) {

			continue
		}
		if err := l.env(ctx, rec); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			l.logf("env %s/%s: %v", rec.App, rec.Name, err)
		}
	}
	return nil
}

func (l *HealthLoop) deploying(ctx context.Context, rec *state.EnvRecord) bool {
	if rec.DeployID == 0 {
		return false
	}
	row, err := l.Manager.Store.Deploy(ctx, rec.DeployID)
	if err != nil {
		return false
	}
	return !row.Done()
}

func (l *HealthLoop) env(ctx context.Context, rec *state.EnvRecord) error {
	m := l.Manager
	cfg, err := decodeConfig(rec)
	if err != nil {
		return err
	}
	cfg = cfg.ForEnv(rec.Name)
	rows, err := m.Store.Services(ctx, rec.ID)
	if err != nil {
		return fmt.Errorf("read the services of env %q: %w", rec.Name, err)
	}
	routes, err := m.Store.RoutesOfEnv(ctx, rec.ID)
	if err != nil {
		return fmt.Errorf("read the routes of env %q: %w", rec.Name, err)
	}
	layout, err := m.layoutOf(rec, cfg)
	if err != nil {
		return err
	}

	held, err := l.heldReplicas(ctx, routes)
	if err != nil {
		return err
	}
	pushed := false
	for _, row := range rows {
		if row.Status == string(ServiceStopped) {

			continue
		}
		check := serviceHealthCheck(cfg, row.Name)
		live, err := m.liveReplicas(ctx, rec, row.Name)
		if err != nil {
			return err
		}
		for _, one := range numbered(live) {
			if held[row.Name][one.index] {
				continue
			}
			port, perr := layout.replicaPort(row.Name, one.index)
			if perr != nil {
				port = 0
			}
			changed, err := l.replica(ctx, rec, row.Name, one, port, check, routes)
			if err != nil {
				return err
			}
			pushed = pushed || changed
		}
	}
	if pushed {
		if err := m.pushRoutes(ctx, nil); err != nil {
			return err
		}
	}
	return l.deps(ctx, rec, cfg)
}

func (l *HealthLoop) heldReplicas(ctx context.Context, routes []state.Route) (map[string]map[int]bool, error) {
	out := map[string]map[int]bool{}
	for _, one := range routes {
		targets, err := l.Manager.Store.Targets(ctx, one.ID)
		if err != nil {
			return nil, fmt.Errorf("read the targets of %s: %w", one.Host, err)
		}
		for _, t := range targets {
			if edge.TargetState(t.State) != edge.TargetHeld {
				continue
			}
			if out[one.Service] == nil {
				out[one.Service] = map[int]bool{}
			}
			out[one.Service][t.Replica] = true
		}
	}
	return out, nil
}

func (l *HealthLoop) replica(ctx context.Context, rec *state.EnvRecord, service string, one replica,
	port int, check *config.Health, routes []state.Route) (bool, error) {
	m := l.Manager
	key := healthKey(rec.ID, service, one.index)
	if one.container == "" {
		return false, nil
	}

	cur, err := m.Driver.Inspect(ctx, one.container)
	if err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return false, fmt.Errorf("inspect %s: %w", one.container, err)
	}
	if err == nil {
		if moved, before := l.restartMoved(key, cur.Restarts); moved {
			m.record(ctx, rec.ID, Event{
				App: rec.App, Env: rec.Name, Service: service, Replica: one.index,
				Action: HealthActionCrashloop, Status: "warning",
				Detail: fmt.Sprintf("%s has restarted %d times (%d since the last pass): it is not staying up",
					one.container, cur.Restarts, cur.Restarts-before),
				JSON: healthJSON(map[string]any{"restarts": cur.Restarts, "container": one.container}),
			})
		}
	}

	probe := &Service{Name: replicaName(service, one.index), Container: one.container, Port: port}
	if nothingToProbe(check, port) {
		return false, nil
	}
	perr := m.checkService(ctx, probe, check)
	if perr == nil {
		return l.healthy(ctx, rec, service, one, key, routes)
	}
	fails := l.fail(key)
	if fails < l.failures() {
		return false, nil
	}
	return l.unhealthy(ctx, rec, service, one, key, fails, perr, routes)
}

func (l *HealthLoop) healthy(ctx context.Context, rec *state.EnvRecord, service string, one replica,
	key string, routes []state.Route) (bool, error) {
	l.pass(key)
	if !l.wasDown(key) {
		return false, nil
	}
	l.up(key)
	l.Manager.record(ctx, rec.ID, Event{
		App: rec.App, Env: rec.Name, Service: service, Replica: one.index,
		Action: HealthActionHealth, Status: "ok",
		Detail: fmt.Sprintf("%s answered again and is back in the pool", one.container),
	})
	return l.setTargetState(ctx, rec, service, one.index, edge.TargetActive, routes)
}

func (l *HealthLoop) unhealthy(ctx context.Context, rec *state.EnvRecord, service string, one replica,
	key string, fails int, cause error, routes []state.Route) (bool, error) {
	m := l.Manager
	first := !l.wasDown(key)
	l.setDown(key)
	changed := false
	if first {
		var err error
		if changed, err = l.setTargetState(ctx, rec, service, one.index, edge.TargetUnhealthy, routes); err != nil {
			return changed, err
		}
		m.record(ctx, rec.ID, Event{
			App: rec.App, Env: rec.Name, Service: service, Replica: one.index,
			Action: HealthActionHealth, Status: "failed",
			Detail: fmt.Sprintf("%s failed %s in a row and is out of the pool: %v",
				one.container, plural(fails, "probe"), cause),
			JSON: healthJSON(map[string]any{"failures": fails, "container": one.container, "error": cause.Error()}),
		})
	}

	if err := m.Driver.Restart(ctx, one.container, m.stopTimeout()); err != nil {
		l.logf("restart %s: %v", one.container, err)
		return changed, nil
	}
	m.record(ctx, rec.ID, Event{
		App: rec.App, Env: rec.Name, Service: service, Replica: one.index,
		Action: HealthActionRestart, Status: "changed",
		Detail: one.container + " restarted after " + plural(fails, "failed probe"),
	})

	l.pass(key)
	l.setDown(key)
	return changed, nil
}

func (l *HealthLoop) deps(ctx context.Context, rec *state.EnvRecord, cfg *config.App) error {
	m := l.Manager
	for i, dep := range cfg.Deps {
		container := ContainerName(rec.App, rec.Name, dep.Name)
		key := healthKey(rec.ID, "dep:"+dep.Name, 0)
		cur, err := m.Driver.Inspect(ctx, container)
		switch {
		case errors.Is(err, runtime.ErrNotFound):
			continue
		case err != nil:
			return fmt.Errorf("inspect %s: %w", container, err)
		}
		if moved, before := l.restartMoved(key, cur.Restarts); moved {
			m.record(ctx, rec.ID, Event{
				App: rec.App, Env: rec.Name, Service: dep.Name,
				Action: HealthActionCrashloop, Status: "warning",
				Detail: fmt.Sprintf("%s has restarted %d times (%d since the last pass): it is not staying up",
					container, cur.Restarts, cur.Restarts-before),
				JSON: healthJSON(map[string]any{"restarts": cur.Restarts, "container": container}),
			})
		}
		ok, detail := m.readyOnce(ctx, dep, container, depPort(rec.PortBase, i))
		if ok {
			if l.wasDown(key) {
				l.up(key)
				m.record(ctx, rec.ID, Event{
					App: rec.App, Env: rec.Name, Service: dep.Name,
					Action: HealthActionHealth, Status: "ok", Detail: container + " answered again",
				})
			}
			l.pass(key)
			continue
		}
		fails := l.fail(key)
		if fails < l.failures() {
			continue
		}
		if !l.wasDown(key) {
			l.setDown(key)
			m.record(ctx, rec.ID, Event{
				App: rec.App, Env: rec.Name, Service: dep.Name,
				Action: HealthActionHealth, Status: "failed",
				Detail: fmt.Sprintf("%s failed %s in a row: %s", container, plural(fails, "readiness check"), detail),
				JSON:   healthJSON(map[string]any{"failures": fails, "container": container, "error": detail}),
			})
		}
		if err := m.Driver.Restart(ctx, container, m.stopTimeout()); err != nil {
			l.logf("restart %s: %v", container, err)
			continue
		}
		m.record(ctx, rec.ID, Event{
			App: rec.App, Env: rec.Name, Service: dep.Name,
			Action: HealthActionRestart, Status: "changed",
			Detail: container + " restarted after " + plural(fails, "failed readiness check"),
		})
		l.pass(key)
		l.setDown(key)
	}
	return nil
}

func (l *HealthLoop) setTargetState(ctx context.Context, rec *state.EnvRecord, service string, replica int,
	want edge.TargetState, routes []state.Route) (bool, error) {
	changed := false
	for _, one := range routes {
		if one.Service != service {
			continue
		}
		targets, err := l.Manager.Store.Targets(ctx, one.ID)
		if err != nil {
			return changed, fmt.Errorf("read the targets of %s: %w", one.Host, err)
		}
		for _, t := range targets {
			if t.Replica != replica || edge.TargetState(t.State) == want {
				continue
			}

			if edge.TargetState(t.State) == edge.TargetHeld {
				continue
			}
			t.State, t.UpdatedAt = string(want), l.Manager.now()
			if err := l.Manager.Store.PutTarget(ctx, t); err != nil {
				return changed, fmt.Errorf("record the target %s/%d: %w", one.Host, replica, err)
			}
			changed = true
		}
	}
	return changed, nil
}

func healthKey(envID int64, service string, replica int) string {
	return fmt.Sprintf("%d/%s/%d", envID, service, replica)
}

func (l *HealthLoop) fail(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fails == nil {
		l.fails = map[string]int{}
	}
	l.fails[key]++
	return l.fails[key]
}

func (l *HealthLoop) pass(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

func (l *HealthLoop) wasDown(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.down[key]
}

func (l *HealthLoop) setDown(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.down == nil {
		l.down = map[string]bool{}
	}
	l.down[key] = true
}

func (l *HealthLoop) up(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.down, key)
}

func (l *HealthLoop) restartMoved(key string, restarts int) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.restarts == nil {
		l.restarts = map[string]int{}
	}
	before, seen := l.restarts[key]
	l.restarts[key] = restarts
	if !seen {
		return false, restarts
	}
	return restarts > before, before
}

func (l *HealthLoop) logf(format string, args ...any) {
	if l == nil || l.Log == nil {
		return
	}

	_, _ = fmt.Fprintf(l.Log, format+"\n", args...)
}

func healthJSON(v map[string]any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}

func serviceHealthCheck(cfg *config.App, service string) *config.Health {
	if cfg == nil {
		return nil
	}
	s, ok := cfg.Service(service)
	if !ok {
		return nil
	}
	return s.Health
}

func nothingToProbe(check *config.Health, port int) bool {
	if check != nil && len(check.Command) > 0 {
		return false
	}
	return port == 0
}

const (
	HealthActionHealth = "health"

	HealthActionCrashloop = "crashloop"

	HealthActionRestart = "restart"
)
