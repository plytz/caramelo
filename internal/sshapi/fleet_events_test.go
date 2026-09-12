package sshapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/state"
)

func TestAFeedForAnEnvironmentOnAMemberIsForwarded(t *testing.T) {
	fwd := &fakeForwarder{code: 0}
	d := &Daemon{
		EnvManager: &env.Manager{},
		Resolver:   &fakeResolver{env: map[string]api.Location{"shop/feat-x": remote("nx2")}},
		Forwarder:  fwd,
	}
	ctx, _ := fleetSession(t, "events", "feat-x", "--app", "shop", "--json")

	err := d.Events(ctx, env.EventsRequest{App: "shop", Env: "feat-x"}, io.Discard, false)
	var fe *api.ForwardedError
	if !errors.As(err, &fe) {
		t.Fatalf("error = %v, want an api.ForwardedError", err)
	}
	if strings.Join(fwd.argv, " ") != "events feat-x --app shop --json" {
		t.Fatalf("argv = %v, want the session's own", fwd.argv)
	}
}

func TestAFeedForTheWholeMachineIsNeverForwarded(t *testing.T) {
	fwd := &fakeForwarder{}
	d := &Daemon{
		Resolver:  &fakeResolver{env: map[string]api.Location{"shop/feat-x": remote("nx2")}},
		Forwarder: fwd,
	}
	ctx, _ := fleetSession(t, "events", "--json")

	_ = d.Events(ctx, env.EventsRequest{}, io.Discard, false)
	if fwd.argv != nil {
		t.Fatalf("the machine's own feed was forwarded: %v", fwd.argv)
	}
}

func TestEnvShowFallsBackToTheDirectoryWhenTheMachineWillNotAnswer(t *testing.T) {
	store := fleetStore(t)
	ctx := context.Background()
	if err := store.PutDirectoryEntry(ctx, state.DirectoryRow{
		App: "shop", Env: "feat-x", Machine: "m1", Address: "10.80.1.1",
		Owner: "laptop", Mode: "dev", UpdatedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatalf("write the directory row: %v", err)
	}
	d := &Daemon{
		Store:     store,
		Resolver:  &fakeResolver{env: map[string]api.Location{"shop/feat-x": remote("m1")}},
		Forwarder: &fakeForwarder{err: errors.New("dial tcp 10.80.1.1:4022: i/o timeout")},
	}
	sessCtx, _ := fleetSession(t, "env", "show", "feat-x", "--app", "shop", "--json")

	det, err := d.Env(sessCtx, "shop", "feat-x")
	if err != nil {
		t.Fatalf("env show of an environment whose machine will not answer: %v", err)
	}
	if !det.Unreachable {
		t.Error("the answer does not say the machine is unreachable")
	}
	if det.Machine != "m1" || det.Env.Owner != "laptop" {
		t.Errorf("the answer is %+v, want what the directory holds", det.Env)
	}
}

func TestEnvShowOfAnEnvironmentInNoDirectoryIsStillAnError(t *testing.T) {
	d := &Daemon{
		Store:     fleetStore(t),
		Resolver:  &fakeResolver{env: map[string]api.Location{"shop/feat-x": remote("m1")}},
		Forwarder: &fakeForwarder{err: errors.New("dial tcp 10.80.1.1:4022: i/o timeout")},
	}
	sessCtx, _ := fleetSession(t, "env", "show", "feat-x", "--app", "shop", "--json")
	if _, err := d.Env(sessCtx, "shop", "feat-x"); err == nil {
		t.Fatal("env show invented an environment nothing holds")
	}
}

func TestAMemberCopiesImagesFromItsHubAndNobodyElse(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	list, err := json.Marshal([]fleet.Machine{
		{Name: "hub1", Role: fleet.RoleHub},
		{Name: "m1", Role: fleet.RoleMember, LastSeen: now},
		{Name: "m2", Role: fleet.RoleMember, LastSeen: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	member := &Daemon{
		Store:     fleetStore(t),
		Now:       func() time.Time { return now },
		Forwarder: &fakeForwarder{out: string(list)},
		Config: serverconfig.Config{SSHPort: serverconfig.DefaultSSHPort,
			Fleet: serverconfig.Fleet{
				Role: "member", Name: "m2",
				Hub: serverconfig.FleetHub{Name: "hub1", PublicKey: "k", Endpoint: "hub:4021", Address: "10.86.0.1"}}},
	}

	sources, err := member.imageSources(ctx)
	if err != nil {
		t.Fatalf("imageSources on a member: %v", err)
	}
	got := map[string]bool{}
	for _, s := range sources {
		got[s.Machine] = s.Reachable
	}
	if len(got) != 2 {
		t.Fatalf("sources = %+v, want the hub and the other member (itself excluded)", sources)
	}
	if !got["hub1"] {
		t.Error("a member cannot copy from its own hub, which is the one machine it can reach")
	}
	if got["m1"] {
		t.Error("a member thinks it can copy from another member; that session is refused at the far end")
	}
}

func TestAHubCopiesImagesFromAnyMemberItHasHeardFrom(t *testing.T) {
	store := fleetStore(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	for _, m := range []state.MachineRow{
		{Name: "hub1", Role: "hub", PublicKey: "hk", Subnet: "10.86.0.0/16"},
		{Name: "m1", Role: "member", PublicKey: "mk", Subnet: "10.80.0.0/16"},
	} {
		if err := store.PutFleetMachine(ctx, m); err != nil {
			t.Fatalf("write the machine %s: %v", m.Name, err)
		}
	}
	if err := store.SetMachineSeen(ctx, "m1", "", now); err != nil {
		t.Fatalf("record that m1 was seen: %v", err)
	}
	hub := &Daemon{
		Store:  store,
		Now:    func() time.Time { return now },
		Config: serverconfig.Config{Fleet: serverconfig.Fleet{Role: "hub", Name: "hub1"}},
	}

	sources, err := hub.imageSources(ctx)
	if err != nil {
		t.Fatalf("imageSources on a hub: %v", err)
	}
	if len(sources) != 1 || sources[0].Machine != "m1" || !sources[0].Reachable {
		t.Fatalf("sources = %+v, want m1 reachable", sources)
	}
}
