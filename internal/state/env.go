package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type App struct {
	Name          string `json:"name"`
	RepoPath      string `json:"repo_path"`
	DefaultBranch string `json:"default_branch,omitempty"`

	Stack     string    `json:"stack,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

const (
	EnvCreating   = "creating"
	EnvReady      = "ready"
	EnvFailed     = "failed"
	EnvDestroying = "destroying"
)

type EnvRecord struct {
	ID        int64  `json:"id"`
	App       string `json:"app"`
	Name      string `json:"name"`
	Branch    string `json:"branch"`
	Commit    string `json:"commit,omitempty"`
	Worktree  string `json:"worktree"`
	PortBase  int    `json:"port_base"`
	PortCount int    `json:"port_count"`
	Status    string `json:"status"`

	ConfigJSON string `json:"config_json,omitempty"`

	VarsJSON string `json:"vars_json,omitempty"`

	VPNIP string `json:"vpn_ip,omitempty"`

	Mode string `json:"mode,omitempty"`

	Protected bool `json:"protected,omitempty"`

	ReleaseID int64 `json:"release_id,omitempty"`
	DeployID  int64 `json:"deploy_id,omitempty"`

	CreatedBy string `json:"created_by,omitempty"`

	Owner string `json:"owner,omitempty"`

	Via       string    `json:"via,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const (
	ResourceBranch    = "branch"
	ResourceWorktree  = "worktree"
	ResourceVolume    = "volume"
	ResourceContainer = "container"
)

type EnvResource struct {
	ID    int64  `json:"id"`
	EnvID int64  `json:"env_id"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Dep   string `json:"dep,omitempty"`

	Service string `json:"service,omitempty"`

	Replica   int       `json:"replica,omitempty"`
	Port      int       `json:"port,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type EnvEvent struct {
	ID     int64  `json:"id"`
	EnvID  int64  `json:"env_id"`
	Action string `json:"action"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`

	Service  string `json:"service,omitempty"`
	Replica  int    `json:"replica,omitempty"`
	Step     string `json:"step,omitempty"`
	Identity string `json:"identity,omitempty"`

	JSON string    `json:"json,omitempty"`
	At   time.Time `json:"at"`

	Machine string `json:"machine,omitempty"`

	App string `json:"app,omitempty"`
	Env string `json:"env,omitempty"`
}

type EventFilter struct {
	EnvID int64

	App string

	Since time.Time

	Limit int
}

type EnvStore interface {
	Apps(ctx context.Context) ([]App, error)

	App(ctx context.Context, name string) (*App, error)

	AddApp(ctx context.Context, a App) error

	SetAppDefaultBranch(ctx context.Context, name, branch string) error

	Envs(ctx context.Context, app string) ([]EnvRecord, error)

	Env(ctx context.Context, app, name string) (*EnvRecord, error)

	CreateEnv(ctx context.Context, r EnvRecord) (*EnvRecord, error)

	UpdateEnvStatus(ctx context.Context, id int64, status string) error

	UpdateEnv(ctx context.Context, r EnvRecord) error

	DeleteEnv(ctx context.Context, id int64) error

	SetEnvOwner(ctx context.Context, id int64, owner string) error

	SetEnvVia(ctx context.Context, id int64, via string) error

	AddResource(ctx context.Context, r EnvResource) error

	Resources(ctx context.Context, envID int64) ([]EnvResource, error)

	DeleteResource(ctx context.Context, id int64) error

	AddEvent(ctx context.Context, e EnvEvent) error

	Events(ctx context.Context, envID int64, limit int) ([]EnvEvent, error)

	QueryEvents(ctx context.Context, f EventFilter) ([]EnvEvent, error)
}

const appColumns = `name, repo_path, default_branch, stack, created_at`

