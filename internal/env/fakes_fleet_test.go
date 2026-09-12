package env

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/machine"
	"github.com/plytz/caramelo/internal/state"
)

type fakeFleet struct {
	mu sync.Mutex

	rows map[int64]FleetRow

	dir map[[2]string]fleet.DirectoryEntry

	machines []fleet.Machine

	failPut      error
	failRows     error
	failMachines error
}

func newFleet(machines ...fleet.Machine) *fakeFleet {
	return &fakeFleet{
		rows:     map[int64]FleetRow{},
		dir:      map[[2]string]fleet.DirectoryEntry{},
		machines: machines,
	}
}

func (f *fakeFleet) EnvFleetRow(_ context.Context, id int64) (FleetRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRows != nil {
		return FleetRow{}, f.failRows
	}
	row, ok := f.rows[id]
	if !ok {
		return FleetRow{}, nil
	}
	return row, nil
}

func (f *fakeFleet) EnvFleetRows(_ context.Context, _ string) (map[int64]FleetRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRows != nil {
		return nil, f.failRows
	}
	out := make(map[int64]FleetRow, len(f.rows))
	for id, row := range f.rows {
		out[id] = row
	}
	return out, nil
}

func (f *fakeFleet) SetEnvFleetRow(_ context.Context, id int64, row FleetRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == 0 {
		return fmt.Errorf("no env id")
	}
	f.rows[id] = row
	return nil
}

func (f *fakeFleet) PutDirectoryEntry(_ context.Context, e fleet.DirectoryEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPut != nil {
		return f.failPut
	}
	f.dir[[2]string{e.App, e.Env}] = e
	return nil
}

func (f *fakeFleet) RemoveDirectoryEntry(_ context.Context, app, env string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.dir, [2]string{app, env})
	return nil
}

func (f *fakeFleet) DirectoryEntries(_ context.Context, machine string) ([]fleet.DirectoryEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fleet.DirectoryEntry, 0, len(f.dir))
	for _, e := range f.dir {
		if machine != "" && e.Machine != machine {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].App != out[j].App {
			return out[i].App < out[j].App
		}
		return out[i].Env < out[j].Env
	})
	return out, nil
}

func (f *fakeFleet) Machines(context.Context) ([]fleet.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failMachines != nil {
		return nil, f.failMachines
	}
	return append([]fleet.Machine(nil), f.machines...), nil
}

func (f *fakeFleet) names() []string {
	entries, _ := f.DirectoryEntries(context.Background(), "")
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Machine+":"+e.App+"/"+e.Env)
	}
	return out
}

type recorder struct {
	mu sync.Mutex

	got []fleet.Announcement

	into FleetState

	err error
}

func (r *recorder) Announce(ctx context.Context, a fleet.Announcement) error {
	r.mu.Lock()
	r.got = append(r.got, a)
	into, err := r.into, r.err
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if into == nil {
		return nil
	}
	_, _, aerr := ApplyAnnouncement(ctx, into, a, time.Unix(0, 0))
	return aerr
}

func (r *recorder) last() (fleet.Announcement, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.got) == 0 {
		return fleet.Announcement{}, false
	}
	return r.got[len(r.got)-1], true
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func gauge(availableMB, reservedMB int64) *machine.Record {
	rec := &machine.Record{}
	rec.Memory.AvailableBytes = availableMB << 20
	rec.Reserved.MemoryBytes = reservedMB << 20
	return rec
}

func member(name, arch string, envs int, g *machine.Record) fleet.Machine {
	return fleet.Machine{
		Name: name, Role: fleet.RoleMember, Arch: arch, Envs: envs,
		Gauge: g, LastSeen: time.Now(), JoinedAt: time.Now(),
	}
}

func hub(name, arch string, envs int, g *machine.Record) fleet.Machine {
	return fleet.Machine{Name: name, Role: fleet.RoleHub, Arch: arch, Envs: envs, Gauge: g, JoinedAt: time.Now()}
}

func (h *harness) onFleet(name string, machines ...fleet.Machine) (*fakeFleet, *recorder) {
	h.t.Helper()
	f := newFleet(machines...)
	rec := &recorder{into: f}
	h.wire(func(w *FleetWiring) {
		w.Machine, w.Fleet, w.Announce = name, f, rec
	})
	return f, rec
}

func (h *harness) envID(name string) int64 {
	h.t.Helper()
	rec, err := h.store.Env(context.Background(), "shop", name)
	if err != nil {
		h.t.Fatalf("read env %q: %v", name, err)
	}
	return rec.ID
}

var _ FleetState = (*fakeFleet)(nil)
var _ Announcer = (*recorder)(nil)
var _ = state.ErrNotFound

func (h *harness) wire(fn func(*FleetWiring)) {
	h.t.Helper()
	h.m.UpdateFleetWiring(fn)
}
