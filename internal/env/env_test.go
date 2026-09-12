package env

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/state"
)

func TestPortIsTheFirstOfTheBlock(t *testing.T) {
	h := newHarness(t)
	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", NoDeps: true})

	if e.Port() != e.PortBase {
		t.Errorf("Port() = %d, want the block's base %d", e.Port(), e.PortBase)
	}
	if got := e.Vars["PORT"]; got != strconv.Itoa(e.Port()) {
		t.Errorf("PORT = %q, want %d", got, e.Port())
	}

	if depPort(e.PortBase, 0) == e.Port() {
		t.Errorf("dependency 0 got port %d, the app's own", e.Port())
	}
}

func TestIdentityTravelsOnTheContext(t *testing.T) {
	if got := IdentityFrom(context.Background()); got != "" {
		t.Errorf("IdentityFrom on a bare context = %q, want empty", got)
	}
	ctx := WithIdentity(context.Background(), "alex@laptop")
	if got := IdentityFrom(ctx); got != "alex@laptop" {
		t.Errorf("IdentityFrom = %q", got)
	}

	h := newHarness(t)
	e, err := h.m.Create(ctx, CreateRequest{App: "shop", Name: "feat-x", NoDeps: true}, &h.out)
	if err != nil {
		t.Fatalf("Create: %v\n%s", err, h.out.String())
	}
	if e.CreatedBy != "alex@laptop" {
		t.Errorf("CreatedBy = %q, want the caller's identity", e.CreatedBy)
	}
	events, err := h.m.events(ctx, e.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no audit trail")
	}
	for _, ev := range events {
		if ev.Identity != "alex@laptop" {
			t.Errorf("event %+v does not say who did it", ev)
		}
	}
}

type brokenStore struct {
	*fakeStore
	err error
}

func (s *brokenStore) Envs(context.Context, string) ([]state.EnvRecord, error) {
	return nil, s.err
}

func (s *brokenStore) Env(context.Context, string, string) (*state.EnvRecord, error) {
	return nil, s.err
}

func (s *brokenStore) Events(context.Context, int64, int) ([]state.EnvEvent, error) {
	return nil, s.err
}

func TestReadsReportAStoreFailure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.m.Store = &brokenStore{fakeStore: h.store, err: errors.New("database is locked")}

	if _, err := h.m.List(ctx, "shop"); err == nil || !strings.Contains(err.Error(), "list envs") {
		t.Errorf("List = %v, want it to name the failed listing", err)
	}
	if _, _, _, err := h.m.Show(ctx, "shop", "feat-x"); err == nil || !strings.Contains(err.Error(), "read env") {
		t.Errorf("Show = %v, want it to name the failed read", err)
	}
	if _, err := h.m.Export(ctx, "shop", "feat-x", FormatShell); err == nil {
		t.Error("Export answered from a broken store")
	}
	if _, err := h.m.Exec(ctx, ExecRequest{App: "shop", Name: "feat-x", Argv: []string{"true"}}); err == nil {
		t.Error("Exec ran against a broken store")
	}
}

func TestReadsRejectBadNamesBeforeTouchingTheStore(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.m.Store = &brokenStore{fakeStore: h.store, err: errors.New("must not be reached")}

	if _, err := h.m.List(ctx, "../etc"); err == nil || strings.Contains(err.Error(), "must not be reached") {
		t.Errorf("List with a bad app = %v, want a name error", err)
	}
	if _, _, _, err := h.m.Show(ctx, "shop", "../etc"); err == nil || strings.Contains(err.Error(), "must not be reached") {
		t.Errorf("Show with a bad env = %v, want a name error", err)
	}

	if _, err := h.m.List(ctx, ""); err == nil || !strings.Contains(err.Error(), "must not be reached") {
		t.Errorf("List of everything = %v, want it to have reached the store", err)
	}
}

func TestRowsWrittenByANewerBuildAreReportedNotIgnored(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", NoDeps: true})

	h.store.mu.Lock()
	for i := range h.store.envs {
		if h.store.envs[i].ID == e.ID {
			h.store.envs[i].VarsJSON = `{"PORT":`
		}
	}
	h.store.mu.Unlock()

	if _, err := h.m.List(ctx, "shop"); err == nil || !strings.Contains(err.Error(), "decode variables") {
		t.Errorf("List = %v, want it to say the variables could not be decoded", err)
	}
	if _, _, _, err := h.m.Show(ctx, "shop", "feat-x"); err == nil || !strings.Contains(err.Error(), "decode variables") {
		t.Errorf("Show = %v, want the same", err)
	}

	h.store.mu.Lock()
	for i := range h.store.envs {
		if h.store.envs[i].ID == e.ID {
			h.store.envs[i].VarsJSON, h.store.envs[i].ConfigJSON = "", `{"deps":`
		}
	}
	h.store.mu.Unlock()
	if _, _, _, err := h.m.Show(ctx, "shop", "feat-x"); err == nil || !strings.Contains(err.Error(), "decode config") {
		t.Errorf("Show = %v, want it to say the config could not be decoded", err)
	}
}

type unreachableRuntime struct {
	*fakeDriver
	err error
}

func (d *unreachableRuntime) Inspect(context.Context, string) (runtime.ContainerState, error) {
	return runtime.ContainerState{}, d.err
}

func TestShowReportsAFailedInspect(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.m.Driver = &unreachableRuntime{fakeDriver: h.driver, err: errors.New("cannot connect to the Docker daemon")}

	_, _, _, err := h.m.Show(context.Background(), "shop", "feat-x")
	if err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatalf("Show = %v, want it to name the failed inspect", err)
	}
}
