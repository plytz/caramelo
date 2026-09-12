package env

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/progress"
)

func TestEventsWritesTheBacklogOldestFirst(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	var buf bytes.Buffer
	if err := h.m.Events(context.Background(), EventsRequest{App: "shop", Env: "feat-x"}, &buf); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 3 {
		t.Fatalf("the feed of a create is %d lines:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "create") {
		t.Errorf("the feed does not begin with the create: %q", lines[0])
	}
	if !strings.Contains(lines[len(lines)-1], "ready") {
		t.Errorf("the feed does not end with the env being ready: %q", lines[len(lines)-1])
	}

	if !strings.HasPrefix(lines[0], "[") {
		t.Errorf("the feed is not the progress line: %q", lines[0])
	}
}

func TestEventsNarrowsByEnvAppAndMachine(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-y"})

	count := func(req EventsRequest) int {
		var buf bytes.Buffer
		if err := h.m.Events(context.Background(), req, &buf); err != nil {
			t.Fatalf("Events(%+v): %v", req, err)
		}
		return len(strings.Split(strings.TrimSpace(buf.String()), "\n"))
	}
	one := count(EventsRequest{App: "shop", Env: "feat-x"})
	app := count(EventsRequest{App: "shop"})
	all := count(EventsRequest{})
	if one >= app {
		t.Errorf("one env has %d events and the app %d", one, app)
	}
	if app != all {
		t.Errorf("the app's feed is %d events and the machine's %d; the only app here is shop", app, all)
	}

	var empty bytes.Buffer
	if err := h.m.Events(context.Background(), EventsRequest{App: "blog"}, &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Len() != 0 {
		t.Errorf("an app with no environments has a feed:\n%s", empty.String())
	}
}

func TestEventsRefusesAnUnknownEnv(t *testing.T) {
	h := newHarness(t)
	err := h.m.Events(context.Background(), EventsRequest{App: "shop", Env: "nosuch"}, nil)
	if err == nil || !strings.Contains(err.Error(), "nosuch") {
		t.Errorf("Events of an unknown env = %v", err)
	}
}

func TestEventsLimitsTheBacklogToTheNewest(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	var all, three bytes.Buffer
	if err := h.m.Events(context.Background(), EventsRequest{}, &all); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Events(context.Background(), EventsRequest{Limit: 3}, &three); err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(three.String()), "\n")
	if len(got) != 3 {
		t.Fatalf("--limit 3 wrote %d lines:\n%s", len(got), three.String())
	}

	whole := strings.Split(strings.TrimSpace(all.String()), "\n")
	if got[2] != whole[len(whole)-1] {
		t.Errorf("the limited feed ends %q, the whole one %q", got[2], whole[len(whole)-1])
	}
}

func TestEventsDropsWhatIsOlderThanSince(t *testing.T) {
	h := newHarness(t)
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	h.m.Now = func() time.Time { return at }
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	var buf bytes.Buffer
	if err := h.m.Events(context.Background(),
		EventsRequest{Since: at.Add(time.Hour)}, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("a feed since an hour from now wrote:\n%s", buf.String())
	}
}

func TestEventsThroughAJSONWriter(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	var buf bytes.Buffer
	w := progress.New(&buf, progress.FormatJSON)
	if err := h.m.Events(context.Background(), EventsRequest{App: "shop", Env: "feat-x"}, w); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var e progress.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("%q is not one JSON object: %v", line, err)
		}
		if e.App != "shop" || e.Env != "feat-x" {
			t.Errorf("event %+v does not name its environment", e)
		}
		if e.Seq == 0 {
			t.Errorf("event %+v carries no sequence", e)
		}
	}
}

func TestEventsFollowsWithoutGapOrDuplicate(t *testing.T) {
	h := newHarness(t)
	hub := progress.NewHub()
	h.store.SetNotifier(hub)
	h.m.Notifier = hub
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var buf syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- h.m.Events(ctx, EventsRequest{App: "shop", Env: "feat-x", Follow: true}, &buf)
	}()

	waitFor(t, func() bool { return hub.Subscribers() == 1 })
	waitFor(t, func() bool { return strings.Contains(buf.String(), "ready") })
	rec, err := h.store.Env(ctx, "shop", "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	h.m.record(ctx, rec.ID, Event{Action: "deploy", Status: progress.StatusChanged, Detail: "after the tail began"})
	waitFor(t, func() bool { return strings.Contains(buf.String(), "after the tail began") })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Events(--follow) = %v", err)
	}
	if n := strings.Count(buf.String(), "after the tail began"); n != 1 {
		t.Errorf("the live event was written %d times:\n%s", n, buf.String())
	}

	seen := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		seen[line]++
	}
	for line, n := range seen {
		if n > 1 && !strings.Contains(line, "container") {
			t.Errorf("%q appears %d times", line, n)
		}
	}
}

func TestEventsFollowFilters(t *testing.T) {
	h := newHarness(t)
	hub := progress.NewHub()
	h.store.SetNotifier(hub)
	h.m.Notifier = hub
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-y"})
	x, _ := h.store.Env(context.Background(), "shop", "feat-x")
	y, _ := h.store.Env(context.Background(), "shop", "feat-y")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mine syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- h.m.Events(ctx, EventsRequest{App: "shop", Env: "feat-x", Follow: true, Limit: 1}, &mine)
	}()
	waitFor(t, func() bool { return hub.Subscribers() == 1 })

	h.m.record(ctx, y.ID, Event{Action: "deploy", Status: progress.StatusChanged, Detail: "not yours"})
	h.m.record(ctx, x.ID, Event{Action: "deploy", Status: progress.StatusChanged, Detail: "yours"})
	waitFor(t, func() bool { return strings.Contains(mine.String(), "yours") })
	cancel()
	<-done

	if strings.Contains(mine.String(), "not yours") {
		t.Errorf("the feed of feat-x carried feat-y's event:\n%s", mine.String())
	}
}

func TestEventsFollowWithNoNotifierEnds(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	var buf bytes.Buffer
	if err := h.m.Events(context.Background(), EventsRequest{Follow: true}, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Error("the backlog was not written")
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the condition never held")
}