func (s *store) Apps(ctx context.Context) ([]App, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+appColumns+` FROM apps ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, fmt.Errorf("list apps: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	return out, nil
}

func (s *store) App(ctx context.Context, name string) (*App, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+appColumns+` FROM apps WHERE name = ?`, name)
	a, err := scanApp(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read app %q: %w", name, err)
	}
	return &a, nil
}

func (s *store) AddApp(ctx context.Context, a App) error {
	if a.Name == "" {
		return errors.New("add app: empty name")
	}
	if a.RepoPath == "" {
		return errors.New("add app: empty repository path")
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO apps (name, repo_path, default_branch, stack, created_at) VALUES (?, ?, ?, ?, ?)`,
		a.Name, a.RepoPath, a.DefaultBranch, a.Stack, formatTime(a.CreatedAt))
	if err != nil {
		if isUniqueConstraint(err) {
			return ErrExists
		}
		return fmt.Errorf("add app %q: %w", a.Name, err)
	}
	return nil
}

func (s *store) SetAppDefaultBranch(ctx context.Context, name, branch string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE apps SET default_branch = ? WHERE name = ?`, branch, name)
	if err != nil {
		return fmt.Errorf("set default branch of app %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set default branch of app %q: %w", name, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func scanApp(sc scanner) (App, error) {
	var a App
	var created string
	if err := sc.Scan(&a.Name, &a.RepoPath, &a.DefaultBranch, &a.Stack, &created); err != nil {
		return App{}, err
	}
	a.CreatedAt = parseTime(created)
	return a, nil
}

const envColumns = `id, app, name, branch, "commit", worktree, port_base, port_count, status,
	config_json, vars_json, vpn_ip, mode, protected, release_id, deploy_id,
	created_by, owner, via, created_at, updated_at`

func (s *store) Envs(ctx context.Context, app string) ([]EnvRecord, error) {
	q := `SELECT ` + envColumns + ` FROM envs`
	var args []any
	if app != "" {
		q += ` WHERE app = ?`
		args = append(args, app)
	}

	q += ` ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list envs: %w", err)
	}
	defer rows.Close()
	var out []EnvRecord
	for rows.Next() {
		r, err := scanEnv(rows)
		if err != nil {
			return nil, fmt.Errorf("list envs: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list envs: %w", err)
	}
	return out, nil
}

func (s *store) Env(ctx context.Context, app, name string) (*EnvRecord, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+envColumns+` FROM envs WHERE app = ? AND name = ?`, app, name)
	r, err := scanEnv(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read env %q of app %q: %w", name, app, err)
	}
	return &r, nil
}

