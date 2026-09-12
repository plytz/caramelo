package edge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
)

type ingressKey struct{}

type ingressState struct{ trusted bool }

func fromIngress(ctx context.Context) (trusted, ok bool) {
	st, ok := ctx.Value(ingressKey{}).(ingressState)
	if !ok {
		return false, false
	}
	return st.trusted, true
}

type ingress struct {
	conf   Ingress
	ln     net.Listener
	server *http.Server

	requests atomic.Int64
	inflight atomic.Int64
}

func (i *ingress) addr() string {
	if i == nil || i.ln == nil {
		return ""
	}
	return i.ln.Addr().String()
}

func (i *ingress) status() *IngressStatus {
	if i == nil {
		return nil
	}
	return &IngressStatus{
		Enabled:  true,
		Addr:     i.addr(),
		Requests: i.requests.Load(),
		Inflight: int(i.inflight.Load()),
	}
}

func (e *Edge) ingressHandler(in *ingress) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		in.requests.Add(1)
		in.inflight.Add(1)
		defer in.inflight.Add(-1)
		ctx := context.WithValue(r.Context(), ingressKey{}, ingressState{trusted: len(in.conf.Trusted) > 0})
		e.proxy.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (e *Edge) setIngress(want *Ingress) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.setIngressLocked(want)
}

func (e *Edge) setIngressLocked(want *Ingress) error {
	if want == nil || !want.Enabled {
		e.closeIngressLocked()
		return nil
	}
	port := ingressPortOf(*want)
	if cur := e.ingress; cur != nil {
		if ingressPortOf(cur.conf) == port {

			cur.conf = *want
			return nil
		}
		e.closeIngressLocked()
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(ingressHost, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("edge: open the private ingress on %s:%d: %w", ingressHost, port, err)
	}
	in := &ingress{conf: *want, ln: ln}
	in.server = &http.Server{
		Handler:           e.ingressHandler(in),
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          log.New(prefixed{e.logw, "edge: ingress: "}, "", 0),
	}
	e.ingress = in
	go func() {
		if err := in.server.Serve(ln); err != nil && !isClosed(err) {
			e.logf("private ingress: %v", err)
		}
	}()
	e.logf("private ingress on %s, trusting %v", ln.Addr(), in.conf.Trusted)
	return nil
}

func (e *Edge) ingressAddr() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ingress.addr()
}

func ingressPortOf(i Ingress) int {
	if i.Port == 0 {
		return IngressPort
	}
	return i.Port
}

func (e *Edge) closeIngressLocked() {
	if e.ingress == nil {
		return
	}
	in := e.ingress
	e.ingress = nil
	if in.server != nil {
		_ = in.server.Close()
	} else if in.ln != nil {
		_ = in.ln.Close()
	}
	e.logf("private ingress closed")
}

const ingressHost = "127.0.0.1"

func isClosed(err error) bool {
	return errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed)
}
