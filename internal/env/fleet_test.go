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

func TestCreateAnnouncesTheEnvironmentToTheDirectory(t *testing.T) {
	h := newHarness(t)
	f, rec := h.onFleet("nx2")

	ctx := WithIdentity(context.Background(), "agent-a")
	e, err := h.m.Create(ctx, CreateRequest{App: "shop", Name: "feat-x", From: "main"}, &h.out)
	if err != nil {
		t.Fatalf("Create: %v\n%s", err, h.out.String())
	}
	if e.Machine != "nx2" || e.Owner != "agent-a" {
		t.Fatalf("env machine/owner = %q/%q, want nx2/agent-a", e.Machine, e.Owner)
	}
	if got, want := f.names(), []string{"nx2:shop/feat-x"}; !equalStrings(got, want) {
		t.Fatalf("directory = %v, want %v", got, want)
	}
	a, ok := rec.last()
	if !ok {
		t.Fatal("nothing was announced")
	}
	if a.Full {
		t.Error("a create announced the whole machine; it should announce the one environment")
	}
	entry := f.dir[[2]string{"shop", "feat-x"}]

	if entry.Owner != "agent-a" || entry.Mode != string(ModeDev) || entry.Address != e.VPNIP {
		t.Fatalf("directory row = %+v, want owner agent-a, mode dev and address %q", entry, e.VPNIP)
	}
}

func TestDestroyAnnouncesWhatIsLeft(t *testing.T) {
	h := newHarness(t)
	f, rec := h.onFleet("nx2")

	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-y", From: "main"})
	if got := len(f.names()); got != 2 {
		t.Fatalf("directory has %d rows, want 2", got)
	}
	if err := h.m.Destroy(context.Background(), DestroyRequest{App: "shop", Name: "feat-x"}, &h.out); err != nil {
		t.Fatalf("Destroy: %v\n%s", err, h.out.String())
	}
	if got, want := f.names(), []string{"nx2:shop/feat-y"}; !equalStrings(got, want) {
		t.Fatalf("directory = %v, want %v", got, want)
	}
	a, _ := rec.last()
	if !a.Full {
		t.Error("the announcement after a destroy is not full; a directory that missed it would keep the row")
	}
}

func TestAnnouncingIsBestEffortAndNeverFailsTheCommand(t *testing.T) {
	h := newHarness(t)
	_, rec := h.onFleet("nx2")
	rec.err = errors.New("the hub is restarting")

	if _, err := h.m.Create(context.Background(), CreateRequest{App: "shop", Name: "feat-x", From: "main"}, &h.out); err != nil {
		t.Fatalf("a create whose announcement failed was refused: %v", err)
	}
	if rec.count() == 0 {
		t.Fatal("nothing was announced at all")
	}
	if err := h.m.Destroy(context.Background(), DestroyRequest{App: "shop", Name: "feat-x"}, &h.out); err != nil {
		t.Fatalf("a destroy whose announcement failed was refused: %v", err)
	}
}

func TestAMachineOfOneAnnouncesNothingAndKeepsNoDirectory(t *testing.T) {
	h := newHarness(t)

	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})
	if e.Machine != "" || e.Owner != "" || e.Via != "" {
		t.Fatalf("env on a machine of one = %+v, want no machine, owner or via", e)
	}
	entries, err := h.m.Directory(context.Background(), "")
	if err != nil {
		t.Fatalf("Directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a machine of one has a directory of %d rows", len(entries))
	}
}

func TestAnnounceAllIsTheWholeTruthAboutThisMachine(t *testing.T) {
	h := newHarness(t)
	f, _ := h.onFleet("nx2")

	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})

	if err := f.PutDirectoryEntry(context.Background(), fleet.DirectoryEntry{
		App: "shop", Env: "gone", Machine: "nx2",
	}); err != nil {
		t.Fatalf("seed the directory: %v", err)
	}
	if err := h.m.AnnounceAll(context.Background()); err != nil {
		t.Fatalf("AnnounceAll: %v", err)
	}
	if got, want := f.names(), []string{"nx2:shop/feat-x"}; !equalStrings(got, want) {
		t.Fatalf("directory after a full announcement = %v, want %v", got, want)
	}
}

