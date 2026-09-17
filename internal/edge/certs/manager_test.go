package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
)

type routes struct {
	mu    sync.Mutex
	hosts map[string]bool
	asked []string
}

func newRoutes(hosts ...string) *routes {
	r := &routes{hosts: map[string]bool{}}
	for _, h := range hosts {
		r.hosts[h] = true
	}
	return r
}

func (r *routes) ask(_ context.Context, host string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, host)
	if !r.hosts[host] {
		return fmt.Errorf("%s is not a routed hostname", host)
	}
	return nil
}

func (r *routes) expose(host string)   { r.mu.Lock(); r.hosts[host] = true; r.mu.Unlock() }
func (r *routes) unexpose(host string) { r.mu.Lock(); delete(r.hosts, host); r.mu.Unlock() }

func (r *routes) unexposeAll() { r.mu.Lock(); r.hosts = map[string]bool{}; r.mu.Unlock() }

func (r *routes) asks() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.asked...)
}

type changes struct {
	mu  sync.Mutex
	got []changed
}

type changed struct {
	change Change
	cert   Certificate
	detail string
}

func (c *changes) record(change Change, cert Certificate, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, changed{change, cert, detail})
}

func (c *changes) of(kind Change) []Certificate {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Certificate
	for _, g := range c.got {
		if g.change == kind {
			out = append(out, g.cert)
		}
	}
	return out
}

func testLog(t *testing.T) LogFunc {
	t.Helper()
	var mu sync.Mutex
	done := false
	t.Cleanup(func() { mu.Lock(); done = true; mu.Unlock() })
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			t.Logf(format, args...)
		}
	}
}

func internalIssuer(t *testing.T, dir string, rt *routes, edit func(*Config)) (Issuer, *changes) {
	t.Helper()
	ch := &changes{}
	cfg := Config{
		Mode:       ModeInternal,
		StorageDir: dir,
		Ask:        rt.ask,
		OnChange:   ch.record,
		Log:        testLog(t),
	}
	if edit != nil {
		edit(&cfg)
	}
	iss, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {

		rt.unexposeAll()
		_ = iss.Close()
		waitForAQuietStore(t, dir)
	})
	return iss, ch
}

func waitForAQuietStore(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(filepath.Join(dir, "locks"))
		if err != nil || len(entries) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Log("the certificate store still holds a lock after ten seconds")
}

