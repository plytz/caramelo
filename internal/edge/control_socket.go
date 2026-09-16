package edge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/plytz/caramelo/internal/edge/certs"
)

const (
	socketFileMode fs.FileMode = 0o600

	controlLineLimit = 8 << 20

	controlWriteTimeout = 10 * time.Second
)

func ListenSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("edge: %s: %w", filepath.Dir(path), err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("edge: %s: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("edge: listen %s: %w", path, err)
	}
	if err := os.Chmod(path, socketFileMode); err != nil {
		ln.Close()
		return nil, fmt.Errorf("edge: %s: %w", path, err)
	}
	return ln, nil
}

func NewServer(h Handler) Server {
	return &controlServer{h: h, conns: map[net.Conn]struct{}{}}
}

type controlServer struct {
	h Handler

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	ln     net.Listener
}

var _ Server = (*controlServer)(nil)

func (s *controlServer) Serve(ctx context.Context, ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("edge: control server is closed")
	}
	s.ln = ln
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || s.isClosed() {
				return nil
			}
			return fmt.Errorf("edge: control socket: %w", err)
		}
		s.track(conn)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer s.untrack(conn)
			s.handle(ctx, conn)
		}()
	}
}

func (s *controlServer) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ln := s.ln
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.conns = map[net.Conn]struct{}{}
	s.mu.Unlock()

	if ln != nil {
		ln.Close()
	}
	for _, c := range conns {
		c.Close()
	}
	return nil
}

func (s *controlServer) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *controlServer) track(c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		c.Close()
		return
	}
	s.conns[c] = struct{}{}
}

func (s *controlServer) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	c.Close()
}

func (s *controlServer) handle(ctx context.Context, conn net.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	var wmu sync.Mutex
	write := func(r Response) error {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		wmu.Lock()
		defer wmu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(controlWriteTimeout))
		defer conn.SetWriteDeadline(time.Time{})
		if _, err := conn.Write(append(b, '\n')); err != nil {
			return err
		}
		return nil
	}

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64<<10), controlLineLimit)
	for sc.Scan() {
		var req Request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			if werr := write(Response{Error: fmt.Sprintf("edge: bad request: %v", err)}); werr != nil {
				return
			}
			continue
		}
		if err := s.dispatch(ctx, req, write); err != nil {
			return
		}
	}
}

func (s *controlServer) dispatch(ctx context.Context, req Request, write func(Response) error) error {
	switch req.Op {
	case OpPushTable:
		if req.Table == nil {
			return write(Response{Error: "edge: push-table without a table"})
		}
		if err := s.h.PushTable(ctx, *req.Table); err != nil {
			return write(Response{Error: err.Error()})
		}
		return write(Response{OK: true})
	case OpStatus:
		st, err := s.h.Status(ctx)
		if err != nil {
			return write(Response{Error: err.Error()})
		}
		return write(Response{OK: true, Status: st})
	case OpCA:
		ca, err := s.h.CA(ctx)
		if err != nil {
			return write(Response{Error: err.Error()})
		}
		return write(Response{OK: true, CA: ca})
	case OpCounts:
		counts, err := s.h.Counts(ctx, req.Since)
		if err != nil {
			return write(Response{Error: err.Error()})
		}
		return write(Response{OK: true, Counts: counts})
	case OpPrune:
		if req.Prune == nil {
			return write(Response{Error: "edge: prune without a request"})
		}
		pruned, err := s.h.Prune(ctx, *req.Prune)
		if err != nil {
			return write(Response{Error: err.Error()})
		}
		return write(Response{OK: true, Pruned: pruned})
	case OpSubscribe:
		err := s.h.Subscribe(ctx, req.Since, func(e Event) error {
			ev := e
			return write(Response{OK: true, Event: &ev})
		})
		if err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
			_ = write(Response{Error: err.Error()})
		}

		return io.EOF
	default:
		return write(Response{Error: fmt.Sprintf("edge: unknown operation %q", req.Op)})
	}
}

func NewClient(path string) Client { return &socketClient{path: path} }

type socketClient struct{ path string }

var _ Client = (*socketClient)(nil)

func (c *socketClient) wrap(err error) error {
	return fmt.Errorf("edge control socket %s: %w", c.path, err)
}

func (c *socketClient) dial(ctx context.Context) (net.Conn, error) {
	d := net.Dialer{Timeout: DialTimeout}
	conn, err := d.DialContext(ctx, "unix", c.path)
	if err != nil {
		return nil, c.wrap(err)
	}
	return conn, nil
}

func (c *socketClient) send(ctx context.Context, req Request) (*Response, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	b, err := json.Marshal(req)
	if err != nil {
		return nil, c.wrap(err)
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, c.wrap(err)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64<<10), controlLineLimit)
	if !sc.Scan() {
		err := sc.Err()
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return nil, c.wrap(err)
	}
	var res Response
	if err := json.Unmarshal(sc.Bytes(), &res); err != nil {
		return nil, c.wrap(err)
	}
	if !res.OK {
		return nil, c.wrap(errors.New(nonEmpty(res.Error, "refused without a reason")))
	}
	return &res, nil
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func (c *socketClient) PushTable(ctx context.Context, t Table) error {
	if err := t.Validate(); err != nil {
		return err
	}
	_, err := c.send(ctx, Request{Op: OpPushTable, Table: &t})
	return err
}

func (c *socketClient) Status(ctx context.Context) (*Status, error) {
	res, err := c.send(ctx, Request{Op: OpStatus})
	if err != nil {
		return nil, err
	}
	if res.Status == nil {
		return nil, c.wrap(errors.New("no status in the answer"))
	}
	return res.Status, nil
}

func (c *socketClient) CA(ctx context.Context) (*certs.CA, error) {
	res, err := c.send(ctx, Request{Op: OpCA})
	if err != nil {
		return nil, err
	}
	if res.CA == nil {
		return nil, c.wrap(errors.New("no CA in the answer"))
	}
	return res.CA, nil
}

func (c *socketClient) Counts(ctx context.Context, since time.Time) (*Counts, error) {
	res, err := c.send(ctx, Request{Op: OpCounts, Since: since})
	if err != nil {
		return nil, err
	}
	if res.Counts == nil {
		return nil, c.wrap(errors.New("no counts in the answer"))
	}
	return res.Counts, nil
}

func (c *socketClient) Prune(ctx context.Context, req certs.PruneRequest) (*certs.PruneResult, error) {
	res, err := c.send(ctx, Request{Op: OpPrune, Prune: &req})
	if err != nil {
		return nil, err
	}
	if res.Pruned == nil {
		return nil, c.wrap(errors.New("no result in the answer"))
	}
	return res.Pruned, nil
}

func (c *socketClient) Subscribe(ctx context.Context, since time.Time, fn func(Event) error) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	b, err := json.Marshal(Request{Op: OpSubscribe, Since: since})
	if err != nil {
		return c.wrap(err)
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return c.wrap(err)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64<<10), controlLineLimit)
	for sc.Scan() {
		var res Response
		if err := json.Unmarshal(sc.Bytes(), &res); err != nil {
			return c.wrap(err)
		}
		if !res.OK {
			return c.wrap(errors.New(nonEmpty(res.Error, "the subscription was refused")))
		}
		if res.Event == nil {
			continue
		}
		if err := fn(*res.Event); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return c.wrap(err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func (c *socketClient) Close() error { return nil }
