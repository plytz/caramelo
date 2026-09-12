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
	"sort"
	"strings"
	"sync"
	"sync/atomic"

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

func (m *manager) Certificates(ctx context.Context) ([]Certificate, error) {
	var out []Certificate
	for _, issuer := range m.issuers {
		prefix := certmagic.StorageKeys.CertsPrefix(issuer.IssuerKey())
		keys, err := m.storage.List(ctx, prefix, true)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("certs: listing %s: %w", prefix, err)
		}
		for _, key := range keys {
			if !strings.HasSuffix(key, ".crt") {
				continue
			}
			c, err := m.certificateAt(ctx, key)
			if err != nil {

				m.logf("certs: %s: %v", key, err)
				continue
			}
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].NotAfter.Before(out[j].NotAfter)
	})
	return out, nil
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
	return c, nil
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
