package state

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func sampleApp(name string) App {
	return App{
		Name:      name,
		RepoPath:  "/mnt/caramelo/apps/" + name + "/repo.git",
		CreatedAt: time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC),
	}
}

func sampleEnv(app, name string, portBase int) EnvRecord {
	return EnvRecord{
		App:       app,
		Name:      name,
		Branch:    name,
		Worktree:  "/mnt/caramelo/apps/" + app + "/envs/" + name + "/src",
		PortBase:  portBase,
		PortCount: 16,
		Status:    EnvCreating,
		CreatedBy: "alex@laptop",
		CreatedAt: time.Date(2026, 9, 8, 11, 0, 0, 0, time.UTC),
	}
}

func TestApps(t *testing.T) {
	ctx := context.Background()
	s, path := tempDB(t)

	if got, err := s.Apps(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty store: %v, %v", got, err)
	}
	if _, err := s.App(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	for _, name := range []string{"shop", "blog"} {
		if err := s.AddApp(ctx, sampleApp(name)); err != nil {
			t.Fatalf("AddApp(%s): %v", name, err)
		}
	}
	got, err := s.Apps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "blog" || got[1].Name != "shop" {
		t.Fatalf("apps are not sorted by name: %+v", got)
	}
	if got[1].RepoPath != "/mnt/caramelo/apps/shop/repo.git" || got[1].DefaultBranch != "" {
		t.Errorf("round trip lost fields: %+v", got[1])
	}
	if !got[1].CreatedAt.Equal(sampleApp("shop").CreatedAt) {
		t.Errorf("created_at = %v", got[1].CreatedAt)
	}

	if err := s.AddApp(ctx, sampleApp("shop")); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate app: err = %v, want ErrExists", err)
	}
	if err := s.AddApp(ctx, App{RepoPath: "/x"}); err == nil {
		t.Error("empty name must be rejected")
	}
	if err := s.AddApp(ctx, App{Name: "x"}); err == nil {
		t.Error("empty repo path must be rejected")
	}

	if err := s.SetAppDefaultBranch(ctx, "shop", "main"); err != nil {
		t.Fatal(err)
	}
	app, err := s.App(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if app.DefaultBranch != "main" {
		t.Errorf("default branch = %q, want main", app.DefaultBranch)
	}
	if err := s.SetAppDefaultBranch(ctx, "nope", "main"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown app: err = %v, want ErrNotFound", err)
	}

	s.Close()
	if got, err := open(t, path).App(ctx, "shop"); err != nil || got.DefaultBranch != "main" {
		t.Errorf("after reopen: %+v, %v", got, err)
	}
}

func TestEnvRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, path := tempDB(t)
	if err := s.AddApp(ctx, sampleApp("shop")); err != nil {
		t.Fatal(err)
	}

	if got, err := s.Envs(ctx, "shop"); err != nil || len(got) != 0 {
		t.Fatalf("no envs yet: %v, %v", got, err)
	}
	if _, err := s.Env(ctx, "shop", "feat-x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	rec, err := s.CreateEnv(ctx, sampleEnv("shop", "feat-x", 20000))
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID == 0 {
		t.Fatal("CreateEnv did not fill in the id")
	}
	if rec.Status != EnvCreating {
		t.Errorf("status = %q, want %q", rec.Status, EnvCreating)
	}

	got, err := s.Env(ctx, "shop", "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != rec.ID || got.PortBase != 20000 || got.PortCount != 16 ||
		got.Worktree != rec.Worktree || got.Branch != "feat-x" || got.CreatedBy != "alex@laptop" {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if !got.CreatedAt.Equal(rec.CreatedAt) || got.UpdatedAt.IsZero() {
		t.Errorf("times: created %v updated %v", got.CreatedAt, got.UpdatedAt)
	}

	got.Commit = "0123456789abcdef0123456789abcdef01234567"
	got.Status = EnvReady
	got.ConfigJSON = `{"name":"shop"}`
	got.VarsJSON = `{"PORT":"20000"}`
	if err := s.UpdateEnv(ctx, *got); err != nil {
		t.Fatal(err)
	}
	back, err := s.Env(ctx, "shop", "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	if back.Commit != got.Commit || back.Status != EnvReady || back.VarsJSON != got.VarsJSON {
		t.Errorf("update lost fields: %+v", back)
	}
	if !back.UpdatedAt.After(back.CreatedAt) {
		t.Errorf("updated_at %v was not touched (created %v)", back.UpdatedAt, back.CreatedAt)
	}

	if err := s.UpdateEnvStatus(ctx, rec.ID, EnvFailed); err != nil {
		t.Fatal(err)
	}
	if back, _ := s.Env(ctx, "shop", "feat-x"); back.Status != EnvFailed {
		t.Errorf("status = %q, want %q", back.Status, EnvFailed)
	}
	if err := s.UpdateEnvStatus(ctx, 9999, EnvReady); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown env: err = %v, want ErrNotFound", err)
	}
	if err := s.UpdateEnv(ctx, EnvRecord{ID: 9999}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown env: err = %v, want ErrNotFound", err)
	}

	s.Close()
	if back, err := open(t, path).Env(ctx, "shop", "feat-x"); err != nil || back.Commit != got.Commit {
		t.Errorf("after reopen: %+v, %v", back, err)
	}
}

func TestEnvsListing(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	for _, name := range []string{"shop", "blog"} {
		if err := s.AddApp(ctx, sampleApp(name)); err != nil {
			t.Fatal(err)
		}
	}
	for i, e := range []EnvRecord{
		sampleEnv("shop", "feat-x", 20000),
		sampleEnv("blog", "feat-y", 20016),
		sampleEnv("shop", "feat-z", 20032),
	} {
		if _, err := s.CreateEnv(ctx, e); err != nil {
			t.Fatalf("env %d: %v", i, err)
		}
	}

	shop, err := s.Envs(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(shop) != 2 || shop[0].Name != "feat-x" || shop[1].Name != "feat-z" {
		t.Fatalf("shop envs = %+v, want feat-x then feat-z", shop)
	}

	all, err := s.Envs(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[1].App != "blog" {
		t.Fatalf("all envs = %+v", all)
	}
	if got, _ := s.Envs(ctx, "nosuchapp"); len(got) != 0 {
		t.Errorf("unknown app: %+v", got)
	}
}

func TestCreateEnvConflicts(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	for _, name := range []string{"shop", "blog"} {
		if err := s.AddApp(ctx, sampleApp(name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateEnv(ctx, sampleEnv("shop", "feat-x", 20000)); err != nil {
		t.Fatal(err)
	}

	t.Run("same app and name", func(t *testing.T) {
		e := sampleEnv("shop", "feat-x", 20016)
		if _, err := s.CreateEnv(ctx, e); !errors.Is(err, ErrExists) {
			t.Fatalf("err = %v, want ErrExists", err)
		}
	})
	t.Run("port block taken by another app", func(t *testing.T) {

		e := sampleEnv("blog", "other", 20000)
		if _, err := s.CreateEnv(ctx, e); !errors.Is(err, ErrExists) {
			t.Fatalf("err = %v, want ErrExists", err)
		}
	})
	t.Run("unknown app is not a retryable conflict", func(t *testing.T) {
		e := sampleEnv("nosuchapp", "feat-a", 20048)
		_, err := s.CreateEnv(ctx, e)
		if err == nil {
			t.Fatal("want an error")
		}
		if errors.Is(err, ErrExists) {
			t.Fatalf("err = %v, must not be ErrExists: the allocator would retry forever", err)
		}
	})
	t.Run("missing fields", func(t *testing.T) {
		if _, err := s.CreateEnv(ctx, EnvRecord{Name: "x", PortBase: 20064}); err == nil {
			t.Error("empty app must be rejected")
		}
		if _, err := s.CreateEnv(ctx, EnvRecord{App: "shop", PortBase: 20064}); err == nil {
			t.Error("empty name must be rejected")
		}
		if _, err := s.CreateEnv(ctx, EnvRecord{App: "shop", Name: "x"}); err == nil {
			t.Error("a zero port base must be rejected")
		}
	})
}

func TestEnvResourcesAndEvents(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	if err := s.AddApp(ctx, sampleApp("shop")); err != nil {
		t.Fatal(err)
	}
	rec, err := s.CreateEnv(ctx, sampleEnv("shop", "feat-x", 20000))
	if err != nil {
		t.Fatal(err)
	}

	resources := []EnvResource{
		{EnvID: rec.ID, Kind: ResourceBranch, Name: "feat-x"},
		{EnvID: rec.ID, Kind: ResourceWorktree, Name: rec.Worktree},
		{EnvID: rec.ID, Kind: ResourceVolume, Name: "caramelo-shop-feat-x-db", Dep: "db"},
		{EnvID: rec.ID, Kind: ResourceContainer, Name: "caramelo-shop-feat-x-db", Dep: "db", Port: 20001},
	}
	for _, r := range resources {
		if err := s.AddResource(ctx, r); err != nil {
			t.Fatalf("AddResource(%s): %v", r.Kind, err)
		}
	}
	got, err := s.Resources(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(resources) {
		t.Fatalf("got %d resources, want %d", len(got), len(resources))
	}

	for i := range resources {
		if got[i].Kind != resources[i].Kind || got[i].Name != resources[i].Name {
			t.Fatalf("resource %d = %+v, want %+v", i, got[i], resources[i])
		}
	}
	container := got[3]
	if container.Dep != "db" || container.Port != 20001 || container.CreatedAt.IsZero() {
		t.Errorf("container resource = %+v", container)
	}

	if err := s.DeleteResource(ctx, container.ID); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteResource(ctx, container.ID); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if got, _ := s.Resources(ctx, rec.ID); len(got) != 3 {
		t.Errorf("after delete: %d resources", len(got))
	}

	if err := s.AddResource(ctx, EnvResource{Kind: ResourceBranch, Name: "x"}); err == nil {
		t.Error("a resource without an env must be rejected")
	}

	again := resources[0]
	for i := 0; i < 3; i++ {
		if err := s.AddResource(ctx, again); err != nil {
			t.Fatalf("recording %s again: %v", again.Kind, err)
		}
	}
	if got, _ := s.Resources(ctx, rec.ID); len(got) != 3 {
		t.Errorf("after recording one resource three more times: %d resources, want 3", len(got))
	}

	for i, e := range []EnvEvent{
		{EnvID: rec.ID, Action: "create", Status: "started", Identity: "alex@laptop"},
		{EnvID: rec.ID, Action: "create", Status: "failed", Detail: "db never became ready", Identity: "alex@laptop"},
		{EnvID: rec.ID, Action: "create", Status: "ok", Identity: "agent@ci"},
	} {
		e.At = time.Date(2026, 9, 8, 12, 0, i, 0, time.UTC)
		if err := s.AddEvent(ctx, e); err != nil {
			t.Fatalf("AddEvent(%d): %v", i, err)
		}
	}
	events, err := s.Events(ctx, rec.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}

	if events[0].Status != "ok" || events[2].Status != "started" {
		t.Fatalf("wrong order: %+v", events)
	}
	if events[1].Detail != "db never became ready" || events[1].Identity != "alex@laptop" {
		t.Errorf("event round trip: %+v", events[1])
	}
	if !events[0].At.Equal(time.Date(2026, 9, 8, 12, 0, 2, 0, time.UTC)) {
		t.Errorf("at = %v", events[0].At)
	}
	limited, err := s.Events(ctx, rec.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 || limited[0].Status != "ok" {
		t.Errorf("limit: %+v", limited)
	}

	if err := s.AddEvent(ctx, EnvEvent{App: "shop", Action: "secrets", Step: "set", Status: "ok",
		Detail: "STRIPE_KEY", Identity: "alex@laptop"}); err != nil {
		t.Fatalf("an event with no env: %v", err)
	}
	machine, err := s.QueryEvents(ctx, EventFilter{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if len(machine) == 0 || machine[0].Action != "secrets" || machine[0].App != "shop" {
		t.Fatalf("the app's feed = %+v, want the secrets event first", machine)
	}
	if machine[0].EnvID != 0 || machine[0].Env != "" {
		t.Errorf("event = %+v, want no environment", machine[0])
	}

	own, err := s.QueryEvents(ctx, EventFilter{EnvID: rec.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range own {
		if e.Action == "secrets" {
			t.Errorf("a machine-wide event is in env %d's feed: %+v", rec.ID, e)
		}
	}
	if err := s.AddEvent(ctx, EnvEvent{}); err == nil {
		t.Error("an event with nothing in it must be rejected")
	}
}

func TestDeleteEnvCascades(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	if err := s.AddApp(ctx, sampleApp("shop")); err != nil {
		t.Fatal(err)
	}
	rec, err := s.CreateEnv(ctx, sampleEnv("shop", "feat-x", 20000))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddResource(ctx, EnvResource{EnvID: rec.ID, Kind: ResourceWorktree, Name: rec.Worktree}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddEvent(ctx, EnvEvent{EnvID: rec.ID, Action: "create", Status: "ok"}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteEnv(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Env(ctx, "shop", "feat-x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if got, err := s.Resources(ctx, rec.ID); err != nil || len(got) != 0 {
		t.Errorf("resources survived the env: %+v, %v", got, err)
	}
	if got, err := s.Events(ctx, rec.ID, 0); err != nil || len(got) != 0 {
		t.Errorf("events survived the env: %+v, %v", got, err)
	}

	if err := s.DeleteEnv(ctx, rec.ID); err != nil {
		t.Errorf("second delete: %v", err)
	}

	if _, err := s.CreateEnv(ctx, sampleEnv("shop", "feat-y", 20000)); err != nil {
		t.Errorf("reusing the freed block: %v", err)
	}
}

func TestConcurrentEnvCreates(t *testing.T) {

	ctx := context.Background()
	s, _ := tempDB(t)
	if err := s.AddApp(ctx, sampleApp("shop")); err != nil {
		t.Fatal(err)
	}

	const agents = 8
	var wg sync.WaitGroup
	created := make(chan int, agents)
	conflicts := make(chan error, agents)
	for i := 0; i < agents; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			for _, base := range []int{20000, 20016 + 16*i} {
				rec, err := s.CreateEnv(ctx, sampleEnv("shop", fmt.Sprintf("feat-%d", i), base))
				switch {
				case err == nil:
					created <- rec.PortBase
					return
				case errors.Is(err, ErrExists):
					continue
				default:
					conflicts <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(created)
	close(conflicts)
	for err := range conflicts {
		t.Fatalf("concurrent create: %v", err)
	}
	seen := map[int]bool{}
	for base := range created {
		if seen[base] {
			t.Fatalf("port block %d handed out twice", base)
		}
		seen[base] = true
	}
	if len(seen) != agents {
		t.Fatalf("%d envs created, want %d", len(seen), agents)
	}
	envs, err := s.Envs(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != agents {
		t.Errorf("%d rows, want %d", len(envs), agents)
	}
}
