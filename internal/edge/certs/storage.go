package certs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
)

var errStoreClosed = errors.New("certs: the certificate store is closed")

const closeGrace = 10 * time.Second

type sealedStorage struct {
	certmagic.Storage

	mu     sync.Mutex
	cond   *sync.Cond
	held   int
	closed bool
}

func newSealedStorage(inner certmagic.Storage) *sealedStorage {
	s := &sealedStorage{Storage: inner}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *sealedStorage) Lock(ctx context.Context, name string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errStoreClosed
	}
	s.held++
	s.mu.Unlock()
	if err := s.Storage.Lock(ctx, name); err != nil {
		s.release()
		return err
	}
	return nil
}

func (s *sealedStorage) Unlock(ctx context.Context, name string) error {
	err := s.Storage.Unlock(ctx, name)
	s.release()
	return err
}

func (s *sealedStorage) release() {
	s.mu.Lock()
	if s.held > 0 {
		s.held--
	}
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *sealedStorage) Store(ctx context.Context, key string, value []byte) error {
	s.mu.Lock()
	refused := s.closed && s.held == 0
	s.mu.Unlock()
	if refused {
		return errStoreClosed
	}
	return s.Storage.Store(ctx, key, value)
}

func (s *sealedStorage) Close() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.held == 0 {
		return true
	}
	deadline := time.AfterFunc(closeGrace, s.cond.Broadcast)
	defer deadline.Stop()
	start := time.Now()
	for s.held > 0 && time.Since(start) < closeGrace {
		s.cond.Wait()
	}
	return s.held == 0
}