func TestApplyAnnouncementLeavesOtherMachinesAlone(t *testing.T) {
	f := newFleet()
	ctx := context.Background()
	now := time.Unix(1, 0)

	if _, _, err := ApplyAnnouncement(ctx, f, fleet.Announcement{
		Machine: "m1", Full: true,
		Envs: []fleet.DirectoryEntry{{App: "shop", Env: "a"}, {App: "shop", Env: "b"}},
	}, now); err != nil {
		t.Fatalf("apply m1: %v", err)
	}
	accepted, removed, err := ApplyAnnouncement(ctx, f, fleet.Announcement{
		Machine: "m2", Full: true,
		Envs: []fleet.DirectoryEntry{{App: "shop", Env: "c"}},
	}, now)
	if err != nil {
		t.Fatalf("apply m2: %v", err)
	}
	if accepted != 1 || removed != 0 {
		t.Fatalf("accepted/removed = %d/%d, want 1/0", accepted, removed)
	}
	if got, want := f.names(), []string{"m1:shop/a", "m1:shop/b", "m2:shop/c"}; !equalStrings(got, want) {
		t.Fatalf("directory = %v, want %v", got, want)
	}

	_, removed, err = ApplyAnnouncement(ctx, f, fleet.Announcement{
		Machine: "m1", Full: true,
		Envs: []fleet.DirectoryEntry{{App: "shop", Env: "a"}},
	}, now)
	if err != nil {
		t.Fatalf("apply m1 again: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if got, want := f.names(), []string{"m1:shop/a", "m2:shop/c"}; !equalStrings(got, want) {
		t.Fatalf("directory = %v, want %v", got, want)
	}
}

func TestApplyAnnouncementIsIdempotentAndPartialRemovesNothing(t *testing.T) {
	f := newFleet()
	ctx := context.Background()
	full := fleet.Announcement{
		Machine: "m1", Full: true,
		Envs: []fleet.DirectoryEntry{{App: "shop", Env: "a"}, {App: "shop", Env: "b"}},
	}
	for i := 0; i < 3; i++ {
		if _, removed, err := ApplyAnnouncement(ctx, f, full, time.Unix(1, 0)); err != nil || (i > 0 && removed != 0) {
			t.Fatalf("apply %d: removed %d, err %v", i, removed, err)
		}
	}

	if _, removed, err := ApplyAnnouncement(ctx, f, fleet.Announcement{
		Machine: "m1", Envs: []fleet.DirectoryEntry{{App: "shop", Env: "a", Owner: "agent-b"}},
	}, time.Unix(2, 0)); err != nil || removed != 0 {
		t.Fatalf("partial apply: removed %d, err %v", removed, err)
	}
	if got, want := f.names(), []string{"m1:shop/a", "m1:shop/b"}; !equalStrings(got, want) {
		t.Fatalf("directory = %v, want %v", got, want)
	}
	if got := f.dir[[2]string{"shop", "a"}].Owner; got != "agent-b" {
		t.Fatalf("owner = %q, want agent-b: a partial announcement replaces the rows it carries", got)
	}
}

func TestApplyAnnouncementStampsTheMachineThatSentIt(t *testing.T) {
	f := newFleet()

	if _, _, err := ApplyAnnouncement(context.Background(), f, fleet.Announcement{
		Machine: "m1",
		Envs:    []fleet.DirectoryEntry{{App: "shop", Env: "a", Machine: "hub"}},
	}, time.Unix(1, 0)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got, want := f.names(), []string{"m1:shop/a"}; !equalStrings(got, want) {
		t.Fatalf("directory = %v, want %v", got, want)
	}
}

func TestApplyAnnouncementRefusesOneWithNoMachineOrNoEnvironment(t *testing.T) {
	f := newFleet()
	if _, _, err := ApplyAnnouncement(context.Background(), f, fleet.Announcement{}, time.Unix(1, 0)); err == nil {
		t.Fatal("an announcement naming no machine was accepted")
	}
	_, _, err := ApplyAnnouncement(context.Background(), f, fleet.Announcement{
		Machine: "m1", Envs: []fleet.DirectoryEntry{{App: "shop"}},
	}, time.Unix(1, 0))
	if err == nil || !strings.Contains(err.Error(), "names no environment") {
		t.Fatalf("error = %v, want one about an entry naming no environment", err)
	}
}

func TestListAndShowReadTheFleetColumns(t *testing.T) {
	h := newHarness(t)
	f, _ := h.onFleet("nx2")
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})

	id := h.envID("feat-x")
	if err := f.SetEnvFleetRow(context.Background(), id, FleetRow{Owner: "agent-b", Via: config.ViaHub}); err != nil {
		t.Fatalf("write the fleet row: %v", err)
	}
	list, err := h.m.List(context.Background(), "shop")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Owner != "agent-b" || list[0].Via != config.ViaHub || list[0].Machine != "nx2" {
		t.Fatalf("list = %+v, want owner agent-b via hub on nx2", list)
	}
	e, _, _, err := h.m.Show(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if e.Owner != "agent-b" || e.Via != config.ViaHub || e.Machine != "nx2" {
		t.Fatalf("show = %+v, want owner agent-b via hub on nx2", e)
	}
}

func TestViaOfPrefersWhatWasWrittenOverTheFile(t *testing.T) {
	h := newHarness(t)
	f, _ := h.onFleet("nx2")
	h.cfg.Envs = map[string]config.EnvOverride{"feat-x": {Via: config.ViaHub}}
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})

	rec, err := h.store.Env(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("read env: %v", err)
	}

	if got := h.m.ViaOf(context.Background(), rec, h.cfg); got != config.ViaHub {
		t.Fatalf("via = %q, want hub", got)
	}

	if err := f.SetEnvFleetRow(context.Background(), rec.ID, FleetRow{Via: config.ViaMember}); err != nil {
		t.Fatalf("write the fleet row: %v", err)
	}
	if got := h.m.ViaOf(context.Background(), rec, h.cfg); got != config.ViaMember {
		t.Fatalf("via = %q, want member: what was written wins over the file", got)
	}
}

