package vpnclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/vpn"
)

const udpIdle = 60 * time.Second

type Target struct {
	Name string

	Kind string

	Port int

	Protocols []vpn.Protocol
}

type Lookup interface {
	EnvTargets(ctx context.Context, machine, app, env string) ([]Target, error)
}

type ControlLookup struct {
	Control Control
}

var _ Lookup = (*ControlLookup)(nil)

func (l *ControlLookup) EnvTargets(ctx context.Context, machine, app, envName string) ([]Target, error) {
	var stdout, stderr bytes.Buffer
	var urls []env.URL
	argv := []string{"env", "url", envName, "--app", app, "--json"}
	code, err := l.Control.Run(ctx, machine, argv, &stdout, &stderr)
	if err != nil {
		return nil, fmt.Errorf("run 'caramelo %s' on %s: %w", strings.Join(argv, " "), machine, err)
	}
	if code != 0 {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = fmt.Sprintf("exit %d", code)
		}
		return nil, fmt.Errorf("%s/%s on %s: %s", app, envName, machine, msg)
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &urls); err != nil {
		return nil, fmt.Errorf("read 'caramelo env url %s --json' from %s: %w", envName, machine, err)
	}
	return targetsFromURLs(urls), nil
}

func targetsFromURLs(urls []env.URL) []Target {
	out := make([]Target, 0, len(urls))
	for _, u := range urls {
		if u.InternalPort <= 0 {
			continue
		}
		proto := vpn.TCP
		if strings.EqualFold(u.Protocol, string(vpn.UDP)) {
			proto = vpn.UDP
		}
		out = append(out, Target{
			Name:      u.Name,
			Kind:      string(u.Kind),
			Port:      u.InternalPort,
			Protocols: []vpn.Protocol{proto},
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type ConnectorOptions struct {
	Dialer Dialer

	Lookup Lookup

	Log io.Writer
}

func NewConnector(opts ConnectorOptions) (Connector, error) {
	if opts.Dialer == nil {
		return nil, errors.New("vpnclient: a connector needs a dialer")
	}
	if opts.Lookup == nil {
		return nil, errors.New("vpnclient: a connector needs a lookup")
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	return &connector{opts: opts}, nil
}

type connector struct {
	opts ConnectorOptions

	mu      sync.Mutex
	closed  bool
	closers []io.Closer
}

var _ Connector = (*connector)(nil)

func (c *connector) Open(ctx context.Context, req ConnectRequest) ([]Listener, error) {
	listen := strings.TrimSpace(req.Listen)
	if listen == "" {
		listen = "127.0.0.1"
	}
	targets, err := c.opts.Lookup.EnvTargets(ctx, req.Machine, req.App, req.Env)
	if err != nil {
		return nil, err
	}
	targets, err = filterTargets(targets, req.Only)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("%s/%s publishes nothing to connect to", req.App, req.Env)
	}
	host := vpn.EnvHost(req.App, req.Env)
	addrs, err := c.opts.Dialer.Resolve(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("look up %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%s has no address", host)
	}
	addr := addrs[0]

	var out []Listener
	for _, t := range targets {
		for _, proto := range protocols(t) {
			l, err := c.open(listen, addr, req, t, proto)
			if err != nil {
				_ = c.Close()
				return nil, err
			}
			out = append(out, l)
		}
	}
	return out, nil
}

func (c *connector) open(listen string, addr netip.Addr, req ConnectRequest, t Target, proto vpn.Protocol) (Listener, error) {
	remote := netip.AddrPortFrom(addr, uint16(t.Port))
	l := Listener{
		Name:     t.Name,
		Kind:     t.Kind,
		Protocol: proto,
		Remote:   remote.String(),
		Host:     vpn.ServiceHost(req.App, req.Env, t.Name),
	}
	switch proto {
	case vpn.TCP:
		ln, err := net.Listen("tcp", net.JoinHostPort(listen, "0"))
		if err != nil {
			return Listener{}, fmt.Errorf("open a local port for %s: %w", t.Name, err)
		}
		c.track(ln)
		l.Local = ln.Addr().String()
		if t.Kind == "service" {
			l.URL = "http://" + l.Local
		}
		go c.serveTCP(ln, remote, t.Name)
	case vpn.UDP:
		pc, err := net.ListenPacket("udp", net.JoinHostPort(listen, "0"))
		if err != nil {
			return Listener{}, fmt.Errorf("open a local udp port for %s: %w", t.Name, err)
		}
		c.track(pc)
		l.Local = pc.LocalAddr().String()
		go c.serveUDP(pc, remote, t.Name)
	default:
		return Listener{}, fmt.Errorf("%s speaks %q, which the tunnel does not carry", t.Name, proto)
	}
	return l, nil
}

func (c *connector) track(closer io.Closer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		_ = closer.Close()
		return
	}
	c.closers = append(c.closers, closer)
}

func (c *connector) logf(format string, args ...any) {
	fmt.Fprintf(c.opts.Log, format+"\n", args...)
}

func (c *connector) serveTCP(ln net.Listener, remote netip.AddrPort, name string) {
	for {
		local, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = local.Close() }()
			up, err := c.opts.Dialer.DialContext(context.Background(), "tcp", remote.String())
			if err != nil {
				c.logf("connect: %s: %v", name, err)
				return
			}
			defer func() { _ = up.Close() }()
			pipe(local, up)
		}()
	}
}

func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(a, b); closeWrite(a) }()
	go func() { defer wg.Done(); _, _ = io.Copy(b, a); closeWrite(b) }()
	wg.Wait()
}

