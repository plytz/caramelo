package certs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	if obtained := ch.of(Obtained); len(obtained) != 1 || obtained[0].Host != "feat-x.shop.test" {
		t.Errorf("obtained = %+v, want one certificate for feat-x.shop.test", obtained)
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
