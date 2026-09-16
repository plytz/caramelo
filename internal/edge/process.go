package edge

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go/http3"

	"github.com/plytz/caramelo/internal/edge/certs"
)

const (
	ShutdownGrace = 3 * time.Second

	sweepInterval = 500 * time.Millisecond

	readHeaderTimeout = 20 * time.Second

	idleQUICTimeout = 30 * time.Second
)

type Options struct {
	StateDir string

	RunDir string

	HTTP3 bool

	TLS          certs.Mode
	Directory    string
	Email        string
	TrustedRoots []string

	Version string

	Log io.Writer

	Access io.Writer

	Drain time.Duration

	ACMEProfile        string
	RenewalWindowRatio float64
	RenewCheckInterval time.Duration

	Now func() time.Time

	Listeners *Listeners
	Bind      BindOptions

	Issuer certs.Issuer

	SocketPath string

	RoutesPath string
}

func (o Options) clock() func() time.Time {
	if o.Now != nil {
		return o.Now
	}
	return time.Now
}

func (o Options) routesPath() string {
	if o.RoutesPath != "" {
		return o.RoutesPath
	}
	return RoutesPath(o.StateDir)
}

func (o Options) socketPath() string {
	if o.SocketPath != "" {
		return o.SocketPath
	}
	return SocketPath(o.RunDir)
}

type Edge struct {
	opts      Options
	now       func() time.Time
	router    *Router
	proxy     *Proxy
	issuer    certs.Issuer
	listeners *Listeners
	ownedList bool
	access    *accessWriter
	logw      io.Writer

	h3      *http3.Server
	https   *http.Server
	plain   *http.Server
	control Server

	mu      sync.Mutex
	started time.Time
	closed  bool

	ingress *ingress
}

var _ Handler = (*Edge)(nil)

func New(o Options) (*Edge, error) {
	if o.Log == nil {
		o.Log = io.Discard
	}
	if o.Access == nil {
		o.Access = o.Log
	}
	if o.TLS == "" {
		o.TLS = certs.DefaultMode
	}
	if o.Drain <= 0 {
		o.Drain = DefaultDrain
	}

	e := &Edge{opts: o, now: o.clock(), logw: o.Log, access: newAccessWriter(o.Access)}

	e.router = NewRouter(RouterOptions{
		Now:        e.now,
		Drain:      o.Drain,
		OnDraining: func(t Target) { e.proxy.closeIdle(t) },
	})

	ln, owned, err := listeners(o)
	if err != nil {
		return nil, err
	}
	e.listeners, e.ownedList = ln, owned

	issuer := o.Issuer
	if issuer == nil {
		cfg := certs.Config{
			Mode:               o.TLS,
			Directory:          o.Directory,
			Email:              o.Email,
			StorageDir:         CertsPath(o.StateDir),
			Ask:                e.ask,
			TrustedRoots:       o.TrustedRoots,
			ACMEProfile:        o.ACMEProfile,
			RenewalWindowRatio: o.RenewalWindowRatio,
			RenewCheckInterval: o.RenewCheckInterval,
			OnChange:           e.certificateChanged,
			Log:                e.logf,
		}
		if issuer, err = certs.New(cfg); err != nil {
			if owned {
				ln.Close()
			}
			return nil, err
		}
	}
	e.issuer = issuer

	e.proxy = NewProxy(proxyOptions{
		router: e.router,
		now:    e.now,
		access: e.access.log,
		logf:   e.logf,
		altSvc: e.altSvc,
	})

	if t, err := LoadTable(o.routesPath()); err != nil {
		e.logf("route table: %v", err)
	} else if len(t.Routes) > 0 {
		if err := e.router.Install(t); err != nil {
			e.logf("route table: %v", err)
		} else {
			e.logf("serving %d route(s) from %s", len(t.Routes), o.routesPath())
		}
	}

	e.control = NewServer(e)
	return e, nil
}

func listeners(o Options) (*Listeners, bool, error) {
	if o.Listeners != nil {
		return o.Listeners, false, nil
	}
	ln, err := Inherit()
	if err != nil {
		return nil, false, err
	}
	if ln != nil {
		if !o.HTTP3 && ln.QUIC != nil {
			ln.QUIC.Close()
			ln.QUIC = nil
		}
		return ln, true, nil
	}
	b := o.Bind
	b.QUIC = o.HTTP3
	ln, err = Bind(b)
	if err != nil {
		return nil, false, err
	}
	return ln, true, nil
}

func (e *Edge) ask(_ context.Context, host string) error {
	if e.router.Has(host) {
		return nil
	}
	return fmt.Errorf("%s is not a hostname this machine serves", NormalizeHost(host))
}

