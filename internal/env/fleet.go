package env

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/state"
)

type FleetState interface {
	EnvFleetRow(ctx context.Context, envID int64) (FleetRow, error)

	EnvFleetRows(ctx context.Context, app string) (map[int64]FleetRow, error)

	SetEnvFleetRow(ctx context.Context, envID int64, row FleetRow) error

	PutDirectoryEntry(ctx context.Context, e fleet.DirectoryEntry) error

	RemoveDirectoryEntry(ctx context.Context, app, env string) error

	DirectoryEntries(ctx context.Context, machine string) ([]fleet.DirectoryEntry, error)

	Machines(ctx context.Context) ([]fleet.Machine, error)
}

type FleetRow struct {
	Owner string `json:"owner,omitempty"`

	Via config.Via `json:"via,omitempty"`
}

type Announcer interface {
	Announce(ctx context.Context, a fleet.Announcement) error
}

type AnnouncerFunc func(ctx context.Context, a fleet.Announcement) error

func (f AnnouncerFunc) Announce(ctx context.Context, a fleet.Announcement) error { return f(ctx, a) }

func (m *Manager) fleetRow(ctx context.Context, envID int64) FleetRow {
	if m.fw().Fleet == nil || envID == 0 {
		return FleetRow{}
	}
	row, err := m.fw().Fleet.EnvFleetRow(ctx, envID)
	if err != nil {

		m.logf("read the fleet row of env %d: %v", envID, err)
		return FleetRow{}
	}
	return row
}

func (m *Manager) setFleetRow(ctx context.Context, envID int64, row FleetRow) error {
	if m.fw().Fleet == nil {
		return nil
	}
	if err := m.fw().Fleet.SetEnvFleetRow(ctx, envID, row); err != nil {
		return fmt.Errorf("record env %d as owned by %q via %s: %w", envID, row.Owner, row.Via.String(), err)
	}
	return nil
}

func (m *Manager) ViaOf(ctx context.Context, rec *state.EnvRecord, cfg *config.App) config.Via {
	if rec == nil {
		return config.ViaMember
	}
	if row := m.fleetRow(ctx, rec.ID); row.Via != "" {
		return row.Via
	}
	if cfg != nil {
		return cfg.ViaOf(rec.Name)
	}
	return config.ViaMember
}

func (m *Manager) directoryEntry(ctx context.Context, rec *state.EnvRecord) fleet.DirectoryEntry {
	row := m.fleetRow(ctx, rec.ID)
	e := fleet.DirectoryEntry{
		App:       rec.App,
		Env:       rec.Name,
		Machine:   m.fw().Machine,
		Address:   rec.VPNIP,
		Owner:     row.Owner,
		Mode:      rec.Mode,
		Via:       row.Via.String(),
		UpdatedAt: m.now(),
	}
	if row.Via == config.ViaHub {
		e.Hosts = m.viaHosts(ctx, rec)
	}
	return e
}

func (m *Manager) viaHosts(ctx context.Context, rec *state.EnvRecord) []fleet.ViaHost {
	rows, err := m.Store.RoutesOfEnv(ctx, rec.ID)
	if err != nil {
		m.logf("read the routes of %s/%s for the announcement: %v", rec.App, rec.Name, err)
		return nil
	}
	out := make([]fleet.ViaHost, 0, len(rows))
	for _, r := range rows {
		out = append(out, fleet.ViaHost{
			Host: r.Host, Service: r.Service, Drain: m.drainFor(r.Host),
		})
	}
	return out
}

func (m *Manager) announce(ctx context.Context, rec *state.EnvRecord) {
	if m.fw().Announce == nil || rec == nil {
		return
	}
	a := fleet.Announcement{
		Machine: m.fw().Machine,
		Envs:    []fleet.DirectoryEntry{m.directoryEntry(ctx, rec)},
		At:      m.now(),
	}
	m.stampGauge(ctx, &a)
	if err := m.fw().Announce.Announce(ctx, a); err != nil {
		m.logf("announce env %s/%s: %v", rec.App, rec.Name, err)
	}
}

