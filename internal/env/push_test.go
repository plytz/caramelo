package env

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/state"
)

type dirtyRepo struct {
	*fakeRepo
	dirty   bool
	changed []string
	err     error
}

func (r *dirtyRepo) Dirty(context.Context, string) (bool, []string, error) {
	return r.dirty, r.changed, r.err
}

func TestWorktreeStatusIsWhatTheMachineHoldingTheCheckoutSees(t *testing.T) {
	h := newHarness(t)
	h.onFleet("nx2")
	repo := &dirtyRepo{fakeRepo: h.repo}
	h.m.Git = repo
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})

	st, err := h.m.WorktreeStatusOf(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("WorktreeStatusOf: %v", err)
	}
	if !st.Exists || st.Dirty || st.Machine != "nx2" || st.Worktree == "" {
		t.Fatalf("status = %+v, want a clean checkout on nx2", st)
	}

	repo.dirty, repo.changed = true, []string{" M app.py"}
	st, err = h.m.WorktreeStatusOf(context.Background(), "shop", "feat-x")
	if err != nil || !st.Dirty || len(st.Changed) != 1 {
		t.Fatalf("status = %+v, err %v; want the change reported", st, err)
	}

	st, err = h.m.WorktreeStatusOf(context.Background(), "shop", "feat-nothing")
	if err != nil || st.Exists {
		t.Fatalf("status = %+v, err %v; want it to say the environment is not here", st, err)
	}
}

func hubForPush(t *testing.T) (*harness, *fakeFleet) {
	t.Helper()
	h := newHarness(t)
	f, _ := h.onFleet("hub")
	h.wire(func(w *FleetWiring) { w.Role = fleet.RoleHub })
	if err := f.PutDirectoryEntry(context.Background(), fleet.DirectoryEntry{
		App: "shop", Env: "feat-x", Machine: "nx2",
	}); err != nil {
		t.Fatalf("seed the directory: %v", err)
	}
	return h, f
}

func TestAPushIntoADirtyCheckoutOnAMemberIsRefused(t *testing.T) {
	h, _ := hubForPush(t)
	h.wire(func(w *FleetWiring) {
		w.RemoteWorktree = func(_ context.Context, machine, app, name string) (*WorktreeStatus, error) {
			return &WorktreeStatus{
				App: app, Env: name, Machine: machine, Worktree: "/mnt/caramelo/apps/shop/envs/feat-x/src",
				Exists: true, Dirty: true, Changed: []string{" M app.py"},
			}, nil
		}
	})
	err := h.m.CheckPush(context.Background(), "shop", []string{"feat-x"})
	if err == nil {
		t.Fatal("a push into a dirty checkout on a member was allowed")
	}
	for _, want := range []string{"nx2", "uncommitted changes", "app.py", "env exec feat-x"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message = %q, want %q in it", err, want)
		}
	}
}

func TestAPushIntoACleanCheckoutOnAMemberLands(t *testing.T) {
	h, _ := hubForPush(t)
	asked := 0
	h.wire(func(w *FleetWiring) {
		w.RemoteWorktree = func(_ context.Context, machine, app, name string) (*WorktreeStatus, error) {
			asked++
			return &WorktreeStatus{App: app, Env: name, Machine: machine, Exists: true}, nil
		}
	})
	if err := h.m.CheckPush(context.Background(), "shop", []string{"feat-x", "main"}); err != nil {
		t.Fatalf("CheckPush: %v", err)
	}
	if asked != 1 {
		t.Fatalf("asked %d times, want once: only the branch a member holds is somebody else's business", asked)
	}
}

func TestAMemberThatCannotBeAskedDoesNotBlockThePush(t *testing.T) {
	h, _ := hubForPush(t)
	h.wire(func(w *FleetWiring) {
		w.RemoteWorktree = func(context.Context, string, string, string) (*WorktreeStatus, error) {
			return nil, errors.New("machine \"nx2\" is unreachable")
		}
	})
	if err := h.m.CheckPush(context.Background(), "shop", []string{"feat-x"}); err != nil {
		t.Fatalf("CheckPush with an unreachable member = %v, want the push allowed", err)
	}

	h.wire(func(w *FleetWiring) { w.RemoteWorktree = nil })
	if err := h.m.CheckPush(context.Background(), "shop", []string{"feat-x"}); err != nil {
		t.Fatalf("CheckPush with no asker = %v, want the push allowed", err)
	}
}

func TestAMachineOfOneChecksNothingAtAll(t *testing.T) {
	h := newHarness(t)
	called := false
	h.wire(func(w *FleetWiring) {
		w.RemoteWorktree = func(context.Context, string, string, string) (*WorktreeStatus, error) {
			called = true
			return nil, nil
		}
	})
	if err := h.m.CheckPush(context.Background(), "shop", []string{"feat-x"}); err != nil {
		t.Fatalf("CheckPush on a machine of one: %v", err)
	}
	if called {
		t.Fatal("a machine of one asked another machine about a worktree")
	}
}