func rootPool(t *testing.T, iss Issuer) *x509.CertPool {
	t.Helper()
	ca, err := iss.CA(context.Background())
	if err != nil {
		t.Fatalf("CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(ca.PEM)) {
		t.Fatal("edge ca did not print a certificate")
	}
	return pool
}

func handshake(t *testing.T, iss Issuer, host string, pool *x509.CertPool) (*x509.Certificate, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srvConf, err := iss.TLSConfig(ctx)
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	deadline := time.Now().Add(20 * time.Second)
	_ = clientConn.SetDeadline(deadline)
	_ = serverConn.SetDeadline(deadline)

	serverErr := make(chan error, 1)
	go func() { serverErr <- tls.Server(serverConn, srvConf).HandshakeContext(ctx) }()

	client := tls.Client(clientConn, &tls.Config{
		ServerName: host,
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	})
	if err := client.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	if err := <-serverErr; err != nil {
		return nil, err
	}
	return client.ConnectionState().PeerCertificates[0], nil
}

func TestInternalModeIssuesDuringTheFirstHandshake(t *testing.T) {
	rt := newRoutes("feat-x.shop.test")
	iss, ch := internalIssuer(t, t.TempDir(), rt, nil)
	pool := rootPool(t, iss)

	leaf, err := handshake(t, iss, "feat-x.shop.test", pool)
	if err != nil {
		t.Fatalf("handshake for a routed host: %v", err)
	}
	if leaf.Issuer.CommonName != InternalCASubject {
		t.Errorf("issuer = %q, want %q", leaf.Issuer.CommonName, InternalCASubject)
	}
	if got := leaf.DNSNames; len(got) != 1 || got[0] != "feat-x.shop.test" {
		t.Errorf("SANs = %v, want [feat-x.shop.test]", got)
	}
	obtained := ch.of(Obtained)
	if len(obtained) != 1 || obtained[0].Host != "feat-x.shop.test" {
		t.Fatalf("obtained = %+v, want one certificate for feat-x.shop.test", obtained)
	}
	if obtained[0].State != Live || obtained[0].IssuerKey != internalIssuerKey || !obtained[0].Managed {
		t.Errorf("obtained = %+v, want it live under %q and managed: it is the configured issuer's",
			obtained[0], internalIssuerKey)
	}

	again, err := handshake(t, iss, "feat-x.shop.test", pool)
	if err != nil {
		t.Fatalf("second handshake: %v", err)
	}
	if again.SerialNumber.Cmp(leaf.SerialNumber) != 0 {
		t.Errorf("serial changed on the second handshake: %s then %s", leaf.SerialNumber, again.SerialNumber)
	}
	if n := len(ch.of(Obtained)); n != 1 {
		t.Errorf("obtained %d certificates, want 1", n)
	}
}

func TestAHostThatIsNotRoutedGetsNoCertificate(t *testing.T) {
	dir := t.TempDir()
	rt := newRoutes("feat-x.shop.test")
	iss, ch := internalIssuer(t, dir, rt, nil)
	pool := rootPool(t, iss)

	if _, err := handshake(t, iss, "nope.shop.test", pool); err == nil {
		t.Fatal("a hostname the machine does not route completed a handshake")
	}
	if asked := rt.asks(); len(asked) == 0 || asked[len(asked)-1] != "nope.shop.test" {
		t.Errorf("the ask hook was asked %v, want it consulted for nope.shop.test", asked)
	}
	if n := len(ch.of(Obtained)); n != 0 {
		t.Errorf("%d certificates were obtained for a name that is not routed", n)
	}
	certs, err := iss.Certificates(context.Background())
	if err != nil {
		t.Fatalf("Certificates: %v", err)
	}
	if len(certs) != 0 {
		t.Errorf("Certificates = %+v, want nothing in the store", certs)
	}

	if entries, err := os.ReadDir(filepath.Join(dir, "certificates")); err == nil && len(entries) > 0 {
		t.Errorf("the store holds %d issuer trees, want none", len(entries))
	}
}

func TestARestartServesFromTheStoreAndAsksAgain(t *testing.T) {
	dir := t.TempDir()
	rt := newRoutes("restart.shop.test")
	first, _ := internalIssuer(t, dir, rt, nil)
	pool := rootPool(t, first)
	leaf, err := handshake(t, first, "restart.shop.test", pool)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, ch := internalIssuer(t, dir, rt, nil)
	same, err := handshake(t, second, "restart.shop.test", pool)
	if err != nil {
		t.Fatalf("handshake after a restart: %v", err)
	}
	if same.SerialNumber.Cmp(leaf.SerialNumber) != 0 {
		t.Errorf("a restart reissued the certificate: %s then %s", leaf.SerialNumber, same.SerialNumber)
	}
	if n := len(ch.of(Obtained)); n != 0 {
		t.Errorf("a restart obtained %d certificates, want 0", n)
	}
	if loaded := ch.of(Loaded); len(loaded) == 0 {
		t.Error("a certificate taken from the store was not reported as loaded")
	}

	if err := second.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rt.unexpose("restart.shop.test")
	third, _ := internalIssuer(t, dir, rt, nil)
	if _, err := handshake(t, third, "restart.shop.test", pool); err == nil {
		t.Error("an unexposed hostname was still served from the store")
	}
}

func TestUnmanageMakesUnexposeImmediate(t *testing.T) {
	rt := newRoutes("unmanage.shop.test")
	iss, _ := internalIssuer(t, t.TempDir(), rt, nil)
	pool := rootPool(t, iss)
	if _, err := handshake(t, iss, "unmanage.shop.test", pool); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	un, ok := iss.(Unmanager)
	if !ok {
		t.Fatal("the issuer does not implement Unmanager: `env unexpose` could not take a name out of the cache")
	}
	rt.unexpose("unmanage.shop.test")
	un.Unmanage([]string{"unmanage.shop.test"})
	if _, err := handshake(t, iss, "unmanage.shop.test", pool); err == nil {
		t.Error("the hostname was still served after it was unexposed and unmanaged")
	}

	rt.expose("unmanage.shop.test")
	if _, err := handshake(t, iss, "unmanage.shop.test", pool); err != nil {
		t.Errorf("re-exposed hostname: %v", err)
	}
}

func TestRenewalReplacesTheCertificate(t *testing.T) {
	rt := newRoutes("renew.shop.test")
	iss, ch := internalIssuer(t, t.TempDir(), rt, func(c *Config) {
		c.RenewalWindowRatio = 1
		c.RenewCheckInterval = 100 * time.Millisecond
	})
	pool := rootPool(t, iss)

	first, err := handshake(t, iss, "renew.shop.test", pool)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	var last *x509.Certificate
	for time.Now().Before(deadline) {
		last, err = handshake(t, iss, "renew.shop.test", pool)
		if err != nil {
			t.Fatalf("handshake while renewing: %v", err)
		}
		if last.SerialNumber.Cmp(first.SerialNumber) != 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if last.SerialNumber.Cmp(first.SerialNumber) == 0 {
		t.Fatalf("the certificate was never renewed inside its renewal window (serial %s)", first.SerialNumber)
	}
	renewed := ch.of(Renewed)
	if len(renewed) == 0 {
		t.Fatal("a renewal was not reported")
	}
	if renewed[0].Serial == "" || renewed[0].Host != "renew.shop.test" {
		t.Errorf("renewed = %+v, want the host and the new serial", renewed[0])
	}
	if renewed[0].Serial == strings.ToUpper(first.SerialNumber.Text(16)) {
		t.Error("the renewal reported the old serial")
	}
}

func TestAnUnexposedHostIsNotRenewed(t *testing.T) {
	rt := newRoutes("gone.shop.test")
	iss, ch := internalIssuer(t, t.TempDir(), rt, func(c *Config) { c.RenewalWindowRatio = 1 })
	pool := rootPool(t, iss)
	if _, err := handshake(t, iss, "gone.shop.test", pool); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	rt.unexpose("gone.shop.test")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, _ = handshake(t, iss, "gone.shop.test", pool)
		time.Sleep(50 * time.Millisecond)
	}
	if n := len(ch.of(Renewed)); n != 0 {
		t.Errorf("%d renewals for a hostname that is not routed any more", n)
	}
}

func TestCertificatesReportWhatEdgeStatusPrints(t *testing.T) {
	rt := newRoutes("feat-x.shop.test", "other.shop.test")
	iss, _ := internalIssuer(t, t.TempDir(), rt, func(c *Config) { c.LeafLifetime = 48 * time.Hour })
	pool := rootPool(t, iss)
	for _, host := range []string{"feat-x.shop.test", "other.shop.test"} {
		if _, err := handshake(t, iss, host, pool); err != nil {
			t.Fatalf("handshake for %s: %v", host, err)
		}
	}
	certs, err := iss.Certificates(context.Background())
	if err != nil {
		t.Fatalf("Certificates: %v", err)
	}
	if len(certs) != 2 {
		t.Fatalf("Certificates = %+v, want two", certs)
	}
	if certs[0].Host != "feat-x.shop.test" || certs[1].Host != "other.shop.test" {
		t.Errorf("hosts = %s, %s; want them sorted", certs[0].Host, certs[1].Host)
	}
	for _, c := range certs {
		switch {
		case c.State != Live:
			t.Errorf("%s: state = %q, want live: it is the configured issuer's", c.Host, c.State)
		case c.IssuerKey != internalIssuerKey:
			t.Errorf("%s: issuer key = %q, want %q", c.Host, c.IssuerKey, internalIssuerKey)
		case c.Serial == "":
			t.Errorf("%s: no serial", c.Host)
		case c.Issuer != InternalCASubject:
			t.Errorf("%s: issuer = %q, want %q", c.Host, c.Issuer, InternalCASubject)
		case !c.Managed:
			t.Errorf("%s: not reported as managed, so nothing would renew it", c.Host)
		case c.Expired(time.Now()):
			t.Errorf("%s: reported as already expired", c.Host)
		case c.NotAfter.Sub(c.NotBefore) < 47*time.Hour:
			t.Errorf("%s: lifetime %s, want the configured 48h", c.Host, c.NotAfter.Sub(c.NotBefore))
		}
	}
}

func TestACertificateDroppedInByHandIsNotManaged(t *testing.T) {
	dir := t.TempDir()
	rt := newRoutes("wildcard.shop.test")
	iss, _ := internalIssuer(t, dir, rt, nil)
	ca := testCA(t, dir, 0)
	issued, err := ca.Issue(context.Background(), csrFor(t, "wildcard.shop.test"))
	if err != nil {
		t.Fatal(err)
	}
	site := filepath.Join(dir, "certificates", internalIssuerKey, "wildcard.shop.test")
	if err := os.MkdirAll(site, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "wildcard.shop.test.crt"), issued.Certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	certs, err := iss.Certificates(context.Background())
	if err != nil {
		t.Fatalf("Certificates: %v", err)
	}
	if len(certs) != 1 {
		t.Fatalf("Certificates = %+v, want the one that was dropped in", certs)
	}
	if certs[0].Managed {
		t.Error("a certificate with no metadata beside it was reported as managed")
	}
}

func TestTLSConfigOffersHTTPAndTheChallengeProtocol(t *testing.T) {
	iss, _ := internalIssuer(t, t.TempDir(), newRoutes(), nil)
	conf, err := iss.TLSConfig(context.Background())
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	if conf.GetCertificate == nil {
		t.Fatal("no GetCertificate: nothing would be issued on demand")
	}
	if len(conf.NextProtos) < 3 || conf.NextProtos[0] != "h2" || conf.NextProtos[1] != "http/1.1" {
		t.Errorf("NextProtos = %v, want h2 and http/1.1 before the ACME protocol", conf.NextProtos)
	}
	if conf.NextProtos[len(conf.NextProtos)-1] != "acme-tls/1" {
		t.Errorf("NextProtos = %v, want the TLS-ALPN-01 responder's protocol", conf.NextProtos)
	}
	if conf.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2", conf.MinVersion)
	}

	other, _ := iss.TLSConfig(context.Background())
	other.NextProtos = []string{"h3"}
	if conf.NextProtos[0] != "h2" {
		t.Error("TLSConfig handed out a shared *tls.Config")
	}
}

