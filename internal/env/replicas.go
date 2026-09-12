package env

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/state"
)

const legacyReplica = 0

type replica struct {
	index int

	container string
	id        string

	running bool

	status string

	tree string
}

func replicaIndexOf(app, env, service, container string) (int, bool) {
	prefix := ServiceContainerName(app, env, service)
	switch {
	case container == prefix:
		return legacyReplica, true
	case !strings.HasPrefix(container, prefix+"-"):
		return 0, false
	}
	n, err := strconv.Atoi(container[len(prefix)+1:])
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

func (m *Manager) liveReplicas(ctx context.Context, rec *state.EnvRecord, service string) ([]replica, error) {
	found, err := m.Driver.ListByLabel(ctx, ServiceLabels(rec.App, rec.Name, service, ""))
	if err != nil {
		return nil, fmt.Errorf("list the containers of service %q of env %q: %w", service, rec.Name, err)
	}
	out := make([]replica, 0, len(found))
	for _, c := range found {
		idx, ok := replicaIndexOf(rec.App, rec.Name, service, c.Name)
		if !ok {
			continue
		}
		out = append(out, replica{
			index:     idx,
			container: c.Name,
			id:        c.ID,
			running:   c.Running(),
			status:    c.Status,
			tree:      c.Labels[LabelTree],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].index < out[j].index })
	return out, nil
}

func (m *Manager) inspectReplica(ctx context.Context, index int, container string) (replica, error) {
	r := replica{index: index, container: container}
	cur, err := m.Driver.Inspect(ctx, container)
	switch {
	case errors.Is(err, runtime.ErrNotFound):
		return r, nil
	case err != nil:
		return r, fmt.Errorf("inspect %s: %w", container, err)
	}
	r.id, r.running, r.status, r.tree = cur.ID, cur.Running(), cur.Status, cur.Labels[LabelTree]
	return r, nil
}

func numbered(replicas []replica) []replica {
	out := make([]replica, 0, len(replicas))
	for _, r := range replicas {
		if r.index >= 1 {
			out = append(out, r)
		}
	}
	return out
}

func legacyOf(replicas []replica) (replica, bool) {
	for _, r := range replicas {
		if r.index == legacyReplica {
			return r, true
		}
	}
	return replica{}, false
}

func freeIndexes(live []replica, n int) []int {
	taken := make(map[int]bool, len(live))
	for _, r := range live {
		taken[r.index] = true
	}
	out := make([]int, 0, n)
	for i := 1; len(out) < n; i++ {
		if !taken[i] {
			out = append(out, i)
		}
	}
	return out
}

func replicaCount(cfg *config.App, service string) int {
	if cfg == nil {
		return config.DefaultReplicas
	}
	s, ok := cfg.Service(service)
	if !ok {
		return config.DefaultReplicas
	}
	return s.Replicas()
}

func (m *Manager) frontPorts(ctx context.Context, rec *state.EnvRecord, cfg *config.App) map[string]int {
	if cfg == nil || len(cfg.Services) == 0 {
		return nil
	}
	l, err := m.layoutOf(rec, cfg)
	if err != nil {
		return nil
	}
	draining := m.drainingReplicas(ctx, rec)
	out := map[string]int{}
	for _, s := range cfg.Services {
		if _, ok := l.services[s.Name]; !ok {
			continue
		}
		live, err := m.liveReplicas(ctx, rec, s.Name)
		if err != nil {
			continue
		}
		running := runningReplicas(numbered(live))

		for _, r := range running {
			if draining[replicaKey{s.Name, r.index}] {
				continue
			}
			if port, err := l.replicaPort(s.Name, r.index); err == nil {
				out[s.Name] = port
			}
			break
		}
		if _, ok := out[s.Name]; ok {
			continue
		}

		for _, r := range running {
			if port, err := l.replicaPort(s.Name, r.index); err == nil {
				out[s.Name] = port
			}
			break
		}
	}
	return out
}

type replicaKey struct {
	service string
	index   int
}

func (m *Manager) drainingReplicas(ctx context.Context, rec *state.EnvRecord) map[replicaKey]bool {
	rows, err := m.Store.RoutesOfEnv(ctx, rec.ID)
	if err != nil || len(rows) == 0 {
		return nil
	}
	out := map[replicaKey]bool{}
	for _, r := range rows {
		targets, err := m.Store.Targets(ctx, r.ID)
		if err != nil {
			continue
		}
		for _, t := range targets {
			if edge.TargetState(t.State) == edge.TargetDraining {
				out[replicaKey{r.Service, t.Replica}] = true
			}
		}
	}
	return out
}

func (m *Manager) replicaViews(ctx context.Context, rec *state.EnvRecord, l portLayout, row state.EnvService,
	targets map[int]state.EdgeTarget, exposed bool) ([]Replica, error) {
	live, err := m.liveReplicas(ctx, rec, row.Name)
	if err != nil {
		return nil, err
	}
	want := row.Replicas
	if want <= 0 {
		want = config.DefaultReplicas
	}

	seen := make(map[int]bool, len(live))
	all := make([]replica, 0, len(live)+want)
	for _, r := range numbered(live) {
		seen[r.index] = true
		all = append(all, r)
	}
	for i := 1; len(all) < want; i++ {
		if seen[i] {
			continue
		}
		all = append(all, replica{index: i, container: ReplicaContainerName(rec.App, rec.Name, row.Name, i)})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].index < all[j].index })

	out := make([]Replica, 0, len(all))
	for _, r := range all {
		port, perr := l.replicaPort(row.Name, r.index)
		if perr != nil {
			port = 0
		}
		v := Replica{
			Service:   row.Name,
			Index:     r.index,
			Container: r.container,
			ID:        r.id,
			Port:      port,
			Status:    replicaStatus(r),
			State:     replicaState(r, row),
			Health:    HealthStatus(""),
		}
		if t, ok := targets[r.index]; ok && exposed {
			v.State = ReplicaState(t.State)
			v.Inflight = t.Inflight
			v.Since = t.UpdatedAt
		}

		if r.id != "" {
			if cur, ierr := m.Driver.Inspect(ctx, r.container); ierr == nil {
				v.Restarts = cur.Restarts
			}
		}
		out = append(out, v)
	}
	return out, nil
}

func replicaStatus(r replica) ServiceStatus {
	switch {
	case r.container == "" || r.id == "" && r.status == "":
		return ServiceMissing
	case r.running:
		return ServiceRunning
	default:
		return ServiceExited
	}
}

func replicaState(r replica, row state.EnvService) ReplicaState {
	switch {
	case replicaStatus(r) != ServiceRunning:
		return ReplicaStopped
	case row.Status == state.ServiceFailed:
		return ReplicaFailed
	case row.Status == state.ServiceStarting:
		return ReplicaStarting
	default:
		return ReplicaActive
	}
}

func targetsByReplica(rows []state.EdgeTarget) map[int]state.EdgeTarget {
	out := make(map[int]state.EdgeTarget, len(rows))
	for _, t := range rows {
		out[t.Replica] = t
	}
	return out
}
