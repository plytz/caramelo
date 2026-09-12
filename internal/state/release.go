package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	EnvModeDev = "dev"

	EnvModeRelease = "release"
)

const (
	DeployKindDeploy = "deploy"

	DeployKindRollback = "rollback"
)

const (
	DeployBuilding = "building"

	DeployMigrating = "migrating"

	DeployStarting = "starting"

	DeployChecking = "checking"

	DeployWatching = "watching"

	DeployPromoted = "promoted"

	DeployRolledBack = "rolled_back"

	DeployFailed = "failed"
)

var DeployStatuses = []string{
	DeployBuilding, DeployMigrating, DeployStarting, DeployChecking, DeployWatching,
	DeployPromoted, DeployRolledBack, DeployFailed,
}

func DeployDone(status string) bool {
	switch status {
	case DeployPromoted, DeployRolledBack, DeployFailed:
		return true
	}
	return false
}

type Release struct {
	ID  int64  `json:"id"`
	App string `json:"app"`

	Commit string `json:"commit"`
	Tree   string `json:"tree"`

	Ref string `json:"ref,omitempty"`

	ConfigJSON string `json:"config_json,omitempty"`

	ImagesJSON string `json:"images_json,omitempty"`

	BuiltBy string `json:"built_by,omitempty"`

	Machine string    `json:"machine,omitempty"`
	BuiltAt time.Time `json:"built_at"`
}

type Deploy struct {
	ID    int64 `json:"id"`
	EnvID int64 `json:"env_id"`

	ReleaseID int64 `json:"release_id,omitempty"`

	FromReleaseID int64 `json:"from_release_id,omitempty"`

	Kind string `json:"kind"`

	Status string `json:"status"`

	Reason string `json:"reason,omitempty"`

	Identity   string    `json:"identity,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`

	Error string `json:"error,omitempty"`
}

func (d Deploy) Done() bool { return DeployDone(d.Status) }

type ReleaseStore interface {
	AddRelease(ctx context.Context, r Release) (*Release, error)

	Release(ctx context.Context, id int64) (*Release, error)

	ReleaseByTree(ctx context.Context, app, tree string) (*Release, error)

	Releases(ctx context.Context, app string, limit int) ([]Release, error)

	DeleteRelease(ctx context.Context, id int64) error
}

type DeployStore interface {
	CreateDeploy(ctx context.Context, d Deploy) (*Deploy, error)

	UpdateDeploy(ctx context.Context, d Deploy) error

	Deploy(ctx context.Context, id int64) (*Deploy, error)

	Deploys(ctx context.Context, envID int64, limit int) ([]Deploy, error)

	UnfinishedDeploys(ctx context.Context) ([]Deploy, error)

	SetEnvRelease(ctx context.Context, envID, releaseID int64) error

	SetEnvDeploy(ctx context.Context, envID, deployID int64) error
}

const releaseColumns = `id, app, "commit", tree, ref, config_json, images_json, built_by, machine, built_at`

const deployColumns = `id, env_id, release_id, from_release_id, kind, status, reason, identity,
	started_at, finished_at, error`

func (s *store) AddRelease(ctx context.Context, r Release) (*Release, error) {
	switch {
	case r.App == "":
		return nil, errors.New("add release: empty app")
	case r.Tree == "":
		return nil, errors.New("add release: empty tree")
	}
	if r.BuiltAt.IsZero() {
		r.BuiltAt = time.Now()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO releases (app, "commit", tree, ref, config_json, images_json, built_by, machine, built_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.App, r.Commit, r.Tree, r.Ref, r.ConfigJSON, r.ImagesJSON, r.BuiltBy, r.Machine, formatTime(r.BuiltAt))
	if err != nil {
		if isUniqueConstraint(err) {

			return nil, ErrExists
		}
		if isForeignKeyConstraint(err) {
			return nil, fmt.Errorf("add release of app %q: no such app", r.App)
		}
		return nil, fmt.Errorf("add release of app %q: %w", r.App, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("add release of app %q: %w", r.App, err)
	}
	r.ID = id
	return &r, nil
}

func (s *store) Release(ctx context.Context, id int64) (*Release, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+releaseColumns+` FROM releases WHERE id = ?`, id)
	r, err := scanRelease(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read release %d: %w", id, err)
	}
	return &r, nil
}

func (s *store) ReleaseByTree(ctx context.Context, app, tree string) (*Release, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+releaseColumns+` FROM releases WHERE app = ? AND tree = ?`, app, tree)
	r, err := scanRelease(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read release of app %q for tree %s: %w", app, tree, err)
	}
	return &r, nil
}

