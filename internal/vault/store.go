package vault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/plytz/caramelo/internal/state"
)

type Rows interface {
	PutSecret(ctx context.Context, e state.VaultEntry) (*state.VaultEntry, error)
	Secret(ctx context.Context, scope, app, env, name string) (*state.VaultEntry, error)
	Secrets(ctx context.Context, scope, app, env string) ([]state.VaultEntry, error)
	SecretsFor(ctx context.Context, app, env string) ([]state.VaultEntry, error)
	RemoveSecret(ctx context.Context, scope, app, env, name string) error
}

type IdentityFunc func(context.Context) string

type Options struct {
	Rows Rows

	Cipher Cipher

	Identity IdentityFunc

	Now func() time.Time
}

type DB struct {
	rows     Rows
	cipher   Cipher
	identity IdentityFunc
	now      func() time.Time
}

var _ Store = (*DB)(nil)

func New(o Options) (*DB, error) {
	if o.Rows == nil {
		return nil, errors.New("vault: no store")
	}
	if o.Cipher == nil {
		return nil, fmt.Errorf("vault: no cipher: %w", ErrNoKey)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &DB{rows: o.Rows, cipher: o.Cipher, identity: o.Identity, now: o.Now}, nil
}

func (d *DB) Set(ctx context.Context, r Ref, value string) (*Entry, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if err := ValidateValue(r.Name, value); err != nil {
		return nil, err
	}
	ciphertext, nonce, err := d.cipher.Seal([]byte(value))
	if err != nil {
		return nil, fmt.Errorf("encrypt the %s secret %s: %w", r.Where(), r.Name, err)
	}
	row, err := d.rows.PutSecret(ctx, state.VaultEntry{
		Scope: string(r.Scope), App: r.App, Env: r.Env, Name: r.Name,
		Ciphertext: ciphertext, Nonce: nonce,
		UpdatedBy: d.who(ctx), UpdatedAt: d.now(),
	})
	if err != nil {
		return nil, fmt.Errorf("store the %s secret %s: %w", r.Where(), r.Name, err)
	}

	out := entryOf(*row)
	return &out, nil
}

func (d *DB) Get(ctx context.Context, r Ref) (*Entry, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	row, err := d.rows.Secret(ctx, string(r.Scope), r.App, r.Env, r.Name)
	if errors.Is(err, state.ErrNotFound) {
		return nil, fmt.Errorf("no %s secret %s: %w", r.Where(), r.Name, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("read the %s secret %s: %w", r.Where(), r.Name, err)
	}
	out := entryOf(*row)
	value, err := d.open(*row)
	if err != nil {
		return nil, err
	}
	out.Value = value
	return &out, nil
}

func (d *DB) List(ctx context.Context, app, env string) ([]Entry, error) {
	rows, err := d.layers(ctx, app, env)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(rows))
	for _, row := range rows {
		out = append(out, entryOf(row))
	}
	Sort(out)
	return out, nil
}

func (d *DB) Remove(ctx context.Context, r Ref) error {
	if err := r.Validate(); err != nil {
		return err
	}
	err := d.rows.RemoveSecret(ctx, string(r.Scope), r.App, r.Env, r.Name)
	if errors.Is(err, state.ErrNotFound) {
		return fmt.Errorf("no %s secret %s: %w", r.Where(), r.Name, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("remove the %s secret %s: %w", r.Where(), r.Name, err)
	}
	return nil
}

func (d *DB) Resolve(ctx context.Context, app, env string) (map[string]string, error) {
	rows, err := d.layers(ctx, app, env)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range byScope(rows) {
		value, err := d.open(row)
		if err != nil {
			return nil, err
		}
		out[row.Name] = value
	}
	return out, nil
}

func (d *DB) Sources(ctx context.Context, app, env string) (map[string]Scope, error) {
	rows, err := d.layers(ctx, app, env)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Scope, len(rows))
	for _, row := range byScope(rows) {
		out[row.Name] = Scope(row.Scope)
	}
	return out, nil
}

func (d *DB) Fingerprint(ctx context.Context, app, env string) (string, error) {
	rows, err := d.layers(ctx, app, env)
	if err != nil {
		return "", err
	}
	win := byScope(rows)
	if len(win) == 0 {
		return "", nil
	}
	h := sha256.New()
	for _, row := range win {
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00", row.Scope, row.Name, row.Version)
	}
	return hex.EncodeToString(h.Sum(nil)[:16]), nil
}

func (d *DB) layers(ctx context.Context, app, env string) ([]state.VaultEntry, error) {
	switch {
	case app != "" && env != "":
		return d.rows.SecretsFor(ctx, app, env)
	case app != "":
		machine, err := d.rows.Secrets(ctx, state.VaultScopeMachine, "", "")
		if err != nil {
			return nil, err
		}
		own, err := d.rows.Secrets(ctx, state.VaultScopeApp, app, "")
		if err != nil {
			return nil, err
		}
		return append(machine, own...), nil
	default:
		return d.rows.Secrets(ctx, state.VaultScopeMachine, "", "")
	}
}

func byScope(rows []state.VaultEntry) []state.VaultEntry {
	ranked := append([]state.VaultEntry(nil), rows...)
	sort.SliceStable(ranked, func(i, j int) bool {
		return Scope(ranked[i].Scope).Rank() < Scope(ranked[j].Scope).Rank()
	})
	win := make(map[string]state.VaultEntry, len(ranked))
	for _, row := range ranked {
		win[row.Name] = row
	}
	out := make([]state.VaultEntry, 0, len(win))
	for _, name := range sortedNames(win) {
		out = append(out, win[name])
	}
	return out
}

func sortedNames(m map[string]state.VaultEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (d *DB) open(row state.VaultEntry) (string, error) {
	plain, err := d.cipher.Open(row.Ciphertext, row.Nonce)
	if err != nil {
		return "", fmt.Errorf("read the %s secret %s: %w", entryOf(row).Where(), row.Name, err)
	}
	return string(plain), nil
}

func (d *DB) who(ctx context.Context) string {
	if d.identity == nil {
		return ""
	}
	return d.identity(ctx)
}

func entryOf(row state.VaultEntry) Entry {
	return Entry{
		Ref:       Ref{Scope: Scope(row.Scope), App: row.App, Env: row.Env, Name: row.Name},
		Version:   row.Version,
		UpdatedBy: row.UpdatedBy,
		UpdatedAt: row.UpdatedAt,
	}
}
