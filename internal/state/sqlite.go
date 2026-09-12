package state

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/machine"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

const timeFormat = time.RFC3339Nano

func Open(path string) (Store, error) {
	if path == "" {
		return nil, errors.New("state: no database path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create state directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	s := &store{db: db, path: path}
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	restrict(path)
	return s, nil
}

func restrict(path string) {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); err != nil {
			continue
		}

		_ = os.Chmod(p, 0o600)
	}
}

func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	u := url.URL{Scheme: "file", Path: mustAbs(path), RawQuery: q.Encode()}
	return u.String()
}

func mustAbs(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

type store struct {
	db   *sql.DB
	path string

	notifiers notifiers
}

func (s *store) Path() string { return s.path }

func (s *store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close state store: %w", err)
	}
	return nil
}

func (s *store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}
	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return err
	}
	ms, err := migrations()
	if err != nil {
		return err
	}
	for _, m := range ms {
		if applied[m.version] {
			continue
		}
		if err := s.apply(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (s *store) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_version`)
	if err != nil {
		return nil, fmt.Errorf("read schema_version: %w", err)
	}
	defer rows.Close()
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("read schema_version: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema_version: %w", err)
	}
	return applied, nil
}

func (s *store) apply(ctx context.Context, m migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", m.version, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`,
		m.version, time.Now().UTC().Format(timeFormat)); err != nil {
		return fmt.Errorf("record migration %d: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", m.version, err)
	}
	return nil
}

type migration struct {
	version int
	name    string
	sql     string
}

func migrations() ([]migration, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var ms []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		numPart, rest, ok := strings.Cut(strings.TrimSuffix(e.Name(), ".sql"), "_")
		if !ok {
			return nil, fmt.Errorf("migration %q: want <version>_<name>.sql", e.Name())
		}
		v, err := strconv.Atoi(numPart)
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", e.Name(), err)
		}
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", e.Name(), err)
		}
		ms = append(ms, migration{version: v, name: rest, sql: string(b)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i := 1; i < len(ms); i++ {
		if ms[i].version == ms[i-1].version {
			return nil, fmt.Errorf("duplicate migration version %d", ms[i].version)
		}
	}
	return ms, nil
}

func (s *store) Machine(ctx context.Context) (*machine.Record, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT json FROM machine WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read machine record: %w", err)
	}
	var r machine.Record
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("decode machine record: %w", err)
	}
	return &r, nil
}

func (s *store) SaveMachine(ctx context.Context, r *machine.Record) error {
	if r == nil {
		return errors.New("save machine record: nil record")
	}
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode machine record: %w", err)
	}
	gauged := r.GaugedAt
	if gauged.IsZero() {
		gauged = time.Now()
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO machine (id, json, gauged_at) VALUES (1, ?, ?)
		ON CONFLICT (id) DO UPDATE SET json = excluded.json, gauged_at = excluded.gauged_at`,
		string(b), gauged.UTC().Format(timeFormat))
	if err != nil {
		return fmt.Errorf("save machine record: %w", err)
	}
	return nil
}

func (s *store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read setting %q: %w", key, err)
	}
	return v, nil
}

func (s *store) SetSetting(ctx context.Context, key, value string) error {
	if key == "" {
		return errors.New("set setting: empty key")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("set setting %q: %w", key, err)
	}
	return nil
}

func (s *store) Keys(ctx context.Context) ([]Key, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, type, public_key, fingerprint, options, added_at FROM "keys" ORDER BY added_at, name`)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		var added string
		if err := rows.Scan(&k.Name, &k.Type, &k.PublicKey, &k.Fingerprint, &k.Options, &added); err != nil {
			return nil, fmt.Errorf("list keys: %w", err)
		}
		k.AddedAt, _ = time.Parse(timeFormat, added)
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	return out, nil
}

func (s *store) AddKey(ctx context.Context, k Key) error {
	if k.Name == "" {
		return errors.New("add key: empty name")
	}
	if k.Fingerprint == "" {
		return errors.New("add key: empty fingerprint")
	}
	if k.AddedAt.IsZero() {
		k.AddedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO "keys" (name, type, public_key, fingerprint, options, added_at) VALUES (?, ?, ?, ?, ?, ?)`,
		k.Name, k.Type, k.PublicKey, k.Fingerprint, k.Options, k.AddedAt.UTC().Format(timeFormat))
	if err != nil {
		if isConstraint(err) {

			return ErrExists
		}
		return fmt.Errorf("add key %q: %w", k.Name, err)
	}
	return nil
}

func (s *store) RemoveKey(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM "keys" WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("remove key %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("remove key %q: %w", name, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func isConstraint(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "constraint")
}

func (s *store) RecordSetupRun(ctx context.Context, r SetupRun) error {
	started := r.StartedAt
	if started.IsZero() {
		started = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO setup_runs (run_id, step, status, detail, error, started_at, duration_ns, version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.RunID, r.Step, r.Status, r.Detail, r.Error,
		started.UTC().Format(timeFormat), int64(r.Duration), r.Version)
	if err != nil {
		return fmt.Errorf("record setup run %q step %q: %w", r.RunID, r.Step, err)
	}
	return nil
}

func (s *store) SetupRuns(ctx context.Context, limit int) ([]SetupRun, error) {
	q := `SELECT run_id, step, status, detail, error, started_at, duration_ns, version
	      FROM setup_runs ORDER BY started_at DESC, rowid DESC`
	var args []any
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list setup runs: %w", err)
	}
	defer rows.Close()
	var out []SetupRun
	for rows.Next() {
		var r SetupRun
		var started string
		var ns int64
		if err := rows.Scan(&r.RunID, &r.Step, &r.Status, &r.Detail, &r.Error, &started, &ns, &r.Version); err != nil {
			return nil, fmt.Errorf("list setup runs: %w", err)
		}
		r.StartedAt, _ = time.Parse(timeFormat, started)
		r.Duration = time.Duration(ns)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list setup runs: %w", err)
	}
	return out, nil
}