func (s *store) CreateEnv(ctx context.Context, r EnvRecord) (*EnvRecord, error) {
	switch {
	case r.App == "":
		return nil, errors.New("create env: empty app")
	case r.Name == "":
		return nil, errors.New("create env: empty name")
	case r.PortBase <= 0:
		return nil, fmt.Errorf("create env %q: port base %d is not a port", r.Name, r.PortBase)
	}
	now := time.Now()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = r.CreatedAt
	}
	if r.Status == "" {
		r.Status = EnvCreating
	}

	if r.Owner == "" {
		r.Owner = r.CreatedBy
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO envs (app, name, branch, "commit", worktree, port_base, port_count, status,
			config_json, vars_json, vpn_ip, mode, protected, release_id, deploy_id,
			created_by, owner, via, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.App, r.Name, r.Branch, r.Commit, r.Worktree, r.PortBase, r.PortCount, r.Status,
		r.ConfigJSON, r.VarsJSON, r.VPNIP, nonEmptyOr(r.Mode, EnvModeDev), r.Protected,
		nullID(r.ReleaseID), nullID(r.DeployID),
		r.CreatedBy, r.Owner, r.Via, formatTime(r.CreatedAt), formatTime(r.UpdatedAt))
	if err != nil {

		if isUniqueConstraint(err) {
			return nil, ErrExists
		}
		if isForeignKeyConstraint(err) {
			return nil, fmt.Errorf("create env %q: no such app %q", r.Name, r.App)
		}
		return nil, fmt.Errorf("create env %q: %w", r.Name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create env %q: %w", r.Name, err)
	}
	r.ID = id
	return &r, nil
}

func (s *store) UpdateEnvStatus(ctx context.Context, id int64, status string) error {
	if status == "" {
		return errors.New("update env status: empty status")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE envs SET status = ?, updated_at = ? WHERE id = ?`,
		status, formatTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("update status of env %d: %w", id, err)
	}
	return affectedOne(res, fmt.Sprintf("update status of env %d", id))
}

func (s *store) UpdateEnv(ctx context.Context, r EnvRecord) error {
	if r.ID == 0 {
		return errors.New("update env: no id")
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE envs SET branch = ?, "commit" = ?, worktree = ?, status = ?,
			config_json = ?, vars_json = ?, updated_at = ? WHERE id = ?`,
		r.Branch, r.Commit, r.Worktree, r.Status, r.ConfigJSON, r.VarsJSON,
		formatTime(time.Now()), r.ID)
	if err != nil {
		return fmt.Errorf("update env %d: %w", r.ID, err)
	}
	return affectedOne(res, fmt.Sprintf("update env %d", r.ID))
}

func (s *store) SetEnvOwner(ctx context.Context, id int64, owner string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE envs SET owner = ?, updated_at = ? WHERE id = ?`,
		owner, formatTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("set the owner of env %d: %w", id, err)
	}
	return affectedOne(res, fmt.Sprintf("set the owner of env %d", id))
}

func (s *store) SetEnvVia(ctx context.Context, id int64, via string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE envs SET via = ?, updated_at = ? WHERE id = ?`,
		via, formatTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("set where env %d is served from: %w", id, err)
	}
	return affectedOne(res, fmt.Sprintf("set where env %d is served from", id))
}

func (s *store) DeleteEnv(ctx context.Context, id int64) error {

	if _, err := s.db.ExecContext(ctx, `DELETE FROM envs WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete env %d: %w", id, err)
	}
	return nil
}

func scanEnv(sc scanner) (EnvRecord, error) {
	var r EnvRecord
	var created, updated string
	var release, deploy sql.NullInt64
	if err := sc.Scan(&r.ID, &r.App, &r.Name, &r.Branch, &r.Commit, &r.Worktree,
		&r.PortBase, &r.PortCount, &r.Status, &r.ConfigJSON, &r.VarsJSON, &r.VPNIP,
		&r.Mode, &r.Protected, &release, &deploy, &r.CreatedBy, &r.Owner, &r.Via,
		&created, &updated); err != nil {
		return EnvRecord{}, err
	}
	r.ReleaseID, r.DeployID = release.Int64, deploy.Int64
	r.CreatedAt, r.UpdatedAt = parseTime(created), parseTime(updated)
	return r, nil
}

func (s *store) AddResource(ctx context.Context, r EnvResource) error {
	switch {
	case r.EnvID == 0:
		return errors.New("add resource: no env id")
	case r.Kind == "":
		return errors.New("add resource: empty kind")
	case r.Name == "":
		return errors.New("add resource: empty name")
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO env_resources (env_id, kind, name, dep, service, replica, port, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (env_id, kind, name, dep, service) DO NOTHING`,
		r.EnvID, r.Kind, r.Name, r.Dep, r.Service, r.Replica, r.Port, formatTime(r.CreatedAt))
	if err != nil {
		return fmt.Errorf("record %s %q of env %d: %w", r.Kind, r.Name, r.EnvID, err)
	}
	return nil
}

