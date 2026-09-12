package sshapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/machine"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/state"
)

type storeFleetState struct{ store state.Store }

var _ env.FleetState = storeFleetState{}

func (f storeFleetState) EnvFleetRow(ctx context.Context, envID int64) (env.FleetRow, error) {
	recs, err := f.store.Envs(ctx, "")
	if err != nil {
		return env.FleetRow{}, fmt.Errorf("read this machine's environments: %w", err)
	}
	for i := range recs {
		if recs[i].ID == envID {
			return fleetRowOf(recs[i]), nil
		}
	}
	return env.FleetRow{}, state.ErrNotFound
}

func (f storeFleetState) EnvFleetRows(ctx context.Context, app string) (map[int64]env.FleetRow, error) {
	recs, err := f.store.Envs(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("read the environments of %q: %w", app, err)
	}
	out := make(map[int64]env.FleetRow, len(recs))
	for i := range recs {
		out[recs[i].ID] = fleetRowOf(recs[i])
	}
	return out, nil
}

func (f storeFleetState) SetEnvFleetRow(ctx context.Context, envID int64, row env.FleetRow) error {
	if err := f.store.SetEnvOwner(ctx, envID, row.Owner); err != nil {
		return fmt.Errorf("record env %d as owned by %q: %w", envID, row.Owner, err)
	}
	if err := f.store.SetEnvVia(ctx, envID, row.Via.String()); err != nil {
		return fmt.Errorf("record env %d as served via %s: %w", envID, row.Via.String(), err)
	}
	return nil
}

func (f storeFleetState) PutDirectoryEntry(ctx context.Context, e fleet.DirectoryEntry) error {
	if err := f.store.PutDirectoryEntry(ctx, directoryRowOf(e)); err != nil {
		return fmt.Errorf("record %s/%s on %s in the directory: %w", e.App, e.Env, e.Machine, err)
	}
	return nil
}

func (f storeFleetState) RemoveDirectoryEntry(ctx context.Context, app, name string) error {
	if err := f.store.DeleteDirectoryEntry(ctx, app, name); err != nil {
		return fmt.Errorf("forget %s/%s in the directory: %w", app, name, err)
	}
	return nil
}

func (f storeFleetState) DirectoryEntries(ctx context.Context, mach string) ([]fleet.DirectoryEntry, error) {
	rows, err := f.store.Directory(ctx, state.DirectoryFilter{Machine: mach})
	if err != nil {
		return nil, fmt.Errorf("read the environment directory: %w", err)
	}
	out := make([]fleet.DirectoryEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, directoryEntryOf(r))
	}
	return out, nil
}

func (f storeFleetState) Machines(ctx context.Context) ([]fleet.Machine, error) {
	rows, err := f.store.FleetMachines(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the fleet's machines: %w", err)
	}
	counts := map[string]int{}
	if dir, derr := f.store.Directory(ctx, state.DirectoryFilter{}); derr == nil {
		for _, d := range dir {
			counts[d.Machine]++
		}
	}
	out := make([]fleet.Machine, 0, len(rows))
	for _, r := range rows {
		m := fleetMachineOf(r)
		m.Envs = counts[m.Name]
		out = append(out, m)
	}
	return out, nil
}

func fleetRowOf(r state.EnvRecord) env.FleetRow {
	return env.FleetRow{Owner: r.Owner, Via: config.Via(r.Via)}
}

func directoryRowOf(e fleet.DirectoryEntry) state.DirectoryRow {
	r := state.DirectoryRow{
		App: e.App, Env: e.Env, Machine: e.Machine, Address: e.Address,
		Owner: e.Owner, Mode: e.Mode, Via: e.Via, UpdatedAt: e.UpdatedAt,
	}
	if len(e.Hosts) > 0 {
		if b, err := json.Marshal(e.Hosts); err == nil {
			r.Hosts = string(b)
		}
	}
	return r
}

func directoryEntryOf(r state.DirectoryRow) fleet.DirectoryEntry {
	e := fleet.DirectoryEntry{
		App: r.App, Env: r.Env, Machine: r.Machine, Address: r.Address,
		Owner: r.Owner, Mode: r.Mode, Via: r.Via, UpdatedAt: r.UpdatedAt,
	}
	if r.Hosts != "" {

		var hosts []fleet.ViaHost
		if err := json.Unmarshal([]byte(r.Hosts), &hosts); err == nil {
			e.Hosts = hosts
		}
	}
	return e
}