func (s *store) Releases(ctx context.Context, app string, limit int) ([]Release, error) {
	q := `SELECT ` + releaseColumns + ` FROM releases`
	var args []any
	if app != "" {
		q += ` WHERE app = ?`
		args = append(args, app)
	}
	q += ` ORDER BY id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list releases: %w", err)
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, fmt.Errorf("list releases: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list releases: %w", err)
	}
	return out, nil
}

func (s *store) DeleteRelease(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM releases WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete release %d: %w", id, err)
	}
	return nil
}

func scanRelease(sc scanner) (Release, error) {
	var r Release
	var built string
	if err := sc.Scan(&r.ID, &r.App, &r.Commit, &r.Tree, &r.Ref, &r.ConfigJSON, &r.ImagesJSON,
		&r.BuiltBy, &r.Machine, &built); err != nil {
		return Release{}, err
	}
	r.BuiltAt = parseTime(built)
	return r, nil
}

func (s *store) CreateDeploy(ctx context.Context, d Deploy) (*Deploy, error) {
	switch {
	case d.EnvID == 0:
		return nil, errors.New("create deploy: no env id")
	case d.Status == "":
		return nil, errors.New("create deploy: empty status")
	}
	if d.Kind == "" {
		d.Kind = DeployKindDeploy
	}
	if d.StartedAt.IsZero() {
		d.StartedAt = time.Now()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO deploys (env_id, release_id, from_release_id, kind, status, reason, identity,
			started_at, finished_at, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.EnvID, nullID(d.ReleaseID), nullID(d.FromReleaseID), d.Kind, d.Status, d.Reason, d.Identity,
		formatTime(d.StartedAt), formatTimeOrEmpty(d.FinishedAt), d.Error)
	if err != nil {
		if isForeignKeyConstraint(err) {
			return nil, fmt.Errorf("create deploy on env %d: no such env or release", d.EnvID)
		}
		return nil, fmt.Errorf("create deploy on env %d: %w", d.EnvID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create deploy on env %d: %w", d.EnvID, err)
	}
	d.ID = id
	return &d, nil
}

func (s *store) UpdateDeploy(ctx context.Context, d Deploy) error {
	if d.ID == 0 {
		return errors.New("update deploy: no id")
	}
	if d.Status == "" {
		return errors.New("update deploy: empty status")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE deploys SET release_id = ?, from_release_id = ?, kind = ?, status = ?, reason = ?,
			finished_at = ?, error = ? WHERE id = ?`,
		nullID(d.ReleaseID), nullID(d.FromReleaseID), nonEmptyOr(d.Kind, DeployKindDeploy), d.Status,
		d.Reason, formatTimeOrEmpty(d.FinishedAt), d.Error, d.ID)
	if err != nil {
		return fmt.Errorf("update deploy %d: %w", d.ID, err)
	}
	return affectedOne(res, fmt.Sprintf("update deploy %d", d.ID))
}

func (s *store) Deploy(ctx context.Context, id int64) (*Deploy, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+deployColumns+` FROM deploys WHERE id = ?`, id)
	d, err := scanDeploy(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read deploy %d: %w", id, err)
	}
	return &d, nil
}

func (s *store) Deploys(ctx context.Context, envID int64, limit int) ([]Deploy, error) {
	q := `SELECT ` + deployColumns + ` FROM deploys`
	var args []any
	if envID != 0 {
		q += ` WHERE env_id = ?`
		args = append(args, envID)
	}
	q += ` ORDER BY id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	return s.queryDeploys(ctx, "list deploys", q, args...)
}

func (s *store) UnfinishedDeploys(ctx context.Context) ([]Deploy, error) {
	q := `SELECT ` + deployColumns + ` FROM deploys WHERE status NOT IN (?, ?, ?) ORDER BY id`
	return s.queryDeploys(ctx, "list unfinished deploys", q,
		DeployPromoted, DeployRolledBack, DeployFailed)
}

func (s *store) queryDeploys(ctx context.Context, what, query string, args ...any) ([]Deploy, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close()
	var out []Deploy
	for rows.Next() {
		d, err := scanDeploy(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

func (s *store) SetEnvRelease(ctx context.Context, envID, releaseID int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE envs SET release_id = ?, updated_at = ? WHERE id = ?`,
		nullID(releaseID), formatTime(time.Now()), envID)
	if err != nil {
		return fmt.Errorf("set the release of env %d: %w", envID, err)
	}
	return affectedOne(res, fmt.Sprintf("set the release of env %d", envID))
}

func (s *store) SetEnvDeploy(ctx context.Context, envID, deployID int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE envs SET deploy_id = ?, updated_at = ? WHERE id = ?`,
		nullID(deployID), formatTime(time.Now()), envID)
	if err != nil {
		return fmt.Errorf("set the deploy of env %d: %w", envID, err)
	}
	return affectedOne(res, fmt.Sprintf("set the deploy of env %d", envID))
}

func scanDeploy(sc scanner) (Deploy, error) {
	var d Deploy
	var release, from sql.NullInt64
	var started, finished string
	if err := sc.Scan(&d.ID, &d.EnvID, &release, &from, &d.Kind, &d.Status, &d.Reason, &d.Identity,
		&started, &finished, &d.Error); err != nil {
		return Deploy{}, err
	}
	d.ReleaseID, d.FromReleaseID = release.Int64, from.Int64
	d.StartedAt, d.FinishedAt = parseTime(started), parseTime(finished)
	return d, nil
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func formatTimeOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return formatTime(t)
}

func nonEmptyOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