func (s *store) Resources(ctx context.Context, envID int64) ([]EnvResource, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, env_id, kind, name, dep, service, replica, port, created_at FROM env_resources
		 WHERE env_id = ? ORDER BY id`, envID)
	if err != nil {
		return nil, fmt.Errorf("list resources of env %d: %w", envID, err)
	}
	defer rows.Close()
	var out []EnvResource
	for rows.Next() {
		var r EnvResource
		var created string
		if err := rows.Scan(&r.ID, &r.EnvID, &r.Kind, &r.Name, &r.Dep, &r.Service, &r.Replica,
			&r.Port, &created); err != nil {
			return nil, fmt.Errorf("list resources of env %d: %w", envID, err)
		}
		r.CreatedAt = parseTime(created)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list resources of env %d: %w", envID, err)
	}
	return out, nil
}

func (s *store) DeleteResource(ctx context.Context, id int64) error {

	if _, err := s.db.ExecContext(ctx, `DELETE FROM env_resources WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete resource %d: %w", id, err)
	}
	return nil
}

func (s *store) AddEvent(ctx context.Context, e EnvEvent) error {

	if e.EnvID == 0 && e.Action == "" {
		return errors.New("add event: no action")
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	var envID any
	if e.EnvID != 0 {
		envID = e.EnvID
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO env_events (env_id, app, env, action, status, detail, service, replica, step, identity, json, at, machine)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		envID, e.App, e.Env, e.Action, e.Status, e.Detail, e.Service, e.Replica, e.Step, e.Identity, e.JSON,
		formatTime(e.At), e.Machine)
	if err != nil {
		return fmt.Errorf("record event %q on env %d: %w", e.Action, e.EnvID, err)
	}

	if id, idErr := res.LastInsertId(); idErr == nil {
		e.ID = id
	}

	s.publish(ctx, e)
	return nil
}

func (s *store) Events(ctx context.Context, envID int64, limit int) ([]EnvEvent, error) {

	q := `SELECT ` + eventColumns + ` FROM env_events WHERE env_id = ? ORDER BY id DESC`
	args := []any{envID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	return s.queryEvents(ctx, fmt.Sprintf("list events of env %d", envID), q, args...)
}

const eventColumns = `id, COALESCE(env_id, 0), action, status, detail, service, replica, step,` +
	` identity, json, at, app, env, machine`

func (s *store) QueryEvents(ctx context.Context, f EventFilter) ([]EnvEvent, error) {

	q := `SELECT e.id, COALESCE(e.env_id, 0), e.action, e.status, e.detail, e.service, e.replica, e.step,
		e.identity, e.json, e.at, COALESCE(NULLIF(v.app, ''), e.app), COALESCE(NULLIF(v.name, ''), e.env),
		e.machine
	      FROM env_events e LEFT JOIN envs v ON v.id = e.env_id`
	var where []string
	var args []any
	switch {
	case f.EnvID != 0:
		where = append(where, `e.env_id = ?`)
		args = append(args, f.EnvID)
	case f.App != "":
		where = append(where, `COALESCE(NULLIF(v.app, ''), e.app) = ?`)
		args = append(args, f.App)
	}
	if !f.Since.IsZero() {
		where = append(where, `e.at >= ?`)
		args = append(args, formatTime(f.Since))
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}

	q += ` ORDER BY e.id DESC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	return s.queryEvents(ctx, "query events", q, args...)
}

func (s *store) queryEvents(ctx context.Context, what, query string, args ...any) ([]EnvEvent, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}

	withEnv := len(cols) > 11
	withMachine := len(cols) > 13
	var out []EnvEvent
	for rows.Next() {
		var e EnvEvent
		var at string
		dest := []any{&e.ID, &e.EnvID, &e.Action, &e.Status, &e.Detail, &e.Service, &e.Replica,
			&e.Step, &e.Identity, &e.JSON, &at}
		if withEnv {
			dest = append(dest, &e.App, &e.Env)
		}
		if withMachine {
			dest = append(dest, &e.Machine)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		e.At = parseTime(at)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

func formatTime(t time.Time) string { return t.UTC().Format(timeFormat) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(timeFormat, s)
	return t
}

func affectedOne(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func isUniqueConstraint(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "constraint") &&
		(strings.Contains(msg, "unique") || strings.Contains(msg, "primary key"))
}

func isForeignKeyConstraint(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "constraint") && strings.Contains(msg, "foreign key")
}
