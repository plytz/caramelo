package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	VaultScopeMachine = "machine"

	VaultScopeApp = "app"

	VaultScopeEnv = "env"
)

var VaultScopes = []string{VaultScopeMachine, VaultScopeApp, VaultScopeEnv}

type VaultEntry struct {
	Scope string `json:"scope"`
	App   string `json:"app,omitempty"`
	Env   string `json:"env,omitempty"`
	Name  string `json:"name"`

	Ciphertext []byte `json:"-"`
	Nonce      []byte `json:"-"`

	Version int `json:"version"`

	UpdatedBy string    `json:"updated_by,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type VaultStore interface {
	PutSecret(ctx context.Context, e VaultEntry) (*VaultEntry, error)

	Secret(ctx context.Context, scope, app, env, name string) (*VaultEntry, error)

	Secrets(ctx context.Context, scope, app, env string) ([]VaultEntry, error)

	SecretsFor(ctx context.Context, app, env string) ([]VaultEntry, error)

	RemoveSecret(ctx context.Context, scope, app, env, name string) error
}

const vaultColumns = `scope, app, env, name, ciphertext, nonce, version, updated_by, updated_at`

func (s *store) PutSecret(ctx context.Context, e VaultEntry) (*VaultEntry, error) {
	switch {
	case e.Scope == "":
		return nil, errors.New("put secret: empty scope")
	case e.Name == "":
		return nil, errors.New("put secret: empty name")
	case len(e.Ciphertext) == 0:
		return nil, fmt.Errorf("put secret %q: no ciphertext", e.Name)
	case len(e.Nonce) == 0:
		return nil, fmt.Errorf("put secret %q: no nonce", e.Name)
	}
	if err := validVaultScope(e.Scope, e.App, e.Env); err != nil {
		return nil, err
	}
	if e.UpdatedAt.IsZero() {
		e.UpdatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO vault (scope, app, env, name, ciphertext, nonce, version, updated_by, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)
		 ON CONFLICT (scope, app, env, name) DO UPDATE SET
			ciphertext = excluded.ciphertext, nonce = excluded.nonce,
			version = vault.version + 1,
			updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		e.Scope, e.App, e.Env, e.Name, e.Ciphertext, e.Nonce, e.UpdatedBy, formatTime(e.UpdatedAt))
	if err != nil {
		return nil, fmt.Errorf("put secret %q: %w", e.Name, err)
	}
	return s.Secret(ctx, e.Scope, e.App, e.Env, e.Name)
}

func (s *store) Secret(ctx context.Context, scope, app, env, name string) (*VaultEntry, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+vaultColumns+` FROM vault
		WHERE scope = ? AND app = ? AND env = ? AND name = ?`, scope, app, env, name)
	e, err := scanVaultEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read secret %q: %w", name, err)
	}
	return &e, nil
}

func (s *store) Secrets(ctx context.Context, scope, app, env string) ([]VaultEntry, error) {
	return s.queryVault(ctx, "list secrets",
		`SELECT `+vaultColumns+` FROM vault WHERE scope = ? AND app = ? AND env = ? ORDER BY name`,
		scope, app, env)
}

func (s *store) SecretsFor(ctx context.Context, app, env string) ([]VaultEntry, error) {
	return s.queryVault(ctx, fmt.Sprintf("list the secrets of env %q of app %q", env, app),
		`SELECT `+vaultColumns+` FROM vault
		 WHERE (scope = ? AND app = '' AND env = '')
		    OR (scope = ? AND app = ? AND env = '')
		    OR (scope = ? AND app = ? AND env = ?)
		 ORDER BY CASE scope WHEN ? THEN 0 WHEN ? THEN 1 ELSE 2 END, name`,
		VaultScopeMachine,
		VaultScopeApp, app,
		VaultScopeEnv, app, env,
		VaultScopeMachine, VaultScopeApp)
}

func (s *store) RemoveSecret(ctx context.Context, scope, app, env, name string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM vault WHERE scope = ? AND app = ? AND env = ? AND name = ?`,
		scope, app, env, name)
	if err != nil {
		return fmt.Errorf("remove secret %q: %w", name, err)
	}
	return affectedOne(res, fmt.Sprintf("remove secret %q", name))
}

func (s *store) queryVault(ctx context.Context, what, query string, args ...any) ([]VaultEntry, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close()
	var out []VaultEntry
	for rows.Next() {
		e, err := scanVaultEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

func scanVaultEntry(sc scanner) (VaultEntry, error) {
	var e VaultEntry
	var updated string
	if err := sc.Scan(&e.Scope, &e.App, &e.Env, &e.Name, &e.Ciphertext, &e.Nonce, &e.Version,
		&e.UpdatedBy, &updated); err != nil {
		return VaultEntry{}, err
	}
	e.UpdatedAt = parseTime(updated)
	return e, nil
}

func validVaultScope(scope, app, env string) error {
	switch scope {
	case VaultScopeMachine:
		if app != "" || env != "" {
			return fmt.Errorf("a %s secret belongs to no app or env, got app %q env %q", scope, app, env)
		}
	case VaultScopeApp:
		if app == "" {
			return fmt.Errorf("an %s secret needs an app", scope)
		}
		if env != "" {
			return fmt.Errorf("an %s secret belongs to no env, got %q", scope, env)
		}
	case VaultScopeEnv:
		if app == "" || env == "" {
			return fmt.Errorf("an %s secret needs an app and an env, got app %q env %q", scope, app, env)
		}
	default:
		return fmt.Errorf("unknown secret scope %q: want %s, %s or %s",
			scope, VaultScopeMachine, VaultScopeApp, VaultScopeEnv)
	}
	return nil
}
