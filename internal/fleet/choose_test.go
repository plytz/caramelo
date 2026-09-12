package fleet

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/config"
)

var testNow = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

func cand(name string, role Role, availableGiB float64, envs int) Candidate {
	return Candidate{
		Machine: Machine{
			Name: name, Role: role, Arch: "arm64", Envs: envs,
			Subnet:   netip.MustParsePrefix("10.86.0.0/16"),
			LastSeen: testNow.Add(-2 * time.Second),
		},
		Gauge: gauge(int64(availableGiB*float64(1<<30)), 256<<20),
	}
}

func TestChooseTakesTheEmptiestMachineThatFits(t *testing.T) {
	fleet := []Candidate{
		cand("hub", RoleHub, 1, 3),
		cand("m1", RoleMember, 4, 1),
		cand("m2", RoleMember, 2, 0),
	}
	d := Decide(t, fleet, Demand{App: "shop", Env: "feat-y", MemoryBytes: 512 << 20}, Preference{})
	if d.Machine != "m1" {
		t.Fatalf("chose %q, want m1 (the most free memory)", d.Machine)
	}
	if !strings.Contains(d.Why, "free") || !strings.Contains(d.Why, "1 env") {
		t.Errorf("why = %q, want the free memory and the environment count", d.Why)
	}
	if len(d.Considered) != 3 || d.Considered[0].Machine != "m1" {
		t.Fatalf("considered = %+v, want every machine with the winner first", d.Considered)
	}

	for _, c := range d.Considered {
		if !c.Fits {
			t.Errorf("%s was refused: %s", c.Machine, c.Reason)
		}
	}
}

func TestChooseBreaksTiesOnEnvironmentsThenName(t *testing.T) {
	d := Decide(t, []Candidate{
		cand("m2", RoleMember, 2, 1),
		cand("m1", RoleMember, 2, 1),
		cand("m3", RoleMember, 2, 0),
	}, Demand{App: "shop", Env: "feat-y"}, Preference{})
	if d.Machine != "m3" {
		t.Fatalf("chose %q, want m3 (fewest environments)", d.Machine)
	}
	d = Decide(t, []Candidate{
		cand("m2", RoleMember, 2, 1),
		cand("m1", RoleMember, 2, 1),
	}, Demand{App: "shop", Env: "feat-y"}, Preference{})
	if d.Machine != "m1" {
		t.Fatalf("chose %q, want m1 (the name breaks the last tie)", d.Machine)
	}
}

func TestChooseOnAFleetOfOne(t *testing.T) {
	d := Decide(t, []Candidate{cand("worker1", RoleHub, 1.5, 2)},
		Demand{App: "shop", Env: "feat-x"}, Preference{})
	if d.Machine != "worker1" {
		t.Fatalf("chose %q, want worker1", d.Machine)
	}
}

func TestChooseHonoursAPin(t *testing.T) {
	fleet := []Candidate{cand("hub", RoleHub, 8, 0), cand("m1", RoleMember, 1, 0)}

	d := Decide(t, fleet, Demand{App: "shop", Env: "feat-x"}, Preference{Pin: "m1"})
	if d.Machine != "m1" {
		t.Fatalf("chose %q, want m1", d.Machine)
	}
	if !strings.Contains(d.Why, "--on m1") {
		t.Errorf("why = %q, want it to say the flag pinned it", d.Why)
	}

	d = Decide(t, fleet, Demand{App: "shop", Env: "feat-x"}, Preference{Pin: "m1", PinnedByFile: true})
	if !strings.Contains(d.Why, "caramelo.yaml") {
		t.Errorf("why = %q, want it to say the file pinned it", d.Why)
	}
}

func TestChoosePlacementHub(t *testing.T) {
	d := Decide(t, []Candidate{cand("m1", RoleMember, 8, 0), cand("worker1", RoleHub, 1, 4)},
		Demand{App: "shop", Env: "feat-x"}, Preference{Placement: config.PlacementHub})
	if d.Machine != "worker1" {
		t.Fatalf("chose %q, want the hub", d.Machine)
	}
	if !strings.Contains(d.Why, "caramelo.yaml") {
		t.Errorf("why = %q, want it to say the file kept it there", d.Why)
	}
}