func fleetMachineOf(r state.MachineRow) fleet.Machine {
	m := fleet.Machine{
		Name: r.Name, Role: fleet.Role(r.Role), PublicKey: r.PublicKey,
		Arch: r.Arch, OS: r.OS, Endpoint: r.Endpoint, Private: r.Private,
		JoinedAt: r.JoinedAt, LastSeen: r.LastSeen,
	}
	if p, err := netip.ParsePrefix(r.Subnet); err == nil {
		m.Subnet = p
	}
	if r.GaugeJSON != "" {
		var rec machine.Record
		if err := json.Unmarshal([]byte(r.GaugeJSON), &rec); err == nil {
			m.Gauge = &rec
		}
	}
	return m
}

func machineRowOf(m fleet.Machine) state.MachineRow {
	r := state.MachineRow{
		Name: m.Name, Role: string(m.Role), PublicKey: m.PublicKey,
		Arch: m.Arch, OS: m.OS, Endpoint: m.Endpoint, Private: m.Private,
		JoinedAt: m.JoinedAt, LastSeen: m.LastSeen,
	}
	if m.Subnet.IsValid() {
		r.Subnet = m.Subnet.String()
	}
	if m.Gauge != nil {
		if b, err := json.Marshal(m.Gauge); err == nil {
			r.GaugeJSON = string(b)
		}
	}
	return r
}

type fleetDirectory struct{ store state.Store }

func (d fleetDirectory) Locate(ctx context.Context, app, name string) (fleet.DirectoryEntry, bool, error) {
	row, err := d.store.DirectoryEntry(ctx, app, name)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return fleet.DirectoryEntry{}, false, nil
	case err != nil:
		return fleet.DirectoryEntry{}, false, fmt.Errorf("look %s/%s up in the directory: %w", app, name, err)
	}
	return directoryEntryOf(*row), true, nil
}

func (d fleetDirectory) Machine(ctx context.Context, name string) (fleet.Machine, bool, error) {
	row, err := d.store.FleetMachine(ctx, name)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return fleet.Machine{}, false, nil
	case err != nil:
		return fleet.Machine{}, false, fmt.Errorf("look the machine %q up: %w", name, err)
	}
	return fleetMachineOf(*row), true, nil
}

type releaseImages struct{ store state.Store }

var _ release.ImageStore = releaseImages{}

func (s releaseImages) PutImage(ctx context.Context, im release.Image) error {
	err := s.store.AddReleaseImage(ctx, state.ReleaseImage{
		ReleaseID: im.ReleaseID, Service: im.Service, Arch: im.Arch,
		Machine: im.Machine, ImageID: im.ID, BuiltAt: im.BuiltAt,
	})
	if err != nil {
		return fmt.Errorf("record the %s image of release %d on %s: %w", im.Service, im.ReleaseID, im.Machine, err)
	}
	return nil
}

func (s releaseImages) Images(ctx context.Context, releaseID int64) (release.Images, error) {
	rows, err := s.store.ReleaseImages(ctx, releaseID)
	if err != nil {
		return nil, fmt.Errorf("read the images of release %d: %w", releaseID, err)
	}
	out := make(release.Images, 0, len(rows))
	for _, r := range rows {
		out = append(out, release.Image{
			ReleaseID: r.ReleaseID, Service: r.Service, Arch: r.Arch,
			Machine: r.Machine, ID: r.ImageID, BuiltAt: r.BuiltAt,
		})
	}
	return out, nil
}

func (s releaseImages) RemoveImages(ctx context.Context, releaseID int64, mach string) error {
	rows, err := s.store.ReleaseImages(ctx, releaseID)
	if err != nil {
		return fmt.Errorf("read the images of release %d: %w", releaseID, err)
	}
	for _, r := range rows {
		if mach != "" && r.Machine != mach {
			continue
		}
		if err := s.store.DeleteReleaseImage(ctx, releaseID, r.Service, r.Arch, r.Machine); err != nil {
			return fmt.Errorf("forget the %s image of release %d on %s: %w", r.Service, releaseID, r.Machine, err)
		}
	}
	return nil
}

type storeMachines struct{ store state.Store }

func (s storeMachines) MachineNamed(ctx context.Context, name string) (fleet.Machine, bool) {
	row, err := s.store.FleetMachine(ctx, name)
	if err != nil {
		return fleet.Machine{}, false
	}
	return fleetMachineOf(*row), true
}

func seenAt(ctx context.Context, store state.Store, name, endpoint string, at time.Time) error {
	if name == "" {
		return nil
	}
	err := store.SetMachineSeen(ctx, name, endpoint, at)
	if errors.Is(err, state.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("record the handshake from %s: %w", name, err)
	}
	return nil
}
