package progress

import (
	"sync"
	"testing"
	"time"
)

func TestHubFansOutToEverySubscriber(t *testing.T) {
	h := NewHub()
	a, stopA := h.Subscribe(4)
	b, stopB := h.Subscribe(4)
	defer stopA()
	defer stopB()
	if h.Subscribers() != 2 {
		t.Fatalf("%d subscribers, want 2", h.Subscribers())
	}

	h.Notify(Event{Action: "deploy", Step: "switch", Status: StatusChanged})
	for name, ch := range map[string]<-chan Event{"a": a, "b": b} {
		select {
		case e := <-ch:
			if e.Action != "deploy" || e.Step != "switch" {
				t.Errorf("%s got %+v", name, e)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s received nothing", name)
		}
	}
}

func TestHubDropsForASlowReaderAndCounts(t *testing.T) {
	h := NewHub()
	slow, stop := h.Subscribe(2)
	defer stop()
	fast, stopFast := h.Subscribe(64)
	defer stopFast()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			h.Notify(Event{Action: "rollout", Replica: i, Status: StatusChanged})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Notify blocked on a reader that is not reading")
	}

	if got := h.Dropped(); got == 0 {
		t.Error("nothing was counted as dropped")
	}

	first := <-slow
	if first.Replica != 0 {
		t.Errorf("the slow reader's first event = %+v", first)
	}

	if n := len(fast); n != 20 {
		t.Errorf("the fast reader holds %d events, want 20", n)
	}
}

func TestHubUnsubscribeClosesOnce(t *testing.T) {
	h := NewHub()
	ch, stop := h.Subscribe(1)
	stop()
	stop()
	if _, open := <-ch; open {
		t.Error("the channel delivered an event after unsubscribe")
	}
	if h.Subscribers() != 0 {
		t.Errorf("%d subscribers after unsubscribe", h.Subscribers())
	}

	h.Notify(Event{Action: "x"})
}

func TestHubClose(t *testing.T) {
	h := NewHub()
	a, stopA := h.Subscribe(1)
	h.Close()
	h.Close()
	if _, open := <-a; open {
		t.Error("a subscriber survived Close")
	}

	stopA()
	h.Notify(Event{Action: "x"})
	after, stop := h.Subscribe(1)
	defer stop()
	if _, open := <-after; open {
		t.Error("subscribing to a closed hub produced an open channel")
	}
}

func TestNilHub(t *testing.T) {
	var h *Hub
	h.Notify(Event{Action: "x"})
	h.Close()
	if h.Subscribers() != 0 || h.Dropped() != 0 {
		t.Error("a nil hub counted something")
	}
	ch, stop := h.Subscribe(0)
	stop()
	for range ch {
		t.Error("a nil hub's channel is not closed")
	}
}

func TestHubIsConcurrencySafe(t *testing.T) {
	h := NewHub()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				h.Notify(Event{Action: "rollout", Replica: i, Status: StatusOK})
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, stop := h.Subscribe(8)
			go func() {
				for range ch {
				}
			}()
			time.Sleep(time.Millisecond)
			stop()
		}()
	}
	wg.Wait()
	h.Close()
}

func TestHubIsANotifier(t *testing.T) {
	var n Notifier = NewHub()
	ch, stop := n.Subscribe(DefaultBuffer)
	defer stop()
	n.Notify(Event{Action: "secret", Status: StatusWarning, Detail: "revealed"})
	select {
	case e := <-ch:
		if e.Detail != "revealed" {
			t.Errorf("= %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("nothing arrived")
	}
}
