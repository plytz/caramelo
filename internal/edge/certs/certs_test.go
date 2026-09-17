package certs

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
)

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": DefaultMode, "acme": ModeACME, "internal": ModeInternal} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q, nil", in, got, err, want)
		}
	}
	if _, err := ParseMode("selfsigned"); err == nil {
		t.Error("ParseMode(selfsigned) = nil, want an error naming the two modes")
	}
}

func TestConfigValidateRefusesIssuingForAnything(t *testing.T) {
	base := Config{Mode: ModeACME, StorageDir: "/var/lib/caramelo/edge/certs", Email: "ops@example.com",
		Ask: func(context.Context, string) error { return nil }}
	if err := base.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	for name, edit := range map[string]func(*Config){
		"no ask hook":     func(c *Config) { c.Ask = nil },
		"no storage":      func(c *Config) { c.StorageDir = "" },
		"an unknown mode": func(c *Config) { c.Mode = Mode("selfsigned") },
		"acme with no account address": func(c *Config) {
			c.Email = ""
		},
	} {
		c := base
		edit(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want an error", name)
		}
	}

	c := base
	c.Email, c.Directory = "", "https://pebble:14000/dir"
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() with a test CA = %v, want nil", err)
	}
}

func TestNewValidatesBeforeItTouchesTheDisk(t *testing.T) {

	dir := filepath.Join(t.TempDir(), "certs")
	if _, err := New(Config{Mode: ModeACME, StorageDir: dir, Email: "ops@example.com"}); err == nil {
		t.Fatal("New accepted a configuration with no ask hook")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused configuration created %s", dir)
	}
}

func TestCertificateExpired(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	c := Certificate{Host: "feat-x.shop.test", NotAfter: now.Add(time.Hour)}
	if c.Expired(now) {
		t.Error("a certificate valid for another hour is not expired")
	}
	if !c.Expired(now.Add(2 * time.Hour)) {
		t.Error("a certificate an hour past its NotAfter is expired")
	}
	if (Certificate{}).Expired(now) {
		t.Error("a certificate with no expiry recorded must not be reported as expired")
	}
}

func TestStorageLayout(t *testing.T) {
	if !strings.HasPrefix(StoreDir, "edge/") {
		t.Errorf("StoreDir = %q, want it under the edge's own directory", StoreDir)
	}
}

func TestCertificateStateIsAClosedVocabulary(t *testing.T) {
	for _, s := range CertificateStates {
		if !s.Valid() {
			t.Errorf("%q is in CertificateStates and not valid", s)
		}
		if s.String() != string(s) {
			t.Errorf("%q prints as %q", s, s.String())
		}
	}
	for _, s := range []CertificateState{"", "archived", "expired"} {
		if s.Valid() {
			t.Errorf("%q is not one of %v", s, CertificateStates)
		}
	}
	if Live == Stale {
		t.Fatal("live and stale are the same word")
	}
}

func TestTheDerivedCertificatesPrefixIsCertmagics(t *testing.T) {
	for _, key := range []string{internalIssuerKey, "acme-v02.api.example.test-directory"} {
		prefix := certmagic.StorageKeys.CertsPrefix(key)
		if got := path.Dir(prefix); got != certsPrefix {
			t.Errorf("CertsPrefix(%q) = %q, whose parent is %q; the listing walks %q",
				key, prefix, got, certsPrefix)
		}
	}
	if issuerDirOf(certmagic.StorageKeys.SiteCert("an issuer:1", "feat-x.shop.test")) !=
		certmagic.StorageKeys.Safe("an issuer:1") {
		t.Error("the issuer directory read back from a site key is not the one certmagic wrote")
	}
	if issuerDirOf("somewhere/else/feat-x.crt") != "" {
		t.Error("a key outside the certificate prefix was read as an issuer tree")
	}
}

func TestTheRetentionDefaultsToThirtyDays(t *testing.T) {
	if DefaultKeepStale != 720*time.Hour {
		t.Errorf("DefaultKeepStale = %s, want 720h", DefaultKeepStale)
	}
	if got := (PruneRequest{}).Keep(); got != DefaultKeepStale {
		t.Errorf("a request that names no retention keeps for %s, want the default %s",
			got, DefaultKeepStale)
	}
	if got := (PruneRequest{KeepFor: KeepFor(0)}).Keep(); got != 0 {
		t.Errorf("--older-than 0 was read as %s; 0 is the explicit escape, not the default", got)
	}
	if got := (PruneRequest{KeepFor: KeepFor(time.Hour)}).Keep(); got != time.Hour {
		t.Errorf("a request for 1h keeps for %s", got)
	}
	if err := (PruneRequest{KeepFor: KeepFor(-time.Second)}).Validate(); err == nil {
		t.Error("a negative retention validated")
	}
	for _, req := range []PruneRequest{{}, {KeepFor: KeepFor(0)}, {KeepFor: KeepFor(time.Hour)}} {
		if err := req.Validate(); err != nil {
			t.Errorf("%+v: %v", req, err)
		}
	}
}
