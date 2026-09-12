package cli

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/cli/ui"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/progress"
)

type eventsService struct {
	api.Service

	req    env.EventsRequest
	follow bool
	events []progress.Event
	err    error
}

func (s *eventsService) Events(_ context.Context, req env.EventsRequest, out io.Writer, follow bool) error {
	s.req, s.follow = req, follow
	if s.err != nil {
		return s.err
	}
	for _, e := range s.events {
		if err := progress.Emit(out, e); err != nil {
			return err
		}
	}
	return nil
}

func sampleFeed() []progress.Event {
	at := time.Date(2026, 9, 10, 9, 30, 0, 0, time.UTC)
	return []progress.Event{
		{App: "shop", Env: "production", Action: "deploy", Step: "build", Status: progress.StatusChanged,
			Detail: "web: caramelo/shop/web:8f21c0b1a4d2", Identity: "alex@laptop", At: at, Seq: 41},
		{App: "shop", Env: "production", Service: "web", Replica: 2, Action: "rollout", Step: "flip",
			Status: progress.StatusChanged, Detail: "web-2 flip: active", At: at.Add(time.Second), Seq: 42},
	}
}

func TestEventsPrintsTheRows(t *testing.T) {
	feed := sampleFeed()
	svc := &eventsService{events: feed}
	code, stdout, stderr := runService(t, context.Background(), svc, "events", "production", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want one per event:\n%s", len(lines), stdout)
	}
	for i, e := range feed {
		if lines[i] != ui.FeedLine(e) {
			t.Errorf("line %d = %q, want %q", i, lines[i], ui.FeedLine(e))
		}
	}
	for _, want := range []string{"shop/production", "deploy build", "changed",
		"caramelo/shop/web:8f21c0b1a4d2", "alex@laptop"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("row %q is missing %q", lines[0], want)
		}
	}

	if !strings.Contains(lines[1], "web-2") {
		t.Errorf("row %q does not name the replica", lines[1])
	}
	if svc.req.App != "shop" || svc.req.Env != "production" {
		t.Errorf("request = %+v", svc.req)
	}
	if svc.follow || svc.req.Follow {
		t.Error("a feed nobody asked to follow was followed")
	}
}

func TestEventsJSONIsOneObjectPerLine(t *testing.T) {
	svc := &eventsService{events: sampleFeed()}
	code, stdout, stderr := runService(t, context.Background(), svc,
		"events", "production", "--app", "shop", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines:\n%s", len(lines), stdout)
	}
	var e progress.Event
	if err := json.Unmarshal([]byte(lines[1]), &e); err != nil {
		t.Fatalf("%q is not one JSON object: %v", lines[1], err)
	}
	if e.Service != "web" || e.Replica != 2 || e.Step != "flip" || e.Seq != 42 {
		t.Errorf("event = %+v; the columns did not survive the wire", e)
	}
}

func TestEventsWithNoEnvironmentIsTheMachine(t *testing.T) {
	svc := &eventsService{}
	code, _, stderr := runService(t, context.Background(), svc, "events", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if svc.req.App != "" || svc.req.Env != "" {
		t.Errorf("request = %+v, want the machine's whole feed", svc.req)
	}
}

func TestEventsFlagsBecomeTheRequest(t *testing.T) {
	svc := &eventsService{}
	before := time.Now()
	code, _, stderr := runService(t, context.Background(), svc,
		"events", "--app", "shop", "-f", "--since", "1h", "--limit", "7")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !svc.follow || !svc.req.Follow {
		t.Error("--follow did not reach the daemon")
	}
	if svc.req.Limit != 7 {
		t.Errorf("limit = %d", svc.req.Limit)
	}
	ago := before.Add(-time.Hour)
	if svc.req.Since.Before(ago.Add(-time.Minute)) || svc.req.Since.After(ago.Add(time.Minute)) {
		t.Errorf("since = %s, want about %s", svc.req.Since, ago)
	}
}

func TestEventsWithoutSinceSendsNoInstant(t *testing.T) {
	svc := &eventsService{}
	if code, _, stderr := runService(t, context.Background(), svc, "events", "--app", "shop"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !svc.req.Since.IsZero() {
		t.Errorf("since = %s, want the zero instant", svc.req.Since)
	}
}

func TestProgressJSONOnALifecycleCommand(t *testing.T) {
	line := "[changed] container: caramelo-shop-feat-x-db on 127.0.0.1:20001"

	svc := &envService{progress: line}
	code, _, stderr := runService(t, context.Background(), svc,
		"env", "create", "feat-x", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, line+"\n") {
		t.Errorf("text progress = %q, want M6's line untouched", stderr)
	}

	svc = &envService{progress: line}
	code, _, stderr = runService(t, context.Background(), svc,
		"env", "create", "feat-x", "--app", "shop", "--progress", "json")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}

	e, ok := ui.ParseEvent(strings.TrimSpace(stderr))
	if !ok {
		t.Fatalf("stderr %q does not decode as an event", stderr)
	}
	if e.Detail != line {
		t.Errorf("event = %+v, want the line carried whole", e)
	}
}
