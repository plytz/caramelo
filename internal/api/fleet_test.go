package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/progress"
)

type fakeDirectory struct {
	envs     map[[2]string]fleet.DirectoryEntry
	machines map[string]fleet.Machine
	err      error
}

func (d *fakeDirectory) Locate(_ context.Context, app, env string) (fleet.DirectoryEntry, bool, error) {
	if d.err != nil {
		return fleet.DirectoryEntry{}, false, d.err
	}
	e, ok := d.envs[[2]string{app, env}]
	return e, ok, nil
}

func (d *fakeDirectory) Machine(_ context.Context, name string) (fleet.Machine, bool, error) {
	if d.err != nil {
		return fleet.Machine{}, false, d.err
	}
	m, ok := d.machines[name]
	return m, ok, nil
}

func member(t *testing.T, name, subnet string, lastSeen time.Time) fleet.Machine {
	t.Helper()
	p, err := netip.ParsePrefix(subnet)
	if err != nil {
		t.Fatalf("parse %q: %v", subnet, err)
	}
	return fleet.Machine{Name: name, Role: fleet.RoleMember, Subnet: p, LastSeen: lastSeen}
}

func TestAMachineOfOneResolvesEverythingLocally(t *testing.T) {
	var r *FleetResolver
	loc, err := r.ResolveEnv(context.Background(), "shop", "feat-x")
	if err != nil || !loc.Local {
		t.Fatalf("location = %+v, err %v, want local", loc, err)
	}
	r = &FleetResolver{}
	if loc, err := r.ResolveEnv(context.Background(), "shop", "feat-x"); err != nil || !loc.Local {
		t.Fatalf("location = %+v, err %v, want local", loc, err)
	}
	if _, err := r.ResolveMachine(context.Background(), "nx2"); err == nil {
		t.Fatal("a machine of one claimed to know another machine")
	}
}

func TestAnEnvironmentTheDirectoryDoesNotKnowIsLocal(t *testing.T) {
	r := &FleetResolver{Self: "hub", Dir: &fakeDirectory{envs: map[[2]string]fleet.DirectoryEntry{}}}
	loc, err := r.ResolveEnv(context.Background(), "shop", "feat-new")
	if err != nil {
		t.Fatalf("ResolveEnv: %v", err)
	}
	if !loc.Local || loc.Machine != "hub" {
		t.Fatalf("location = %+v, want this machine: `env create` has to resolve to somewhere", loc)
	}
}

func TestAnEnvironmentOnAMemberResolvesToItsTunnelAddress(t *testing.T) {
	now := time.Now()
	dir := &fakeDirectory{
		envs:     map[[2]string]fleet.DirectoryEntry{{"shop", "feat-x"}: {App: "shop", Env: "feat-x", Machine: "nx2"}},
		machines: map[string]fleet.Machine{"nx2": member(t, "nx2", "10.87.0.0/16", now)},
	}
	r := &FleetResolver{Self: "hub", Dir: dir, Now: func() time.Time { return now }}

	loc, err := r.ResolveEnv(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("ResolveEnv: %v", err)
	}
	if loc.Local || loc.Machine != "nx2" || loc.Address != "10.87.0.1:4022" || !loc.Reachable {
		t.Fatalf("location = %+v, want nx2 at 10.87.0.1:4022, reachable", loc)
	}
}

func TestAnEnvironmentOnThisMachineIsLocalEvenThoughTheDirectoryKnowsIt(t *testing.T) {
	dir := &fakeDirectory{
		envs: map[[2]string]fleet.DirectoryEntry{{"shop", "feat-x"}: {Machine: "hub"}},
	}
	r := &FleetResolver{Self: "hub", Dir: dir}
	loc, err := r.ResolveEnv(context.Background(), "shop", "feat-x")
	if err != nil || !loc.Local {
		t.Fatalf("location = %+v, err %v, want local", loc, err)
	}
}

func TestAMemberNotHeardFromIsUnreachableAndSaysWhen(t *testing.T) {
	now := time.Now()
	old := now.Add(-fleet.UnreachableAfter - time.Second)
	dir := &fakeDirectory{
		envs:     map[[2]string]fleet.DirectoryEntry{{"shop", "feat-x"}: {Machine: "nx2"}},
		machines: map[string]fleet.Machine{"nx2": member(t, "nx2", "10.87.0.0/16", old)},
	}
	r := &FleetResolver{Self: "hub", Dir: dir, Now: func() time.Time { return now }}

	loc, err := r.ResolveEnv(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("ResolveEnv: %v", err)
	}
	if loc.Reachable {
		t.Fatalf("location = %+v, want unreachable", loc)
	}
	msg := Unreachable(loc).Error()
	if !strings.Contains(msg, "nx2") || !strings.Contains(msg, "last seen") {
		t.Fatalf("message = %q, want the machine and when it was last seen", msg)
	}

	f := &TunnelForwarder{}
	if _, err := f.Forward(context.Background(), loc, []string{"up"}, nil, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "nx2") {
		t.Fatalf("forward error = %v, want a refusal naming the machine", err)
	}
}