func (e *Edge) certificateChanged(change certs.Change, cert certs.Certificate, detail string) {
	if change == certs.Failed {
		e.logf("certificate for %s: %s", cert.Host, detail)
	} else {
		e.logf("certificate %s for %s (serial %s, until %s)", change, cert.Host, cert.Serial,
			cert.NotAfter.UTC().Format(time.RFC3339))
	}
	c := cert
	e.router.Events().publish(Event{
		Kind:        EventCertificate,
		Host:        NormalizeHost(cert.Host),
		Certificate: &c,
		Detail:      string(change) + detailSuffix(detail),
		At:          e.now(),
	})
}

func detailSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}

func Run(ctx context.Context, o Options) error {
	e, err := New(o)
	if err != nil {
		return err
	}
	return e.Serve(ctx)
}

func (e *Edge) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tlsConf, err := e.tlsConfig(ctx)
	if err != nil {
		return err
	}

	e.mu.Lock()
	e.started = e.now()
	e.mu.Unlock()

	if err := e.setIngress(e.router.Ingress()); err != nil {
		return err
	}

	errs := make(chan error, 4)
	var wg sync.WaitGroup

	if e.opts.HTTP3 && e.listeners.QUIC != nil {
		h3conf := tlsConf.Clone()
		h3conf.NextProtos = []string{http3.NextProtoH3}
		e.h3 = &http3.Server{
			Handler:     e.proxy,
			TLSConfig:   h3conf,
			Port:        quicPort(e.listeners),
			IdleTimeout: idleQUICTimeout,
		}
		pc := e.listeners.QUIC
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.h3.Serve(pc); err != nil && ctx.Err() == nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("edge: http/3: %w", err)
			}
		}()
	}

	e.https = &http.Server{
		Handler:           e.proxy,
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          log.New(prefixed{e.logw, "edge: tls: "}, "", 0),
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		l := tls.NewListener(e.listeners.HTTPS, tlsConf)
		if err := e.https.Serve(l); err != nil && ctx.Err() == nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("edge: https: %w", err)
		}
	}()

	if e.listeners.HTTP != nil {

		handler := e.issuer.HTTPChallengeHandler(e.proxy.HTTPHandler())
		e.plain = &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: readHeaderTimeout,
			ErrorLog:          log.New(prefixed{e.logw, "edge: http: "}, "", 0),
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.plain.Serve(e.listeners.HTTP); err != nil && ctx.Err() == nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("edge: http: %w", err)
			}
		}()
	}

	sockLn, err := ListenSocket(e.opts.socketPath())
	if err != nil {
		e.shutdown()
		wg.Wait()
		return err
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := e.control.Serve(ctx, sockLn); err != nil && ctx.Err() == nil {
			errs <- err
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		e.sweep(ctx)
	}()

	e.logf("edge %s listening on %v (http3=%v, tls=%s)", e.opts.Version, e.listeners.Names(), e.h3 != nil, e.opts.TLS)

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errs:
	}
	cancel()
	e.shutdown()
	wg.Wait()
	return runErr
}

func (e *Edge) sweep(ctx context.Context) {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.router.Sweep(e.now())
		}
	}
}

func (e *Edge) shutdown() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), ShutdownGrace)
	defer cancel()

	e.mu.Lock()
	e.closeIngressLocked()
	e.mu.Unlock()

	var wg sync.WaitGroup
	if e.https != nil {
		wg.Add(1)
		go func() { defer wg.Done(); _ = e.https.Shutdown(ctx) }()
	}
	if e.plain != nil {
		wg.Add(1)
		go func() { defer wg.Done(); _ = e.plain.Shutdown(ctx) }()
	}
	if e.h3 != nil {
		wg.Add(1)
		go func() { defer wg.Done(); _ = e.h3.Shutdown(ctx) }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}

	if e.https != nil {
		_ = e.https.Close()
	}
	if e.plain != nil {
		_ = e.plain.Close()
	}
	if e.h3 != nil {
		_ = e.h3.Close()
	}
	_ = e.control.Close()
	e.router.Events().close()
	e.proxy.close()
	if e.issuer != nil {
		_ = e.issuer.Close()
	}
	if e.ownedList {
		_ = e.listeners.Close()
	}
	_ = os.Remove(e.opts.socketPath())
}

func (e *Edge) Close() error {
	e.shutdown()
	return nil
}