func TestAFleetColumnThatCannotBeReadDoesNotFailAListing(t *testing.T) {
	h := newHarness(t)
	f, _ := h.onFleet("nx2")
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})
	f.failRows = errors.New("the database is locked")

	if _, err := h.m.List(context.Background(), "shop"); err == nil {
		t.Fatal("List swallowed a database error")
	}

	e, _, _, err := h.m.Show(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if e.Owner != "" {
		t.Fatalf("owner = %q, want empty", e.Owner)
	}
}

func TestCreateOnAMemberFetchesBeforeItPlans(t *testing.T) {
	h := newHarness(t)
	const pushed = "9999999999999999999999999999999999999999"
	fetched := 0
	h.wire(func(w *FleetWiring) {
		w.Machine = "m2"
		w.Mirror = func(_ context.Context, app, _ string) error {
			fetched++
			h.repo.mu.Lock()
			h.repo.branches["main"] = pushed
			h.repo.commits[pushed] = true
			h.repo.mu.Unlock()
			return nil
		}
	})

	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", On: "m2", NoDeps: true})
	if fetched != 1 {
		t.Errorf("the hub was fetched from %d time(s), want once before the branch was planned", fetched)
	}
	if e.Commit != pushed {
		t.Errorf("the environment was built at %q, want the commit the hub holds (%q)", e.Commit, pushed)
	}
}

func TestCreateOnAMemberSurvivesAHubItCannotReach(t *testing.T) {
	h := newHarness(t)
	h.wire(func(w *FleetWiring) {
		w.Machine = "m2"
		w.Mirror = func(context.Context, string, string) error {
			return errors.New("the hub is not answering")
		}
	})

	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", On: "m2", NoDeps: true})
	if e.Commit != mainCommit {
		t.Errorf("the environment was built at %q, want what this machine already had (%q)", e.Commit, mainCommit)
	}
	if !strings.Contains(h.out.String(), "the hub is not answering") {
		t.Errorf("the create said nothing about the hub it could not reach:\n%s", h.out.String())
	}
}

func TestCreateOnAMemberRefusesARefItCannotFetch(t *testing.T) {
	h := newHarness(t)
	h.wire(func(w *FleetWiring) {
		w.Machine = "m2"
		w.Mirror = func(context.Context, string, string) error {
			return errors.New("the hub is not answering")
		}
	})

	_, err := h.create(CreateRequest{App: "shop", Name: "feat-x", From: "nope", On: "m2", NoDeps: true})
	if err == nil || !strings.Contains(err.Error(), "the hub is not answering") {
		t.Fatalf("error = %v, want the hub named", err)
	}
}
