package certs

import (
	"context"
	"errors"
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
