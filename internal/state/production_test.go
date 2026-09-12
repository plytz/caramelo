package state

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/progress"
)

func TestFreshDatabaseHasTheProductionSchema(t *testing.T) {
	s, _ := tempDB(t)
	db := s.(*store).db
	ctx := context.Background()
	now := time.Now().UTC().Format(timeFormat)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO apps (name, repo_path, created_at) VALUES ('shop', '/repo', ?)`, now); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	envID := insertEnvRow(t, db, "production")

	ins := `INSERT INTO releases (app, "commit", tree, built_at) VALUES (?, ?, ?, ?)`
	if _, err := db.ExecContext(ctx, ins, "shop", "c1", "t1", now); err != nil {
		t.Fatalf("insert release: %v", err)
	}
	if _, err := db.ExecContext(ctx, ins, "shop", "c2", "t1", now); err == nil {
		t.Error("the same tree of the same app was allowed a second release row")
	}
	if _, err := db.ExecContext(ctx, ins, "nosuch", "c1", "t9", now); err == nil {
		t.Error("a release of an app that was never pushed was accepted")
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO deploys (env_id, kind, status, started_at) VALUES (?, 'deploy', 'building', ?)`,
		envID, now); err != nil {
		t.Fatalf("insert deploy with no release: %v", err)
	}

	vins := `INSERT INTO vault (scope, app, env, name, ciphertext, nonce, updated_at)
	         VALUES (?, ?, ?, 'GREETING', x'00', x'01', ?)`
	for _, sc := range []struct{ scope, app, env string }{
		{VaultScopeMachine, "", ""},
		{VaultScopeApp, "shop", ""},
		{VaultScopeEnv, "shop", "production"},
	} {
		if _, err := db.ExecContext(ctx, vins, sc.scope, sc.app, sc.env, now); err != nil {
			t.Errorf("insert %s secret: %v", sc.scope, err)
		}
	}
	if _, err := db.ExecContext(ctx, vins, VaultScopeEnv, "shop", "production", now); err == nil {
		t.Error("the same secret was allowed twice at one scope")
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO vault (scope, app, env, name, ciphertext, nonce, updated_at)
		 VALUES ('env', 'shop', 'not-created-yet', 'DB_PASSWORD', x'00', x'01', ?)`, now); err != nil {
		t.Errorf("a secret for an environment that does not exist yet was refused: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO env_events (env_id, action, status, detail, service, replica, step, json, at)
		 VALUES (?, 'deploy', 'ok', 'd', 'web', 2, 'flip', '{"n":1}', ?)`, envID, now); err != nil {
		t.Fatalf("insert event: %v", err)
	}
}

