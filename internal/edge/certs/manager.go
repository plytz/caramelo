package certs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"
)

type manager struct {
	cfg     Config
	storage *sealedStorage
	logger  *zap.Logger

	magic   atomic.Pointer[certmagic.Config]
	cache   *certmagic.Cache
	issuers []certmagic.Issuer

	acme *certmagic.ACMEIssuer
	ca   *internalCA

	closeOnce sync.Once
}

var _ Issuer = (*manager)(nil)

func newManager(cfg Config) (*manager, error) {

	if err := os.MkdirAll(cfg.StorageDir, 0o700); err != nil {
		return nil, fmt.Errorf("certs: certificate store %s: %w", cfg.StorageDir, err)
	}

	mode, err := ParseMode(string(cfg.Mode))
	if err != nil {
		return nil, err
	}
	cfg.Mode = mode
	m := &manager{
		cfg:     cfg,
		storage: newSealedStorage(&certmagic.FileStorage{Path: cfg.StorageDir}),
		logger:  newLogger(cfg.Log),
	}
	m.cache = certmagic.NewCache(certmagic.CacheOptions{

		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
			if magic := m.magic.Load(); magic != nil {
				return magic, nil
			}
			return nil, errors.New("certs: the certificate manager is still starting")
		},
		RenewCheckInterval: cfg.RenewCheckInterval,
		Logger:             m.logger,
	})
	m.magic.Store(certmagic.New(m.cache, certmagic.Config{
		Storage: m.storage,
		Logger:  m.logger,

		OnDemand:           &certmagic.OnDemandConfig{DecisionFunc: m.ask},
		OnEvent:            m.onEvent,
		RenewalWindowRatio: cfg.RenewalWindowRatio,

		DisableARI: cfg.Mode == ModeInternal,
	}))
	if err := m.setIssuers(context.Background()); err != nil {
		m.cache.Stop()
		return nil, err
	}
	return m, nil
}

func (m *manager) setIssuers(ctx context.Context) error {
	switch m.cfg.Mode {
	case ModeInternal:
		ca, err := loadInternalCA(ctx, m.storage, m.cfg.LeafLifetime)
		if err != nil {
			return err
		}
		m.ca = ca
		m.issuers = []certmagic.Issuer{ca}
	default:
		roots, err := trustedRoots(m.cfg.TrustedRoots)
		if err != nil {
			return err
		}
		m.acme = certmagic.NewACMEIssuer(m.magic.Load(), certmagic.ACMEIssuer{
			CA:    m.cfg.directory(),
			Email: m.cfg.Email,

			Agreed:  true,
			Profile: m.cfg.ACMEProfile,

			TrustedRoots: roots,
			Logger:       m.logger,
		})
		m.issuers = []certmagic.Issuer{m.acme}
	}
	m.magic.Load().Issuers = m.issuers
	return nil
}

func (m *manager) ask(ctx context.Context, name string) error {
	host := normalizeHost(name)
	if host == "" {
		return errors.New("certs: a certificate was asked for with no server name")
	}
	if m.cfg.Ask == nil {
		return fmt.Errorf("certs: %s: no ask hook: this machine serves no names", host)
	}
	if err := m.cfg.Ask(ctx, host); err != nil {
		return fmt.Errorf("certs: %s: %w", host, err)
	}
	return nil
}

func (m *manager) TLSConfig(context.Context) (*tls.Config, error) {
	conf := m.magic.Load().TLSConfig()
	conf.NextProtos = append([]string{"h2", "http/1.1"}, conf.NextProtos...)
	conf.MinVersion = tls.VersionTLS12
	return conf, nil
}

func (m *manager) HTTPChallengeHandler(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	if m.acme == nil {
		return next
	}
	return m.acme.HTTPChallengeHandler(next)
}

var certsPrefix = path.Dir(certmagic.StorageKeys.CertsPrefix("x"))

const pruneLockName = "caramelo_certs_prune"

type storedCertificate struct {
	Certificate

	crtKey string

	sitePrefix string

	treePrefix string

	unreadable error
}

func (m *manager) Certificates(ctx context.Context) ([]Certificate, error) {
	stored, err := m.stored(ctx)
	if err != nil {
		return nil, err
	}
	var out []Certificate
	for _, s := range stored {
		if s.unreadable != nil {
			continue
		}
		out = append(out, s.Certificate)
	}
	return out, nil
}

