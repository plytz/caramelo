package sshapi

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/edge/certs"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/state"
)

type tableRecorder struct {
	mu     sync.Mutex
	tables []edge.Table
}

func (r *tableRecorder) PushTable(_ context.Context, t edge.Table) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tables = append(r.tables, t)
	return nil
}

func (r *tableRecorder) last() (edge.Table, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tables) == 0 {
		return edge.Table{}, false
	}
	return r.tables[len(r.tables)-1], true
}

func (r *tableRecorder) Status(context.Context) (*edge.Status, error) { return &edge.Status{}, nil }

func (r *tableRecorder) Subscribe(context.Context, time.Time, func(edge.Event) error) error {
	return nil
}

func (r *tableRecorder) CA(context.Context) (*certs.CA, error) { return nil, nil }

func (r *tableRecorder) Counts(_ context.Context, since time.Time) (*edge.Counts, error) {
	return &edge.Counts{Since: since}, nil
}

func (r *tableRecorder) Prune(_ context.Context, req certs.PruneRequest) (*certs.PruneResult, error) {
	return &certs.PruneResult{KeepFor: req.Keep(), DryRun: req.DryRun}, nil
}

func (r *tableRecorder) Close() error { return nil }

func fleetStore(t *testing.T) state.Store {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "caramelo.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.PutFleetMachine(context.Background(), state.MachineRow{
		Name: "m1", Role: "member", PublicKey: "m1-key", Subnet: "10.87.0.0/16",
		Arch: "arm64", JoinedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatalf("PutFleetMachine: %v", err)
	}
	return store
}

func TestAnnouncedViaHostsSurviveTheStoreAndBecomeARoute(t *testing.T) {
	store := fleetStore(t)
	ctx := context.Background()
	m := (&env.Manager{Store: store, Edge: &tableRecorder{}}).WithFleet(
		env.FleetWiring{Fleet: storeFleetState{store}, Machine: "hub"})
	d := &Daemon{Store: store, EnvManager: m, KnownMachines: machines("m1"),
		Now: func() time.Time { return time.Unix(2, 0) }}

	announced := WithSession(ctx, api.Session{Transport: "tunnel", Identity: "m1", Peer: "m1"})
	if _, err := d.MachineAnnounce(announced, fleet.Announcement{
		Machine: "m1", Full: true,
		Envs: []fleet.DirectoryEntry{{
			App: "shop", Env: "feat-p", Machine: "m1", Address: "10.87.1.2", Via: "hub",
			Hosts: []fleet.ViaHost{{Host: "feat-p.shop.test", Service: "web", Drain: 5 * time.Second}},
		}},
	}); err != nil {
		t.Fatalf("MachineAnnounce: %v", err)
	}

	entries, err := storeFleetState{store}.DirectoryEntries(ctx, "")
	if err != nil {
		t.Fatalf("DirectoryEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory = %+v, want one row", entries)
	}
	if got := entries[0].Hosts; len(got) != 1 || got[0].Host != "feat-p.shop.test" ||
		got[0].Service != "web" || got[0].Drain != 5*time.Second {
		t.Fatalf("hosts = %+v, want the announced one back whole", got)
	}

	rec := &tableRecorder{}
	hub := (&env.Manager{Store: store, Edge: rec}).WithFleet(
		env.FleetWiring{Fleet: storeFleetState{store}, Machine: "hub"})
	if err := hub.PushRoutes(ctx); err != nil {
		t.Fatalf("PushRoutes: %v", err)
	}
	table, ok := rec.last()
	if !ok {
		t.Fatal("no table was pushed")
	}
	var via *edge.Route
	for i := range table.Routes {
		if table.Routes[i].Kind == edge.KindVia {
			via = &table.Routes[i]
		}
	}
	if via == nil {
		t.Fatalf("table = %+v, want a via route for the member's name", table.Routes)
	}
	if via.Host != "feat-p.shop.test" || via.Via != "m1" || via.Service != "web" {
		t.Fatalf("via route = %+v, want feat-p.shop.test served via m1", via)
	}
	if via.Drain != 5*time.Second {
		t.Fatalf("via route drain = %s, want the announced 5s", via.Drain)
	}
	if len(via.Targets) != 1 {
		t.Fatalf("targets = %+v, want the one relay port", via.Targets)
	}
}

