package env

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/fleet"
)

type placer struct {
	dec        fleet.Decision
	err        error
	candidates []fleet.Candidate
	demand     fleet.Demand
	pref       fleet.Preference
	calls      int
}

func (p *placer) choose(c []fleet.Candidate, d fleet.Demand, pref fleet.Preference, _ time.Time) (fleet.Decision, error) {
	p.calls++
	p.candidates, p.demand, p.pref = c, d, pref
	if p.err != nil {
		return p.dec, p.err
	}
	if p.dec.Machine == "" {
		p.dec.Machine = "hub"
	}
	return p.dec, nil
}

func (h *harness) onHub(machines ...fleet.Machine) (*fakeFleet, *placer) {
	h.t.Helper()
	f, _ := h.onFleet("hub", machines...)
	h.wire(func(w *FleetWiring) { w.Role = fleet.RoleHub })
	p := &placer{}
	h.m.Place = p.choose
	return f, p
}

func TestPlacementOnAMachineOfOneIsAlwaysItself(t *testing.T) {
	h := newHarness(t)
	dec, err := h.m.PlaceEnv(context.Background(), CreateRequest{App: "shop", Name: "feat-x"}, h.cfg)
	if err != nil {
		t.Fatalf("PlaceEnv: %v", err)
	}
	if dec.Machine != "" {
		t.Fatalf("machine = %q, want this machine's (which has no fleet name)", dec.Machine)
	}

	_, err = h.m.PlaceEnv(context.Background(), CreateRequest{App: "shop", Name: "feat-x", On: "nx2"}, h.cfg)
	if err == nil || !strings.Contains(err.Error(), "not part of a fleet") {
		t.Fatalf("error = %v, want one saying this machine is not part of a fleet", err)
	}
}

func TestPlacementOnAFleetOfOneNeverAsksPlacement(t *testing.T) {
	h := newHarness(t)
	_, p := h.onHub(hub("hub", "amd64", 0, gauge(2048, 256)))

	dec, err := h.m.PlaceEnv(context.Background(), CreateRequest{App: "shop", Name: "feat-x"}, h.cfg)
	if err != nil {
		t.Fatalf("PlaceEnv: %v", err)
	}
	if dec.Machine != "hub" || p.calls != 0 {
		t.Fatalf("machine = %q after %d calls, want hub with no call at all", dec.Machine, p.calls)
	}
}

func TestPlacementRanksTheFleetAndCarriesTheDemand(t *testing.T) {
	h := newHarness(t)
	_, p := h.onHub(
		hub("hub", "amd64", 2, gauge(1024, 256)),
		member("nx2", "arm64", 0, gauge(4096, 256)),
	)
	p.dec = fleet.Decision{Machine: "nx2", Why: "nx2: 3.7 GiB free, 0 envs"}

	h.cfg.Services = []config.Service{{
		Name: "web", Run: "python app.py", ReplicaCount: 2,
		Resources: &config.Resources{Memory: 256 << 20, CPU: 0.5},
	}}

	dec, err := h.m.PlaceEnv(context.Background(), CreateRequest{App: "shop", Name: "feat-x"}, h.cfg)
	if err != nil {
		t.Fatalf("PlaceEnv: %v", err)
	}
	if dec.Machine != "nx2" {
		t.Fatalf("machine = %q, want nx2", dec.Machine)
	}
	want := int64(2*(256<<20)) + DepMemory("postgres:16-alpine") + DepMemory("redis:7-alpine")
	if p.demand.MemoryBytes != want {
		t.Fatalf("demand = %d bytes, want %d (two replicas of 256 MiB plus the dependencies)", p.demand.MemoryBytes, want)
	}
	if p.demand.CPU != 1 {
		t.Fatalf("demand cpu = %v, want 1 (0.5 twice)", p.demand.CPU)
	}
	if len(p.candidates) != 2 {
		t.Fatalf("candidates = %d, want 2", len(p.candidates))
	}
}

func TestPlacementCountsTheEnvironmentsTheDirectoryKnows(t *testing.T) {
	h := newHarness(t)
	f, p := h.onHub(
		hub("hub", "amd64", 0, gauge(1024, 256)),
		member("nx2", "arm64", 0, gauge(1024, 256)),
	)
	for _, e := range []fleet.DirectoryEntry{
		{App: "shop", Env: "a", Machine: "hub"},
		{App: "shop", Env: "b", Machine: "hub"},
		{App: "shop", Env: "c", Machine: "nx2"},
	} {
		if err := f.PutDirectoryEntry(context.Background(), e); err != nil {
			t.Fatalf("seed the directory: %v", err)
		}
	}
	if _, err := h.m.PlaceEnv(context.Background(), CreateRequest{App: "shop", Name: "feat-x"}, h.cfg); err != nil {
		t.Fatalf("PlaceEnv: %v", err)
	}
	counts := map[string]int{}
	for _, c := range p.candidates {
		counts[c.Machine.Name] = c.Machine.Envs
	}
	if counts["hub"] != 2 || counts["nx2"] != 1 {
		t.Fatalf("environment counts = %v, want hub 2 and nx2 1", counts)
	}
}