func TestChooseRefusesWithTheNumbers(t *testing.T) {
	small := cand("m1", RoleMember, 1, 0)
	gone := cand("m2", RoleMember, 8, 0)
	gone.Machine.LastSeen = testNow.Add(-10 * time.Minute)
	never := cand("m3", RoleMember, 8, 0)
	never.Machine.LastSeen = time.Time{}
	ungauged := cand("m4", RoleMember, 8, 0)
	ungauged.Gauge = nil
	wrongArch := cand("m5", RoleMember, 8, 0)
	wrongArch.Machine.Arch = "amd64"

	d, err := Choose([]Candidate{small, gone, never, ungauged, wrongArch},
		Demand{App: "shop", Env: "big", MemoryBytes: 4 << 30, Arch: "arm64"}, Preference{}, testNow)
	if !errors.Is(err, ErrNoMachine) {
		t.Fatalf("err = %v, want ErrNoMachine", err)
	}
	if d.Machine != "" {
		t.Fatalf("a refusal chose %q", d.Machine)
	}
	if len(d.Considered) != 5 {
		t.Fatalf("considered %d machines, want 5", len(d.Considered))
	}
	msg := err.Error()
	for _, want := range []string{
		"shop/big",
		"4.0 GiB asked",
		"last seen 10m0s ago",
		"never been heard from",
		"never said how big it is",
		"amd64 and this environment",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not mention %q", msg, want)
		}
	}

	for _, c := range d.Considered {
		if c.Fits || c.Reason == "" {
			t.Errorf("%s: fits=%v reason=%q, want a refused machine with a reason", c.Machine, c.Fits, c.Reason)
		}
	}
}

func TestChooseRefusesAPinItCannotHonour(t *testing.T) {
	fleet := []Candidate{cand("hub", RoleHub, 8, 0), cand("m1", RoleMember, 1, 0)}

	_, err := Choose(fleet, Demand{App: "shop", Env: "big", MemoryBytes: 4 << 30},
		Preference{Pin: "m1"}, testNow)
	if !errors.Is(err, ErrNoMachine) {
		t.Fatalf("err = %v, want ErrNoMachine", err)
	}
	if !strings.Contains(err.Error(), "--on m1") || !strings.Contains(err.Error(), "4.0 GiB asked") {
		t.Errorf("refusal = %q, want the flag and the numbers", err)
	}

	_, err = Choose(fleet, Demand{App: "shop", Env: "feat-x"}, Preference{Pin: "nx9"}, testNow)
	if !errors.Is(err, ErrNoMachine) {
		t.Fatalf("err = %v, want ErrNoMachine", err)
	}
	if !strings.Contains(err.Error(), "no such machine") || !strings.Contains(err.Error(), "hub, m1") {
		t.Errorf("refusal = %q, want the fleet listed", err)
	}
}

func TestChooseWithNothingToChooseFrom(t *testing.T) {
	_, err := Choose(nil, Demand{App: "shop", Env: "feat-x"}, Preference{}, testNow)
	if !errors.Is(err, ErrNoMachine) || !strings.Contains(err.Error(), "no machine in the fleet") {
		t.Fatalf("err = %v, want a refusal naming the empty fleet", err)
	}
	_, err = Choose([]Candidate{cand("m1", RoleMember, 8, 0)},
		Demand{App: "shop", Env: "feat-x"}, Preference{Placement: config.PlacementHub}, testNow)
	if !errors.Is(err, ErrNoMachine) || !strings.Contains(err.Error(), "no hub is in the fleet") {
		t.Fatalf("err = %v, want a refusal naming the missing hub", err)
	}
}

func TestChooseAndTheEnvironmentThatAskedForNothing(t *testing.T) {
	full := cand("m1", RoleMember, 1, 0)
	full.Committed = 8 << 30
	if _, err := Choose([]Candidate{full}, Demand{App: "shop", Env: "feat-x"}, Preference{}, testNow); err == nil {
		t.Fatal("a machine with nothing free took an environment")
	}
	d := Decide(t, []Candidate{cand("m1", RoleMember, 1, 0)}, Demand{App: "shop", Env: "feat-x"}, Preference{})
	if d.Machine != "m1" {
		t.Fatalf("chose %q, want m1: an app with no resources: fits where there is room", d.Machine)
	}
}

func Decide(t *testing.T, candidates []Candidate, d Demand, pref Preference) Decision {
	t.Helper()
	dec, err := Choose(candidates, d, pref, testNow)
	if err != nil {
		t.Fatalf("Choose: %v", err)
	}
	return dec
}
