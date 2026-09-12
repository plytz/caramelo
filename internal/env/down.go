package env

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/plytz/caramelo/internal/state"
)

func (m *Manager) Down(ctx context.Context, req DownRequest, progress io.Writer) ([]Service, error) {
	if err := ValidateName("app", req.App); err != nil {
		return nil, err
	}
	if err := ValidateName("env", req.Name); err != nil {
		return nil, err
	}
	defer m.lockEnv(req.App, req.Name)()

	rec, err := m.env(ctx, req.App, req.Name)
	if err != nil {
		return nil, err
	}

	progress = m.feed(ctx, rec, progress)
	if rec.Protected && !req.Force {
		return nil, fmt.Errorf("env %q is protected: `caramelo down %s --force` stops it. "+
			"A deploy is how a protected environment changes; down takes it off the air",
			req.Name, req.Name)
	}
	rows, err := m.serviceRows(ctx, rec)
	if err != nil {
		return nil, err
	}
	wanted, err := pickRows(rows, req.Services)
	if err != nil {
		return nil, err
	}
	m.event(ctx, rec.ID, "down", "started", plural(len(wanted), "service"))

	out := make([]Service, 0, len(wanted))
	for _, row := range wanted {
		svc := serviceFromRow(row)
		svc.Status, svc.Health = ServiceStopped, ""

		live, err := m.liveReplicas(ctx, rec, row.Name)
		if err != nil {
			return nil, err
		}
		if len(live) == 0 {
			progressf(progress, "ok", "service", "%s is already down", svc.Name)
		}
		for _, r := range live {
			svc.Replicas = append(svc.Replicas, Replica{
				Service: row.Name, Index: r.index, Container: r.container, ID: r.id,
				State: ReplicaStopped, Status: ServiceStopped,
			})
			if svc.ID == "" {
				svc.ID = r.id
			}
			if err := m.Driver.Remove(ctx, r.container, true); err != nil {
				return nil, fmt.Errorf("remove %s: %w", r.container, err)
			}
			svc.Change = ChangeRemoved
			progressf(progress, "changed", "service", "%s removed", replicaName(row.Name, r.index))
		}
		row.Status = state.ServiceStopped
		row.UpdatedAt = m.now()
		if err := m.Store.PutService(ctx, row); err != nil {
			return nil, fmt.Errorf("record service %q: %w", svc.Name, err)
		}
		out = append(out, svc)
	}

	if err := m.clearTargets(ctx, rec, out, progress); err != nil {
		return nil, err
	}

	if cfg, err := decodeConfig(rec); err == nil {
		m.republish(ctx, rec, cfg, progress)
	}
	m.event(ctx, rec.ID, "down", "ok", plural(len(out), "service"))
	return out, nil
}

func (m *Manager) serviceRows(ctx context.Context, rec *state.EnvRecord) ([]state.EnvService, error) {
	rows, err := m.Store.Services(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("read the services of env %q: %w", rec.Name, err)
	}
	known := make(map[string]bool, len(rows))
	for _, r := range rows {
		known[r.Name] = true
	}
	cfg, err := decodeConfig(rec)
	if err != nil {
		return nil, err
	}
	for _, s := range cfg.Services {
		if known[s.Name] {
			continue
		}
		rows = append(rows, state.EnvService{
			EnvID:         rec.ID,
			Name:          s.Name,
			Image:         s.Image,
			Container:     ReplicaContainerName(rec.App, rec.Name, s.Name, 1),
			ContainerPort: s.Port,
			Protocol:      string(s.Protocol),
			Status:        state.ServiceStopped,
		})
	}
	return rows, nil
}

func pickRows(rows []state.EnvService, names []string) ([]state.EnvService, error) {
	if len(names) == 0 {
		return rows, nil
	}
	out := make([]state.EnvService, 0, len(names))
	for _, n := range names {
		found := false
		for _, r := range rows {
			if r.Name == n {
				out, found = append(out, r), true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("no such service %q: this env has %s", n, quoteNames(rowNames(rows)))
		}
	}
	return out, nil
}

func rowNames(rows []state.EnvService) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}

func serviceFromRow(r state.EnvService) Service {
	return Service{
		Name:          r.Name,
		Container:     r.Container,
		Image:         r.Image,
		Port:          r.Port,
		ContainerPort: r.ContainerPort,
		Protocol:      r.Protocol,
		URL:           serviceURL(r.Port, r.Protocol),
		Status:        ServiceStatus(r.Status),
		UpdatedAt:     r.UpdatedAt,
	}
}