func TestPinFromTheFlagAndFromTheFile(t *testing.T) {
	h := newHarness(t)
	_, p := h.onHub(
		hub("hub", "amd64", 0, gauge(1024, 256)),
		member("nx2", "arm64", 0, gauge(1024, 256)),
	)
	if _, err := h.m.PlaceEnv(context.Background(), CreateRequest{App: "shop", Name: "feat-x", On: "nx2"}, h.cfg); err != nil {
		t.Fatalf("PlaceEnv --on nx2: %v", err)
	}
	if p.pref.Pin != "nx2" || p.pref.PinnedByFile {
		t.Fatalf("preference = %+v, want nx2 pinned by the flag", p.pref)
	}

	h.cfg.Envs = map[string]config.EnvOverride{"feat-y": {Machine: "nx2"}}
	if _, err := h.m.PlaceEnv(context.Background(), CreateRequest{App: "shop", Name: "feat-y"}, h.cfg); err != nil {
		t.Fatalf("PlaceEnv from the file: %v", err)
	}
	if p.pref.Pin != "nx2" || !p.pref.PinnedByFile {
		t.Fatalf("preference = %+v, want nx2 pinned by the file", p.pref)
	}
}

func TestTheFlagMayNotDisagreeWithTheFile(t *testing.T) {
	h := newHarness(t)
	h.onHub(hub("hub", "amd64", 0, gauge(1024, 256)), member("nx2", "arm64", 0, gauge(1024, 256)))
	h.cfg.Envs = map[string]config.EnvOverride{"production": {Machine: "nx2"}}

	_, err := h.m.PlaceEnv(context.Background(), CreateRequest{App: "shop", Name: "production", On: "hub"}, h.cfg)
	if err == nil || !strings.Contains(err.Error(), "envs.production.machine") {
		t.Fatalf("error = %v, want one naming the key that pins it", err)
	}

	if _, err := h.m.PlaceEnv(context.Background(), CreateRequest{App: "shop", Name: "production", On: "nx2"}, h.cfg); err != nil {
		t.Fatalf("--on naming the machine the file names: %v", err)
	}
}

func TestPlacementHubKeepsAnAppOnTheHub(t *testing.T) {
	h := newHarness(t)
	_, p := h.onHub(
		hub("hub-1", "amd64", 0, gauge(1024, 256)),
		member("nx2", "arm64", 0, gauge(4096, 256)),
	)
	h.wire(func(w *FleetWiring) { w.Machine = "hub-1" })
	h.cfg.Placement = config.PlacementHub

	if _, err := h.m.PlaceEnv(context.Background(), CreateRequest{App: "shop", Name: "feat-x"}, h.cfg); err != nil {
		t.Fatalf("PlaceEnv: %v", err)
	}
	if p.pref.Pin != "hub-1" || !p.pref.PinnedByFile {
		t.Fatalf("preference = %+v, want the hub's own name, pinned by the file", p.pref)
	}
	if p.pref.Placement != config.PlacementHub {
		t.Fatalf("placement = %q, want hub", p.pref.Placement)
	}
}

func TestOnHubIsResolvedToTheHubsName(t *testing.T) {
	h := newHarness(t)
	_, p := h.onHub(
		hub("hub-1", "amd64", 0, gauge(1024, 256)),
		member("nx2", "arm64", 0, gauge(1024, 256)),
	)
	h.wire(func(w *FleetWiring) { w.Machine = "hub-1" })
	if _, err := h.m.PlaceEnv(context.Background(), CreateRequest{App: "shop", Name: "feat-x", On: "hub"}, h.cfg); err != nil {
		t.Fatalf("PlaceEnv --on hub: %v", err)
	}
	if p.pref.Pin != "hub-1" {
		t.Fatalf("pin = %q, want the hub's own name: placement names machines, not roles", p.pref.Pin)
	}
}