func (m *Manager) announceGone(ctx context.Context, app, name string) {
	if m.fw().Announce == nil {
		return
	}
	if err := m.AnnounceAll(ctx); err != nil {
		m.logf("announce the environments left after destroying %s/%s: %v", app, name, err)
	}
}

func (m *Manager) AnnounceAll(ctx context.Context) error {
	if m.fw().Announce == nil {
		return nil
	}
	recs, err := m.Store.Envs(ctx, "")
	if err != nil {
		return fmt.Errorf("read this machine's environments: %w", err)
	}
	entries := make([]fleet.DirectoryEntry, 0, len(recs))
	for i := range recs {
		entries = append(entries, m.directoryEntry(ctx, &recs[i]))
	}
	sortEntries(entries)
	a := fleet.Announcement{
		Machine: m.fw().Machine,
		Envs:    entries,
		Full:    true,
		At:      m.now(),
	}
	m.stampGauge(ctx, &a)
	if err := m.fw().Announce.Announce(ctx, a); err != nil {
		return fmt.Errorf("announce %s to the fleet: %w", plural(len(entries), "environment"), err)
	}
	return nil
}

func (m *Manager) Directory(ctx context.Context, app string) ([]fleet.DirectoryEntry, error) {
	if m.fw().Fleet == nil {
		return nil, nil
	}
	entries, err := m.fw().Fleet.DirectoryEntries(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("read the environment directory: %w", err)
	}
	if app != "" {
		kept := entries[:0]
		for _, e := range entries {
			if e.App == app {
				kept = append(kept, e)
			}
		}
		entries = kept
	}
	sortEntries(entries)
	return entries, nil
}

func ApplyAnnouncement(ctx context.Context, st FleetState, a fleet.Announcement, now time.Time) (accepted, removed int, err error) {
	if st == nil {
		return 0, 0, fmt.Errorf("apply the announcement of machine %q: this machine keeps no directory", a.Machine)
	}
	if a.Machine == "" {
		return 0, 0, fmt.Errorf("apply an announcement: it names no machine")
	}
	keep := make(map[[2]string]bool, len(a.Envs))
	for _, e := range a.Envs {
		if e.App == "" || e.Env == "" {
			return accepted, removed, fmt.Errorf("apply the announcement of machine %q: an entry names no environment", a.Machine)
		}

		e.Machine = a.Machine
		if e.UpdatedAt.IsZero() {
			e.UpdatedAt = now
		}
		if err := st.PutDirectoryEntry(ctx, e); err != nil {
			return accepted, removed, fmt.Errorf("record env %s/%s on machine %q: %w", e.App, e.Env, a.Machine, err)
		}
		accepted++
		keep[[2]string{e.App, e.Env}] = true
	}
	if !a.Full {
		return accepted, 0, nil
	}
	have, err := st.DirectoryEntries(ctx, a.Machine)
	if err != nil {
		return accepted, 0, fmt.Errorf("read the directory rows of machine %q: %w", a.Machine, err)
	}
	for _, e := range have {
		if keep[[2]string{e.App, e.Env}] {
			continue
		}
		if err := st.RemoveDirectoryEntry(ctx, e.App, e.Env); err != nil {
			return accepted, removed, fmt.Errorf("forget env %s/%s of machine %q: %w", e.App, e.Env, a.Machine, err)
		}
		removed++
	}
	return accepted, removed, nil
}

func sortEntries(entries []fleet.DirectoryEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].App != entries[j].App {
			return entries[i].App < entries[j].App
		}
		return entries[i].Env < entries[j].Env
	})
}

func (m *Manager) stampGauge(ctx context.Context, a *fleet.Announcement) {
	a.Arch = m.fw().Arch
	if m.fw().Gauge == nil {
		return
	}
	if rec := m.fw().Gauge(ctx); rec != nil {
		a.Gauge = rec
		if a.Arch == "" {
			a.Arch = rec.OS.Arch
		}
		a.OS = rec.OS.ID
	}
}