func TestACMEModeNeedsNoCAToStart(t *testing.T) {
	dir := t.TempDir()
	iss, err := New(Config{
		Mode: ModeACME,

		Directory:  "https://127.0.0.1:1/dir",
		StorageDir: dir,
		Ask:        newRoutes().ask,
		Log:        testLog(t),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer iss.Close()

	certs, err := iss.Certificates(context.Background())
	if err != nil {
		t.Errorf("Certificates on an empty store: %v", err)
	}
	if len(certs) != 0 {
		t.Errorf("Certificates = %+v, want none", certs)
	}
	if _, err := iss.CA(context.Background()); !errors.Is(err, ErrNoInternalCA) {
		t.Errorf("CA in acme mode = %v, want ErrNoInternalCA", err)
	}
}

func TestHTTPChallengeHandlerAnswersFromStorage(t *testing.T) {
	dir := t.TempDir()
	iss, err := New(Config{
		Mode: ModeACME, Directory: "https://ca.example.test:14000/dir",
		StorageDir: dir, Ask: newRoutes("feat-x.shop.test").ask, Log: testLog(t),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer iss.Close()

	const token = "AbCd-1234_token"
	const keyAuth = "AbCd-1234_token.thumbprint"
	challenge := fmt.Sprintf(`{"type":"http-01","url":"https://ca.example.test/chall/1",`+
		`"status":"pending","token":%q,"keyAuthorization":%q,`+
		`"identifier":{"type":"dns","value":"feat-x.shop.test"}}`, token, keyAuth)

	key := filepath.Join(dir, "acme", "ca.example.test-14000-dir", "challenge_tokens", "feat-x.shop.test.json")
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte(challenge), 0o600); err != nil {
		t.Fatal(err)
	}

	reached := false
	handler := iss.HTTPChallengeHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusMovedPermanently)
	}))

	req := httptest.NewRequest(http.MethodGet, "/.well-known/acme-challenge/"+token, nil)
	req.Host = "feat-x.shop.test"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if reached {
		t.Error("the challenge request fell through to the redirect handler")
	}
	if got := rec.Body.String(); got != keyAuth {
		t.Errorf("challenge response = %q, want the key authorization %q", got, keyAuth)
	}

	reached = false
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !reached {
		t.Error("an ordinary request did not reach the handler behind the challenge responder")
	}
}

func TestInternalModeSolvesNoHTTPChallenge(t *testing.T) {
	iss, _ := internalIssuer(t, t.TempDir(), newRoutes(), nil)
	reached := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
	req := httptest.NewRequest(http.MethodGet, "/.well-known/acme-challenge/anything", nil)
	req.Host = "feat-x.shop.test"
	iss.HTTPChallengeHandler(next).ServeHTTP(httptest.NewRecorder(), req)
	if !reached {
		t.Error("internal mode intercepted an ACME challenge request; it has no challenge to solve")
	}
}

