package edge

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	historyLimit = 512

	subscriberBuffer = 256
)

var ErrSubscriberTooSlow = errors.New("edge: subscriber fell behind and was dropped; subscribe again with a since")

type broker struct {
	mu      sync.Mutex
	next    int
	subs    map[int]*subscriber
	history []Event
	closed  bool
}

type subscriber struct {
	ch     chan Event
	lagged bool
}

func newBroker() *broker { return &broker{subs: map[int]*subscriber{}} }

func (b *broker) publish(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.history = append(b.history, e)
	if len(b.history) > historyLimit {
		b.history = append([]Event(nil), b.history[len(b.history)-historyLimit:]...)
	}
	for _, s := range b.subs {
		if s.lagged {
			continue
		}
		select {
		case s.ch <- e:
		default:
			s.lagged = true
			close(s.ch)
		}
	}
}

func (b *broker) since(t time.Time) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t.IsZero() {
		return nil
	}
	out := make([]Event, 0, len(b.history))
	for _, e := range b.history {
		if !e.At.Before(t) {
			out = append(out, e)
		}
	}
	return out
}

func (b *broker) subscribe(ctx context.Context, since time.Time, fn func(Event) error) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return errors.New("edge: the edge is shutting down")
	}
	replay := make([]Event, 0, len(b.history))
	if !since.IsZero() {
		for _, e := range b.history {
			if !e.At.Before(since) {
				replay = append(replay, e)
			}
		}
	}
	s := &subscriber{ch: make(chan Event, subscriberBuffer)}
	id := b.next
	b.next++
	b.subs[id] = s
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}()

	for _, e := range replay {
		if err := fn(e); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e, ok := <-s.ch:
			if !ok {
				b.mu.Lock()
				shuttingDown := b.closed
				b.mu.Unlock()
				if shuttingDown {
					return nil
				}
				return ErrSubscriberTooSlow
			}
			if err := fn(e); err != nil {
				return err
			}
		}
	}
}

func (b *broker) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, s := range b.subs {
		if !s.lagged {
			s.lagged = true
			close(s.ch)
		}
		delete(b.subs, id)
	}
}