func TestABranchOnThisMachineIsLeftToGit(t *testing.T) {
	h, f := hubForPush(t)
	if err := f.PutDirectoryEntry(context.Background(), fleet.DirectoryEntry{
		App: "shop", Env: "feat-here", Machine: "hub",
	}); err != nil {
		t.Fatalf("seed the directory: %v", err)
	}
	h.wire(func(w *FleetWiring) {
		w.RemoteWorktree = func(context.Context, string, string, string) (*WorktreeStatus, error) {
			t.Fatal("the hub asked another machine about its own environment")
			return nil, nil
		}
	})
	if err := h.m.CheckPush(context.Background(), "shop", []string{"feat-here"}); err != nil {
		t.Fatalf("CheckPush: %v", err)
	}
}

type mirrorRepo struct {
	*fakeRepo
	fetched  []string
	updated  []string
	fetchErr error
}

func (r *mirrorRepo) EnsureMirror(context.Context, string, string) (bool, error) { return false, nil }

func (r *mirrorRepo) FetchMirror(context.Context, string) error { return nil }

func (r *mirrorRepo) FetchBranch(_ context.Context, _, branch string) error {
	r.fetched = append(r.fetched, branch)
	return r.fetchErr
}

func (r *mirrorRepo) UpdateWorktree(_ context.Context, _, path, branch string) error {
	r.updated = append(r.updated, path+" "+branch)
	return nil
}

func (h *harness) envRecord(t *testing.T, name string) *state.EnvRecord {
	t.Helper()
	rec, err := h.store.Env(context.Background(), "shop", name)
	if err != nil {
		t.Fatalf("read env %q: %v", name, err)
	}
	return rec
}

func (h *harness) pushEvents(t *testing.T) []state.EnvEvent {
	t.Helper()
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	var out []state.EnvEvent
	for _, e := range h.store.events {
		if e.Action == "push" {
			out = append(out, e)
		}
	}
	return out
}

func TestRecordPushWritesThePushFactsAndOneEvent(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})
	before := h.envRecord(t, "feat-x")

	at := time.Date(2026, 9, 17, 9, 30, 0, 0, time.UTC)
	h.m.Now = func() time.Time { return at }

	e, err := h.m.RecordPush(context.Background(), "shop", "feat-x", featureCommit, "blob-store", "alex@laptop")
	if err != nil {
		t.Fatalf("RecordPush: %v", err)
	}
	if e == nil || e.Commit != featureCommit || e.SourceBranch != "blob-store" ||
		e.PushedBy != "alex@laptop" || !e.PushedAt.Equal(at) {
		t.Fatalf("RecordPush returned %+v", e)
	}

	rec := h.envRecord(t, "feat-x")
	if rec.Commit != featureCommit {
		t.Errorf("commit = %q, want the commit that landed", rec.Commit)
	}
	if rec.SourceBranch != "blob-store" || rec.PushedBy != "alex@laptop" || !rec.PushedAt.Equal(at) {
		t.Errorf("push facts = %q, %q, %v", rec.SourceBranch, rec.PushedBy, rec.PushedAt)
	}
	if rec.Branch != before.Branch || rec.CreatedBy != before.CreatedBy || !rec.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("the push overwrote a creation fact: %+v, was %+v", rec, before)
	}

	events := h.pushEvents(t)
	if len(events) != 1 {
		t.Fatalf("%d push events, want 1: %+v", len(events), events)
	}
	ev := events[0]
	if ev.EnvID != rec.ID || ev.Status != "changed" {
		t.Errorf("event = %+v, want one on env %d", ev, rec.ID)
	}
	if ev.Identity != "alex@laptop" {
		t.Errorf("identity = %q; the pusher comes from the session, not from the context", ev.Identity)
	}
	if want := "checkout updated to " + short(featureCommit) + " from blob-store"; ev.Detail != want {
		t.Errorf("detail = %q, want %q", ev.Detail, want)
	}
}

func TestRecordPushLeavesTheSourceEmptyWhenTheClientSentNone(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})

	if _, err := h.m.RecordPush(context.Background(), "shop", "feat-x", featureCommit, "", "alex@laptop"); err != nil {
		t.Fatalf("RecordPush: %v", err)
	}
	if got := h.envRecord(t, "feat-x").SourceBranch; got != "" {
		t.Errorf("source branch = %q; a push that named no branch must leave it blank", got)
	}
	if want := "checkout updated to " + short(featureCommit); h.pushEvents(t)[0].Detail != want {
		t.Errorf("detail = %q, want %q", h.pushEvents(t)[0].Detail, want)
	}
}

