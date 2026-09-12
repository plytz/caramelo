package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	TargetStarting = "starting"

	TargetActive = "active"

	TargetDraining = "draining"

	TargetStopped = "stopped"

	TargetUnhealthy = "unhealthy"

	TargetHeld = "held"
)

const RouteKindHTTPS = "https"

type Route struct {
	ID    int64 `json:"id"`
	EnvID int64 `json:"env_id"`

	Service string `json:"service"`

	Host string `json:"host"`

	Kind string `json:"kind"`

	Managed   bool      `json:"managed"`
	CreatedAt time.Time `json:"created_at"`
}

type EdgeTarget struct {
	RouteID int64 `json:"route_id"`

	Replica int `json:"replica"`

	Port int `json:"port"`

	State string `json:"state"`

	Inflight  int       `json:"inflight"`
	UpdatedAt time.Time `json:"updated_at"`
}

type EdgeStore interface {
	Routes(ctx context.Context) ([]Route, error)

	RoutesOfEnv(ctx context.Context, envID int64) ([]Route, error)

	RouteByHost(ctx context.Context, host string) (*Route, error)

	AddRoute(ctx context.Context, r Route) (*Route, error)

	DeleteRoute(ctx context.Context, id int64) error

	Targets(ctx context.Context, routeID int64) ([]EdgeTarget, error)

	PutTarget(ctx context.Context, t EdgeTarget) error

	SetTargets(ctx context.Context, routeID int64, targets []EdgeTarget) error

	DeleteTarget(ctx context.Context, routeID int64, replica int) error

	SetRouteManaged(ctx context.Context, id int64, managed bool) error
}

const routeColumns = `id, env_id, service, host, kind, managed, created_at`

const targetColumns = `route_id, replica, port, state, inflight, updated_at`

func normalizeHost(s string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
}

func (s *store) Routes(ctx context.Context) ([]Route, error) {
	return s.queryRoutes(ctx, "list routes", `SELECT `+routeColumns+` FROM routes ORDER BY host`)
}

func (s *store) RoutesOfEnv(ctx context.Context, envID int64) ([]Route, error) {
	return s.queryRoutes(ctx, fmt.Sprintf("list routes of env %d", envID),
		`SELECT `+routeColumns+` FROM routes WHERE env_id = ? ORDER BY id`, envID)
}

func (s *store) queryRoutes(ctx context.Context, what, query string, args ...any) ([]Route, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close()
	var out []Route
	for rows.Next() {
		r, err := scanRoute(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

func (s *store) RouteByHost(ctx context.Context, host string) (*Route, error) {
	host = normalizeHost(host)
	row := s.db.QueryRowContext(ctx, `SELECT `+routeColumns+` FROM routes WHERE host = ?`, host)
	r, err := scanRoute(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read route %q: %w", host, err)
	}
	return &r, nil
}

func (s *store) AddRoute(ctx context.Context, r Route) (*Route, error) {
	r.Host = normalizeHost(r.Host)
	switch {
	case r.EnvID == 0:
		return nil, errors.New("add route: no env id")
	case r.Host == "":
		return nil, errors.New("add route: empty host")
	case r.Service == "":
		return nil, fmt.Errorf("add route %q: empty service", r.Host)
	}
	if r.Kind == "" {
		r.Kind = RouteKindHTTPS
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO routes (env_id, service, host, kind, managed, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		r.EnvID, r.Service, r.Host, r.Kind, r.Managed, formatTime(r.CreatedAt))
	if err != nil {
		if isUniqueConstraint(err) {
			return nil, ErrExists
		}
		if isForeignKeyConstraint(err) {
			return nil, fmt.Errorf("add route %q: no such env %d", r.Host, r.EnvID)
		}
		return nil, fmt.Errorf("add route %q: %w", r.Host, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("add route %q: %w", r.Host, err)
	}
	r.ID = id
	return &r, nil
}

func (s *store) DeleteRoute(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM routes WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete route %d: %w", id, err)
	}
	return nil
}

func (s *store) Targets(ctx context.Context, routeID int64) ([]EdgeTarget, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+targetColumns+` FROM edge_targets WHERE route_id = ? ORDER BY replica`, routeID)
	if err != nil {
		return nil, fmt.Errorf("list targets of route %d: %w", routeID, err)
	}
	defer rows.Close()
	var out []EdgeTarget
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, fmt.Errorf("list targets of route %d: %w", routeID, err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list targets of route %d: %w", routeID, err)
	}
	return out, nil
}

func (s *store) PutTarget(ctx context.Context, t EdgeTarget) error {
	return putTarget(ctx, s.db, t)
}

func (s *store) SetTargets(ctx context.Context, routeID int64, targets []EdgeTarget) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set targets of route %d: %w", routeID, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM edge_targets WHERE route_id = ?`, routeID); err != nil {
		return fmt.Errorf("set targets of route %d: %w", routeID, err)
	}
	for _, t := range targets {
		t.RouteID = routeID
		if err := putTarget(ctx, tx, t); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set targets of route %d: %w", routeID, err)
	}
	return nil
}

func (s *store) DeleteTarget(ctx context.Context, routeID int64, replica int) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM edge_targets WHERE route_id = ? AND replica = ?`, routeID, replica); err != nil {
		return fmt.Errorf("delete replica %d of route %d: %w", replica, routeID, err)
	}
	return nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func putTarget(ctx context.Context, db execer, t EdgeTarget) error {
	switch {
	case t.RouteID == 0:
		return errors.New("record target: no route id")
	case t.Replica < 1:
		return fmt.Errorf("record target of route %d: replica index %d is not a replica",
			t.RouteID, t.Replica)
	}

	if t.State == "" {
		t.State = TargetStarting
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = time.Now()
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO edge_targets (route_id, replica, port, state, inflight, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (route_id, replica) DO UPDATE SET
			port = excluded.port,
			state = excluded.state,
			inflight = excluded.inflight,
			updated_at = excluded.updated_at`,
		t.RouteID, t.Replica, t.Port, t.State, t.Inflight, formatTime(t.UpdatedAt))
	if err != nil {
		if isForeignKeyConstraint(err) {
			return fmt.Errorf("record replica %d: no such route %d", t.Replica, t.RouteID)
		}
		return fmt.Errorf("record replica %d of route %d: %w", t.Replica, t.RouteID, err)
	}
	return nil
}

func scanRoute(sc scanner) (Route, error) {
	var r Route
	var created string
	if err := sc.Scan(&r.ID, &r.EnvID, &r.Service, &r.Host, &r.Kind, &r.Managed, &created); err != nil {
		return Route{}, err
	}
	r.CreatedAt = parseTime(created)
	return r, nil
}

func (s *store) SetRouteManaged(ctx context.Context, id int64, managed bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE routes SET managed = ? WHERE id = ?`, managed, id)
	if err != nil {
		return fmt.Errorf("set route %d managed: %w", id, err)
	}
	return affectedOne(res, fmt.Sprintf("set route %d managed", id))
}

func scanTarget(sc scanner) (EdgeTarget, error) {
	var t EdgeTarget
	var updated string
	if err := sc.Scan(&t.RouteID, &t.Replica, &t.Port, &t.State, &t.Inflight, &updated); err != nil {
		return EdgeTarget{}, err
	}
	t.UpdatedAt = parseTime(updated)
	return t, nil
}