func (m *manager) stored(ctx context.Context) ([]storedCertificate, error) {
	trees, err := m.storage.List(ctx, certsPrefix, false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("certs: listing %s: %w", certsPrefix, err)
	}
	var out []storedCertificate
	for _, tree := range trees {
		issuerKey, state := m.issuerOf(path.Base(tree))
		keys, err := m.storage.List(ctx, tree, true)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("certs: listing %s: %w", tree, err)
		}
		for _, key := range keys {
			if !strings.HasSuffix(key, ".crt") {
				continue
			}
			s := storedCertificate{
				crtKey:     key,
				sitePrefix: path.Dir(key),
				treePrefix: tree,
			}
			c, err := m.certificateAt(ctx, key)
			if err != nil {

				m.logf("certs: %s: %v", key, err)
				s.unreadable = err
				c = Certificate{
					Host:      normalizeHost(path.Base(s.sitePrefix)),
					IssuerKey: issuerKey,
					State:     state,
				}
			}
			s.Certificate = c
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		if out[i].State != out[j].State {
			return out[i].State == Live
		}
		return out[i].NotAfter.Before(out[j].NotAfter)
	})
	return out, nil
}

func (m *manager) issuerOf(dir string) (string, CertificateState) {
	for _, issuer := range m.issuers {
		key := issuer.IssuerKey()

		if certmagic.StorageKeys.Safe(key) == dir {
			return key, Live
		}
	}
	return dir, Stale
}

func (m *manager) certificateAt(ctx context.Context, key string) (Certificate, error) {
	b, err := m.storage.Load(ctx, key)
	if err != nil {
		return Certificate{}, err
	}
	leaf, err := parseCertificatePEM(b)
	if err != nil {
		return Certificate{}, err
	}
	c := certificateFrom(leaf)
	c.Managed = m.storage.Exists(ctx, strings.TrimSuffix(key, ".crt")+".json")
	issuerKey, state := m.issuerOf(issuerDirOf(key))
	c.IssuerKey, c.State = issuerKey, state
	c.Managed = c.Managed && state == Live
	return c, nil
}

func issuerDirOf(key string) string {
	rest, ok := strings.CutPrefix(key, certsPrefix+"/")
	if !ok {
		return ""
	}
	dir, _, _ := strings.Cut(rest, "/")
	return dir
}

func (m *manager) Prune(ctx context.Context, req PruneRequest) (*PruneResult, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	keep := req.Keep()
	res := &PruneResult{KeepFor: keep, DryRun: req.DryRun}

	stored, err := m.stored(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var victims []storedCertificate
	for _, s := range stored {
		if s.State != Stale {
			continue
		}
		removable, err := m.removable(ctx, s, keep, now)
		if err != nil {
			m.logf("certs: %s: kept: %v", s.crtKey, err)
			res.Kept = append(res.Kept, s.Certificate)
			continue
		}
		if !removable {
			res.Kept = append(res.Kept, s.Certificate)
			continue
		}
		victims = append(victims, s)
	}
	if len(victims) == 0 || req.DryRun {
		for _, v := range victims {
			res.Removed = append(res.Removed, v.Certificate)
		}
		return res, nil
	}

	if err := m.storage.Lock(ctx, pruneLockName); err != nil {
		return nil, fmt.Errorf("certs: locking the certificate store to prune it: %w", err)
	}
	defer func() { _ = m.storage.Unlock(context.WithoutCancel(ctx), pruneLockName) }()

	trees := map[string]bool{}
	for _, v := range victims {
		removable, err := m.removable(ctx, v, keep, now)
		if err != nil {
			m.logf("certs: %s: kept: %v", v.crtKey, err)
			res.Kept = append(res.Kept, v.Certificate)
			continue
		}
		if !removable {
			res.Kept = append(res.Kept, v.Certificate)
			continue
		}
		if err := m.storage.Delete(ctx, v.sitePrefix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return res, fmt.Errorf("certs: removing %s: %w", v.sitePrefix, err)
		}
		trees[v.treePrefix] = true
		res.Removed = append(res.Removed, v.Certificate)
		if m.cfg.OnChange != nil {
			m.cfg.OnChange(Pruned, v.Certificate, "")
		}
	}
	for tree := range trees {
		if err := m.removeEmptyTree(ctx, tree); err != nil {
			m.logf("certs: %s: %v", tree, err)
		}
	}
	return res, nil
}

func (m *manager) removable(ctx context.Context, s storedCertificate, keep time.Duration, now time.Time) (bool, error) {
	if s.State != Stale {
		return false, nil
	}
	if s.unreadable != nil {
		return false, s.unreadable
	}
	info, err := m.storage.Stat(ctx, s.crtKey)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("when it was last written: %w", err)
	}
	if keep == 0 {
		return true, nil
	}
	return s.Expired(now) && now.Sub(info.Modified) >= keep, nil
}

