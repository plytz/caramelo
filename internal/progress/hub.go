package progress

import (
	"sync"
)

type Hub struct {
	mu      sync.Mutex
	subs    map[int64]*subscriber
	next    int64
	dropped int64
	closed  bool
}

type subscriber struct {
	ch      chan Event
	dropped int64
}

func NewHub() *Hub { return &Hub{subs: map[int64]*subscriber{}} }

func (h *Hub) Notify(e Event) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	for _, s := range h.subs {
		select {
		case s.ch <- e:
		default:
			s.dropped++
			h.dropped++
		}
	}
}

func (h *Hub) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	if h == nil {
		ch := make(chan Event)
		close(ch)
		return ch, func() {}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		ch := make(chan Event)
		close(ch)
		return ch, func() {}
	}
	h.next++
	id := h.next
	s := &subscriber{ch: make(chan Event, buffer)}
	h.subs[id] = s
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if _, ok := h.subs[id]; !ok {

				return
			}
			delete(h.subs, id)
			close(s.ch)
		})
	}
}

func (h *Hub) Subscribers() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

func (h *Hub) Dropped() int64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dropped
}

func (h *Hub) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for id, s := range h.subs {
		delete(h.subs, id)
		close(s.ch)
	}
}

var _ Notifier = (*Hub)(nil)