func TestResolvingAMachineTheFleetDoesNotHave(t *testing.T) {
	r := &FleetResolver{Self: "hub", Dir: &fakeDirectory{machines: map[string]fleet.Machine{}}}
	_, err := r.ResolveMachine(context.Background(), "nx9")
	if err == nil || !strings.Contains(err.Error(), "machine list") {
		t.Fatalf("error = %v, want one naming the command that lists them", err)
	}
}

func TestADirectoryThatCannotBeReadIsAnErrorAndNotALocalAnswer(t *testing.T) {
	r := &FleetResolver{Self: "hub", Dir: &fakeDirectory{err: errors.New("the database is locked")}}
	if _, err := r.ResolveEnv(context.Background(), "shop", "feat-x"); err == nil {
		t.Fatal("a directory that could not be read resolved to this machine, which would run the command in the wrong place")
	}
}

func TestForwardingRefusesALocationThatIsThisMachine(t *testing.T) {
	f := &TunnelForwarder{}
	_, err := f.Forward(context.Background(), Location{Local: true, Reachable: true}, []string{"up"}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "resolver") {
		t.Fatalf("error = %v, want one saying the resolver is wrong", err)
	}
}

func TestForwardingWithNoTunnelSaysSoRatherThanDialling(t *testing.T) {
	f := &TunnelForwarder{}
	loc := Location{Machine: "nx2", Address: "10.87.0.1:4022", Reachable: true}
	_, err := f.Forward(context.Background(), loc, []string{"up"}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "tunnel is not up") {
		t.Fatalf("error = %v, want one about this machine's tunnel", err)
	}
}

func TestTheHubStampsTheMachineOnEveryEventThatPassesThrough(t *testing.T) {
	var out bytes.Buffer
	s := &MachineStamp{W: &out, Machine: "nx2"}
	w := progress.New(s, progress.FormatJSON)
	for _, e := range []progress.Event{
		{Action: "up", Status: progress.StatusStarted, App: "shop", Env: "feat-x"},
		{Action: "rollout", Status: progress.StatusChanged, Detail: "web-1 flip"},
	} {
		if err := w.Emit(e); err != nil {
			t.Fatalf("emit: %v", err)
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var e progress.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		if e.Machine != "nx2" {
			t.Fatalf("event %q carries machine %q, want nx2", e.Action, e.Machine)
		}
	}
}

func TestStampingLeavesEverythingThatIsNotAnEventAlone(t *testing.T) {
	cases := []string{
		"[changed] rollout: web-1 flip\n",
		`{"id":3,"app":"shop","name":"feat-x"}` + "\n",
		"total 4\ndrwxr-xr-x 2 caramelo caramelo 4096 .\n",
		`{"action":"up","status":"ok","machine":"nx9"}` + "\n",
	}
	for _, in := range cases {
		var out bytes.Buffer
		s := &MachineStamp{W: &out, Machine: "nx2"}
		if _, err := s.Write([]byte(in)); err != nil {
			t.Fatalf("write %q: %v", in, err)
		}
		if err := s.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		if out.String() != in {
			t.Fatalf("stamping changed %q into %q", in, out.String())
		}
	}
}

func TestStampingReassemblesALineThatArrivedInPieces(t *testing.T) {
	var out bytes.Buffer
	s := &MachineStamp{W: &out, Machine: "nx2"}
	line := `{"action":"deploy","status":"changed","detail":"flip"}` + "\n"
	for i := 0; i < len(line); i += 7 {
		end := i + 7
		if end > len(line) {
			end = len(line)
		}
		if _, err := s.Write([]byte(line[i:end])); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	var e progress.Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &e); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if e.Machine != "nx2" || e.Action != "deploy" {
		t.Fatalf("event = %+v, want the deploy stamped nx2", e)
	}
}

func TestStampingOnAMachineOfOneChangesNothing(t *testing.T) {
	var out bytes.Buffer
	s := &MachineStamp{W: &out}
	line := `{"action":"up","status":"ok"}` + "\n"
	if _, err := s.Write([]byte(line)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if out.String() != line {
		t.Fatalf("output = %q, want it untouched", out.String())
	}
}

func TestStampingDoesNotHoldAPromptThatIsWaitingForInput(t *testing.T) {
	var out bytes.Buffer
	s := &MachineStamp{W: &out, Machine: "nx2"}
	if _, err := s.Write([]byte("password: ")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if out.String() != "password: " {
		t.Fatalf("output = %q, want the prompt through at once", out.String())
	}

	if _, err := s.Write([]byte("ok\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if out.String() != "password: ok\n" {
		t.Fatalf("output = %q, want the whole line", out.String())
	}
}

func TestStampingHoldsAPartialEventButNotForEver(t *testing.T) {
	var out bytes.Buffer
	s := &MachineStamp{W: &out, Machine: "nx2"}
	if _, err := s.Write([]byte(`{"action":"deploy","stat`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q, want half an event held back", out.String())
	}
	if _, err := s.Write([]byte("us\":\"changed\"}\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	var e progress.Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &e); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if e.Machine != "nx2" {
		t.Fatalf("event = %+v, want it stamped once it was whole", e)
	}

	out.Reset()
	s = &MachineStamp{W: &out, Machine: "nx2"}
	big := append([]byte("{"), bytes.Repeat([]byte("x"), partialLimit)...)
	if _, err := s.Write(big); err != nil {
		t.Fatalf("write: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("a '{' longer than any event is still being held")
	}
}