type halfCloser interface{ CloseWrite() error }

func closeWrite(c net.Conn) {
	if hc, ok := c.(halfCloser); ok {
		if err := hc.CloseWrite(); err == nil {
			return
		}
	}
	_ = c.Close()
}

func (c *connector) serveUDP(pc net.PacketConn, remote netip.AddrPort, name string) {
	type flow struct {
		conn net.Conn
		mu   sync.Mutex
	}
	var (
		mu    sync.Mutex
		flows = map[string]*flow{}
	)
	buf := make([]byte, 64*1024)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			mu.Lock()
			for _, f := range flows {
				_ = f.conn.Close()
			}
			mu.Unlock()
			return
		}
		key := src.String()
		mu.Lock()
		f, ok := flows[key]
		if !ok {
			up, derr := c.opts.Dialer.DialContext(context.Background(), "udp", remote.String())
			if derr != nil {
				mu.Unlock()
				c.logf("connect: %s: %v", name, derr)
				continue
			}
			f = &flow{conn: up}
			flows[key] = f
			go func(src net.Addr, key string, f *flow) {
				defer func() {
					mu.Lock()
					delete(flows, key)
					mu.Unlock()
					_ = f.conn.Close()
				}()
				rbuf := make([]byte, 64*1024)
				for {
					_ = f.conn.SetReadDeadline(time.Now().Add(udpIdle))
					rn, rerr := f.conn.Read(rbuf)
					if rerr != nil {
						return
					}
					if _, werr := pc.WriteTo(rbuf[:rn], src); werr != nil {
						return
					}
				}
			}(src, key, f)
		}
		mu.Unlock()
		f.mu.Lock()
		_, werr := f.conn.Write(buf[:n])
		f.mu.Unlock()
		if werr != nil {
			c.logf("connect: %s: %v", name, werr)
		}
	}
}

func (c *connector) Close() error {
	c.mu.Lock()
	closers := c.closers
	c.closers, c.closed = nil, true
	c.mu.Unlock()
	var err error
	for _, closer := range closers {
		if cerr := closer.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

func filterTargets(targets []Target, only []string) ([]Target, error) {
	if len(only) == 0 {
		return targets, nil
	}
	byName := map[string]Target{}
	for _, t := range targets {
		byName[t.Name] = t
	}
	out := make([]Target, 0, len(only))
	var missing []string
	for _, name := range only {
		t, ok := byName[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		out = append(out, t)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("no service or dependency called %s (this environment has %s)",
			strings.Join(missing, ", "), strings.Join(names(targets), ", "))
	}
	return out, nil
}

func names(targets []Target) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.Name)
	}
	return out
}

func protocols(t Target) []vpn.Protocol {
	if len(t.Protocols) == 0 {
		return []vpn.Protocol{vpn.TCP}
	}
	return t.Protocols
}

func PortOf(l Listener) int {
	_, p, err := net.SplitHostPort(l.Local)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}
