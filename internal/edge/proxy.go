package edge

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

const RedirectStatus = http.StatusPermanentRedirect

const (
	targetDialTimeout   = 2 * time.Second
	targetIdleTimeout   = 90 * time.Second
	targetKeepAlive     = 30 * time.Second
	targetMaxIdleConns  = 32
	targetTLSHandshake  = 5 * time.Second
	targetExpectTimeout = 1 * time.Second
)

type proxyOptions struct {
	router *Router

	now func() time.Time

	access func(AccessLog)

	logf func(string, ...any)

	altSvc func(http.Header) error
}

type Proxy struct {
	opts proxyOptions
	now  func() time.Time

	mu         sync.Mutex
	transports map[string]*http.Transport
}

func NewProxy(o proxyOptions) *Proxy {
	now := o.now
	if now == nil {
		now = time.Now
	}
	return &Proxy{opts: o, now: now, transports: map[string]*http.Transport{}}
}

type dialStateKey struct{}

type dialState struct {
	mu     sync.Mutex
	failed bool
	err    error
}

func (d *dialState) fail(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failed, d.err = true, err
}

func (d *dialState) result() (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.failed, d.err
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := p.now()
	host := NormalizeHost(r.Host)
	entry := AccessLog{
		Host:      host,
		Method:    r.Method,
		Path:      r.URL.RequestURI(),
		Proto:     protoName(r),
		Client:    clientAddr(r),
		WebSocket: isUpgrade(r),
	}

	if p.opts.altSvc != nil && r.ProtoMajor < 3 {
		_ = p.opts.altSvc(w.Header())
	}

	route, pool, ok := p.opts.router.Lookup(host)
	if !ok {

		entry.Status = http.StatusNotFound
		entry.Error = "no route for this host"
		http.Error(w, "404 no environment is exposed under this name\n", http.StatusNotFound)
		p.finish(entry, start, 0)
		return
	}
	entry.App, entry.Env, entry.Service = route.App, route.Env, route.Service

	var skip []int
	for attempt := 0; ; attempt++ {
		target, ok := pool.Pick(skip...)
		if !ok {
			entry.Status = http.StatusBadGateway
			entry.Error = "no target is taking requests"

			pool.countUnrouted()
			http.Error(w, "502 no replica of this service is taking requests\n", http.StatusBadGateway)
			p.finish(entry, start, 0)
			return
		}
		entry.Replica, entry.Target = target.Replica, target.Addr()

		res, release := p.forward(w, r, pool, target)
		entry.Status, entry.Bytes = res.status, res.bytes

		if res.dialFailed {

			detail := "connection refused"
			if res.err != nil {
				detail = res.err.Error()
			}
			_ = pool.Mark(Mark{Replica: target.Replica, Kind: MarkFail, At: p.now(), Detail: detail})
			p.logfEdge("target %s (replica %d) of %s: %v", target.Addr(), target.Replica, host, res.err)
			if attempt == 0 && !res.wrote && retryable(r) {

				pool.countRequest(target.Replica, 0, true)
				skip = append(skip, target.Replica)
				release()
				continue
			}
		}
		if res.err != nil && !res.wrote {
			entry.Status = http.StatusBadGateway
			entry.Error = res.err.Error()
			http.Error(w, "502 the service did not answer\n", http.StatusBadGateway)
		} else if res.err != nil {
			entry.Error = res.err.Error()
		}
		if entry.WebSocket && entry.Status == 0 && res.err == nil {

			entry.Status = http.StatusSwitchingProtocols
		}

		pool.countRequest(target.Replica, entry.Status, res.dialFailed)
		p.finish(entry, start, res.bytes)
		release()
		return
	}
}

type result struct {
	status     int
	bytes      int64
	wrote      bool
	dialFailed bool
	err        error
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, pool *RoutePool, target Target) (result, func()) {
	ctx, cancel := context.WithCancel(r.Context())
	st := &dialState{}
	ctx = context.WithValue(ctx, dialStateKey{}, st)

	id, err := pool.begin(target.Replica, cancel)
	if err != nil {
		cancel()
		return result{err: err}, func() {}
	}
	release := func() {
		cancel()
		pool.end(target.Replica, id)
	}

	rec := &recorder{ResponseWriter: w}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = target.Addr()

			pr.Out.Host = pr.In.Host
			trusted, viaIngress := fromIngress(pr.In.Context())
			if viaIngress && trusted {

				pr.Out.Header["X-Forwarded-For"] = pr.In.Header["X-Forwarded-For"]
				pr.Out.Header["X-Forwarded-Proto"] = pr.In.Header["X-Forwarded-Proto"]
				pr.Out.Header["X-Forwarded-Host"] = pr.In.Header["X-Forwarded-Host"]
				if pr.Out.Header.Get("X-Forwarded-Proto") == "" {
					pr.Out.Header.Set("X-Forwarded-Proto", "https")
				}
				if pr.Out.Header.Get("X-Forwarded-Host") == "" {
					pr.Out.Header.Set("X-Forwarded-Host", NormalizeHost(pr.In.Host))
				}
				forwardSpoofable(pr)
				return
			}

			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			pr.Out.Header.Set("X-Forwarded-Host", NormalizeHost(pr.In.Host))

			for _, h := range spoofableClientHeaders {
				pr.Out.Header.Del(h)
			}
			if addr := clientAddr(pr.In); addr != "" {
				pr.Out.Header.Set("X-Real-IP", addr)
			}
		},
		Transport: p.transport(target.Addr()),
	}

	var perr error
	rp.ErrorHandler = func(_ http.ResponseWriter, _ *http.Request, err error) { perr = err }
	rp.ServeHTTP(rec, r.WithContext(ctx))

	failed, dialErr := st.result()
	res := result{status: rec.status, bytes: rec.bytes, wrote: rec.wrote, dialFailed: failed, err: perr}
	if failed && dialErr != nil {
		res.err = dialErr
	}
	return res, release
}

