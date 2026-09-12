package progress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWithMachine(t *testing.T) {
	e := Event{Action: "up", Status: StatusChanged}
	if got := e.WithMachine("m1"); got.Machine != "m1" {
		t.Errorf("machine = %q, want m1", got.Machine)
	}
	if got := (Event{Machine: "m2"}).WithMachine("m1"); got.Machine != "m2" {
		t.Errorf("machine = %q: a hop must not rewrite where it happened", got.Machine)
	}
	if got := e.WithMachine(""); got.Machine != "" {
		t.Errorf("machine = %q, want empty: a machine of one writes M7's events", got.Machine)
	}
	if e.Machine != "" {
		t.Error("WithMachine must not modify the event it was called on")
	}
}

func TestRelayStampsAJSONStream(t *testing.T) {
	var out bytes.Buffer
	var seen []Event
	r := NewRelay(&out, "m1", func(e Event) { seen = append(seen, e) })

	events := []Event{
		{Seq: 1, App: "shop", Env: "feat-x", Action: "up", Status: StatusStarted},
		{Seq: 2, App: "shop", Env: "feat-x", Action: "container", Status: StatusChanged, Detail: "shop-feat-x-web-1"},
	}
	var stream bytes.Buffer
	enc := json.NewEncoder(&stream)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}

	b := stream.Bytes()
	for i := range b {
		if _, err := r.Write(b[i : i+1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	got := decodeAll(t, out.String())
	if len(got) != 2 {
		t.Fatalf("relayed %d events, want 2: %q", len(got), out.String())
	}
	for i, e := range got {
		if e.Machine != "m1" {
			t.Errorf("event %d machine = %q, want m1", i, e.Machine)
		}
		if e.Action != events[i].Action || e.Detail != events[i].Detail || e.Seq != events[i].Seq {
			t.Errorf("event %d = %+v, want %+v with a machine", i, e, events[i])
		}
	}
	if len(seen) != 2 || seen[1].Machine != "m1" {
		t.Errorf("the callback saw %+v", seen)
	}
}

func TestRelayPassesThroughWhatIsNotAnEvent(t *testing.T) {
	var out bytes.Buffer
	r := NewRelay(&out, "m1", nil)
	in := "warning: docker printed this\n" +
		`{"action":"up","status":"ok"}` + "\n" +
		"[changed] rollout: web-1 flip: active\n"
	if _, err := io.WriteString(r, in); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines: %q", len(lines), out.String())
	}
	if lines[0] != "warning: docker printed this" || lines[2] != "[changed] rollout: web-1 flip: active" {
		t.Errorf("plain lines were not passed through: %q", out.String())
	}
	if !strings.Contains(lines[1], `"machine":"m1"`) {
		t.Errorf("the event was not stamped: %q", lines[1])
	}
}

func TestRelayFlushesOnClose(t *testing.T) {
	var out bytes.Buffer
	r := NewRelay(&out, "m2", nil)
	if _, err := io.WriteString(r, `{"action":"deploy","status":"failed"}`); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Error("a line with no newline must be held, not guessed at")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	got := decodeAll(t, out.String())
	if len(got) != 1 || got[0].Machine != "m2" || got[0].Status != StatusFailed {
		t.Errorf("after Close: %+v (%q)", got, out.String())
	}

	before := out.Len()
	if err := r.Close(); err != nil || out.Len() != before {
		t.Errorf("second Close wrote %d bytes: %v", out.Len()-before, err)
	}
}

func TestRelayWithNoDestination(t *testing.T) {
	var seen []Event
	r := NewRelay(nil, "m1", func(e Event) { seen = append(seen, e) })
	if _, err := io.WriteString(r, `{"action":"up","status":"ok"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Machine != "m1" {
		t.Errorf("seen = %+v", seen)
	}
}

type fakeSource struct {
	mu       sync.Mutex
	attempts map[string]int
	writers  map[string]*io.PipeWriter
	fail     map[string]error
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		attempts: map[string]int{},
		writers:  map[string]*io.PipeWriter{},
		fail:     map[string]error{},
	}
}

func (s *fakeSource) Follow(_ context.Context, machine string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts[machine]++
	if err := s.fail[machine]; err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	s.writers[machine] = pw
	return pr, nil
}

func (s *fakeSource) send(t *testing.T, machine string, e Event) {
	t.Helper()
	s.mu.Lock()
	w := s.writers[machine]
	s.mu.Unlock()
	if w == nil {
		t.Fatalf("no open stream for %s", machine)
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		t.Fatalf("write to %s: %v", machine, err)
	}
}

func (s *fakeSource) end(machine string) {
	s.mu.Lock()
	w := s.writers[machine]
	delete(s.writers, machine)
	s.mu.Unlock()
	if w != nil {
		_ = w.Close()
	}
}

func (s *fakeSource) attemptsOf(machine string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[machine]
}

func (s *fakeSource) setFail(machine string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		delete(s.fail, machine)
		return
	}
	s.fail[machine] = err
}

func (s *fakeSource) waitOpen(t *testing.T, machine string) {
	t.Helper()
	s.until(t, "a stream to "+machine, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.writers[machine] != nil
	})
}

func (s *fakeSource) waitAttempts(t *testing.T, machine string, n int) {
	t.Helper()
	s.until(t, fmt.Sprintf("%d attempts at %s", n, machine), func() bool {
		return s.attemptsOf(machine) >= n
	})
}

func (s *fakeSource) until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("waited for %s and it never happened", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFleetCarriesEveryMachine(t *testing.T) {
	src := newFakeSource()
	hub := NewHub()
	defer hub.Close()
	ch, stop := hub.Subscribe(16)
	defer stop()

	f := NewFleet(src, hub)
	defer f.Close()
	f.Follow(context.Background(), "m1")
	f.Follow(context.Background(), "m2")
	src.waitOpen(t, "m1")
	src.waitOpen(t, "m2")

	src.send(t, "m1", Event{Seq: 7, App: "shop", Env: "feat-x", Action: "up", Status: StatusChanged})
	src.send(t, "m2", Event{Seq: 3, App: "shop", Env: "prod", Action: "deploy", Status: StatusStarted})

	byMachine := map[string]Event{}
	for len(byMachine) < 2 {
		e := recv(t, ch)
		if e.Action == ActionFeed {
			continue
		}
		byMachine[e.Machine] = e
	}
	if e := byMachine["m1"]; e.Action != "up" || e.Env != "feat-x" || e.Seq != 7 {
		t.Errorf("m1 event = %+v", e)
	}
	if e := byMachine["m2"]; e.Action != "deploy" || e.Env != "prod" {
		t.Errorf("m2 event = %+v", e)
	}
	if got := f.Machines(); len(got) != 2 || got[0] != "m1" || got[1] != "m2" {
		t.Errorf("Machines = %v", got)
	}

	f.Follow(context.Background(), "m1")
	if got := f.Machines(); len(got) != 2 {
		t.Errorf("Machines after a second Follow = %v", got)
	}
}

func TestFleetReconnectsAndDropsTheOverlap(t *testing.T) {
	src := newFakeSource()
	hub := NewHub()
	defer hub.Close()
	ch, stop := hub.Subscribe(32)
	defer stop()

	f := NewFleet(src, hub)
	f.Retry = func(int) time.Duration { return 0 }
	defer f.Close()
	f.Follow(context.Background(), "m1")
	src.waitOpen(t, "m1")

	src.send(t, "m1", Event{Seq: 1, Action: "create", Status: StatusChanged})
	src.send(t, "m1", Event{Seq: 2, Action: "up", Status: StatusChanged})
	if e := nextReal(t, ch); e.Seq != 1 {
		t.Fatalf("first event = %+v", e)
	}
	if e := nextReal(t, ch); e.Seq != 2 {
		t.Fatalf("second event = %+v", e)
	}

	src.end("m1")
	src.waitOpen(t, "m1")
	src.send(t, "m1", Event{Seq: 1, Action: "create", Status: StatusChanged})
	src.send(t, "m1", Event{Seq: 2, Action: "up", Status: StatusChanged})
	src.send(t, "m1", Event{Seq: 3, Action: "deploy", Status: StatusStarted})
	if e := nextReal(t, ch); e.Seq != 3 {
		t.Fatalf("after the reconnect the next event was %+v, want seq 3 only", e)
	}
	if got := src.attemptsOf("m1"); got < 2 {
		t.Errorf("attempts = %d, want the feed to have been opened again", got)
	}
}

func TestFleetPassesUnrecordedEvents(t *testing.T) {
	src := newFakeSource()
	hub := NewHub()
	defer hub.Close()
	ch, stop := hub.Subscribe(16)
	defer stop()
	f := NewFleet(src, hub)
	defer f.Close()
	f.Follow(context.Background(), "m1")
	src.waitOpen(t, "m1")

	src.send(t, "m1", Event{Seq: 9, Action: "up", Status: StatusStarted})
	src.send(t, "m1", Event{Action: "message", Status: StatusWarning, Detail: "one"})
	src.send(t, "m1", Event{Action: "message", Status: StatusWarning, Detail: "two"})
	if e := nextReal(t, ch); e.Seq != 9 {
		t.Fatalf("first = %+v", e)
	}
	for _, want := range []string{"one", "two"} {
		e := nextReal(t, ch)
		if e.Detail != want || e.Machine != "m1" {
			t.Errorf("got %+v, want %q from m1", e, want)
		}
	}
}

func TestFleetReportsLostAndRegained(t *testing.T) {
	src := newFakeSource()
	src.setFail("m1", errors.New("no route to host"))
	hub := NewHub()
	defer hub.Close()
	ch, stop := hub.Subscribe(32)
	defer stop()

	f := NewFleet(src, hub)
	f.Retry = func(int) time.Duration { return 0 }
	defer f.Close()
	f.Follow(context.Background(), "m1")

	e := recv(t, ch)
	if e.Action != ActionFeed || e.Status != StatusWarning || e.Machine != "m1" {
		t.Fatalf("first event = %+v, want the feed being lost", e)
	}
	if !strings.Contains(e.Detail, "no route to host") {
		t.Errorf("detail = %q, want the reason in it", e.Detail)
	}

	src.waitAttempts(t, "m1", 3)
	src.setFail("m1", nil)

	e = recv(t, ch)
	if e.Action != ActionFeed || e.Status != StatusOK {
		t.Fatalf("next event = %+v, want the feed being regained and nothing in between", e)
	}
}

func TestFleetForgetEndsASilentFollow(t *testing.T) {
	src := newFakeSource()
	f := NewFleet(src, nil)
	f.Follow(context.Background(), "m1")
	src.waitOpen(t, "m1")

	done := make(chan struct{})
	go func() {
		f.Forget("m1")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Forget did not end a follow whose member had nothing to say")
	}
	if got := f.Machines(); len(got) != 0 {
		t.Errorf("Machines = %v, want none", got)
	}

	f.Forget("m1")
	f.Close()

	f.Follow(context.Background(), "m2")
	if got := f.Machines(); len(got) != 0 {
		t.Errorf("Machines after Close = %v", got)
	}
}

func TestFleetSetDeclaresTheWholeSet(t *testing.T) {
	src := newFakeSource()
	hub := NewHub()
	defer hub.Close()
	ch, stop := hub.Subscribe(32)
	defer stop()
	f := NewFleet(src, hub)
	defer f.Close()

	ctx := context.Background()
	f.Set(ctx, []string{"m1", "m2"})
	src.waitOpen(t, "m1")
	src.waitOpen(t, "m2")
	src.send(t, "m1", Event{Seq: 4, Action: "up", Status: StatusChanged})
	if e := nextReal(t, ch); e.Seq != 4 {
		t.Fatalf("first = %+v", e)
	}

	f.Set(ctx, []string{"m1", "m3", ""})
	src.waitOpen(t, "m3")
	if got := f.Machines(); len(got) != 2 || got[0] != "m1" || got[1] != "m3" {
		t.Fatalf("Machines = %v, want m1 and m3", got)
	}
	if n := src.attemptsOf("m1"); n != 1 {
		t.Errorf("m1 was opened %d times: a machine in both sets keeps its stream", n)
	}

	src.send(t, "m1", Event{Seq: 4, Action: "up", Status: StatusChanged})
	src.send(t, "m1", Event{Seq: 5, Action: "down", Status: StatusChanged})
	if e := nextReal(t, ch); e.Seq != 5 {
		t.Errorf("next = %+v, want seq 5: the overlap must still be dropped", e)
	}
}

func TestFleetRetryBackoff(t *testing.T) {
	f := NewFleet(nil, nil)
	if d := f.retry(1); d != RetryMin {
		t.Errorf("first retry = %v, want %v", d, RetryMin)
	}
	if d := f.retry(2); d != 2*RetryMin {
		t.Errorf("second retry = %v", d)
	}
	for _, n := range []int{6, 20, 1 << 20} {
		if d := f.retry(n); d != RetryMax {
			t.Errorf("retry %d = %v, want it bounded at %v", n, d, RetryMax)
		}
	}
}

func TestFleetWithNoNotifier(t *testing.T) {
	src := newFakeSource()
	f := NewFleet(src, nil)
	defer f.Close()
	f.Follow(context.Background(), "m1")
	src.waitOpen(t, "m1")
	src.send(t, "m1", Event{Seq: 1, Action: "up", Status: StatusOK})

}

func decodeAll(t *testing.T, s string) []Event {
	t.Helper()
	var out []Event
	dec := json.NewDecoder(strings.NewReader(s))
	for {
		var e Event
		err := dec.Decode(&e)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("decode %q: %v", s, err)
		}
		out = append(out, e)
	}
}

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("the feed was closed")
		}
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no event arrived")
		return Event{}
	}
}

func nextReal(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	for {
		e := recv(t, ch)
		if e.Action != ActionFeed {
			return e
		}
	}
}
