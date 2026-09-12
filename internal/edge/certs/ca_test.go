package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
)

func testCA(t *testing.T, dir string, lifetime time.Duration) *internalCA {
	t.Helper()
	ca, err := loadInternalCA(context.Background(), &certmagic.FileStorage{Path: dir}, lifetime)
	if err != nil {
		t.Fatalf("loadInternalCA: %v", err)
	}
	return ca
}

func csrFor(t *testing.T, names ...string) *x509.CertificateRequest {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.CertificateRequest{DNSNames: names}
	if len(names) > 0 {
		tmpl.Subject = pkix.Name{CommonName: names[0]}
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		t.Fatal(err)
	}
	return csr
}

func TestInternalCAIsGeneratedOnceAndKept(t *testing.T) {
	dir := t.TempDir()
	first := testCA(t, dir, 0)
	root := first.Root()
	if root.Subject != InternalCASubject {
		t.Errorf("subject = %q, want %q", root.Subject, InternalCASubject)
	}
	if root.PEM == "" {
		t.Fatal("no PEM to trust")
	}
	if time.Until(root.NotAfter) < 9*365*24*time.Hour {
		t.Errorf("the root expires at %s, which is too soon to be worth distributing", root.NotAfter)
	}

	block, err := parseCertificatePEM([]byte(root.PEM))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(block.Raw)
	if root.Fingerprint != hex.EncodeToString(sum[:]) {
		t.Errorf("fingerprint = %q, want the SHA-256 of the certificate", root.Fingerprint)
	}
	if !block.IsCA || block.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("the root cannot sign certificates")
	}

	second := testCA(t, dir, 0)
	if second.Root().Fingerprint != root.Fingerprint {
		t.Error("the internal CA was regenerated on the second start")
	}
}

func TestInternalCASignsALeafForTheNamesAsked(t *testing.T) {
	ca := testCA(t, t.TempDir(), 36*time.Hour)
	issued, err := ca.Issue(context.Background(), csrFor(t, "feat-x.shop.test", "www.feat-x.shop.test"))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	leaf, err := parseCertificatePEM(issued.Certificate)
	if err != nil {
		t.Fatalf("the issued certificate does not parse: %v", err)
	}
	if len(leaf.DNSNames) != 2 || leaf.DNSNames[0] != "feat-x.shop.test" {
		t.Errorf("SANs = %v, want both names from the request", leaf.DNSNames)
	}
	if leaf.IsCA {
		t.Error("a leaf was issued with the CA bit set")
	}
	if got := leaf.NotAfter.Sub(leaf.NotBefore); got < 36*time.Hour || got > 38*time.Hour {
		t.Errorf("lifetime = %s, want the configured 36h plus the skew allowance", got)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(ca.Root().PEM)) {
		t.Fatal("the root did not parse")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		DNSName: "feat-x.shop.test", Roots: pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("verifying the leaf against the printed root: %v", err)
	}

	other, err := ca.Issue(context.Background(), csrFor(t, "feat-x.shop.test"))
	if err != nil {
		t.Fatal(err)
	}
	otherLeaf, err := parseCertificatePEM(other.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	if otherLeaf.SerialNumber.Cmp(leaf.SerialNumber) == 0 {
		t.Error("two certificates were issued with the same serial")
	}
}

func TestInternalCARefusesNonsense(t *testing.T) {
	ca := testCA(t, t.TempDir(), 0)
	if _, err := ca.Issue(context.Background(), csrFor(t)); err == nil {
		t.Error("a request naming no host was signed")
	}
	if _, err := ca.Issue(context.Background(), nil); err == nil {
		t.Error("a nil request was signed")
	}

	csr := csrFor(t, "feat-x.shop.test")
	csr.Signature = append([]byte(nil), csr.Signature...)
	csr.Signature[0] ^= 0xff
	if _, err := ca.Issue(context.Background(), csr); err == nil {
		t.Error("a request with a broken signature was signed")
	}
}

func TestInternalCAIssuerKeyNamesItsOwnTree(t *testing.T) {
	ca := testCA(t, t.TempDir(), 0)
	if got := ca.IssuerKey(); got != internalIssuerKey {
		t.Errorf("IssuerKey = %q, want %q", got, internalIssuerKey)
	}
}

func TestSerialNumbersAreNeverZero(t *testing.T) {
	for range 8 {
		n, err := serialNumber()
		if err != nil {
			t.Fatal(err)
		}
		if n.Sign() <= 0 {
			t.Fatalf("serial %s is not a positive number", n)
		}
	}
}
