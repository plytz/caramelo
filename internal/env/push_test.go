package env

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/fleet"
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
