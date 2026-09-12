package env

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/state"
)

func TestTheFeedDoesNotChangeTheBytes(t *testing.T) {
	h := newHarness(t)
	rec := &state.EnvRecord{ID: 7, App: "shop", Name: "feat-x"}
	var plain, wrapped bytes.Buffer
	w := h.m.feed(context.Background(), rec, &wrapped)

	write := func(out io.Writer) {
		progressf(out, "ok", "ports", "%d-%d", 20000, 20031)
		progressf(out, "changed", "container", "%s on %s:%d", "caramelo-shop-feat-x-db", DepHost, 20001)
		progressf(out, "warning", "edge", "no edge on this machine")
		emit(out, Event{Action: "rollout", Status: progress.StatusSkipped, Service: "web", Replica: 1,
			Step: string(StepDrain), Detail: "web-1 drain: nothing was replaced"})
	}
	write(&plain)
	write(w)
	if plain.String() != wrapped.String() {
		t.Errorf("wrapped progress =\n%q\nplain      =\n%q", wrapped.String(), plain.String())
	}

	rows, err := h.store.Events(context.Background(), 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("%d rows recorded, want 4:\n%+v", len(rows), rows)
	}

	last := rows[0]
	if last.Action != "rollout" || last.Service != "web" || last.Replica != 1 ||
		last.Step != string(StepDrain) || last.Status != progress.StatusSkipped {
		t.Errorf("the rollout row lost its columns: %+v", last)
	}
	for _, r := range rows {
		if r.App != "shop" || r.Env != "feat-x" {
			t.Errorf("row %+v does not name its environment", r)
		}
		if r.At.IsZero() {
			t.Errorf("row %+v has no instant", r)
		}
	}
}

func TestTheFeedRecordsWithNobodyWatching(t *testing.T) {
	h := newHarness(t)
	rec := &state.EnvRecord{ID: 7, App: "shop", Name: "feat-x"}
	w := h.m.feed(context.Background(), rec, nil)
	progressf(w, "changed", "env", "feat-x ready on port %d", 20000)

	rows, err := h.store.Events(context.Background(), 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "env" {
		t.Fatalf("rows = %+v, want the one line the command did not print", rows)
	}
}

func TestTheFeedRecordsAfterTheClientHangsUp(t *testing.T) {
	h := newHarness(t)
	rec := &state.EnvRecord{ID: 7, App: "shop", Name: "feat-x"}
	ctx, cancel := context.WithCancel(context.Background())
	w := h.m.feed(ctx, rec, nil)
	cancel()
	progressf(w, "failed", "rollout", "web-1 start: no such image")

	rows, err := h.store.Events(context.Background(), 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want the failure", rows)
	}
}

func TestTheFeedDoesNotNest(t *testing.T) {
	h := newHarness(t)
	rec := &state.EnvRecord{ID: 7, App: "shop", Name: "feat-x"}
	var buf bytes.Buffer
	w := h.m.feed(context.Background(), rec, &buf)
	again := h.m.feed(context.Background(), rec, w)
	progressf(again, "ok", "ports", "20000-20031")

	rows, err := h.store.Events(context.Background(), 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("%d rows for one line: %+v", len(rows), rows)
	}
	if got := buf.String(); got != "[ok] ports: 20000-20031\n" {
		t.Errorf("progress = %q", got)
	}
}

func TestTheFeedLeavesAWriterAloneWithNoEnv(t *testing.T) {
	h := newHarness(t)
	var buf bytes.Buffer
	if w := h.m.feed(context.Background(), nil, &buf); w != &buf {
		t.Error("a nil record wrapped the writer anyway")
	}
	if w := h.m.feed(context.Background(), &state.EnvRecord{}, &buf); w != &buf {
		t.Error("a record with no id wrapped the writer anyway")
	}
}

func TestTheFeedDoesNotRecordRawWrites(t *testing.T) {
	h := newHarness(t)
	rec := &state.EnvRecord{ID: 7, App: "shop", Name: "feat-x"}
	var buf bytes.Buffer
	w := h.m.feed(context.Background(), rec, &buf)
	if _, err := w.Write([]byte("traceback (most recent call last)\n")); err != nil {
		t.Fatal(err)
	}
	rows, err := h.store.Events(context.Background(), 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("raw bytes became %d rows: %+v", len(rows), rows)
	}
	if !strings.Contains(buf.String(), "traceback") {
		t.Errorf("the bytes did not reach the terminal: %q", buf.String())
	}
}

func TestTheRolloutIsReadableInTheTailEnvShowCarries(t *testing.T) {
	h, _ := edgeHarness(t, 2)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	h.cfg.Services[0].Run = "python app.py --v2"
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	tail, err := h.m.events(context.Background(), 1, eventTail)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range tail {
		if e.Action == "rollout" && e.Step != "" {
			seen[e.Step] = true
		}
	}
	for _, want := range []RolloutStepName{StepFlip, StepDrain, StepStop} {
		if !seen[string(want)] {
			t.Errorf("no %q step in the newest %d events; the tail is too short for a rollout:\n%+v",
				want, eventTail, tail)
		}
	}
}

func TestCreateLeavesItsProgressInTheTrail(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	rows, err := h.store.Events(context.Background(), 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	byAction := map[string]state.EnvEvent{}
	for _, r := range rows {
		byAction[r.Action] = r
	}
	for _, want := range []string{"create", "ports", "branch", "worktree", "config", "container", "ready", "env"} {
		if _, ok := byAction[want]; !ok {
			t.Errorf("no %q among the create's events: %+v", want, rows)
		}
	}

	if got := byAction["container"].Detail; !strings.Contains(got, "caramelo-shop-feat-x-") {
		t.Errorf("container event = %q", got)
	}
}
