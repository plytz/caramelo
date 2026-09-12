package state

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMigration6Schema(t *testing.T) {
	s, _ := tempDB(t)
	db := s.(*store).db
	ctx := context.Background()
	now := time.Now().UTC().Format(timeFormat)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO apps (name, repo_path, created_at) VALUES ('shop', '/repo', ?)`, now); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	newEnv := func(name string, base int) int64 {
		t.Helper()
		res, err := db.ExecContext(ctx, `INSERT INTO envs
			(app, name, branch, worktree, port_base, port_count, status, created_at, updated_at)
			VALUES ('shop', ?, ?, '/wt', ?, 32, 'ready', ?, ?)`, name, name, base, now, now)
		if err != nil {
			t.Fatalf("insert env %s: %v", name, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("env id: %v", err)
		}
		return id
	}
	featX, featY := newEnv("feat-x", 20000), newEnv("feat-y", 20032)

	insertRoute := `INSERT INTO routes (env_id, service, host, kind, created_at) VALUES (?, ?, ?, 'https', ?)`
	res, err := db.ExecContext(ctx, insertRoute, featX, "web", "feat-x.shop.test", now)
	if err != nil {
		t.Fatalf("insert route: %v", err)
	}
	routeID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("route id: %v", err)
	}

	if _, err := db.ExecContext(ctx, insertRoute, featY, "web", "feat-x.shop.test", now); err == nil {
		t.Error("two environments were allowed to serve the same hostname")
	}

	if _, err := db.ExecContext(ctx, insertRoute, featX, "api", "api.feat-x.shop.test", now); err != nil {
		t.Errorf("a second hostname for one environment was refused: %v", err)
	}

	insertTarget := `INSERT INTO edge_targets (route_id, replica, port, state, inflight, updated_at)
	                 VALUES (?, ?, ?, ?, 0, ?)`
	if _, err := db.ExecContext(ctx, insertTarget, routeID, 1, 20002, TargetActive, now); err != nil {
		t.Fatalf("insert target: %v", err)
	}
	if _, err := db.ExecContext(ctx, insertTarget, routeID, 2, 20003, TargetStarting, now); err != nil {
		t.Fatalf("insert second target: %v", err)
	}
	if _, err := db.ExecContext(ctx, insertTarget, routeID, 1, 20004, TargetDraining, now); err == nil {
		t.Error("one replica of one route was recorded twice")
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM envs WHERE id = ?`, featX); err != nil {
		t.Fatalf("delete env: %v", err)
	}
	for _, table := range []string{"routes", "edge_targets"} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s still holds %d row(s) after the environment was destroyed", table, n)
		}
	}
}

