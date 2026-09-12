package vault

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Scope string

const (
	ScopeMachine Scope = "machine"

	ScopeApp Scope = "app"

	ScopeEnv Scope = "env"

	ScopeDefault Scope = "default"
)

var Scopes = []Scope{ScopeDefault, ScopeMachine, ScopeApp, ScopeEnv}

var WritableScopes = []Scope{ScopeMachine, ScopeApp, ScopeEnv}

func ParseScope(s string) (Scope, error) {
	switch sc := Scope(strings.ToLower(strings.TrimSpace(s))); sc {
	case ScopeMachine, ScopeApp, ScopeEnv:
		return sc, nil
	default:
		return "", fmt.Errorf("unknown secret scope %q: want %s, %s or %s",
			s, ScopeMachine, ScopeApp, ScopeEnv)
	}
}

func (s Scope) String() string { return string(s) }

func (s Scope) Rank() int {
	for i, sc := range Scopes {
		if sc == s {
			return i
		}
	}
	return -1
}

func (s Scope) Beats(other Scope) bool { return s.Rank() > other.Rank() }

type Ref struct {
	Scope Scope  `json:"scope"`
	App   string `json:"app,omitempty"`
	Env   string `json:"env,omitempty"`
	Name  string `json:"name"`
}

func (r Ref) Validate() error {
	if err := ValidateName(r.Name); err != nil {
		return err
	}
	switch r.Scope {
	case ScopeMachine:
		if r.App != "" || r.Env != "" {
			return fmt.Errorf("a %s secret belongs to no app or environment, got app %q env %q",
				r.Scope, r.App, r.Env)
		}
	case ScopeApp:
		if r.App == "" {
			return fmt.Errorf("an %s secret needs an app", r.Scope)
		}
		if r.Env != "" {
			return fmt.Errorf("an %s secret belongs to no environment, got %q", r.Scope, r.Env)
		}
	case ScopeEnv:
		if r.App == "" || r.Env == "" {
			return fmt.Errorf("an %s secret needs an app and an environment, got app %q env %q",
				r.Scope, r.App, r.Env)
		}
	default:
		return fmt.Errorf("unknown secret scope %q: want %s, %s or %s",
			r.Scope, ScopeMachine, ScopeApp, ScopeEnv)
	}
	return nil
}

func (r Ref) Where() string {
	switch r.Scope {
	case ScopeApp:
		return "app " + r.App
	case ScopeEnv:
		return "env " + r.Env
	default:
		return string(r.Scope)
	}
}

func MachineRef(name string) Ref { return Ref{Scope: ScopeMachine, Name: name} }

func AppRef(app, name string) Ref { return Ref{Scope: ScopeApp, App: app, Name: name} }

func EnvRef(app, env, name string) Ref {
	return Ref{Scope: ScopeEnv, App: app, Env: env, Name: name}
}

type Entry struct {
	Ref

	Value string `json:"value,omitempty"`

	Version int `json:"version"`

	UpdatedBy string    `json:"updated_by,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

const Redacted = "<secret>"

func (e Entry) Redact() Entry {
	e.Value = ""
	return e
}

func Sort(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Scope != b.Scope {
			return a.Scope.Rank() < b.Scope.Rank()
		}
		return a.Name < b.Name
	})
}

func ValidateName(name string) error {
	if name == "" {
		return errors.New("a secret needs a name")
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("the secret name %q is longer than %d characters", name, MaxNameLen)
	}
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			if i == 0 && r >= '0' && r <= '9' {
				return fmt.Errorf("the secret name %q starts with a digit: "+
					"a secret is delivered as an environment variable, so it must be a valid one", name)
			}
			return fmt.Errorf("the secret name %q has a %q in it: "+
				"use upper-case letters, digits and underscores (it becomes an environment variable)",
				name, string(r))
		}
	}
	return nil
}

const MaxNameLen = 128

const MaxValueLen = 64 << 10

func ValidateValue(name, value string) error {
	if len(value) > MaxValueLen {
		return fmt.Errorf("the value of %s is %d bytes, more than the %d this vault holds",
			name, len(value), MaxValueLen)
	}
	return nil
}

type Store interface {
	Set(ctx context.Context, r Ref, value string) (*Entry, error)

	Get(ctx context.Context, r Ref) (*Entry, error)

	List(ctx context.Context, app, env string) ([]Entry, error)

	Remove(ctx context.Context, r Ref) error

	Resolve(ctx context.Context, app, env string) (map[string]string, error)

	Sources(ctx context.Context, app, env string) (map[string]Scope, error)

	Fingerprint(ctx context.Context, app, env string) (string, error)
}

type Cipher interface {
	Seal(plaintext []byte) (ciphertext, nonce []byte, err error)

	Open(ciphertext, nonce []byte) ([]byte, error)
}

var (
	ErrNotFound = errors.New("vault: no such secret")

	ErrNoKey = errors.New("vault: no key")

	ErrNotImplemented = errors.New("vault: not implemented")
)

const KeyFile = "vault.key"

func KeyPath(stateDir string) string { return filepath.Join(stateDir, KeyFile) }

const KeyMode = 0o600

const KeySize = 32

const SecretsDirName = "secrets"

func SecretsDir(runDir string) string { return filepath.Join(runDir, SecretsDirName) }

const (
	SecretsDirMode  = 0o700
	SecretsFileMode = 0o600
)

type NotImplemented struct{}

var (
	_ Store  = NotImplemented{}
	_ Cipher = NotImplemented{}
)

func (NotImplemented) Set(_ context.Context, r Ref, _ string) (*Entry, error) {
	return nil, fmt.Errorf("set the %s secret %s: %w", r.Where(), r.Name, ErrNotImplemented)
}

func (NotImplemented) Get(_ context.Context, r Ref) (*Entry, error) {
	return nil, fmt.Errorf("read the %s secret %s: %w", r.Where(), r.Name, ErrNotImplemented)
}

func (NotImplemented) List(_ context.Context, app, env string) ([]Entry, error) {
	return nil, fmt.Errorf("list the secrets of app %q env %q: %w", app, env, ErrNotImplemented)
}

func (NotImplemented) Remove(_ context.Context, r Ref) error {
	return fmt.Errorf("remove the %s secret %s: %w", r.Where(), r.Name, ErrNotImplemented)
}

func (NotImplemented) Resolve(_ context.Context, app, env string) (map[string]string, error) {
	return nil, fmt.Errorf("resolve the secrets of app %q env %q: %w", app, env, ErrNotImplemented)
}

func (NotImplemented) Sources(_ context.Context, app, env string) (map[string]Scope, error) {
	return nil, fmt.Errorf("resolve the secrets of app %q env %q: %w", app, env, ErrNotImplemented)
}

func (NotImplemented) Fingerprint(_ context.Context, app, env string) (string, error) {
	return "", fmt.Errorf("fingerprint the secrets of app %q env %q: %w", app, env, ErrNotImplemented)
}

func (NotImplemented) Seal([]byte) ([]byte, []byte, error) {
	return nil, nil, ErrNotImplemented
}

func (NotImplemented) Open([]byte, []byte) ([]byte, error) {
	return nil, ErrNotImplemented
}
