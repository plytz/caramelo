package progress

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

func (e Event) WithMachine(name string) Event {
	if e.Machine == "" {
		e.Machine = name
	}
	return e
}

const ActionFeed = "feed"

type Relay struct {
	dst     io.Writer
	machine string
	on      func(Event)

	mu   sync.Mutex
	buf  []byte
	done bool
}

const MaxLine = 1 << 20

func NewRelay(dst io.Writer, machine string, on func(Event)) *Relay {
	return &Relay{dst: dst, machine: machine, on: on}
}

func (r *Relay) Write(p []byte) (int, error) {
	if r == nil {
		return len(p), nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	for {
		i := bytes.IndexByte(r.buf, '\n')
		if i < 0 {
			if len(r.buf) > MaxLine {

				line := r.buf
				r.buf = nil
				if err := r.passThrough(line); err != nil {
					return 0, err
				}
			}
			return len(p), nil
		}
		line := r.buf[:i]
		r.buf = r.buf[i+1:]
		if err := r.line(line); err != nil {
			return 0, err
		}
	}
}

func (r *Relay) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return nil
	}
	r.done = true
	if len(r.buf) == 0 {
		return nil
	}
	line := r.buf
	r.buf = nil
	return r.line(line)
}

func (r *Relay) line(line []byte) error {
	e, ok := decodeEvent(line)
	if !ok {

		out := make([]byte, 0, len(line)+1)
		return r.passThrough(append(append(out, line...), '\n'))
	}
	e = e.WithMachine(r.machine)
	if r.on != nil {
		r.on(e)
	}
	if r.dst == nil {
		return nil
	}
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("relay the feed of machine %q: %w", r.machine, err)
	}
	if _, err := r.dst.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("relay the feed of machine %q: %w", r.machine, err)
	}
	return nil
}

func (r *Relay) passThrough(b []byte) error {
	if r.dst == nil || len(b) == 0 {
		return nil
	}
	if _, err := r.dst.Write(b); err != nil {
		return fmt.Errorf("relay the feed of machine %q: %w", r.machine, err)
	}
	return nil
}

func decodeEvent(line []byte) (Event, bool) {
	s := bytes.TrimSpace(line)
	if len(s) == 0 || s[0] != '{' {
		return Event{}, false
	}
	var e Event
	if err := json.Unmarshal(s, &e); err != nil {
		return Event{}, false
	}
	if e.Action == "" && e.Status == "" {
		return Event{}, false
	}
	return e, true
}

type Source interface {
	Follow(ctx context.Context, machine string) (io.ReadCloser, error)
}

type SourceFunc func(ctx context.Context, machine string) (io.ReadCloser, error)

func (f SourceFunc) Follow(ctx context.Context, machine string) (io.ReadCloser, error) {
	return f(ctx, machine)
}

const (
	RetryMin = 1 * time.Second
	RetryMax = 30 * time.Second
)

type Fleet struct {
	src  Source
	into Notifier

	Retry func(attempt int) time.Duration

	After func(d time.Duration) <-chan time.Time

	mu      sync.Mutex
	follows map[string]*follow
	closed  bool
}

type follow struct {
	cancel context.CancelFunc
	done   chan struct{}

	seq int64

	up    bool
	known bool
}

func NewFleet(src Source, into Notifier) *Fleet {
	return &Fleet{src: src, into: into, follows: map[string]*follow{}}
}

func (f *Fleet) Follow(ctx context.Context, machine string) {
	if f == nil || machine == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	if _, ok := f.follows[machine]; ok {
		return
	}
	fctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	fw := &follow{cancel: cancel, done: make(chan struct{})}
	f.follows[machine] = fw
	go f.run(fctx, machine, fw)
}

func (f *Fleet) Forget(machine string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	fw, ok := f.follows[machine]
	if ok {
		delete(f.follows, machine)
	}
	f.mu.Unlock()
	if !ok {
		return
	}
	fw.cancel()
	<-fw.done
}

func (f *Fleet) Set(ctx context.Context, machines []string) {
	if f == nil {
		return
	}
	want := make(map[string]bool, len(machines))
	for _, m := range machines {
		if m != "" {
			want[m] = true
		}
	}
	for _, m := range f.Machines() {
		if !want[m] {
			f.Forget(m)
		}
	}
	for m := range want {
		f.Follow(ctx, m)
	}
}

func (f *Fleet) Machines() []string {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.follows))
	for m := range f.follows {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func (f *Fleet) Close() {
	if f == nil {
		return
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	fws := make([]*follow, 0, len(f.follows))
	for m, fw := range f.follows {
		fws = append(fws, fw)
		delete(f.follows, m)
	}
	f.mu.Unlock()
	for _, fw := range fws {
		fw.cancel()
	}
	for _, fw := range fws {
		<-fw.done
	}
}

func (f *Fleet) run(ctx context.Context, machine string, fw *follow) {
	defer close(fw.done)
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return
		}
		connected, err := f.stream(ctx, machine, fw)
		if ctx.Err() != nil {
			return
		}
		if connected {

			attempt = 1
		}
		if err == nil {

			err = errors.New("the member closed the feed")
		}
		f.report(machine, fw, err)
		select {
		case <-ctx.Done():
			return
		case <-f.after(f.retry(attempt)):
		}
	}
}

func (f *Fleet) stream(ctx context.Context, machine string, fw *follow) (bool, error) {
	rc, err := f.src.Follow(ctx, machine)
	if err != nil {
		return false, fmt.Errorf("follow the feed of machine %q: %w", machine, err)
	}

	var once sync.Once
	closeOnce := func() { once.Do(func() { _ = rc.Close() }) }
	defer closeOnce()
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			closeOnce()
		case <-stopped:
		}
	}()
	f.report(machine, fw, nil)

	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLine)
	for sc.Scan() {
		e, ok := decodeEvent(sc.Bytes())
		if !ok {
			continue
		}
		f.publish(machine, fw, e)
	}
	if err := sc.Err(); err != nil {
		return true, fmt.Errorf("read the feed of machine %q: %w", machine, err)
	}
	return true, nil
}

func (f *Fleet) publish(machine string, fw *follow, e Event) {
	e = e.WithMachine(machine)
	f.mu.Lock()
	if e.Seq != 0 {
		if e.Seq <= fw.seq {
			f.mu.Unlock()
			return
		}
		fw.seq = e.Seq
	}
	f.mu.Unlock()
	if f.into != nil {
		f.into.Notify(e)
	}
}

func (f *Fleet) report(machine string, fw *follow, err error) {
	f.mu.Lock()
	was, known := fw.up, fw.known
	fw.up, fw.known = err == nil, true
	f.mu.Unlock()
	if known && was == (err == nil) {
		return
	}
	e := Event{Action: ActionFeed, Machine: machine, At: time.Now()}
	if err != nil {
		e.Status, e.Step, e.Detail = StatusWarning, "lost", err.Error()
	} else {
		e.Status, e.Step, e.Detail = StatusOK, "following", "the feed of "+machine+" is being carried"
	}
	if f.into != nil {
		f.into.Notify(e)
	}
}

func (f *Fleet) retry(attempt int) time.Duration {
	if f.Retry != nil {
		return f.Retry(attempt)
	}
	d := RetryMin << min(attempt-1, 16)
	if d > RetryMax || d <= 0 {
		d = RetryMax
	}
	return d
}

func (f *Fleet) after(d time.Duration) <-chan time.Time {
	if f.After != nil {
		return f.After(d)
	}
	return time.After(d)
}
