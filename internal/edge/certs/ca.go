package certs

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
)

const (
	InternalCASubject = "Caramelo Internal CA"

	InternalCALifetime = 10 * 365 * 24 * time.Hour

	DefaultLeafLifetime = 30 * 24 * time.Hour

	internalIssuerKey = "internal"

	caRootCertKey = "caramelo/internal_ca/root.crt"
	caRootKeyKey  = "caramelo/internal_ca/root.key"
	caLockName    = "caramelo_internal_ca"
)

var ErrNoInternalCA = errors.New("certs: this machine issues from a public certificate authority: there is no internal CA root to trust (set tls: internal for names with no public DNS)")

type internalCA struct {
	storage  certmagic.Storage
	lifetime time.Duration

	mu      sync.RWMutex
	cert    *x509.Certificate
	certPEM []byte
	key     crypto.Signer
}

var _ certmagic.Issuer = (*internalCA)(nil)

func loadInternalCA(ctx context.Context, storage certmagic.Storage, lifetime time.Duration) (*internalCA, error) {
	if lifetime <= 0 {
		lifetime = DefaultLeafLifetime
	}
	ca := &internalCA{storage: storage, lifetime: lifetime}
	if err := ca.load(ctx); err == nil {
		return ca, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	if err := storage.Lock(ctx, caLockName); err != nil {
		return nil, fmt.Errorf("certs: locking the internal CA: %w", err)
	}
	defer func() { _ = storage.Unlock(context.WithoutCancel(ctx), caLockName) }()
	if err := ca.load(ctx); err == nil {
		return ca, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := ca.generate(ctx); err != nil {
		return nil, err
	}
	return ca, nil
}

func (c *internalCA) load(ctx context.Context) error {
	certPEM, err := c.storage.Load(ctx, caRootCertKey)
	if err != nil {
		return err
	}
	keyPEM, err := c.storage.Load(ctx, caRootKeyKey)
	if err != nil {
		return err
	}
	cert, err := parseCertificatePEM(certPEM)
	if err != nil {
		return fmt.Errorf("certs: internal CA root: %w", err)
	}
	key, err := parsePrivateKeyPEM(keyPEM)
	if err != nil {
		return fmt.Errorf("certs: internal CA key: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cert, c.certPEM, c.key = cert, certPEM, key
	return nil
}

func (c *internalCA) generate(ctx context.Context) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("certs: generating the internal CA key: %w", err)
	}
	serial, err := serialNumber()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: InternalCASubject, Organization: []string{"Caramelo"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(InternalCALifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return fmt.Errorf("certs: creating the internal CA root: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("certs: parsing the internal CA root: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("certs: encoding the internal CA key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	if err := c.storage.Store(ctx, caRootKeyKey, keyPEM); err != nil {
		return fmt.Errorf("certs: storing the internal CA key: %w", err)
	}
	if err := c.storage.Store(ctx, caRootCertKey, certPEM); err != nil {
		return fmt.Errorf("certs: storing the internal CA root: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cert, c.certPEM, c.key = cert, certPEM, key
	return nil
}

func (c *internalCA) IssuerKey() string { return internalIssuerKey }

func (c *internalCA) Issue(_ context.Context, csr *x509.CertificateRequest) (*certmagic.IssuedCertificate, error) {
	if csr == nil || (len(csr.DNSNames) == 0 && len(csr.IPAddresses) == 0) {
		return nil, errors.New("certs: internal CA: nothing to issue for: the request names no host")
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("certs: internal CA: the request is not signed by its own key: %w", err)
	}
	c.mu.RLock()
	root, key := c.cert, c.key
	c.mu.RUnlock()
	if root == nil || key == nil {
		return nil, errors.New("certs: internal CA: no root loaded")
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}
	name := csr.Subject.CommonName
	if len(csr.DNSNames) > 0 {
		name = csr.DNSNames[0]
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              csr.DNSNames,
		IPAddresses:           csr.IPAddresses,
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(c.lifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, root, csr.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("certs: internal CA: signing %s: %w", name, err)
	}
	return &certmagic.IssuedCertificate{
		Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Metadata:    map[string]any{"issuer": InternalCASubject},
	}, nil
}

func (c *internalCA) Root() CA {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cert == nil {
		return CA{}
	}
	sum := sha256.Sum256(c.cert.Raw)
	return CA{
		Subject:     c.cert.Subject.CommonName,
		PEM:         string(c.certPEM),
		Fingerprint: hex.EncodeToString(sum[:]),
		NotAfter:    c.cert.NotAfter,
	}
}

func serialNumber() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("certs: serial number: %w", err)
	}
	return n.Add(n, big.NewInt(1)), nil
}

func parseCertificatePEM(b []byte) (*x509.Certificate, error) {
	for block, rest := pem.Decode(b); block != nil; block, rest = pem.Decode(rest) {
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
	}
	return nil, errors.New("no CERTIFICATE block")
}

func parsePrivateKeyPEM(b []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("%T cannot sign", key)
	}
	return signer, nil
}