func (m *manager) removeEmptyTree(ctx context.Context, tree string) error {
	left, err := m.storage.List(ctx, tree, false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("listing what is left of %s: %w", tree, err)
	}
	if len(left) > 0 {
		return nil
	}
	if err := m.storage.Delete(ctx, tree); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the empty %s: %w", tree, err)
	}
	return nil
}

func (m *manager) CA(context.Context) (CA, error) {
	if m.ca == nil {
		return CA{}, ErrNoInternalCA
	}
	root := m.ca.Root()
	if root.PEM == "" {
		return CA{}, errors.New("certs: the internal CA root is not loaded")
	}
	return root, nil
}

func (m *manager) Unmanage(hosts []string) {
	subjects := make([]certmagic.SubjectIssuer, 0, len(hosts))
	for _, host := range hosts {
		if h := normalizeHost(host); h != "" {
			subjects = append(subjects, certmagic.SubjectIssuer{Subject: h})
		}
	}
	if len(subjects) > 0 {
		m.cache.RemoveManaged(subjects)
	}
}

func (m *manager) Close() error {
	var err error
	m.closeOnce.Do(func() {
		m.cache.Stop()
		if !m.storage.Close() {
			err = errors.New("certs: a certificate was still being written when the store closed")
		}
	})
	return err
}

func (m *manager) onEvent(ctx context.Context, event string, data map[string]any) error {
	if m.cfg.OnChange == nil {
		return nil
	}
	switch event {
	case "cert_obtained":
		change := Obtained
		if renewal, _ := data["renewal"].(bool); renewal {
			change = Renewed
		}
		c := Certificate{Host: normalizeHost(stringOf(data["identifier"])), Managed: true}

		if key := stringOf(data["certificate_path"]); key != "" {
			if stored, err := m.certificateAt(ctx, key); err == nil {
				c = stored
			}
		}
		m.cfg.OnChange(change, c, "")
	case "cert_failed":
		host := normalizeHost(stringOf(data["identifier"]))
		detail := ""
		if err, ok := data["error"].(error); ok && err != nil {
			detail = err.Error()
		}
		m.cfg.OnChange(Failed, Certificate{Host: host}, detail)
	case "cached_managed_cert":
		for _, name := range sansOf(data["sans"]) {
			m.cfg.OnChange(Loaded, Certificate{Host: normalizeHost(name), Managed: true}, "")
		}
	}
	return nil
}

func (m *manager) logf(format string, args ...any) {
	if m.cfg.Log != nil {
		m.cfg.Log(format, args...)
	}
}

func (c Config) directory() string {
	if d := strings.TrimSpace(c.Directory); d != "" {
		return d
	}
	return LetsEncryptProduction
}

func trustedRoots(roots []string) (*x509.CertPool, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	pool := x509.NewCertPool()
	for _, root := range roots {
		pemBytes := []byte(root)
		if !strings.Contains(root, "-----BEGIN") {
			b, err := os.ReadFile(root)
			if err != nil {
				return nil, fmt.Errorf("certs: trusted root %s: %w", root, err)
			}
			pemBytes = b
		}
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("certs: trusted root %s: no certificate in it", short(root))
		}
	}
	return pool, nil
}

func certificateFrom(leaf *x509.Certificate) Certificate {
	host := leaf.Subject.CommonName
	if len(leaf.DNSNames) > 0 {
		host = leaf.DNSNames[0]
	}
	return Certificate{
		Host:      normalizeHost(host),
		Issuer:    leaf.Issuer.CommonName,
		Serial:    strings.ToUpper(leaf.SerialNumber.Text(16)),
		NotBefore: leaf.NotBefore,
		NotAfter:  leaf.NotAfter,
	}
}

func normalizeHost(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".")
	return strings.ToLower(s)
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

func sansOf(v any) []string {
	switch names := v.(type) {
	case []string:
		return names
	case []any:
		out := make([]string, 0, len(names))
		for _, n := range names {
			if s := stringOf(n); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}
