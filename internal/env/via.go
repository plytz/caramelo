package env

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/state"
)

const ViaRelayPort = 18443

func (m *Manager) SetVia(ctx context.Context, app, name string, via config.Via, progress io.Writer) (config.Via, error) {
	rec, err := m.env(ctx, app, name)
	if err != nil {
		return "", err
	}
	return m.setViaLocked(ctx, rec, via, progress)
}

func (m *Manager) setViaLocked(ctx context.Context, rec *state.EnvRecord, via config.Via, progress io.Writer) (config.Via, error) {
	cfg, err := decodeConfig(rec)
	if err != nil {
		return "", err
	}
	current := m.ViaOf(ctx, rec, cfg)
	if via == "" {
		return current, nil
	}
	if _, err := config.ParseVia(string(via)); err != nil {
		return "", fmt.Errorf("expose env %q: %w", rec.Name, err)
	}
	if err := m.canServeVia(via); err != nil {
		return "", fmt.Errorf("expose env %q: %w", rec.Name, err)
	}
	if via == current {
		progressf(progress, "ok", "route", "already served via %s", m.viaName(via))
		return current, nil
	}
	row := m.fleetRow(ctx, rec.ID)
	row.Via = via
	if err := m.setFleetRow(ctx, rec.ID, row); err != nil {
		return "", err
	}
	m.event(ctx, rec.ID, "route", "changed", "served via "+m.viaName(via))
	progressf(progress, "changed", "route", "served via %s", m.viaName(via))

	m.announce(ctx, rec)
	return via, nil
}

func (m *Manager) canServeVia(via config.Via) error {
	if via == config.ViaNode && m.fw().Private {
		return fmt.Errorf("this machine was joined with --private, so it has no public listener and "+
			"cannot serve a name itself: leave it on `--via %s`, or re-add the machine without --private",
			config.ViaHub)
	}
	if via == config.ViaHub && m.fw().Fleet == nil {
		return fmt.Errorf("this machine is not part of a fleet, so there is no hub to serve the name: " +
			"join it to one with `caramelo machine join`, or use `--via node`")
	}
	return nil
}

func (m *Manager) viaName(via config.Via) string {
	if via != config.ViaHub {
		return string(config.ViaNode)
	}
	if name := m.hubName(context.Background()); name != "" {
		return "the hub (" + name + ")"
	}
	return "the hub"
}

func (m *Manager) hubName(ctx context.Context) string {
	if m.fw().Fleet == nil {
		return ""
	}
	machines, err := m.fw().Fleet.Machines(ctx)
	if err != nil {
		m.logf("read the fleet's machines: %v", err)
		return ""
	}
	return hubOf(machines)
}

func (m *Manager) ingress(ctx context.Context) *edge.Ingress { return m.IngressWanted(ctx) }

func (m *Manager) IngressWanted(ctx context.Context) *edge.Ingress {
	if m.fw().Fleet == nil || m.fw().Role.IsHub() {
		return nil
	}
	machines, err := m.fw().Fleet.Machines(ctx)
	if err != nil {
		m.logf("read the fleet's machines for the ingress: %v", err)
		return nil
	}
	var hubAddr string
	for _, mm := range machines {
		if mm.Role == fleet.RoleHub {
			if addr := mm.Address(); addr.IsValid() {
				hubAddr = addr.String()
			}
			break
		}
	}
	if !m.fw().Private && !m.anyServedViaHub(ctx) {
		return &edge.Ingress{Enabled: false}
	}
	ing := &edge.Ingress{Enabled: true, Port: edge.IngressPort}
	if hubAddr != "" {
		ing.Trusted = []string{hubAddr}
	}
	return ing
}

func (m *Manager) anyServedViaHub(ctx context.Context) bool {
	rows, err := m.fw().Fleet.EnvFleetRows(ctx, "")
	if err != nil {
		m.logf("read the fleet columns: %v", err)
		return false
	}
	for _, row := range rows {
		if row.Via == config.ViaHub {
			return true
		}
	}
	return false
}

func ViaRoute(host, machine string, app, envName, service string, ports []int, drain time.Duration) edge.Route {
	r := edge.Route{
		Host:    edge.NormalizeHost(host),
		Kind:    edge.KindVia,
		App:     app,
		Env:     envName,
		Service: service,
		Via:     machine,
		Drain:   drain,
	}
	for i, p := range ports {
		r.Targets = append(r.Targets, edge.Target{
			Replica: i + 1,
			Port:    p,
			State:   edge.TargetActive,
		})
	}
	return r
}

func ViaRelayPortFor(mach fleet.Machine) (int, bool) {
	addr := mach.Address()
	if !addr.IsValid() || !addr.Is4() {
		return 0, false
	}
	octet := int(addr.As4()[1])
	return ViaRelayPort + octet, true
}
