package edge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/edge/certs"
	"github.com/plytz/caramelo/internal/testutil"
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

	prunes   []certs.PruneRequest
	pruneErr error
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

func (f *fakeHandler) Prune(_ context.Context, req certs.PruneRequest) (*certs.PruneResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pruneErr != nil {
		return nil, f.pruneErr
	}
	f.prunes = append(f.prunes, req)
	return &certs.PruneResult{
		KeepFor: req.Keep(),
		DryRun:  req.DryRun,
		Removed: []certs.Certificate{{
			Host: "feat-i.shop.internal", IssuerKey: "internal", State: certs.Stale,
		}},
	}, nil
}

func (f *fakeHandler) pruned() []certs.PruneRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]certs.PruneRequest{}, f.prunes...)
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

func TestAPruneCrossesTheControlSocket(t *testing.T) {
	h := newFakeHandler()
	c, _ := serveControl(t, h)

	keep := 48 * time.Hour
	res, err := c.Prune(t.Context(), certs.PruneRequest{KeepFor: &keep, DryRun: true})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	got := h.pruned()
	if len(got) != 1 || got[0].KeepFor == nil || *got[0].KeepFor != keep || !got[0].DryRun {
		t.Fatalf("the edge was asked %+v, want the retention and the dry run as they were sent", got)
	}
	if res.KeepFor != keep || !res.DryRun {
		t.Errorf("result = %+v, want the retention it ran with", res)
	}
	if len(res.Removed) != 1 || res.Removed[0].Host != "feat-i.shop.internal" {
		t.Fatalf("result = %+v, want the removed certificate", res)
	}
	if res.Removed[0].IssuerKey != "internal" || res.Removed[0].State != certs.Stale {
		t.Errorf("removed = %+v, want the issuer key and the state carried across", res.Removed[0])
	}

	none, err := c.Prune(t.Context(), certs.PruneRequest{})
	if err != nil {
		t.Fatalf("Prune with no retention: %v", err)
	}
	if none.KeepFor != certs.DefaultKeepStale {
		t.Errorf("a request with no retention ran with %s, want the default", none.KeepFor)
	}
}

func TestAPruneWithNoRequestIsAnAnswerAndNotAHangUp(t *testing.T) {
	h := newFakeHandler()
	c, path := serveControl(t, h)

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(`{"op":"prune"}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	sc := bufio.NewScanner(conn)
	if !sc.Scan() {
		t.Fatalf("the edge hung up on a prune with no request: %v", sc.Err())
	}
	var res Response
	if err := json.Unmarshal(sc.Bytes(), &res); err != nil {
		t.Fatalf("the answer is not a response: %v (%q)", err, sc.Text())
	}
	if res.OK || !strings.Contains(res.Error, "prune") {
		t.Errorf("answer = %+v, want a refusal naming the operation", res)
	}
	if len(h.pruned()) != 0 {
		t.Error("a prune with no request reached the edge anyway")
	}

	if _, err := c.Status(t.Context()); err != nil {
		t.Errorf("Status after a refusal: %v", err)
	}
}

func TestAPruneThatFailsIsTheEdgesReason(t *testing.T) {
	h := newFakeHandler()
	h.pruneErr = errors.New("the certificate store is closed")
	c, _ := serveControl(t, h)

	if _, err := c.Prune(t.Context(), certs.PruneRequest{}); err == nil ||
		!strings.Contains(err.Error(), "store is closed") {
		t.Errorf("Prune = %v, want the edge's reason", err)
	}
}