func TestOneNameAnnouncedTwiceLeavesTheHubsTableValid(t *testing.T) {
	store := fleetStore(t)
	ctx := context.Background()
	if err := store.PutFleetMachine(ctx, state.MachineRow{
		Name: "m2", Role: "member", PublicKey: "m2-key", Subnet: "10.88.0.0/16",
		Arch: "arm64", JoinedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatalf("PutFleetMachine: %v", err)
	}
	st := storeFleetState{store}
	hosts := []fleet.ViaHost{{Host: "www.shop.example", Service: "web"}}
	for _, m := range []string{"m1", "m2"} {
		if err := st.PutDirectoryEntry(ctx, fleet.DirectoryEntry{
			App: "shop", Env: "on-" + m, Machine: m, Via: "hub", Hosts: hosts,
		}); err != nil {
			t.Fatalf("PutDirectoryEntry for %s: %v", m, err)
		}
	}

	rec := &tableRecorder{}
	hub := (&env.Manager{Store: store, Edge: rec}).WithFleet(
		env.FleetWiring{Fleet: st, Machine: "hub"})
	if err := hub.PushRoutes(ctx); err != nil {
		t.Fatalf("PushRoutes: %v", err)
	}
	table, ok := rec.last()
	if !ok {
		t.Fatal("no table was pushed")
	}
	if err := table.Validate(); err != nil {
		t.Fatalf("the pushed table is invalid: %v", err)
	}
	var via []edge.Route
	for _, r := range table.Routes {
		if r.Kind == edge.KindVia {
			via = append(via, r)
		}
	}
	if len(via) != 1 {
		t.Fatalf("via routes = %+v, want the first claim only", via)
	}
	if via[0].Via != "m1" {
		t.Fatalf("the name went to %s, want the first claimant m1", via[0].Via)
	}
}

func TestABundleIsOnlyAnsweredToTheMachineThatHoldsTheEnv(t *testing.T) {
	store := fleetStore(t)
	ctx := context.Background()
	if err := store.PutFleetMachine(ctx, state.MachineRow{
		Name: "m2", Role: "member", PublicKey: "m2-key", Subnet: "10.88.0.0/16",
		Arch: "arm64", JoinedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatalf("PutFleetMachine: %v", err)
	}
	st := storeFleetState{store}
	if err := st.PutDirectoryEntry(ctx, fleet.DirectoryEntry{
		App: "shop", Env: "production", Machine: "m1",
	}); err != nil {
		t.Fatalf("PutDirectoryEntry: %v", err)
	}
	d := &Daemon{Store: store, KnownMachines: machines("m1", "m2")}

	asking := func(peer string) context.Context {
		return WithSession(ctx, api.Session{Transport: "tunnel", Identity: peer, Peer: peer})
	}

	_, err := d.VaultBundle(asking("m2"), api.BundleRequest{App: "shop", Env: "production"})
	if err == nil || !strings.Contains(err.Error(), "runs on machine m1") {
		t.Fatalf("error = %v, want a refusal naming the machine that holds it", err)
	}

	_, err = d.VaultBundle(asking("m1"), api.BundleRequest{App: "shop", Env: "production"})
	if err == nil || !strings.Contains(err.Error(), "no vault") {
		t.Fatalf("error = %v, want the holder to get past the guard", err)
	}

	_, err = d.VaultBundle(asking("m2"), api.BundleRequest{App: "shop", Env: "feat-new"})
	if err == nil || !strings.Contains(err.Error(), "no vault") {
		t.Fatalf("error = %v, want a brand-new environment to be answered", err)
	}
}