func TestOnHubAgreesWithAFileThatPinsTheHubByName(t *testing.T) {
	h := newHarness(t)
	_, p := h.onHub(
		hub("hub-1", "amd64", 0, gauge(1024, 256)),
		member("nx2", "arm64", 0, gauge(1024, 256)),
	)
	h.wire(func(w *FleetWiring) { w.Machine = "hub-1" })
	h.cfg.Envs = map[string]config.EnvOverride{"production": {Machine: "hub-1"}}

	if _, err := h.m.PlaceEnv(context.Background(),
		CreateRequest{App: "shop", Name: "production", On: "hub"}, h.cfg); err != nil {
		t.Fatalf("--on hub against a file that pins the hub by name: %v", err)
	}
	if p.pref.Pin != "hub-1" {
		t.Fatalf("pin = %q, want the hub's own name", p.pref.Pin)
	}

	h.cfg.Envs = map[string]config.EnvOverride{"production": {Machine: "hub"}}
	if _, err := h.m.PlaceEnv(context.Background(),
		CreateRequest{App: "shop", Name: "production", On: "hub-1"}, h.cfg); err != nil {
		t.Fatalf("--on hub-1 against a file that pins hub: %v", err)
	}
}

func TestAPinNamingAMachineTheFleetDoesNotHaveIsRefusedWithTheList(t *testing.T) {
	h := newHarness(t)
	h.onHub(hub("hub", "amd64", 0, gauge(1024, 256)), member("nx2", "arm64", 0, gauge(1024, 256)))

	_, err := h.m.PlaceEnv(context.Background(), CreateRequest{App: "shop", Name: "feat-x", On: "nx9"}, h.cfg)
	if err == nil || !strings.Contains(err.Error(), "nx9") || !strings.Contains(err.Error(), "hub, nx2") {
		t.Fatalf("error = %v, want one naming nx9 and listing the machines there are", err)
	}
}

func TestPlacementReportsWhyNothingCouldTakeIt(t *testing.T) {
	h := newHarness(t)
	_, p := h.onHub(hub("hub", "amd64", 0, gauge(512, 256)), member("nx2", "arm64", 0, gauge(512, 256)))
	p.err = fleet.ErrNoMachine
	p.dec = fleet.Decision{Considered: []fleet.Consideration{{Machine: "nx2", FreeBytes: 1 << 20, Fits: false, Reason: "1 MiB free, 1 GiB asked"}}}

	dec, err := h.m.PlaceEnv(context.Background(), CreateRequest{App: "shop", Name: "feat-x"}, h.cfg)
	if !errors.Is(err, fleet.ErrNoMachine) {
		t.Fatalf("error = %v, want fleet.ErrNoMachine", err)
	}
	if !strings.Contains(err.Error(), `place env "feat-x"`) {
		t.Fatalf("error = %v, want the environment named", err)
	}
	if len(dec.Considered) != 1 {
		t.Fatalf("the refusal lost the numbers it was refused on: %+v", dec)
	}
}

func TestCreateRefusesAnEnvironmentMeantForAnotherMachine(t *testing.T) {
	h := newHarness(t)
	h.onFleet("nx2")

	_, err := h.m.Create(context.Background(), CreateRequest{App: "shop", Name: "feat-x", From: "main", On: "nx3"}, &h.out)
	if err == nil || !strings.Contains(err.Error(), "nx3") || !strings.Contains(err.Error(), "nx2") {
		t.Fatalf("error = %v, want one naming both the machine asked for and this one", err)
	}
	if _, err := h.store.Env(context.Background(), "shop", "feat-x"); err == nil {
		t.Fatal("an environment meant for another machine was created here anyway")
	}

	if _, err := h.m.Create(context.Background(), CreateRequest{App: "shop", Name: "feat-x", From: "main", On: "nx2"}, &h.out); err != nil {
		t.Fatalf("create on the machine it names: %v\n%s", err, h.out.String())
	}
}

func TestDemandOfAConfigThatSaysNothingIsZeroButForItsDependencies(t *testing.T) {
	d := DemandOf("shop", "feat-x", &config.App{Name: "shop"})
	if d.MemoryBytes != 0 || d.CPU != 0 {
		t.Fatalf("demand = %+v, want zero for a configuration that declares nothing", d)
	}
	one := DemandOf("shop", "feat-x", &config.App{
		Name: "shop",
		Deps: []config.Dep{{Name: "db", Image: "postgres:16-alpine"}},
	})
	if one.MemoryBytes != DepMemory("postgres") {
		t.Fatalf("demand = %d, want one Postgres (%d)", one.MemoryBytes, DepMemory("postgres"))
	}
	if DepMemory("something-nobody-measured") != DefaultDepMemory {
		t.Fatal("an unmeasured image is not the default")
	}
}