func (e *Edge) tlsConfig(ctx context.Context) (*tls.Config, error) {
	base, err := e.issuer.TLSConfig(ctx)
	if err != nil {
		return nil, err
	}
	conf := base.Clone()
	inner := conf.GetCertificate
	if inner == nil {
		return nil, errors.New("edge: the certificate manager returned a configuration that cannot answer a handshake")
	}
	conf.GetCertificate = func(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
		host := NormalizeHost(hi.ServerName)
		if host == "" {

			return nil, errors.New("edge: no server name was offered: this machine serves hostnames, not addresses")
		}
		if !e.router.Has(host) {
			return nil, fmt.Errorf("edge: %s is not a hostname this machine serves", host)
		}
		return inner(hi)
	}

	for _, proto := range []string{"h2", "http/1.1"} {
		if !slices.Contains(conf.NextProtos, proto) {
			conf.NextProtos = append(conf.NextProtos, proto)
		}
	}
	return conf, nil
}

func (e *Edge) altSvc(h http.Header) error {
	if e.h3 == nil {
		return nil
	}
	return e.h3.SetQUICHeaders(h)
}

func (e *Edge) PushTable(_ context.Context, t Table) error {
	before := e.router.Hosts()
	if err := e.router.Install(t); err != nil {
		return err
	}
	if err := SaveTable(e.opts.routesPath(), e.router.Pushed()); err != nil {
		return err
	}

	if err := e.setIngress(e.router.Ingress()); err != nil {
		return err
	}
	e.unmanage(dropped(before, e.router.Hosts()))
	e.logf("route table: %d route(s) installed", len(t.Routes))
	return nil
}

func dropped(before, after []string) []string {
	kept := make(map[string]bool, len(after))
	for _, h := range after {
		kept[h] = true
	}
	var gone []string
	for _, h := range before {
		if !kept[h] {
			gone = append(gone, h)
		}
	}
	return gone
}

func (e *Edge) unmanage(hosts []string) {
	if len(hosts) == 0 {
		return
	}
	u, ok := e.issuer.(certs.Unmanager)
	if !ok {
		return
	}
	u.Unmanage(hosts)
	e.logf("no longer serving %s: dropped from the certificate cache", strings.Join(hosts, ", "))
}

func (e *Edge) Status(ctx context.Context) (*Status, error) {
	e.mu.Lock()
	started := e.started
	in := e.ingress
	e.mu.Unlock()

	table := e.router.Table()
	st := &Status{
		Ingress:        in.status(),
		Running:        true,
		Version:        e.opts.Version,
		StartedAt:      started,
		Listeners:      e.listeners.Names(),
		HTTP3:          e.h3 != nil,
		TLS:            e.opts.TLS,
		Routes:         table.Routes,
		TableUpdatedAt: e.router.TableUpdatedAt(),
	}
	if e.opts.TLS == certs.ModeACME {
		st.ACMEDirectory = e.opts.Directory
		if st.ACMEDirectory == "" {
			st.ACMEDirectory = certs.LetsEncryptProduction
		}
	}
	list, err := e.issuer.Certificates(ctx)
	if err != nil {

		st.Error = err.Error()
	}
	st.Certificates = list
	return st, nil
}

func (e *Edge) Subscribe(ctx context.Context, since time.Time, fn func(Event) error) error {
	return e.router.Events().subscribe(ctx, since, fn)
}

func (e *Edge) Counts(_ context.Context, since time.Time) (*Counts, error) {
	e.mu.Lock()
	started := e.started
	e.mu.Unlock()

	if since.IsZero() || since.Before(started) {
		since = started
	}
	return &Counts{Since: since, At: e.now(), Hosts: e.router.Counts(since)}, nil
}

func (e *Edge) Prune(ctx context.Context, req certs.PruneRequest) (*certs.PruneResult, error) {
	res, err := e.issuer.Prune(ctx, req)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, errors.New("edge: the certificate manager pruned nothing and said nothing")
	}
	return res, nil
}

func (e *Edge) CA(ctx context.Context) (*certs.CA, error) {
	ca, err := e.issuer.CA(ctx)
	if err != nil {
		return nil, err
	}
	return &ca, nil
}

func (e *Edge) Addrs() []string { return e.listeners.Names() }

func (e *Edge) logf(format string, args ...any) {
	if e.logw == nil {
		return
	}
	fmt.Fprintf(e.logw, "edge: "+format+"\n", args...)
}

func quicPort(l *Listeners) int {
	if l == nil || l.QUIC == nil {
		return 0
	}
	if a, ok := l.QUIC.LocalAddr().(*net.UDPAddr); ok {
		return a.Port
	}
	return 0
}

type prefixed struct {
	w      io.Writer
	prefix string
}

func (p prefixed) Write(b []byte) (int, error) {
	if p.w == nil {
		return len(b), nil
	}
	if _, err := io.WriteString(p.w, p.prefix); err != nil {
		return 0, err
	}
	return p.w.Write(b)
}