func insertEnvRow(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	now := time.Now().UTC().Format(timeFormat)
	res, err := db.ExecContext(context.Background(), `INSERT INTO envs
		(app, name, branch, worktree, port_base, port_count, status, created_at, updated_at)
		VALUES ('shop', ?, ?, '/wt', 20000, 32, 'ready', ?, ?)`, name, name, now, now)
	if err != nil {
		t.Fatalf("insert env %s: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("env id: %v", err)
	}
	return id
}

func TestReleaseStore(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	if err := s.AddApp(ctx, sampleApp("shop")); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	r, err := s.AddRelease(ctx, Release{App: "shop", Commit: "c1", Tree: "t1", Ref: "HEAD",
		ConfigJSON: `{"name":"shop"}`, ImagesJSON: `{"web":"caramelo/shop/web:t1"}`,
		BuiltBy: "alex@laptop", BuiltAt: at})
	if err != nil {
		t.Fatalf("AddRelease: %v", err)
	}
	if r.ID == 0 {
		t.Error("AddRelease returned no id")
	}

	if _, err := s.AddRelease(ctx, Release{App: "shop", Commit: "c2", Tree: "t1"}); !errors.Is(err, ErrExists) {
		t.Errorf("a second release of tree t1: %v, want ErrExists", err)
	}
	got, err := s.ReleaseByTree(ctx, "shop", "t1")
	if err != nil {
		t.Fatalf("ReleaseByTree: %v", err)
	}
	if !reflect.DeepEqual(*got, *r) {
		t.Errorf("ReleaseByTree = %+v, want %+v", *got, *r)
	}
	if one, err := s.Release(ctx, r.ID); err != nil || one.Tree != "t1" {
		t.Errorf("Release(%d) = %+v, %v", r.ID, one, err)
	}
	if _, err := s.ReleaseByTree(ctx, "shop", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReleaseByTree of an unbuilt tree: %v, want ErrNotFound", err)
	}
	if _, err := s.Release(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Release(9999): %v, want ErrNotFound", err)
	}

	if _, err := s.AddRelease(ctx, Release{App: "nosuch", Tree: "t2"}); err == nil {
		t.Error("a release of an unknown app was accepted")
	}

	second, err := s.AddRelease(ctx, Release{App: "shop", Commit: "c2", Tree: "t2"})
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.Releases(ctx, "shop", 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != r.ID {
		t.Errorf("Releases = %+v, want [%d %d]", list, second.ID, r.ID)
	}
	if one, err := s.Releases(ctx, "shop", 1); err != nil || len(one) != 1 || one[0].ID != second.ID {
		t.Errorf("Releases(limit 1) = %+v, %v", one, err)
	}

	for i := 0; i < 2; i++ {
		if err := s.DeleteRelease(ctx, second.ID); err != nil {
			t.Errorf("DeleteRelease pass %d: %v", i, err)
		}
	}
}

func TestDeployStore(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	if err := s.AddApp(ctx, sampleApp("shop")); err != nil {
		t.Fatal(err)
	}
	e, err := s.CreateEnv(ctx, sampleEnv("shop", "production", 20000))
	if err != nil {
		t.Fatal(err)
	}
	rel, err := s.AddRelease(ctx, Release{App: "shop", Commit: "c1", Tree: "t1"})
	if err != nil {
		t.Fatal(err)
	}

	d, err := s.CreateDeploy(ctx, Deploy{EnvID: e.ID, ReleaseID: rel.ID, Status: DeployBuilding,
		Identity: "alex@laptop", StartedAt: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("CreateDeploy: %v", err)
	}
	switch {
	case d.ID == 0:
		t.Error("CreateDeploy returned no id")
	case d.Kind != DeployKindDeploy:
		t.Errorf("kind %q, want the default %q", d.Kind, DeployKindDeploy)
	case d.Done():
		t.Error("a building deploy reports itself done")
	case !d.FinishedAt.IsZero():
		t.Errorf("a building deploy finished at %v", d.FinishedAt)
	}

	unfinished, err := s.UnfinishedDeploys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(unfinished) != 1 || unfinished[0].ID != d.ID {
		t.Errorf("UnfinishedDeploys = %+v, want just %d", unfinished, d.ID)
	}

	d.Status = DeployPromoted
	d.Reason = "watch ended clean"
	d.FinishedAt = time.Date(2026, 9, 9, 12, 5, 0, 0, time.UTC)
	if err := s.UpdateDeploy(ctx, *d); err != nil {
		t.Fatalf("UpdateDeploy: %v", err)
	}
	got, err := s.Deploy(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Done() || got.Reason != "watch ended clean" || !got.FinishedAt.Equal(d.FinishedAt) {
		t.Errorf("updated deploy = %+v", got)
	}
	if left, err := s.UnfinishedDeploys(ctx); err != nil || len(left) != 0 {
		t.Errorf("UnfinishedDeploys after promotion = %+v, %v", left, err)
	}
	if _, err := s.Deploy(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Deploy(9999): %v, want ErrNotFound", err)
	}
	if err := s.UpdateDeploy(ctx, Deploy{ID: 9999, Status: DeployFailed}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateDeploy of a gone row: %v, want ErrNotFound", err)
	}

	if err := s.SetEnvRelease(ctx, e.ID, rel.ID); err != nil {
		t.Fatalf("SetEnvRelease: %v", err)
	}
	if err := s.SetEnvDeploy(ctx, e.ID, d.ID); err != nil {
		t.Fatalf("SetEnvDeploy: %v", err)
	}
	rec, err := s.Env(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ReleaseID != rel.ID || rec.DeployID != d.ID {
		t.Errorf("env release %d deploy %d, want %d %d", rec.ReleaseID, rec.DeployID, rel.ID, d.ID)
	}

	if err := s.SetEnvDeploy(ctx, e.ID, 0); err != nil {
		t.Fatal(err)
	}
	if rec, err = s.Env(ctx, "shop", "production"); err != nil || rec.DeployID != 0 {
		t.Errorf("env deploy after clearing = %d, %v", rec.DeployID, err)
	}
	if err := s.SetEnvRelease(ctx, 9999, rel.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetEnvRelease on a gone env: %v, want ErrNotFound", err)
	}

	if err := s.DeleteRelease(ctx, rel.ID); err != nil {
		t.Fatal(err)
	}
	if got, err = s.Deploy(ctx, d.ID); err != nil {
		t.Fatalf("the deploy went with its release: %v", err)
	}
	if got.ReleaseID != 0 {
		t.Errorf("deploy still points at release %d", got.ReleaseID)
	}

	if err := s.DeleteEnv(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Deploy(ctx, d.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a deploy of a destroyed env survived: %v", err)
	}
}

func TestDeployDone(t *testing.T) {
	for _, st := range []string{DeployBuilding, DeployMigrating, DeployStarting, DeployChecking, DeployWatching} {
		if DeployDone(st) {
			t.Errorf("%s reports done", st)
		}
	}
	for _, st := range []string{DeployPromoted, DeployRolledBack, DeployFailed} {
		if !DeployDone(st) {
			t.Errorf("%s does not report done", st)
		}
	}
	if len(DeployStatuses) != 8 {
		t.Errorf("%d statuses, want 8", len(DeployStatuses))
	}
}

func TestVaultStore(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	put := func(scope, app, env, name string, ct byte) *VaultEntry {
		t.Helper()
		e, err := s.PutSecret(ctx, VaultEntry{Scope: scope, App: app, Env: env, Name: name,
			Ciphertext: []byte{ct}, Nonce: []byte{1, 2, 3}, UpdatedBy: "alex@laptop", UpdatedAt: at})
		if err != nil {
			t.Fatalf("PutSecret %s/%s: %v", scope, name, err)
		}
		return e
	}

	put(VaultScopeEnv, "shop", "production", "DB_PASSWORD", 0xaa)
	machine := put(VaultScopeMachine, "", "", "GREETING", 0x01)
	app := put(VaultScopeApp, "shop", "", "GREETING", 0x02)
	env := put(VaultScopeEnv, "shop", "production", "GREETING", 0x03)
	for _, e := range []*VaultEntry{machine, app, env} {
		if e.Version != 1 {
			t.Errorf("%s secret starts at version %d, want 1", e.Scope, e.Version)
		}
	}

	again := put(VaultScopeEnv, "shop", "production", "GREETING", 0x04)
	if again.Version != 2 {
		t.Errorf("rewritten secret is version %d, want 2", again.Version)
	}
	if got := again.Ciphertext; len(got) != 1 || got[0] != 0x04 {
		t.Errorf("rewritten ciphertext = %v", got)
	}

	one, err := s.Secret(ctx, VaultScopeApp, "shop", "", "GREETING")
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	if len(one.Ciphertext) != 1 || one.Ciphertext[0] != 0x02 || one.UpdatedBy != "alex@laptop" {
		t.Errorf("app secret = %+v", one)
	}
	if _, err := s.Secret(ctx, VaultScopeEnv, "shop", "production", "NOPE"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Secret of a name nobody set: %v, want ErrNotFound", err)
	}

	layer, err := s.Secrets(ctx, VaultScopeEnv, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	if names := entryNames(layer); !reflect.DeepEqual(names, []string{"DB_PASSWORD", "GREETING"}) {
		t.Errorf("env layer = %v", names)
	}

	all, err := s.SecretsFor(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{
		{VaultScopeMachine, "GREETING"},
		{VaultScopeApp, "GREETING"},
		{VaultScopeEnv, "DB_PASSWORD"},
		{VaultScopeEnv, "GREETING"},
	}
	var got [][2]string
	for _, e := range all {
		got = append(got, [2]string{e.Scope, e.Name})
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SecretsFor = %v, want %v", got, want)
	}

	other, err := s.SecretsFor(ctx, "shop", "staging")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 2 {
		t.Errorf("staging sees %d secrets, want 2: %v", len(other), other)
	}

	if err := s.RemoveSecret(ctx, VaultScopeApp, "shop", "", "GREETING"); err != nil {
		t.Fatalf("RemoveSecret: %v", err)
	}
	if err := s.RemoveSecret(ctx, VaultScopeApp, "shop", "", "GREETING"); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing it twice: %v, want ErrNotFound", err)
	}
}

func TestVaultScopeValidation(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	for _, e := range []VaultEntry{
		{Scope: VaultScopeMachine, App: "shop", Name: "K"},
		{Scope: VaultScopeMachine, Env: "production", Name: "K"},
		{Scope: VaultScopeApp, Name: "K"},
		{Scope: VaultScopeApp, App: "shop", Env: "production", Name: "K"},
		{Scope: VaultScopeEnv, App: "shop", Name: "K"},
		{Scope: VaultScopeEnv, Env: "production", Name: "K"},
		{Scope: "global", Name: "K"},
		{Scope: VaultScopeMachine},
	} {
		e.Ciphertext, e.Nonce = []byte{1}, []byte{2}
		if _, err := s.PutSecret(ctx, e); err == nil {
			t.Errorf("PutSecret(%+v) was accepted", e)
		}
	}

	for _, e := range []VaultEntry{
		{Scope: VaultScopeMachine, Name: "K", Nonce: []byte{2}},
		{Scope: VaultScopeMachine, Name: "K", Ciphertext: []byte{1}},
	} {
		if _, err := s.PutSecret(ctx, e); err == nil {
			t.Errorf("PutSecret(%+v) was accepted", e)
		}
	}
}

func entryNames(in []VaultEntry) []string {
	out := make([]string, 0, len(in))
	for _, e := range in {
		out = append(out, e.Name)
	}
	return out
}

func TestQueryEventsIsTheMachinesFeed(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	if err := s.AddApp(ctx, sampleApp("shop")); err != nil {
		t.Fatal(err)
	}
	prod, err := s.CreateEnv(ctx, sampleEnv("shop", "production", 20000))
	if err != nil {
		t.Fatal(err)
	}
	feat, err := s.CreateEnv(ctx, sampleEnv("shop", "feat-x", 20032))
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	add := func(envID int64, e EnvEvent) {
		t.Helper()
		e.EnvID = envID
		if err := s.AddEvent(ctx, e); err != nil {
			t.Fatalf("AddEvent: %v", err)
		}
	}
	add(prod.ID, EnvEvent{Action: "deploy", Status: "started", At: base})
	add(feat.ID, EnvEvent{Action: "up", Status: "ok", At: base.Add(time.Minute)})
	add(prod.ID, EnvEvent{Action: "rollout", Status: "ok", Service: "web", Replica: 2, Step: "flip",
		Detail: "web-2 flip: active", JSON: `{"inflight":3}`, Identity: "alex@laptop",
		At: base.Add(2 * time.Minute)})

	all, err := s.QueryEvents(ctx, EventFilter{})
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("%d events, want 3", len(all))
	}
	first := all[0]
	if first.Action != "rollout" || first.Service != "web" || first.Replica != 2 ||
		first.Step != "flip" || first.JSON != `{"inflight":3}` {
		t.Errorf("newest event = %+v", first)
	}
	if first.App != "shop" || first.Env != "production" {
		t.Errorf("newest event names %s/%s, want shop/production", first.App, first.Env)
	}

	mine, err := s.QueryEvents(ctx, EventFilter{EnvID: feat.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].Env != "feat-x" {
		t.Errorf("feat-x's events = %+v", mine)
	}

	recent, err := s.QueryEvents(ctx, EventFilter{Since: base.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 {
		t.Errorf("%d events since the second one, want 2", len(recent))
	}

	capped, err := s.QueryEvents(ctx, EventFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) != 1 || capped[0].Action != "rollout" {
		t.Errorf("QueryEvents(limit 1) = %+v", capped)
	}

	per, err := s.Events(ctx, prod.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(per) != 2 || per[0].Step != "flip" {
		t.Errorf("Events(production) = %+v", per)
	}
}

func TestRouteManaged(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	if err := s.AddApp(ctx, sampleApp("shop")); err != nil {
		t.Fatal(err)
	}
	e, err := s.CreateEnv(ctx, sampleEnv("shop", "feat-x", 20000))
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.AddRoute(ctx, Route{EnvID: e.ID, Service: "web", Host: "feat-x.shop.test",
		Kind: RouteKindHTTPS, Managed: true})
	if err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if !r.Managed {
		t.Error("AddRoute lost Managed")
	}
	back, err := s.RouteByHost(ctx, "feat-x.shop.test")
	if err != nil || !back.Managed {
		t.Errorf("RouteByHost = %+v, %v", back, err)
	}

	if err := s.SetRouteManaged(ctx, r.ID, false); err != nil {
		t.Fatalf("SetRouteManaged: %v", err)
	}
	if back, err = s.RouteByHost(ctx, "feat-x.shop.test"); err != nil || back.Managed {
		t.Errorf("route after unmanaging = %+v, %v", back, err)
	}
	if err := s.SetRouteManaged(ctx, 9999, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetRouteManaged on a gone route: %v, want ErrNotFound", err)
	}
}

func TestUpdateEnvKeepsModeAndProtection(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	if err := s.AddApp(ctx, sampleApp("shop")); err != nil {
		t.Fatal(err)
	}
	rec := sampleEnv("shop", "production", 20000)
	rec.Mode, rec.Protected = EnvModeRelease, true
	created, err := s.CreateEnv(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if created.Mode != EnvModeRelease || !created.Protected {
		t.Fatalf("CreateEnv lost the mode: %+v", created)
	}

	stale := *created
	stale.Mode, stale.Protected = "", false
	stale.Commit = "c1"
	if err := s.UpdateEnv(ctx, stale); err != nil {
		t.Fatal(err)
	}
	back, err := s.Env(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case back.Commit != "c1":
		t.Errorf("the update did not land: commit %q", back.Commit)
	case back.Mode != EnvModeRelease:
		t.Errorf("mode became %q", back.Mode)
	case !back.Protected:
		t.Error("the environment lost its protection")
	}
}

func TestCreateEnvDefaultsToDevMode(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	if err := s.AddApp(ctx, sampleApp("shop")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateEnv(ctx, sampleEnv("shop", "feat-x", 20000)); err != nil {
		t.Fatal(err)
	}
	back, err := s.Env(ctx, "shop", "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	if back.Mode != EnvModeDev || back.Protected {
		t.Errorf("a plain create gave mode %q protected %v, want %q false", back.Mode, back.Protected, EnvModeDev)
	}
}

type recorder struct {
	mu     sync.Mutex
	events []progress.Event
}

func (r *recorder) Notify(e progress.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) Subscribe(int) (<-chan progress.Event, func()) {
	ch := make(chan progress.Event)
	close(ch)
	return ch, func() {}
}

func (r *recorder) all() []progress.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]progress.Event(nil), r.events...)
}

func TestAddEventPublishesToTheNotifier(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	if err := s.AddApp(ctx, sampleApp("shop")); err != nil {
		t.Fatal(err)
	}
	e, err := s.CreateEnv(ctx, sampleEnv("shop", "production", 20000))
	if err != nil {
		t.Fatal(err)
	}

	if err := s.AddEvent(ctx, EnvEvent{EnvID: e.ID, Action: "create", Status: "ok"}); err != nil {
		t.Fatal(err)
	}

	rec := &recorder{}
	s.SetNotifier(rec)
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	if err := s.AddEvent(ctx, EnvEvent{EnvID: e.ID, Action: "rollout", Status: "changed",
		Service: "web", Replica: 2, Step: "flip", Detail: "web-2 flip: active",
		Identity: "alex@laptop", JSON: `{"inflight":3}`, At: at}); err != nil {
		t.Fatal(err)
	}
	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("%d events published, want 1", len(got))
	}

	if got[0].Seq == 0 {
		t.Error("the published event carries no sequence")
	}
	want := progress.Event{Seq: got[0].Seq, App: "shop", Env: "production", Service: "web", Replica: 2,
		Action: "rollout", Step: "flip", Status: "changed", Detail: "web-2 flip: active",
		Identity: "alex@laptop", At: at, JSON: []byte(`{"inflight":3}`)}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("published = %+v\nwant        %+v", got[0], want)
	}

	s.SetNotifier(nil)
	if err := s.AddEvent(ctx, EnvEvent{EnvID: e.ID, Action: "destroy", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.all()) != 1 {
		t.Error("an event was published after the notifier was removed")
	}
}

func TestEventConversionRoundTrip(t *testing.T) {
	in := progress.Event{App: "shop", Env: "production", Service: "web", Replica: 2,
		Action: "deploy", Step: "watch", Status: "changed", Detail: "60s, 0 errors",
		Identity: "alex@laptop", At: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC),
		JSON: []byte(`{"requests":412}`)}
	row := EventFromProgress(7, in)
	if row.EnvID != 7 {
		t.Errorf("env id = %d", row.EnvID)
	}
	if got := row.Progress(); !reflect.DeepEqual(got, in) {
		t.Errorf("round trip = %+v\nwant         %+v", got, in)
	}

	if got := (EnvEvent{Action: "x"}).Progress(); got.JSON != nil {
		t.Errorf("an event with no JSON carries %q", got.JSON)
	}
}
