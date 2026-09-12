package state

import (
	"context"
	"testing"
	"time"
)

func TestQueryEventsFiltersByApp(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	for _, app := range []string{"shop", "blog"} {
		if err := s.AddApp(ctx, sampleApp(app)); err != nil {
			t.Fatal(err)
		}
	}
	shopProd, err := s.CreateEnv(ctx, sampleEnv("shop", "production", 20000))
	if err != nil {
		t.Fatal(err)
	}
	shopFeat, err := s.CreateEnv(ctx, sampleEnv("shop", "feat-x", 20032))
	if err != nil {
		t.Fatal(err)
	}
	blog, err := s.CreateEnv(ctx, sampleEnv("blog", "main", 20064))
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	add := func(envID int64, action string, at time.Time) {
		t.Helper()
		if err := s.AddEvent(ctx, EnvEvent{EnvID: envID, Action: action, Status: "ok", At: at}); err != nil {
			t.Fatalf("AddEvent: %v", err)
		}
	}
	add(shopProd.ID, "deploy", base)
	add(shopFeat.ID, "up", base.Add(time.Minute))
	add(blog.ID, "up", base.Add(2*time.Minute))

	mine, err := s.QueryEvents(ctx, EventFilter{App: "shop"})
	if err != nil {
		t.Fatalf("QueryEvents(app shop): %v", err)
	}
	if len(mine) != 2 {
		t.Fatalf("%d events for shop, want 2", len(mine))
	}
	for _, e := range mine {
		if e.App != "shop" {
			t.Errorf("event of %s in shop's feed: %+v", e.App, e)
		}
	}

	one, err := s.QueryEvents(ctx, EventFilter{App: "shop", EnvID: blog.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Env != "main" {
		t.Errorf("QueryEvents(app shop, env blog/main) = %+v", one)
	}

	none, err := s.QueryEvents(ctx, EventFilter{App: "nosuch"})
	if err != nil || len(none) != 0 {
		t.Errorf("QueryEvents(app nosuch) = %+v, %v", none, err)
	}
}

func TestEventSequenceIsTheRowID(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	if err := s.AddApp(ctx, sampleApp("shop")); err != nil {
		t.Fatal(err)
	}
	rec, err := s.CreateEnv(ctx, sampleEnv("shop", "production", 20000))
	if err != nil {
		t.Fatal(err)
	}
	seen := &recorder{}
	s.SetNotifier(seen)
	base := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	for _, action := range []string{"deploy", "rollout", "promote"} {
		if err := s.AddEvent(ctx, EnvEvent{EnvID: rec.ID, Action: action, Status: "ok", At: base}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.QueryEvents(ctx, EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("%d rows, want 3", len(rows))
	}
	published := seen.all()
	if len(published) != 3 {
		t.Fatalf("%d published, want 3", len(published))
	}

	for i, row := range rows {
		live := published[len(published)-1-i]
		if row.ID == 0 {
			t.Fatalf("row %d has no id: %+v", i, row)
		}
		if got := row.Progress().Seq; got != row.ID {
			t.Errorf("row %d: event seq %d, row id %d", i, got, row.ID)
		}
		if live.Seq != row.ID {
			t.Errorf("row %d: published seq %d, row id %d", i, live.Seq, row.ID)
		}
		if live.Action != row.Action {
			t.Errorf("row %d: published %q, row %q", i, live.Action, row.Action)
		}
	}
	if !(rows[2].ID < rows[1].ID && rows[1].ID < rows[0].ID) {
		t.Errorf("ids do not rise with the writes: %d, %d, %d", rows[2].ID, rows[1].ID, rows[0].ID)
	}
}
