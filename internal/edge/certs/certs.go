package certs

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const StoreDir = "edge/certs"

type Mode string

const (
	ModeACME Mode = "acme"

	ModeInternal Mode = "internal"
)

var Modes = []Mode{ModeACME, ModeInternal}

const DefaultMode = ModeACME

func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.TrimSpace(s)); m {
	case "":
		return DefaultMode, nil
	case ModeACME, ModeInternal:
		return m, nil
	default:
		return "", fmt.Errorf("unknown tls mode %q: want %s", s, joinModes())
	}
}

func joinModes() string {
	out := make([]string, len(Modes))
	for i, m := range Modes {
		out[i] = string(m)
	}
	return strings.Join(out, " or ")
}

const LetsEncryptProduction = "https://acme-v02.api.letsencrypt.org/directory"

type AskFunc func(ctx context.Context, host string) error

type Config struct {
	Mode Mode

	Directory string

	Email string

	StorageDir string

	Ask AskFunc

	TrustedRoots []string

	ACMEProfile string

	RenewalWindowRatio float64

	RenewCheckInterval time.Duration

	LeafLifetime time.Duration

	OnChange ChangeFunc

	Log LogFunc
}

type LogFunc func(format string, args ...any)

type Change string

const (
	Obtained Change = "obtained"

	Renewed Change = "renewed"

	Loaded Change = "loaded"

	Failed Change = "failed"
)

type ChangeFunc func(change Change, cert Certificate, detail string)

func (c Config) Validate() error {
	mode, err := ParseMode(string(c.Mode))
	if err != nil {
		return err
	}
	if c.StorageDir == "" {
		return errors.New("certs: no storage directory")
	}
	if c.Ask == nil {
		return errors.New("certs: no ask hook: the edge issues only for hostnames it routes")
	}
	if mode == ModeACME && c.Email == "" && c.Directory == "" {

		return errors.New("certs: acme_email is required with the default ACME directory")
	}
	if c.RenewalWindowRatio < 0 || c.RenewalWindowRatio > 1 {
		return fmt.Errorf("certs: renewal window ratio %v: want 0 (the default) to 1", c.RenewalWindowRatio)
	}
	if c.RenewCheckInterval < 0 {
		return fmt.Errorf("certs: renew check interval %s: want a positive duration, or 0 for the default", c.RenewCheckInterval)
	}
	if c.LeafLifetime < 0 {
		return fmt.Errorf("certs: leaf lifetime %s: want a positive duration, or 0 for the default", c.LeafLifetime)
	}
	return nil
}

type Certificate struct {
	Host string `json:"host"`

	Issuer string `json:"issuer,omitempty"`

	Serial string `json:"serial,omitempty"`

	NotBefore time.Time `json:"not_before,omitempty"`
	NotAfter  time.Time `json:"not_after,omitempty"`

	Managed bool `json:"managed"`
}

func (c Certificate) Expired(t time.Time) bool {
	return !c.NotAfter.IsZero() && t.After(c.NotAfter)
}

type CA struct {
	Subject string `json:"subject,omitempty"`

	PEM string `json:"pem"`

	Fingerprint string `json:"fingerprint,omitempty"`

	NotAfter time.Time `json:"not_after,omitempty"`
}

type Issuer interface {
	TLSConfig(ctx context.Context) (*tls.Config, error)

	HTTPChallengeHandler(next http.Handler) http.Handler

	Certificates(ctx context.Context) ([]Certificate, error)

	CA(ctx context.Context) (CA, error)

	Close() error
}

type Unmanager interface {
	Unmanage(hosts []string)
}

func New(cfg Config) (Issuer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return newManager(cfg)
}