func TestNewRefusesAConfigurationThatCouldNeverWork(t *testing.T) {
	base := func() Config {
		return Config{Mode: ModeInternal, StorageDir: t.TempDir(), Ask: newRoutes().ask}
	}
	for name, edit := range map[string]func(*Config){
		"no ask hook":          func(c *Config) { c.Ask = nil },
		"a renewal ratio > 1":  func(c *Config) { c.RenewalWindowRatio = 1.5 },
		"a negative interval":  func(c *Config) { c.RenewCheckInterval = -time.Second },
		"a negative lifetime":  func(c *Config) { c.LeafLifetime = -time.Hour },
		"a mode that does not": func(c *Config) { c.Mode = Mode("selfsigned") },
	} {
		cfg := base()
		edit(&cfg)
		if iss, err := New(cfg); err == nil {
			_ = iss.Close()
			t.Errorf("%s: New = nil, want an error", name)
		}
	}
}

func TestNewCreatesThePrivateStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "edge", "certs")
	iss, _ := internalIssuer(t, dir, newRoutes(), nil)
	defer iss.Close()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("the store was not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("store mode = %o, want 0700: it holds private keys", perm)
	}
}

func TestTrustedRootsTakesPEMOrAPath(t *testing.T) {
	dir := t.TempDir()

	iss, _ := internalIssuer(t, dir, newRoutes(), nil)
	ca, err := iss.CA(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "root.pem")
	if err := os.WriteFile(path, []byte(ca.PEM), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, roots := range map[string][]string{
		"inline PEM": {ca.PEM},
		"a path":     {path},
		"both":       {ca.PEM, path},
	} {
		pool, err := trustedRoots(roots)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		} else if pool == nil {
			t.Errorf("%s: no pool", name)
		}
	}
	if pool, err := trustedRoots(nil); err != nil || pool != nil {
		t.Errorf("no roots = %v, %v; want the system pool and no error", pool, err)
	}
	if _, err := trustedRoots([]string{filepath.Join(dir, "missing.pem")}); err == nil {
		t.Error("a trusted root that is not there was accepted")
	}
	if _, err := trustedRoots([]string{"-----BEGIN CERTIFICATE-----\nnonsense\n-----END CERTIFICATE-----"}); err == nil {
		t.Error("a PEM block with no certificate in it was accepted")
	}
}

