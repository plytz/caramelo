//go:build integration

package itest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	EdgeDialTimeout    = 10 * time.Second
	EdgeRequestTimeout = 15 * time.Second
)

type EdgeClient struct {
	Machine *Machine
	Host    string
	Roots   *x509.CertPool

	rootPEM string

	mu     sync.Mutex
	caFile string
}

func NewEdgeClient(m *Machine, rootPEMs ...string) (*EdgeClient, error) {
	if m == nil {
		return nil, errors.New("itest: an edge client needs a machine to reach")
	}
	c := &EdgeClient{Machine: m, Host: m.HostIP()}
	var pems []string
	for _, pem := range rootPEMs {
		if strings.TrimSpace(pem) == "" {
			continue
		}
		if c.Roots == nil {
			c.Roots = x509.NewCertPool()
		}
		if !c.Roots.AppendCertsFromPEM([]byte(pem)) {
			return nil, fmt.Errorf("itest: not a PEM certificate: %q", truncate(pem, 80))
		}
		pems = append(pems, strings.TrimSpace(pem))
	}
	c.rootPEM = strings.Join(pems, "\n") + "\n"
	if len(pems) == 0 {
		c.rootPEM = ""
	}
	return c, nil
}

func MustEdgeClient(t testing.TB, m *Machine, rootPEMs ...string) *EdgeClient {
	t.Helper()
	c, err := NewEdgeClient(m, rootPEMs...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return c
}

func (c *EdgeClient) timeout(d time.Duration) time.Duration {
	if c.Machine == nil {
		return Scale(d)
	}
	return c.Machine.Budget().For(d)
}

func (c *EdgeClient) TLSConfig(serverName string) *tls.Config {
	return &tls.Config{
		ServerName: serverName,
		RootCAs:    c.Roots,
		MinVersion: tls.VersionTLS12,
	}
}

func (c *EdgeClient) Published(port int, proto string) (int, error) {
	published, err := c.Machine.HostPortProto(port, proto)
	if err != nil {
		return 0, fmt.Errorf("itest: the edge client cannot reach %s/%s: %w", strconv.Itoa(port), proto, err)
	}
	return published, nil
}

func (c *EdgeClient) hostAddr(network, addr string) (string, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("itest: %q is not a host:port: %w", addr, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return "", fmt.Errorf("itest: %q is not a host:port: %w", addr, err)
	}
	proto := "tcp"
	if strings.HasPrefix(network, "udp") {
		proto = "udp"
	}
	published, err := c.Published(n, proto)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(published)), nil
}

func (c *EdgeClient) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	target, err := c.hostAddr(network, addr)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: c.timeout(EdgeDialTimeout)}
	return d.DialContext(ctx, network, target)
}

type EdgeResponse struct {
	Status int
	Proto  string
	Header http.Header
	Body   string
	Cert   *x509.Certificate
}

func (r EdgeResponse) AltSvc() string { return r.Header.Get("Alt-Svc") }

func (r EdgeResponse) Serial() string {
	if r.Cert == nil {
		return ""
	}
	return strings.ToUpper(r.Cert.SerialNumber.Text(16))
}

func (c *EdgeClient) httpClient(h2 bool, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		DialContext:         c.dial,
		TLSClientConfig:     &tls.Config{RootCAs: c.Roots, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:   h2,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     30 * time.Second,
	}
	if !h2 {
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (c *EdgeClient) Client(timeout time.Duration) *http.Client {
	if timeout == 0 {
		timeout = c.timeout(EdgeRequestTimeout)
	}
	return c.httpClient(true, timeout)
}

func (c *EdgeClient) Client1(timeout time.Duration) *http.Client {
	if timeout == 0 {
		timeout = c.timeout(EdgeRequestTimeout)
	}
	return c.httpClient(false, timeout)
}

func (c *EdgeClient) Get(ctx context.Context, rawURL string) (*EdgeResponse, error) {
	client := c.Client(0)
	defer client.CloseIdleConnections()
	return c.Do(ctx, client, rawURL)
}

func (c *EdgeClient) Get1(ctx context.Context, rawURL string) (*EdgeResponse, error) {
	client := c.Client1(0)
	defer client.CloseIdleConnections()
	return c.Do(ctx, client, rawURL)
}

func (c *EdgeClient) Do(ctx context.Context, client *http.Client, rawURL string) (*EdgeResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rawURL, err)
	}
	out := &EdgeResponse{
		Status: resp.StatusCode,
		Proto:  resp.Proto,
		Header: resp.Header,
		Body:   string(body),
	}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		out.Cert = resp.TLS.PeerCertificates[0]
	}
	return out, nil
}

func (c *EdgeClient) GetOK(t testing.TB, ctx context.Context, rawURL string) *EdgeResponse {
	t.Helper()
	res, err := c.Get(ctx, rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("GET %s: HTTP %d\n%s", rawURL, res.Status, truncate(res.Body, 400))
	}
	return res
}