func forwardSpoofable(pr *httputil.ProxyRequest) {
	real := pr.In.Header.Get("X-Real-IP")
	for _, h := range spoofableClientHeaders {
		pr.Out.Header.Del(h)
	}
	if real != "" {
		pr.Out.Header.Set("X-Real-IP", real)
	}
}

var spoofableClientHeaders = []string{
	"X-Real-IP",
	"X-Client-IP",
	"True-Client-IP",
	"CF-Connecting-IP",
	"Fastly-Client-IP",
	"Fly-Client-IP",
	"X-Cluster-Client-IP",
	"X-Forwarded-Port",
}

func (p *Proxy) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := p.now()
		host := NormalizeHost(r.Host)
		entry := AccessLog{
			Host:   host,
			Method: r.Method,
			Path:   r.URL.RequestURI(),
			Proto:  protoName(r),
			Client: clientAddr(r),
		}
		if route, _, ok := p.opts.router.Lookup(host); ok {
			entry.App, entry.Env, entry.Service = route.App, route.Env, route.Service
			to := (&url.URL{Scheme: "https", Host: host}).String() + r.URL.RequestURI()
			entry.Status = RedirectStatus
			http.Redirect(w, r, to, RedirectStatus)
			p.finish(entry, start, 0)
			return
		}
		entry.Status = http.StatusNotFound
		entry.Error = "no route for this host"
		http.Error(w, "404 no environment is exposed under this name\n", http.StatusNotFound)
		p.finish(entry, start, 0)
	})
}

func (p *Proxy) transport(addr string) *http.Transport {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := p.transports[addr]; ok {
		return t
	}
	t := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: targetDialTimeout, KeepAlive: targetKeepAlive}
			c, err := d.DialContext(ctx, network, address)
			if err != nil {
				if st, ok := ctx.Value(dialStateKey{}).(*dialState); ok {
					st.fail(err)
				}
			}
			return c, err
		},
		MaxIdleConns:          targetMaxIdleConns,
		MaxIdleConnsPerHost:   targetMaxIdleConns,
		IdleConnTimeout:       targetIdleTimeout,
		TLSHandshakeTimeout:   targetTLSHandshake,
		ExpectContinueTimeout: targetExpectTimeout,

		DisableCompression: true,

		ForceAttemptHTTP2: false,
	}
	p.transports[addr] = t
	return t
}

func (p *Proxy) closeIdle(t Target) {
	p.mu.Lock()
	tr := p.transports[t.Addr()]
	p.mu.Unlock()
	if tr != nil {
		tr.CloseIdleConnections()
	}
}

func (p *Proxy) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for addr, t := range p.transports {
		t.CloseIdleConnections()
		delete(p.transports, addr)
	}
}

func (p *Proxy) finish(e AccessLog, start time.Time, bytes int64) {
	if p.opts.access == nil {
		return
	}
	e.At = p.now()
	e.Duration = e.At.Sub(start)
	if e.Bytes == 0 {
		e.Bytes = bytes
	}
	p.opts.access(e)
}

func (p *Proxy) logfEdge(format string, args ...any) {
	if p.opts.logf != nil {
		p.opts.logf(format, args...)
	}
}

func retryable(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace,
		http.MethodPut, http.MethodDelete:
	default:
		return false
	}
	if isUpgrade(r) {
		return false
	}
	return r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0
}

func isUpgrade(r *http.Request) bool {
	if r.Header.Get("Upgrade") == "" {
		return false
	}
	for _, v := range r.Header.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

func protoName(r *http.Request) string {
	switch r.ProtoMajor {
	case 3:
		return "h3"
	case 2:
		return "h2"
	default:
		return "http/1.1"
	}
}

func clientAddr(r *http.Request) string {
	if r.RemoteAddr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.status, r.wrote = http.StatusOK, true
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