func TestNormalizeHostMatchesTheRouteTable(t *testing.T) {
	for in, want := range map[string]string{
		"Feat-X.Shop.Test":  "feat-x.shop.test",
		"feat-x.shop.test.": "feat-x.shop.test",
		"  feat-x  ":        "feat-x",
		"":                  "",
	} {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTheStoreLayoutIsTheOneEdgeStatusReads(t *testing.T) {
	dir := t.TempDir()
	rt := newRoutes("layout.shop.test")
	iss, _ := internalIssuer(t, dir, rt, nil)
	if _, err := handshake(t, iss, "layout.shop.test", rootPool(t, iss)); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	for _, name := range []string{"layout.shop.test.crt", "layout.shop.test.key", "layout.shop.test.json"} {
		path := filepath.Join(dir, "certificates", internalIssuerKey, "layout.shop.test", name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}

func acmeManager(t *testing.T, dir, directory string, rt *routes) *manager {
	t.Helper()
	if rt == nil {
		rt = newRoutes()
	}
	iss, err := New(Config{
		Mode:       ModeACME,
		Directory:  directory,
		StorageDir: dir,
		Ask:        rt.ask,
		Log:        testLog(t),
	})
	if err != nil {
		t.Fatalf("New for %s: %v", directory, err)
	}
	t.Cleanup(func() { _ = iss.Close() })
	m, ok := iss.(*manager)
	if !ok {
		t.Fatalf("New returned %T, want the manager", iss)
	}
	return m
}

func issuerKeyOf(t *testing.T, m *manager) string {
	t.Helper()
	if len(m.issuers) != 1 {
		t.Fatalf("the manager holds %d issuers, want one", len(m.issuers))
	}
	key := m.issuers[0].IssuerKey()
	if key == "" {
		t.Fatal("the issuer has no storage key")
	}
	return key
}

func forgeLeaf(t *testing.T, dir, issuerKey, host string, notBefore, notAfter time.Time) string {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caSerial, err := serialNumber()
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "Forged CA for " + issuerKey},
		NotBefore:             notBefore.Add(-time.Hour),
		NotAfter:              notAfter.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := serialNumber()
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, key.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writeKey(t, dir, certmagic.StorageKeys.SitePrivateKey(issuerKey, host),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	writeKey(t, dir, certmagic.StorageKeys.SiteMeta(issuerKey, host),
		[]byte(`{"sans":["`+host+`"]}`))
	writeKey(t, dir, certmagic.StorageKeys.SiteCert(issuerKey, host),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return filepath.Join(dir, filepath.FromSlash(certmagic.StorageKeys.CertsSitePrefix(issuerKey, host)))
}

func writeKey(t *testing.T, dir, key string, value []byte) string {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, value, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func lastWritten(t *testing.T, dir, issuerKey, host string, when time.Time) {
	t.Helper()
	site := certmagic.StorageKeys.CertsSitePrefix(issuerKey, host)
	for _, name := range []string{".crt", ".key", ".json"} {
		path := filepath.Join(dir, filepath.FromSlash(site),
			certmagic.StorageKeys.Safe(host)+name)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatalf("set the clock on %s: %v", path, err)
		}
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat %s: %v", path, err)
	}
	return err == nil
}

type staleStore struct {
	dir string

	live *manager

	liveKey string

	firstKey  string
	secondKey string

	firstSite  string
	secondSite string

	internalSite string
}

func threeTreeStore(t *testing.T) staleStore {
	t.Helper()
	dir := t.TempDir()
	now := time.Now()

	first := acmeManager(t, dir, "https://ca-one.example.test:14000/dir", nil)
	firstKey := issuerKeyOf(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close the first issuer: %v", err)
	}

	second := acmeManager(t, dir, "https://ca-two.example.test:14000/dir", nil)
	secondKey := issuerKeyOf(t, second)
	if err := second.Close(); err != nil {
		t.Fatalf("Close the second issuer: %v", err)
	}
	if firstKey == secondKey {
		t.Fatalf("two ACME directories share the storage key %q", firstKey)
	}

	s := staleStore{dir: dir, firstKey: firstKey, secondKey: secondKey}
	s.internalSite = forgeLeaf(t, dir, internalIssuerKey, hostInternal,
		now.Add(-90*24*time.Hour), now.Add(-60*24*time.Hour))
	lastWritten(t, dir, internalIssuerKey, hostInternal, now.Add(-60*24*time.Hour))

	s.firstSite = forgeLeaf(t, dir, firstKey, hostX, now.Add(-90*24*time.Hour), now.Add(-45*24*time.Hour))
	lastWritten(t, dir, firstKey, hostX, now.Add(-45*24*time.Hour))

	s.live = acmeManager(t, dir, "https://ca-two.example.test:14000/dir", newRoutes(hostX))
	s.liveKey = issuerKeyOf(t, s.live)
	if s.liveKey != secondKey {
		t.Fatalf("the configured issuer's key is %q, want %q", s.liveKey, secondKey)
	}
	s.secondSite = forgeLeaf(t, dir, secondKey, hostX, now.Add(-time.Hour), now.Add(60*24*time.Hour))
	return s
}

const (
	hostX        = "feat-x.shop.test"
	hostInternal = "feat-i.shop.internal"
)

func TestCertificatesAccountForAnIssuerThatIsNoLongerConfigured(t *testing.T) {
	s := threeTreeStore(t)
	list, err := s.live.Certificates(context.Background())
	if err != nil {
		t.Fatalf("Certificates: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("Certificates = %+v, want the whole store: one live and two from issuers this "+
			"machine no longer uses", list)
	}

	if list[0].Host != hostInternal || list[1].Host != hostX || list[2].Host != hostX {
		t.Fatalf("hosts = %q, %q, %q; want them sorted by host", list[0].Host, list[1].Host, list[2].Host)
	}
	if list[1].State != Live || list[2].State != Stale {
		t.Errorf("%s is listed %q then %q; want the live row before the stale one",
			hostX, list[1].State, list[2].State)
	}
	if list[1].IssuerKey != s.liveKey {
		t.Errorf("the live certificate is stored under %q, want %q", list[1].IssuerKey, s.liveKey)
	}
	for _, c := range []Certificate{list[0], list[2]} {
		if c.State != Stale {
			t.Errorf("%s (%s) is %q, want stale", c.Host, c.IssuerKey, c.State)
		}
		if c.IssuerKey == "" || c.IssuerKey == s.liveKey {
			t.Errorf("%s names issuer key %q, want the tree it was found in", c.Host, c.IssuerKey)
		}
		if c.Serial == "" || c.NotAfter.IsZero() {
			t.Errorf("%s = %+v, want the leaf read like any other", c.Host, c)
		}
	}
	if list[0].IssuerKey != internalIssuerKey {
		t.Errorf("the internal tree is reported as %q, want %q", list[0].IssuerKey, internalIssuerKey)
	}

	if want := certmagic.StorageKeys.Safe(s.firstKey); list[2].IssuerKey != want {
		t.Errorf("the previous CA's tree is reported as %q, want the directory it is stored under, %q",
			list[2].IssuerKey, want)
	}
}

func TestAStaleCertificateIsNotManagedEvenWithItsMetadataBeside(t *testing.T) {
	s := threeTreeStore(t)
	meta := filepath.Join(s.firstSite, certmagic.StorageKeys.Safe(hostX)+".json")
	if !exists(t, meta) {
		t.Fatalf("the fixture did not store %s, so this proves nothing", meta)
	}
	list, err := s.live.Certificates(context.Background())
	if err != nil {
		t.Fatalf("Certificates: %v", err)
	}
	for _, c := range list {
		switch {
		case c.State == Live && !c.Managed:
			t.Errorf("%s (%s) is live and not managed: nothing would renew it", c.Host, c.IssuerKey)
		case c.State == Stale && c.Managed:
			t.Errorf("%s (%s) is reported as managed, but nothing renews an issuer this machine "+
				"does not use", c.Host, c.IssuerKey)
		}
	}
}

func TestPruneRemovesAStaleCertificateThatExpiredAndWentUntouched(t *testing.T) {
	s := threeTreeStore(t)
	ctx := context.Background()

	res, err := s.live.Prune(ctx, PruneRequest{KeepFor: KeepFor(30 * 24 * time.Hour)})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(res.Removed) != 2 {
		t.Fatalf("removed %+v, want both stale certificates: expired and untouched for longer "+
			"than the retention", res.Removed)
	}
	if len(res.Kept) != 0 {
		t.Errorf("kept %+v, want nothing: every stale certificate was removable", res.Kept)
	}
	for _, c := range res.Removed {
		if c.State != Stale {
			t.Errorf("%s was removed in state %q; only stale material may go", c.Host, c.State)
		}
	}
	if exists(t, s.firstSite) || exists(t, s.internalSite) {
		t.Errorf("a removed site is still on disk: %s, %s", s.firstSite, s.internalSite)
	}
	if !exists(t, s.secondSite) {
		t.Errorf("the configured issuer's site %s was removed", s.secondSite)
	}
}

func TestPruneKeepsAStaleCertificateThatIsFreshOrStillValid(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	gone := acmeManager(t, dir, "https://ca-gone.example.test:14000/dir", nil)
	goneKey := issuerKeyOf(t, gone)
	if err := gone.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	forgeLeaf(t, dir, goneKey, "expired-but-fresh.shop.test", now.Add(-72*time.Hour), now.Add(-time.Hour))
	lastWritten(t, dir, goneKey, "expired-but-fresh.shop.test", now.Add(-time.Hour))

	forgeLeaf(t, dir, goneKey, "untouched-but-valid.shop.test", now.Add(-90*24*time.Hour),
		now.Add(90*24*time.Hour))
	lastWritten(t, dir, goneKey, "untouched-but-valid.shop.test", now.Add(-90*24*time.Hour))

	m := acmeManager(t, dir, "https://ca-now.example.test:14000/dir", nil)
	res, err := m.Prune(context.Background(), PruneRequest{KeepFor: KeepFor(30 * 24 * time.Hour)})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(res.Removed) != 0 {
		t.Errorf("removed %+v; a stale certificate goes only when it is expired AND untouched "+
			"past the retention", res.Removed)
	}
	if len(res.Kept) != 2 {
		t.Errorf("kept %+v, want both of them reported", res.Kept)
	}
	for _, host := range []string{"expired-but-fresh.shop.test", "untouched-but-valid.shop.test"} {
		site := filepath.Join(dir, filepath.FromSlash(certmagic.StorageKeys.CertsSitePrefix(goneKey, host)))
		if !exists(t, site) {
			t.Errorf("%s was removed", site)
		}
	}
}

func TestPruneWithNoRetentionRemovesEveryStaleCertificate(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	gone := acmeManager(t, dir, "https://ca-gone.example.test:14000/dir", nil)
	goneKey := issuerKeyOf(t, gone)
	if err := gone.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	site := forgeLeaf(t, dir, goneKey, "still-valid.shop.test", now.Add(-time.Hour), now.Add(90*24*time.Hour))

	m := acmeManager(t, dir, "https://ca-now.example.test:14000/dir", nil)
	res, err := m.Prune(context.Background(), PruneRequest{KeepFor: KeepFor(0)})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0].Host != "still-valid.shop.test" {
		t.Fatalf("removed %+v, want the still-valid stale certificate: 0 is the explicit escape",
			res.Removed)
	}
	if exists(t, site) {
		t.Errorf("%s survived a prune with no retention", site)
	}
	if res.KeepFor != 0 {
		t.Errorf("the result says the retention was %s, want the 0 it was asked for", res.KeepFor)
	}
}

func TestPruneDryRunRemovesNothingAndSaysTheSame(t *testing.T) {
	s := threeTreeStore(t)
	ctx := context.Background()
	keep := PruneRequest{KeepFor: KeepFor(30 * 24 * time.Hour)}

	dry := keep
	dry.DryRun = true
	planned, err := s.live.Prune(ctx, dry)
	if err != nil {
		t.Fatalf("Prune --dry-run: %v", err)
	}
	if !planned.DryRun {
		t.Error("the result does not say it was a dry run")
	}
	if len(planned.Removed) != 2 {
		t.Fatalf("a dry run reported %+v, want what would go", planned.Removed)
	}
	if !exists(t, s.firstSite) || !exists(t, s.internalSite) {
		t.Fatal("a dry run removed something")
	}

	done, err := s.live.Prune(ctx, keep)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if !sameHosts(planned.Removed, done.Removed) {
		t.Errorf("the dry run named %v and the prune removed %v", hostsOf(planned.Removed), hostsOf(done.Removed))
	}
}

func TestASecondPruneIsANoOp(t *testing.T) {
	s := threeTreeStore(t)
	ctx := context.Background()
	req := PruneRequest{KeepFor: KeepFor(30 * 24 * time.Hour)}
	if _, err := s.live.Prune(ctx, req); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	again, err := s.live.Prune(ctx, req)
	if err != nil {
		t.Fatalf("a second Prune: %v", err)
	}
	if len(again.Removed) != 0 || len(again.Kept) != 0 {
		t.Errorf("a second prune reported %+v / %+v, want an empty store of stale material",
			again.Removed, again.Kept)
	}
	trees, err := s.live.storage.List(ctx, certsPrefix, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(trees) != 1 || filepath.Base(trees[0]) != certmagic.StorageKeys.Safe(s.liveKey) {
		t.Errorf("the store holds %v, want only the configured issuer's tree", trees)
	}
}

func TestPruneLeavesTheConfiguredIssuersTreeExactlyAsItWas(t *testing.T) {
	s := threeTreeStore(t)
	ctx := context.Background()
	before, err := s.live.Certificates(ctx)
	if err != nil {
		t.Fatalf("Certificates: %v", err)
	}
	var live Certificate
	for _, c := range before {
		if c.State == Live {
			live = c
		}
	}
	if live.Serial == "" {
		t.Fatal("the fixture has no live certificate, so this proves nothing")
	}
	files := filesUnder(t, s.secondSite)

	if _, err := s.live.Prune(ctx, PruneRequest{KeepFor: KeepFor(0)}); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got := filesUnder(t, s.secondSite); !equalStrings(got, files) {
		t.Errorf("the configured issuer's site holds %v, want %v", got, files)
	}
	after, err := s.live.Certificates(ctx)
	if err != nil {
		t.Fatalf("Certificates after the prune: %v", err)
	}
	if len(after) != 1 || after[0].Serial != live.Serial || after[0].State != Live {
		t.Errorf("what is left is %+v, want the live certificate %s untouched", after, live.Serial)
	}
}

func TestPruneNeverTouchesTheRootTheAccountOrTheLocks(t *testing.T) {
	s := threeTreeStore(t)
	ctx := context.Background()

	ca := testCA(t, s.dir, 0)
	if ca.Root().PEM == "" {
		t.Fatal("the internal CA did not generate a root")
	}
	planted := []string{
		writeKey(t, s.dir, caRootCertKey, []byte(ca.Root().PEM)),
		writeKey(t, s.dir, "acme/ca-one.example.test-14000-dir/challenge_tokens/"+hostX+".json",
			[]byte(`{"type":"http-01"}`)),
		writeKey(t, s.dir, "acme/ca-one.example.test-14000-dir/users/default/default.key",
			[]byte("-----BEGIN PRIVATE KEY-----\n")),
		writeKey(t, s.dir, "ocsp/"+hostX, []byte("staple")),
		writeKey(t, s.dir, "locks/caramelo_internal_ca.lock", []byte("{}")),
		filepath.Join(s.dir, filepath.FromSlash(caRootKeyKey)),
	}

	if _, err := s.live.Prune(ctx, PruneRequest{KeepFor: KeepFor(0)}); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	for _, path := range planted {
		if !exists(t, path) {
			t.Errorf("a prune removed %s, which is not certificate material of a previous issuer", path)
		}
	}
}

func TestPruneNeverRemovesWhatItCannotJudge(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	gone := acmeManager(t, dir, "https://ca-gone.example.test:14000/dir", nil)
	goneKey := issuerKeyOf(t, gone)
	if err := gone.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	site := forgeLeaf(t, dir, goneKey, "unreadable.shop.test", now.Add(-90*24*time.Hour),
		now.Add(-60*24*time.Hour))
	lastWritten(t, dir, goneKey, "unreadable.shop.test", now.Add(-60*24*time.Hour))
	crt := filepath.Join(site, certmagic.StorageKeys.Safe("unreadable.shop.test")+".crt")
	if err := os.WriteFile(crt, []byte("this is not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := acmeManager(t, dir, "https://ca-now.example.test:14000/dir", nil)
	res, err := m.Prune(context.Background(), PruneRequest{KeepFor: KeepFor(0)})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(res.Removed) != 0 {
		t.Errorf("removed %+v: a leaf that cannot be parsed cannot be judged either", res.Removed)
	}
	if len(res.Kept) != 1 || res.Kept[0].Host != "unreadable.shop.test" {
		t.Errorf("kept %+v, want the site it could not read named", res.Kept)
	}
	if !exists(t, crt) {
		t.Errorf("%s was removed", crt)
	}
}

func TestPruneKeepsTheTreeOfAStaleCertificateItMayNotRemoveYet(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	keep := 30 * 24 * time.Hour
	gone := acmeManager(t, dir, "https://ca-gone.example.test:14000/dir", nil)
	goneKey := issuerKeyOf(t, gone)
	if err := gone.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	const expiredHost = "long-expired.shop.test"
	const validHost = "still-valid.shop.test"
	expiredSite := forgeLeaf(t, dir, goneKey, expiredHost, now.Add(-120*24*time.Hour), now.Add(-90*24*time.Hour))
	lastWritten(t, dir, goneKey, expiredHost, now.Add(-90*24*time.Hour))
	validSite := forgeLeaf(t, dir, goneKey, validHost, now.Add(-90*24*time.Hour), now.Add(90*24*time.Hour))
	lastWritten(t, dir, goneKey, validHost, now.Add(-90*24*time.Hour))

	m := acmeManager(t, dir, "https://ca-now.example.test:14000/dir", nil)
	ctx := context.Background()
	res, err := m.Prune(ctx, PruneRequest{KeepFor: KeepFor(keep)})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0].Host != expiredHost {
		t.Fatalf("removed %v, want only %s: the other leaf of the same tree is still valid",
			hostsOf(res.Removed), expiredHost)
	}
	if len(res.Kept) != 1 || res.Kept[0].Host != validHost {
		t.Fatalf("kept %v, want %s", hostsOf(res.Kept), validHost)
	}
	if exists(t, expiredSite) {
		t.Errorf("%s survived the prune", expiredSite)
	}
	crt := filepath.Join(validSite, certmagic.StorageKeys.Safe(validHost)+".crt")
	if !exists(t, crt) {
		t.Fatalf("%s went with the other site of its tree; the retention decided to keep it", crt)
	}
	tree := certmagic.StorageKeys.CertsPrefix(goneKey)
	if !treeIsStored(t, m, tree) {
		t.Errorf("%s was removed while it still held %s", tree, validHost)
	}

	forgeLeaf(t, dir, goneKey, validHost, now.Add(-120*24*time.Hour), now.Add(-90*24*time.Hour))
	lastWritten(t, dir, goneKey, validHost, now.Add(-90*24*time.Hour))
	again, err := m.Prune(ctx, PruneRequest{KeepFor: KeepFor(keep)})
	if err != nil {
		t.Fatalf("a second Prune: %v", err)
	}
	if len(again.Removed) != 1 || again.Removed[0].Host != validHost {
		t.Fatalf("removed %v, want %s once its leaf expired and went untouched too",
			hostsOf(again.Removed), validHost)
	}
	if treeIsStored(t, m, tree) {
		t.Errorf("%s is still stored with no site left in it", tree)
	}
}

func treeIsStored(t *testing.T, m *manager, tree string) bool {
	t.Helper()
	trees, err := m.storage.List(context.Background(), certsPrefix, false)
	if err != nil {
		t.Fatalf("List %s: %v", certsPrefix, err)
	}
	for _, got := range trees {
		if got == tree {
			return true
		}
	}
	return false
}

type statFailingStorage struct {
	certmagic.Storage

	key string
	err error
}

func (s statFailingStorage) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	if key == s.key {
		return certmagic.KeyInfo{}, s.err
	}
	return s.Storage.Stat(ctx, key)
}

func TestPruneKeepsASiteWhenTheStoreCannotSayWhenItWasWritten(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	gone := acmeManager(t, dir, "https://ca-gone.example.test:14000/dir", nil)
	goneKey := issuerKeyOf(t, gone)
	if err := gone.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	const host = "unstattable.shop.test"
	site := forgeLeaf(t, dir, goneKey, host, now.Add(-120*24*time.Hour), now.Add(-90*24*time.Hour))
	lastWritten(t, dir, goneKey, host, now.Add(-90*24*time.Hour))

	m := acmeManager(t, dir, "https://ca-now.example.test:14000/dir", nil)
	m.storage.Storage = statFailingStorage{
		Storage: m.storage.Storage,
		key:     certmagic.StorageKeys.SiteCert(goneKey, host),
		err:     errors.New("the store cannot say when this was written"),
	}
	crt := filepath.Join(site, certmagic.StorageKeys.Safe(host)+".crt")

	for _, req := range []PruneRequest{{KeepFor: KeepFor(0)}, {}} {
		res, err := m.Prune(context.Background(), req)
		if err != nil {
			t.Fatalf("Prune with a retention of %s: %v", req.Keep(), err)
		}
		if len(res.Removed) != 0 {
			t.Errorf("a retention of %s removed %v: a site whose clock cannot be read cannot be "+
				"judged either", req.Keep(), hostsOf(res.Removed))
		}
		if len(res.Kept) != 1 || res.Kept[0].Host != host {
			t.Errorf("a retention of %s kept %v, want the site it could not stat named",
				req.Keep(), hostsOf(res.Kept))
		}
		if !exists(t, crt) {
			t.Fatalf("%s was removed with a retention of %s", crt, req.Keep())
		}
	}
}

func TestPruneKeepsASiteThatWentAwayWhileItWasBeingJudged(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	gone := acmeManager(t, dir, "https://ca-gone.example.test:14000/dir", nil)
	goneKey := issuerKeyOf(t, gone)
	if err := gone.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	const host = "vanished.shop.test"
	site := forgeLeaf(t, dir, goneKey, host, now.Add(-120*24*time.Hour), now.Add(-90*24*time.Hour))
	lastWritten(t, dir, goneKey, host, now.Add(-90*24*time.Hour))

	m := acmeManager(t, dir, "https://ca-now.example.test:14000/dir", nil)
	m.storage.Storage = statFailingStorage{
		Storage: m.storage.Storage,
		key:     certmagic.StorageKeys.SiteCert(goneKey, host),
		err:     os.ErrNotExist,
	}
	res, err := m.Prune(context.Background(), PruneRequest{KeepFor: KeepFor(0)})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(res.Removed) != 0 {
		t.Errorf("removed %v, want nothing: the leaf was already gone when it was judged",
			hostsOf(res.Removed))
	}
	if len(res.Kept) != 1 || res.Kept[0].Host != host {
		t.Errorf("kept %v, want the site named", hostsOf(res.Kept))
	}
	if !exists(t, site) {
		t.Errorf("%s was removed", site)
	}
}

func TestPruneKeepsASiteWhoseLeafItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode, so an unreadable leaf cannot be forged here")
	}
	dir := t.TempDir()
	now := time.Now()
	gone := acmeManager(t, dir, "https://ca-gone.example.test:14000/dir", nil)
	goneKey := issuerKeyOf(t, gone)
	if err := gone.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	const host = "unreadable-leaf.shop.test"
	site := forgeLeaf(t, dir, goneKey, host, now.Add(-120*24*time.Hour), now.Add(-90*24*time.Hour))
	lastWritten(t, dir, goneKey, host, now.Add(-90*24*time.Hour))
	crt := filepath.Join(site, certmagic.StorageKeys.Safe(host)+".crt")
	if err := os.Chmod(crt, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(crt, 0o600) })

	m := acmeManager(t, dir, "https://ca-now.example.test:14000/dir", nil)
	res, err := m.Prune(context.Background(), PruneRequest{KeepFor: KeepFor(0)})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(res.Removed) != 0 {
		t.Errorf("removed %v: a leaf that cannot be read cannot be judged either", hostsOf(res.Removed))
	}
	if len(res.Kept) != 1 || res.Kept[0].Host != host {
		t.Errorf("kept %v, want the site it could not read named", hostsOf(res.Kept))
	}
	if !exists(t, crt) {
		t.Errorf("%s was removed", crt)
	}
}

func TestPruneRefusesARetentionThatIsNotOne(t *testing.T) {
	s := threeTreeStore(t)
	if _, err := s.live.Prune(context.Background(), PruneRequest{KeepFor: KeepFor(-time.Hour)}); err == nil {
		t.Error("a negative retention was accepted")
	}
	if !exists(t, s.firstSite) {
		t.Error("a refused prune removed something anyway")
	}
}

func TestPruneReportsEveryRemovalAsAChange(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	gone := acmeManager(t, dir, "https://ca-gone.example.test:14000/dir", nil)
	goneKey := issuerKeyOf(t, gone)
	if err := gone.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	forgeLeaf(t, dir, goneKey, "reported.shop.test", now.Add(-90*24*time.Hour), now.Add(-60*24*time.Hour))
	lastWritten(t, dir, goneKey, "reported.shop.test", now.Add(-60*24*time.Hour))

	ch := &changes{}
	iss, err := New(Config{
		Mode: ModeACME, Directory: "https://ca-now.example.test:14000/dir", StorageDir: dir,
		Ask: newRoutes().ask, OnChange: ch.record, Log: testLog(t),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer iss.Close()
	if _, err := iss.Prune(context.Background(), PruneRequest{KeepFor: KeepFor(0)}); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	pruned := ch.of(Pruned)
	if len(pruned) != 1 || pruned[0].Host != "reported.shop.test" {
		t.Errorf("the feed saw %+v, want one pruned certificate", pruned)
	}
}

func hostsOf(list []Certificate) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.Host+" ("+c.IssuerKey+")")
	}
	sort.Strings(out)
	return out
}

func sameHosts(a, b []Certificate) bool { return equalStrings(hostsOf(a), hostsOf(b)) }

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out = append(out, fmt.Sprintf("%s:%d", e.Name(), len(b)))
	}
	sort.Strings(out)
	return out
}
