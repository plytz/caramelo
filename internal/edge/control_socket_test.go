package edge

import (
	"context"
	"errors"
	"github.com/plytz/caramelo/internal/testutil"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/edge/certs"
)

type fakeHandler struct {
	mu      sync.Mutex
	tables  []Table
	pushErr error
	status  Status
	ca      *certs.CA
	events  chan Event

	counts    *Counts
	countsErr error
}

func newFakeHandler() *fakeHandler {
	return &fakeHandler{events: make(chan Event, 8), status: Status{Running: true, Version: "test"}}
}

func (f *fakeHandler) PushTable(_ context.Context, t Table) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pushErr != nil {
		return f.pushErr
	}
	f.tables = append(f.tables, t)
	return nil
}

func (f *fakeHandler) Status(context.Context) (*Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.status
	return &s, nil
}

func (f *fakeHandler) Subscribe(ctx context.Context, _ time.Time, fn func(Event) error) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e := <-f.events:
			if err := fn(e); err != nil {
				return err
			}
		}
	}
}

func (f *fakeHandler) Counts(_ context.Context, since time.Time) (*Counts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.countsErr != nil {
		return nil, f.countsErr
	}
	if f.counts != nil {
		out := *f.counts
		out.Since = since
		return &out, nil
	}
	return &Counts{Since: since}, nil
}

func (f *fakeHandler) CA(context.Context) (*certs.CA, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ca == nil {
		return nil, errors.New("this machine issues from a public CA; there is nothing to trust by hand")
	}
	return f.ca, nil
}

func (f *fakeHandler) pushed() []Table {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Table(nil), f.tables...)
}

func serveControl(t *testing.T, h Handler) (Client, string) {
	t.Helper()
	path := filepath.Join(testutil.ShortDir(t), "edge.sock")
	ln, err := ListenSocket(path)
	if err != nil {
		t.Fatalf("ListenSocket: %v", err)
	}
	srv := NewServer(h)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		srv.Close()
		<-done
	})
	return NewClient(path), path
}

func TestCaramelodPushesATableAndAsksWhatTheEdgeIsDoing(t *testing.T) {
	h := newFakeHandler()
	h.status.Routes = []Route{route("feat-x.shop.test", Target{Replica: 1, Port: 20002, State: TargetActive})}
	c, _ := serveControl(t, h)

	table := Table{UpdatedAt: time.Now().UTC().Truncate(time.Second), Routes: []Route{
		route("feat-x.shop.test", Target{Replica: 1, Port: 20002, State: TargetActive}),
	}}
	if err := c.PushTable(t.Context(), table); err != nil {
		t.Fatalf("PushTable: %v", err)
	}
	got := h.pushed()
	if len(got) != 1 || len(got[0].Routes) != 1 || got[0].Routes[0].Host != "feat-x.shop.test" {
		t.Fatalf("the edge received %+v", got)
	}
	if !got[0].UpdatedAt.Equal(table.UpdatedAt) {
		t.Errorf("UpdatedAt = %v, want %v", got[0].UpdatedAt, table.UpdatedAt)
	}

	st, err := c.Status(t.Context())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Running || st.Version != "test" || len(st.Routes) != 1 {
		t.Errorf("Status = %+v", st)
	}
}

func TestARefusedRequestIsAnAnswerAndNotAHangUp(t *testing.T) {
	h := newFakeHandler()
	h.pushErr = errors.New("route feat-x.shop.test: replica 1: port 0 is out of range")
	c, path := serveControl(t, h)

	err := c.PushTable(t.Context(), Table{Routes: []Route{route("feat-x.shop.test",
		Target{Replica: 1, Port: 20002, State: TargetActive})}})
	if err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("PushTable = %v, want the edge's reason", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want it to name the socket", err)
	}

	if _, err := c.Status(t.Context()); err != nil {
		t.Errorf("Status after a refusal: %v", err)
	}

	if _, err := c.CA(t.Context()); err == nil {
		t.Error("CA in acme mode = nil, want an error")
	}
}

func TestSubscribeStreamsUntilTheCallerHangsUp(t *testing.T) {
	h := newFakeHandler()
	c, _ := serveControl(t, h)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	seen := make(chan Event, 4)
	done := make(chan error, 1)
	go func() {
		done <- c.Subscribe(ctx, time.Time{}, func(e Event) error { seen <- e; return nil })
	}()

	target := Target{Replica: 1, Port: 20002, State: TargetDraining}
	h.events <- Event{Kind: EventDrained, Host: "feat-x.shop.test", Target: &target, At: time.Now()}
	got := collect(t, seen, 1, 2*time.Second)[0]
	if got.Kind != EventDrained || got.Target == nil || got.Target.Replica != 1 {
		t.Fatalf("event = %+v", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Subscribe = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe did not return when its context was cancelled")
	}
}

func TestSubscribeStopsWhenTheCallerSaysSo(t *testing.T) {
	h := newFakeHandler()
	c, _ := serveControl(t, h)

	stop := errors.New("that is all I needed")
	target := Target{Replica: 1, Port: 20002, State: TargetDraining}
	h.events <- Event{Kind: EventDrained, Target: &target, At: time.Now()}
	err := c.Subscribe(t.Context(), time.Time{}, func(Event) error { return stop })
	if !errors.Is(err, stop) {
		t.Errorf("Subscribe = %v, want the caller's own error unchanged", err)
	}
}

func TestAClientOfAnEdgeThatIsNotThereSaysWhichSocket(t *testing.T) {
	path := filepath.Join(testutil.ShortDir(t), "edge.sock")
	c := NewClient(path)
	err := c.PushTable(t.Context(), Table{})
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("PushTable = %v, want an error naming the socket", err)
	}
	if _, err := c.Status(t.Context()); err == nil {
		t.Error("Status = nil on a machine with no edge")
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}

func TestTheControlSocketIsTheDaemonUsersAlone(t *testing.T) {
	path := filepath.Join(testutil.ShortDir(t), "edge.sock")
	ln, err := ListenSocket(path)
	if err != nil {
		t.Fatalf("ListenSocket: %v", err)
	}
	defer ln.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != fs.FileMode(socketFileMode) {
		t.Errorf("mode = %v, want %v: whoever can open it can route the machine's traffic", perm, socketFileMode)
	}

	ln.Close()
	ln2, err := ListenSocket(path)
	if err != nil {
		t.Fatalf("ListenSocket over a stale socket: %v", err)
	}
	ln2.Close()
}