func TestMigration6AddsReplicaColumns(t *testing.T) {
	s, _ := tempDB(t)
	ctx := context.Background()
	db := s.(*store).db
	now := time.Now().UTC().Format(timeFormat)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO apps (name, repo_path, created_at) VALUES ('shop', '/repo', ?)`, now); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	res, err := db.ExecContext(ctx, `INSERT INTO envs
		(app, name, branch, worktree, port_base, port_count, status, created_at, updated_at)
		VALUES ('shop', 'feat-x', 'feat-x', '/wt', 20000, 32, 'ready', ?, ?)`, now, now)
	if err != nil {
		t.Fatalf("insert env: %v", err)
	}
	envID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("env id: %v", err)
	}

	if err := s.PutService(ctx, EnvService{EnvID: envID, Name: "web", Status: ServiceRunning}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	services, err := s.Services(ctx, envID)
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if len(services) != 1 || services[0].Replicas != 1 {
		t.Fatalf("services = %+v, want one service with one replica", services)
	}

	if err := s.PutService(ctx, EnvService{EnvID: envID, Name: "web", Status: ServiceRunning, Replicas: 2}); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	if services, err = s.Services(ctx, envID); err != nil || len(services) != 1 || services[0].Replicas != 2 {
		t.Fatalf("services = %+v (%v), want one service with two replicas", services, err)
	}

	for _, r := range []EnvResource{
		{EnvID: envID, Kind: ResourceContainer, Name: "caramelo-shop-feat-x-web-1", Service: "web", Replica: 1, Port: 20002},
		{EnvID: envID, Kind: ResourceContainer, Name: "caramelo-shop-feat-x-web-2", Service: "web", Replica: 2, Port: 20003},
		{EnvID: envID, Kind: ResourceNetwork, Name: "caramelo-shop-feat-x"},
	} {
		if err := s.AddResource(ctx, r); err != nil {
			t.Fatalf("AddResource %s: %v", r.Name, err)
		}
	}
	resources, err := s.Resources(ctx, envID)
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	want := map[string]int{
		"caramelo-shop-feat-x-web-1": 1,
		"caramelo-shop-feat-x-web-2": 2,
		"caramelo-shop-feat-x":       0,
	}
	if len(resources) != len(want) {
		t.Fatalf("resources = %+v, want %d", resources, len(want))
	}
	for _, r := range resources {
		if got, ok := want[r.Name]; !ok || r.Replica != got {
			t.Errorf("resource %q has replica %d, want %d", r.Name, r.Replica, want[r.Name])
		}
	}
}

func edgeFixture(t *testing.T) (Store, int64, int64) {
	t.Helper()
	s, _ := tempDB(t)
	ctx := context.Background()
	if err := s.AddApp(ctx, App{Name: "shop", RepoPath: "/repo", DefaultBranch: "main"}); err != nil {
		t.Fatalf("AddApp: %v", err)
	}
	ids := make([]int64, 0, 2)
	for i, name := range []string{"feat-x", "feat-y"} {
		e, err := s.CreateEnv(ctx, EnvRecord{App: "shop", Name: name, Branch: name,
			Worktree: "/wt/" + name, PortBase: 20000 + i*32, PortCount: 32, Status: EnvReady})
		if err != nil {
			t.Fatalf("CreateEnv %s: %v", name, err)
		}
		ids = append(ids, e.ID)
	}
	return s, ids[0], ids[1]
}

func TestRoutesRoundTrip(t *testing.T) {
	s, featX, _ := edgeFixture(t)
	ctx := context.Background()

	r, err := s.AddRoute(ctx, Route{EnvID: featX, Service: "web", Host: "Feat-X.Shop.Test."})
	if err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	switch {
	case r.ID == 0:
		t.Error("AddRoute returned a route with no id")
	case r.Host != "feat-x.shop.test":
		t.Errorf("host = %q, want it lowercased and without the trailing dot", r.Host)
	case r.Kind != RouteKindHTTPS:
		t.Errorf("kind = %q, want %q by default", r.Kind, RouteKindHTTPS)
	case r.CreatedAt.IsZero():
		t.Error("created_at was not filled in")
	}

	for _, host := range []string{"feat-x.shop.test", "FEAT-X.shop.test.", "  feat-x.shop.test "} {
		got, err := s.RouteByHost(ctx, host)
		if err != nil {
			t.Fatalf("RouteByHost(%q): %v", host, err)
		}
		if got.ID != r.ID {
			t.Errorf("RouteByHost(%q) = route %d, want %d", host, got.ID, r.ID)
		}
	}
	if _, err := s.RouteByHost(ctx, "nope.shop.test"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RouteByHost of an unrouted name = %v, want ErrNotFound", err)
	}

	if _, err := s.AddRoute(ctx, Route{EnvID: featX, Service: "api", Host: "api.feat-x.shop.test"}); err != nil {
		t.Fatalf("AddRoute (second name): %v", err)
	}
	of, err := s.RoutesOfEnv(ctx, featX)
	if err != nil {
		t.Fatalf("RoutesOfEnv: %v", err)
	}
	if len(of) != 2 || of[0].Host != "feat-x.shop.test" || of[1].Host != "api.feat-x.shop.test" {
		t.Fatalf("RoutesOfEnv = %+v, want both names oldest first", of)
	}
	all, err := s.Routes(ctx)
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	if len(all) != 2 || all[0].Host != "api.feat-x.shop.test" {
		t.Fatalf("Routes = %+v, want every route by host", all)
	}

	if err := s.DeleteRoute(ctx, r.ID); err != nil {
		t.Fatalf("DeleteRoute: %v", err)
	}
	if err := s.DeleteRoute(ctx, r.ID); err != nil {
		t.Errorf("DeleteRoute of a route that is already gone = %v, want nil", err)
	}
	if _, err := s.RouteByHost(ctx, "feat-x.shop.test"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RouteByHost after the delete = %v, want ErrNotFound", err)
	}
}

func TestAddRouteRefusesATakenHost(t *testing.T) {
	s, featX, featY := edgeFixture(t)
	ctx := context.Background()
	if _, err := s.AddRoute(ctx, Route{EnvID: featX, Service: "web", Host: "feat-x.shop.test"}); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	for _, tc := range []struct {
		what string
		env  int64
		host string
	}{
		{"the same environment", featX, "feat-x.shop.test"},
		{"another environment", featY, "feat-x.shop.test"},
		{"another spelling", featY, "FEAT-X.shop.test."},
	} {
		if _, err := s.AddRoute(ctx, Route{EnvID: tc.env, Service: "web", Host: tc.host}); !errors.Is(err, ErrExists) {
			t.Errorf("AddRoute from %s = %v, want ErrExists", tc.what, err)
		}
	}
}

func TestAddRouteValidates(t *testing.T) {
	s, featX, _ := edgeFixture(t)
	ctx := context.Background()
	for what, r := range map[string]Route{
		"no env":     {Service: "web", Host: "feat-x.shop.test"},
		"no host":    {EnvID: featX, Service: "web"},
		"no service": {EnvID: featX, Host: "feat-x.shop.test"},
	} {
		if _, err := s.AddRoute(ctx, r); err == nil {
			t.Errorf("AddRoute with %s was accepted", what)
		}
	}
	_, err := s.AddRoute(ctx, Route{EnvID: 9999, Service: "web", Host: "ghost.shop.test"})
	if err == nil || !strings.Contains(err.Error(), "no such env") {
		t.Errorf("AddRoute onto a missing env = %v, want it to name the env", err)
	}
}

func TestTargetsRoundTrip(t *testing.T) {
	s, featX, _ := edgeFixture(t)
	ctx := context.Background()
	r, err := s.AddRoute(ctx, Route{EnvID: featX, Service: "web", Host: "feat-x.shop.test"})
	if err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	if err := s.PutTarget(ctx, EdgeTarget{RouteID: r.ID, Replica: 1, Port: 20002}); err != nil {
		t.Fatalf("PutTarget: %v", err)
	}
	got, err := s.Targets(ctx, r.ID)
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(got) != 1 || got[0].State != TargetStarting || got[0].UpdatedAt.IsZero() {
		t.Fatalf("targets = %+v, want one starting target with a timestamp", got)
	}

	if err := s.PutTarget(ctx, EdgeTarget{RouteID: r.ID, Replica: 1, Port: 20002,
		State: TargetActive, Inflight: 3}); err != nil {
		t.Fatalf("PutTarget (rewrite): %v", err)
	}
	if err := s.PutTarget(ctx, EdgeTarget{RouteID: r.ID, Replica: 2, Port: 20003,
		State: TargetStarting}); err != nil {
		t.Fatalf("PutTarget (second replica): %v", err)
	}
	got, err = s.Targets(ctx, r.ID)
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(got) != 2 || got[0].Replica != 1 || got[0].State != TargetActive || got[0].Inflight != 3 {
		t.Fatalf("targets = %+v, want replica 1 active with 3 in flight and replica 2 after it", got)
	}

	if err := s.SetTargets(ctx, r.ID, []EdgeTarget{
		{Replica: 3, Port: 20004, State: TargetActive},
		{Replica: 4, Port: 20005, State: TargetActive},
	}); err != nil {
		t.Fatalf("SetTargets: %v", err)
	}
	got, err = s.Targets(ctx, r.ID)
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(got) != 2 || got[0].Replica != 3 || got[1].Replica != 4 {
		t.Fatalf("targets = %+v, want only the replicas SetTargets was given", got)
	}

	if err := s.DeleteTarget(ctx, r.ID, 3); err != nil {
		t.Fatalf("DeleteTarget: %v", err)
	}
	if err := s.DeleteTarget(ctx, r.ID, 3); err != nil {
		t.Errorf("DeleteTarget of a replica that is already gone = %v, want nil", err)
	}
	if got, err = s.Targets(ctx, r.ID); err != nil || len(got) != 1 {
		t.Fatalf("targets = %+v (%v), want just replica 4", got, err)
	}

	if err := s.DeleteRoute(ctx, r.ID); err != nil {
		t.Fatalf("DeleteRoute: %v", err)
	}
	if got, err = s.Targets(ctx, r.ID); err != nil || len(got) != 0 {
		t.Fatalf("targets after the route was deleted = %+v (%v), want none", got, err)
	}
}

func TestPutTargetValidates(t *testing.T) {
	s, featX, _ := edgeFixture(t)
	ctx := context.Background()
	r, err := s.AddRoute(ctx, Route{EnvID: featX, Service: "web", Host: "feat-x.shop.test"})
	if err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	for what, tg := range map[string]EdgeTarget{
		"no route":     {Replica: 1, Port: 20002},
		"replica zero": {RouteID: r.ID, Replica: 0, Port: 20002},
	} {
		if err := s.PutTarget(ctx, tg); err == nil {
			t.Errorf("PutTarget with %s was accepted", what)
		}
	}
	err = s.PutTarget(ctx, EdgeTarget{RouteID: 9999, Replica: 1, Port: 20002})
	if err == nil || !strings.Contains(err.Error(), "no such route") {
		t.Errorf("PutTarget onto a missing route = %v, want it to name the route", err)
	}
}

func TestSetTargetsIsAllOrNothing(t *testing.T) {
	s, featX, _ := edgeFixture(t)
	ctx := context.Background()
	r, err := s.AddRoute(ctx, Route{EnvID: featX, Service: "web", Host: "feat-x.shop.test"})
	if err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := s.PutTarget(ctx, EdgeTarget{RouteID: r.ID, Replica: 1, Port: 20002, State: TargetActive}); err != nil {
		t.Fatalf("PutTarget: %v", err)
	}
	if err := s.SetTargets(ctx, r.ID, []EdgeTarget{
		{Replica: 2, Port: 20003, State: TargetActive},
		{Replica: 0, Port: 20004, State: TargetActive},
	}); err == nil {
		t.Fatal("SetTargets with an index that is not a replica was accepted")
	}
	got, err := s.Targets(ctx, r.ID)
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(got) != 1 || got[0].Replica != 1 {
		t.Fatalf("targets = %+v, want the ones that were there before the failed set", got)
	}
}
