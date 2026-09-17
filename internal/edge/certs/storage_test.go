package certs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
)

func TestClosedStoreRefusesNewLocks(t *testing.T) {
	s := newSealedStorage(&certmagic.FileStorage{Path: t.TempDir()})
	ctx := context.Background()
	if err := s.Lock(ctx, "before"); err != nil {
		t.Fatalf("Lock before Close: %v", err)
	}
	if err := s.Unlock(ctx, "before"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if !s.Close() {
		t.Fatal("Close with nothing held should report a quiet store")
	}
	if err := s.Lock(ctx, "after"); !errors.Is(err, errStoreClosed) {
		t.Errorf("Lock after Close = %v, want errStoreClosed", err)
	}
	if err := s.Store(ctx, "after.crt", []byte("x")); !errors.Is(err, errStoreClosed) {
		t.Errorf("Store after Close = %v, want errStoreClosed", err)
	}
}

func TestCloseWaitsForAWriterThatHoldsALock(t *testing.T) {
	s := newSealedStorage(&certmagic.FileStorage{Path: t.TempDir()})
	ctx := context.Background()
	if err := s.Lock(ctx, "renewing"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	closed := make(chan bool, 1)
	go func() { closed <- s.Close() }()
	select {
	case <-closed:
		t.Fatal("Close returned while a lock was held")
	case <-time.After(100 * time.Millisecond):
	}

	if err := s.Store(ctx, "renewing.crt", []byte("x")); err != nil {
		t.Errorf("Store while holding a lock across Close: %v", err)
	}
	if err := s.Unlock(ctx, "renewing"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	select {
	case quiet := <-closed:
		if !quiet {
			t.Error("Close reported a store that was not quiet")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after the lock was released")
	}
}

type lockCounter struct {
	certmagic.Storage

	mu    sync.Mutex
	taken []string
}

func (c *lockCounter) Lock(ctx context.Context, name string) error {
	c.mu.Lock()
	c.taken = append(c.taken, name)
	c.mu.Unlock()
	return c.Storage.Lock(ctx, name)
}

func (c *lockCounter) locks() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.taken...)
}

func countingManager(t *testing.T, m *manager) *lockCounter {
	t.Helper()
	counter := &lockCounter{Storage: m.storage.Storage}
	m.storage = newSealedStorage(counter)
	return counter
}

func TestAPruneWithNothingToDoTakesNoLock(t *testing.T) {
	s := threeTreeStore(t)
	counter := countingManager(t, s.live)
	ctx := context.Background()

	if _, err := s.live.Prune(ctx, PruneRequest{DryRun: true, KeepFor: KeepFor(0)}); err != nil {
		t.Fatalf("Prune --dry-run: %v", err)
	}
	if got := counter.locks(); len(got) != 0 {
		t.Errorf("a dry run took %v; it writes nothing, so it needs no lock", got)
	}

	if _, err := s.live.Prune(ctx, PruneRequest{KeepFor: KeepFor(0)}); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got := counter.locks(); len(got) != 1 || got[0] != pruneLockName {
		t.Fatalf("a prune with work took %v, want exactly %q once", got, pruneLockName)
	}

	if _, err := s.live.Prune(ctx, PruneRequest{KeepFor: KeepFor(0)}); err != nil {
		t.Fatalf("a second Prune: %v", err)
	}
	if got := counter.locks(); len(got) != 1 {
		t.Errorf("a prune with nothing left to remove took %v; a stale lock file must not stall it", got)
	}
}

func TestAPruneWithWorkIsRefusedWhileTheStoreIsClosing(t *testing.T) {
	s := threeTreeStore(t)
	if err := s.live.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := s.live.Prune(context.Background(), PruneRequest{KeepFor: KeepFor(0)})
	if !errors.Is(err, errStoreClosed) {
		t.Errorf("Prune on a closed store = %v, want errStoreClosed", err)
	}
	if !exists(t, s.firstSite) {
		t.Errorf("%s was removed by a prune the store refused", s.firstSite)
	}
}
