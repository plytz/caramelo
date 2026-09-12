package edge

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/edge/certs"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

type testIssuer struct {
	rootPEM  []byte
	root     *x509.Certificate
	rootKey  *ecdsa.PrivateKey
	mu       sync.Mutex
	byHost   map[string]*tls.Certificate
	issued   map[string]int
	failCert error
	gone     []string
}

func (i *testIssuer) Unmanage(hosts []string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.gone = append(i.gone, hosts...)
}

func (i *testIssuer) unmanaged() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string{}, i.gone...)
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate root key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Caramelo Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	root, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse root: %v", err)
	}
	return &testIssuer{
		rootPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		root:    root,
		rootKey: key,
		byHost:  map[string]*tls.Certificate{},
		issued:  map[string]int{},
	}
}

var _ certs.Issuer = (*testIssuer)(nil)

func (i *testIssuer) TLSConfig(context.Context) (*tls.Config, error) {
	return &tls.Config{
		GetCertificate: i.get,
		NextProtos:     []string{"h2", "http/1.1"},
		MinVersion:     tls.VersionTLS12,
	}, nil
}

func (i *testIssuer) get(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if i.failCert != nil {
		return nil, i.failCert
	}
	host := NormalizeHost(hi.ServerName)
	i.mu.Lock()
	defer i.mu.Unlock()
	if c, ok := i.byHost[host]; ok {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, i.root, &key.PublicKey, i.rootKey)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der, i.root.Raw}, PrivateKey: key, Leaf: leaf}
	i.byHost[host] = c
	i.issued[host]++
	return c, nil
}

func (i *testIssuer) HTTPChallengeHandler(next http.Handler) http.Handler { return next }

func (i *testIssuer) Certificates(context.Context) ([]certs.Certificate, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]certs.Certificate, 0, len(i.byHost))
	for host, c := range i.byHost {
		out = append(out, certs.Certificate{
			Host:      host,
			Issuer:    i.root.Subject.CommonName,
			Serial:    c.Leaf.SerialNumber.Text(16),
			NotBefore: c.Leaf.NotBefore,
			NotAfter:  c.Leaf.NotAfter,
			Managed:   true,
		})
	}
	return out, nil
}

func (i *testIssuer) CA(context.Context) (certs.CA, error) {
	return certs.CA{Subject: i.root.Subject.CommonName, PEM: string(i.rootPEM), NotAfter: i.root.NotAfter}, nil
}

func (i *testIssuer) Close() error { return nil }

func (i *testIssuer) issuedFor(host string) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.issued[NormalizeHost(host)]
}

func (i *testIssuer) pool(t *testing.T) *x509.CertPool {
	t.Helper()
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(i.rootPEM) {
		t.Fatal("the test root is not a certificate")
	}
	return p
}

type replica struct {
	srv      *httptest.Server
	index    int
	port     int
	hold     chan struct{}
	released sync.Once
	mu       sync.Mutex
	seen     []*http.Request
	body     string
	closed   bool
}

func newReplica(t *testing.T, index int) *replica {
	t.Helper()
	r := &replica{index: index, body: fmt.Sprintf("replica %d", index), hold: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		r.record(req)
		w.Header().Set("X-Replica", strconv.Itoa(r.index))
		fmt.Fprint(w, r.currentBody())
	})

	mux.HandleFunc("/slow", func(w http.ResponseWriter, req *http.Request) {
		r.record(req)
		select {
		case <-time.After(2 * time.Second):
		case <-req.Context().Done():
			return
		}
		fmt.Fprintf(w, "slow from replica %d", r.index)
	})

	mux.HandleFunc("/hold", func(w http.ResponseWriter, req *http.Request) {
		r.record(req)
		select {
		case <-r.hold:
			fmt.Fprintf(w, "held by replica %d", r.index)
		case <-req.Context().Done():
		}
	})

	mux.HandleFunc("/ws", func(w http.ResponseWriter, req *http.Request) {
		r.record(req)
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer conn.Close()
		fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nX-Replica: %d\r\n\r\n", r.index)
		if err := brw.Flush(); err != nil {
			return
		}

		sc := bufio.NewScanner(brw)
		for sc.Scan() {
			if _, err := io.WriteString(conn, sc.Text()+"\n"); err != nil {
				return
			}
		}
	})
	r.srv = httptest.NewServer(mux)
	u, err := url.Parse(r.srv.URL)
	if err != nil {
		t.Fatalf("replica url: %v", err)
	}
	r.port, err = strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("replica port: %v", err)
	}
	t.Cleanup(r.stop)
	return r
}

func (r *replica) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, req.Clone(context.Background()))
}

func (r *replica) requests() []*http.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*http.Request(nil), r.seen...)
}

func (r *replica) currentBody() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body
}

func (r *replica) release() { r.released.Do(func() { close(r.hold) }) }

func (r *replica) setBody(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.body = s
}

func (r *replica) stop() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.mu.Unlock()
	r.srv.Close()
}

func (r *replica) target(state TargetState) Target {
	return Target{Replica: r.index, Port: r.port, State: state}
}

func deadPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func route(host string, targets ...Target) Route {
	return Route{Host: host, Kind: KindHTTPS, App: "shop", Env: "feat-x", Service: "web", Targets: targets}
}

func collect(t *testing.T, ch <-chan Event, want int, timeout time.Duration) []Event {
	t.Helper()
	var out []Event
	deadline := time.After(timeout)
	for len(out) < want {
		select {
		case e := <-ch:
			out = append(out, e)
		case <-deadline:
			t.Fatalf("waited %s for %d event(s), got %d: %+v", timeout, want, len(out), out)
		}
	}
	return out
}