func (c *EdgeClient) GetWithin(ctx context.Context, rawURL string, budget time.Duration) (*EdgeResponse, error) {
	deadline := time.Now().Add(budget)
	var last error
	for {
		res, err := c.Get(ctx, rawURL)
		switch {
		case err == nil && res.Status == http.StatusOK:
			return res, nil
		case err != nil:
			last = err
		default:
			last = fmt.Errorf("HTTP %d", res.Status)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s never answered 200 within %s: %w", rawURL, budget, last)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s never answered 200: %w (last: %v)", rawURL, ctx.Err(), last)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (c *EdgeClient) Certificate(ctx context.Context, host string) (*x509.Certificate, error) {
	conn, err := c.handshake(ctx, host)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, fmt.Errorf("%s: the handshake produced no certificate", host)
	}
	return certs[0], nil
}

func (c *EdgeClient) Handshake(ctx context.Context, host string) error {
	conn, err := c.handshake(ctx, host)
	if err != nil {
		return err
	}
	return conn.Close()
}

func (c *EdgeClient) handshake(ctx context.Context, host string) (*tls.Conn, error) {
	raw, err := c.dial(ctx, "tcp", net.JoinHostPort(host, "443"))
	if err != nil {
		return nil, err
	}
	conn := tls.Client(raw, c.TLSConfig(host))
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("tls handshake for %s: %w", host, err)
	}
	return conn, nil
}

func SerialOf(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	return strings.ToUpper(cert.SerialNumber.Text(16))
}

type CurlResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

func (c *EdgeClient) CurlArgs(rawURL string) ([]string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("itest: curl %s: %w", rawURL, err)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("itest: curl %s: no host", rawURL)
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			return nil, fmt.Errorf("itest: curl %s: %q is not an http scheme", rawURL, u.Scheme)
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return nil, fmt.Errorf("itest: curl %s: %q is not a port: %w", rawURL, port, err)
	}
	published, err := c.Published(n, "tcp")
	if err != nil {
		return nil, err
	}
	args := []string{"--connect-to",
		fmt.Sprintf("%s:%s:%s:%d", host, port, c.Host, published)}
	ca, err := c.caFilePath()
	if err != nil {
		return nil, err
	}
	if ca != "" {
		args = append(args, "--cacert", ca)
	}
	return args, nil
}

func (c *EdgeClient) Curl(ctx context.Context, rawURL string, args ...string) (CurlResult, error) {
	bin, err := exec.LookPath("curl")
	if err != nil {
		return CurlResult{}, fmt.Errorf("itest: the edge suites need curl on the host: %w", err)
	}
	base, err := c.CurlArgs(rawURL)
	if err != nil {
		return CurlResult{}, err
	}
	argv := append(append(base, args...), rawURL)
	cmd := exec.CommandContext(ctx, bin, argv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	res := CurlResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return res, fmt.Errorf("itest: curl %s: %w", rawURL, runErr)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, fmt.Errorf("itest: curl %s: %w", rawURL, ctxErr)
	}
	return res, nil
}

func (c *EdgeClient) MustCurl(t testing.TB, rawURL string, args ...string) CurlResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout(EdgeRequestTimeout*2))
	defer cancel()
	res, err := c.Curl(ctx, rawURL, args...)
	if err != nil {
		t.Fatalf("itest: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("curl %s: exit %d\nstdout:\n%sstderr:\n%s", rawURL, res.ExitCode, res.Stdout, res.Stderr)
	}
	return res
}

func (c *EdgeClient) caFilePath() (string, error) {
	if c.rootPEM == "" {
		return "", nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.caFile != "" {
		return c.caFile, nil
	}
	dir, err := SharedCacheDir("roots")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	sum := sha256.Sum256([]byte(c.rootPEM))
	path := filepath.Join(dir, hex.EncodeToString(sum[:])[:16]+".pem")
	if err := os.WriteFile(path, []byte(c.rootPEM), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	c.caFile = path
	return path, nil
}

type WSConn struct {
	conn net.Conn
}

func (c *EdgeClient) DialWebSocket(ctx context.Context, rawURL string) (*WSConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	port := u.Port()
	var conn net.Conn
	switch u.Scheme {
	case "wss":
		if port == "" {
			port = "443"
		}
		raw, dialErr := c.dial(ctx, "tcp", net.JoinHostPort(host, port))
		if dialErr != nil {
			return nil, dialErr
		}
		tc := tls.Client(raw, c.TLSConfig(host))
		if err := tc.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, fmt.Errorf("wss handshake for %s: %w", host, err)
		}
		conn = tc
	case "ws":
		if port == "" {
			port = "80"
		}
		if conn, err = c.dial(ctx, "tcp", net.JoinHostPort(host, port)); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%s: not a websocket URL", rawURL)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	ws, err := newWSConn(conn, host, u.RequestURI())
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ws, nil
}

func (w *WSConn) Deadline(t time.Time) error { return w.conn.SetDeadline(t) }

func (w *WSConn) Echo(s string) (string, error) {
	if err := w.Send(s); err != nil {
		return "", err
	}
	return w.Receive()
}

func (w *WSConn) Send(s string) error { return wsWrite(w.conn, wsOpText, []byte(s)) }

func (w *WSConn) Receive() (string, error) {
	for {
		op, payload, err := wsRead(w.conn)
		if err != nil {
			return "", err
		}
		switch op {
		case wsOpText, wsOpBinary:
			return string(payload), nil
		case wsOpPing:
			if err := wsWrite(w.conn, wsOpPong, payload); err != nil {
				return "", err
			}
		case wsOpClose:
			return "", ErrWSClosed
		}
	}
}

func (w *WSConn) WaitForClose(budget time.Duration) (time.Duration, error) {
	start := time.Now()
	if err := w.conn.SetDeadline(start.Add(budget)); err != nil {
		return 0, err
	}
	for {
		op, _, err := wsRead(w.conn)
		switch {
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			return time.Since(start), nil
		case err != nil:
			return time.Since(start), err
		case op == wsOpClose:
			return time.Since(start), nil
		}
	}
}

func (w *WSConn) Close() error { return w.conn.Close() }

var ErrWSClosed = errors.New("itest: websocket closed by the peer")