func TestAPushToABranchNoEnvHasCheckedOutStaysOnTheFeed(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})

	e, err := h.m.RecordPush(context.Background(), "shop", "main", mainCommit, "main", "alex@laptop")
	if err != nil || e != nil {
		t.Fatalf("RecordPush into a branch with no env = %+v, %v; want nothing recorded on a row", e, err)
	}
	if rec := h.envRecord(t, "feat-x"); rec.PushedBy != "" || rec.PushedAt != (time.Time{}) {
		t.Errorf("the push was written onto env %s, which does not own that branch: %+v", rec.Name, rec)
	}
	events := h.pushEvents(t)
	if len(events) != 1 {
		t.Fatalf("%d push events, want the push on the feed anyway: %+v", len(events), events)
	}
	ev := events[0]
	if ev.EnvID != 0 || ev.App != "shop" || ev.Env != "main" || ev.Status != "ok" {
		t.Errorf("event = %+v, want an app-scoped push on shop/main", ev)
	}
	if ev.Identity != "alex@laptop" {
		t.Errorf("identity = %q", ev.Identity)
	}
}

type refusingStore struct {
	*fakeStore
	err error
}

func (s *refusingStore) RecordEnvPush(context.Context, int64, string, string, string, time.Time) error {
	return s.err
}

func TestRecordPushReturnsWhatTheStoreSaid(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})
	h.m.Store = &refusingStore{fakeStore: h.store, err: errors.New("the disk is full")}

	_, err := h.m.RecordPush(context.Background(), "shop", "feat-x", featureCommit, "", "alex@laptop")
	if err == nil || !strings.Contains(err.Error(), "record the push into shop/feat-x") ||
		!strings.Contains(err.Error(), "the disk is full") {
		t.Fatalf("RecordPush over a store that refuses = %v, want the failure wrapped", err)
	}
}

func TestAPushDoesNotDisturbWhatTheReleaseRecorded(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "production", From: "main", Release: true})
	h.store.mu.Lock()
	h.store.envs[0].ReleaseID, h.store.envs[0].DeployID = 7, 9
	h.store.mu.Unlock()

	if _, err := h.m.RecordPush(context.Background(), "shop", "production", featureCommit, "blob-store", "ci"); err != nil {
		t.Fatalf("RecordPush: %v", err)
	}
	rec := h.envRecord(t, "production")
	if rec.ReleaseID != 7 || rec.DeployID != 9 {
		t.Errorf("the push cleared what the release recorded: release %d deploy %d", rec.ReleaseID, rec.DeployID)
	}
	if rec.Mode != string(ModeRelease) || rec.Commit != featureCommit {
		t.Errorf("env = %+v, want a release env whose checkout moved", rec)
	}
}

func TestSyncBranchRecordsWhatTheMemberFetched(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-y", From: "main"})
	repo := &mirrorRepo{fakeRepo: h.repo}
	h.m.Git = repo

	at := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	h.m.Now = func() time.Time { return at }
	h.repo.mu.Lock()
	h.repo.branches["feat-y"] = featureCommit
	h.repo.mu.Unlock()

	ctx := WithIdentity(context.Background(), "hub")
	e, err := h.m.SyncBranch(ctx, "shop", "feat-y")
	if err != nil {
		t.Fatalf("SyncBranch: %v", err)
	}
	if len(repo.fetched) != 1 || repo.fetched[0] != "feat-y" {
		t.Errorf("fetched %v, want the env's branch", repo.fetched)
	}
	if len(repo.updated) != 1 || !strings.HasSuffix(repo.updated[0], " feat-y") {
		t.Errorf("updated %v, want the worktree moved onto the branch", repo.updated)
	}
	if e.Commit != featureCommit {
		t.Errorf("commit = %q, want %q", e.Commit, featureCommit)
	}

	rec := h.envRecord(t, "feat-y")
	if rec.Commit != featureCommit || rec.PushedBy != "hub" || !rec.PushedAt.Equal(at) {
		t.Errorf("the member recorded %+v", rec)
	}
	if rec.SourceBranch != "" {
		t.Errorf("source branch = %q; a member is never told where the push came from", rec.SourceBranch)
	}

	events := h.pushEvents(t)
	if len(events) != 1 {
		t.Fatalf("%d push events, want 1: %+v", len(events), events)
	}
	if want := "checkout updated to " + short(featureCommit); events[0].Detail != want {
		t.Errorf("detail = %q, want the same words the hub uses (%q)", events[0].Detail, want)
	}
	if events[0].Status != "changed" {
		t.Errorf("status = %q, want changed", events[0].Status)
	}
}
