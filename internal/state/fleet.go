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
	MachineRoleHub = "hub"

	MachineRoleMember = "member"
)

type MachineRow struct {
	Name string `json:"name"`

	Role string `json:"role"`

	PublicKey string `json:"public_key"`

	Subnet string `json:"subnet"`

	Arch string `json:"arch,omitempty"`
	OS   string `json:"os,omitempty"`

	Endpoint string `json:"endpoint,omitempty"`

	Private bool `json:"private,omitempty"`

	JoinedAt time.Time `json:"joined_at"`
	LastSeen time.Time `json:"last_seen,omitempty"`

	GaugeJSON string `json:"gauge_json,omitempty"`
}

type DirectoryRow struct {
	App     string `json:"app"`
	Env     string `json:"env"`
	Machine string `json:"machine"`

	Address string `json:"address,omitempty"`

	Owner string `json:"owner,omitempty"`

	Mode string `json:"mode,omitempty"`
	Via  string `json:"via,omitempty"`

	Hosts string `json:"hosts,omitempty"`

	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

type DirectoryFilter struct {
	App     string
	Machine string
	Owner   string
}

type ReleaseImage struct {
	ReleaseID int64  `json:"release_id"`
	Service   string `json:"service"`
	Arch      string `json:"arch"`
	Machine   string `json:"machine"`

	ImageID string    `json:"image_id,omitempty"`
	BuiltAt time.Time `json:"built_at,omitempty"`
}

type JoinToken struct {
	Hash string `json:"hash"`

	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`

	UsedBy string    `json:"used_by,omitempty"`
	UsedAt time.Time `json:"used_at,omitempty"`
}

func (t JoinToken) Used() bool { return t.UsedBy != "" || !t.UsedAt.IsZero() }

type FleetStore interface {
	FleetMachines(ctx context.Context) ([]MachineRow, error)

	FleetMachine(ctx context.Context, name string) (*MachineRow, error)

	FleetMachineByKey(ctx context.Context, publicKey string) (*MachineRow, error)

	FleetHub(ctx context.Context) (*MachineRow, error)

	PutFleetMachine(ctx context.Context, m MachineRow) error

	SetMachineSeen(ctx context.Context, name, endpoint string, at time.Time) error

	SetMachineGauge(ctx context.Context, name, gaugeJSON string) error

	DeleteFleetMachine(ctx context.Context, name string) error

	Directory(ctx context.Context, f DirectoryFilter) ([]DirectoryRow, error)

	DirectoryEntry(ctx context.Context, app, env string) (*DirectoryRow, error)

	PutDirectoryEntry(ctx context.Context, r DirectoryRow) error

	DeleteDirectoryEntry(ctx context.Context, app, env string) error

	ReplaceMachineDirectory(ctx context.Context, machine string, rows []DirectoryRow) error

	AddReleaseImage(ctx context.Context, img ReleaseImage) error

	ReleaseImages(ctx context.Context, releaseID int64) ([]ReleaseImage, error)

	DeleteReleaseImage(ctx context.Context, releaseID int64, service, arch, machine string) error

	DeleteMachineImages(ctx context.Context, machine string) error

	AddJoinToken(ctx context.Context, t JoinToken) error

	JoinToken(ctx context.Context, hash string) (*JoinToken, error)

	RedeemJoinToken(ctx context.Context, hash, machine string, at time.Time) error

	DeleteExpiredJoinTokens(ctx context.Context, t time.Time) (int, error)
}

const machineColumns = `name, role, public_key, subnet, arch, os, endpoint, private,
	joined_at, last_seen, gauge_json`

func (s *store) FleetMachines(ctx context.Context) ([]MachineRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+machineColumns+` FROM machines
		 ORDER BY CASE WHEN role = ? THEN 0 ELSE 1 END, name`, MachineRoleHub)
	if err != nil {
		return nil, fmt.Errorf("list fleet machines: %w", err)
	}
	defer rows.Close()
	var out []MachineRow
	for rows.Next() {
		m, err := scanMachineRow(rows)
		if err != nil {
			return nil, fmt.Errorf("list fleet machines: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list fleet machines: %w", err)
	}
	return out, nil
}

func (s *store) FleetMachine(ctx context.Context, name string) (*MachineRow, error) {
	return s.oneMachine(ctx, fmt.Sprintf("read fleet machine %q", name),
		`SELECT `+machineColumns+` FROM machines WHERE name = ?`, name)
}

func (s *store) FleetMachineByKey(ctx context.Context, publicKey string) (*MachineRow, error) {
	if strings.TrimSpace(publicKey) == "" {
		return nil, ErrNotFound
	}
	return s.oneMachine(ctx, "read fleet machine by key",
		`SELECT `+machineColumns+` FROM machines WHERE public_key = ?`, publicKey)
}

func (s *store) FleetHub(ctx context.Context) (*MachineRow, error) {
	return s.oneMachine(ctx, "read the fleet's hub",
		`SELECT `+machineColumns+` FROM machines WHERE role = ? ORDER BY name LIMIT 1`, MachineRoleHub)
}

func (s *store) oneMachine(ctx context.Context, what, query string, args ...any) (*MachineRow, error) {
	m, err := scanMachineRow(s.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return &m, nil
}

func (s *store) PutFleetMachine(ctx context.Context, m MachineRow) error {
	switch {
	case strings.TrimSpace(m.Name) == "":
		return errors.New("put fleet machine: empty name")
	case strings.TrimSpace(m.PublicKey) == "":
		return fmt.Errorf("put fleet machine %q: empty public key", m.Name)
	case strings.TrimSpace(m.Subnet) == "":
		return fmt.Errorf("put fleet machine %q: empty subnet", m.Name)
	}
	if m.Role == "" {
		m.Role = MachineRoleHub
	}
	if m.JoinedAt.IsZero() {
		m.JoinedAt = time.Now()
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO machines (name, role, public_key, subnet, arch, os, endpoint, private,
			joined_at, last_seen, gauge_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (name) DO UPDATE SET
			role = excluded.role, public_key = excluded.public_key, subnet = excluded.subnet,
			arch = excluded.arch, os = excluded.os, private = excluded.private,
			endpoint = CASE WHEN excluded.endpoint = '' THEN machines.endpoint ELSE excluded.endpoint END,
			last_seen = CASE WHEN excluded.last_seen = '' THEN machines.last_seen ELSE excluded.last_seen END,
			gauge_json = CASE WHEN excluded.gauge_json = '' THEN machines.gauge_json ELSE excluded.gauge_json END`,
		m.Name, m.Role, m.PublicKey, m.Subnet, m.Arch, m.OS, m.Endpoint, m.Private,
		formatTime(m.JoinedAt), formatTimeOrEmpty(m.LastSeen), m.GaugeJSON)
	if err != nil {
		if isUniqueConstraint(err) {
			return ErrExists
		}
		return fmt.Errorf("put fleet machine %q: %w", m.Name, err)
	}
	return nil
}

func (s *store) SetMachineSeen(ctx context.Context, name, endpoint string, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE machines SET last_seen = ?,
			endpoint = CASE WHEN ? = '' THEN endpoint ELSE ? END
		 WHERE name = ?`,
		formatTime(at), endpoint, endpoint, name)
	if err != nil {
		return fmt.Errorf("record a handshake from machine %q: %w", name, err)
	}
	return affectedOne(res, fmt.Sprintf("record a handshake from machine %q", name))
}

func (s *store) SetMachineGauge(ctx context.Context, name, gaugeJSON string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE machines SET gauge_json = ? WHERE name = ?`, gaugeJSON, name)
	if err != nil {
		return fmt.Errorf("record the gauge of machine %q: %w", name, err)
	}
	return affectedOne(res, fmt.Sprintf("record the gauge of machine %q", name))
}

func (s *store) DeleteFleetMachine(ctx context.Context, name string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM machines WHERE name = ?`, name); err != nil {
		return fmt.Errorf("delete fleet machine %q: %w", name, err)
	}
	return nil
}

func scanMachineRow(sc scanner) (MachineRow, error) {
	var m MachineRow
	var joined, seen string
	if err := sc.Scan(&m.Name, &m.Role, &m.PublicKey, &m.Subnet, &m.Arch, &m.OS, &m.Endpoint,
		&m.Private, &joined, &seen, &m.GaugeJSON); err != nil {
		return MachineRow{}, err
	}
	m.JoinedAt, m.LastSeen = parseTime(joined), parseTime(seen)
	return m, nil
}

const directoryColumns = `app, env, machine, address, owner, mode, via, hosts, updated_at`

func (s *store) Directory(ctx context.Context, f DirectoryFilter) ([]DirectoryRow, error) {
	q := `SELECT ` + directoryColumns + ` FROM env_directory`
	var where []string
	var args []any
	if f.App != "" {
		where = append(where, `app = ?`)
		args = append(args, f.App)
	}
	if f.Machine != "" {
		where = append(where, `machine = ?`)
		args = append(args, f.Machine)
	}
	if f.Owner != "" {
		where = append(where, `owner = ?`)
		args = append(args, f.Owner)
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	q += ` ORDER BY app, env`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("read the environment directory: %w", err)
	}
	defer rows.Close()
	var out []DirectoryRow
	for rows.Next() {
		r, err := scanDirectoryRow(rows)
		if err != nil {
			return nil, fmt.Errorf("read the environment directory: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the environment directory: %w", err)
	}
	return out, nil
}

func (s *store) DirectoryEntry(ctx context.Context, app, env string) (*DirectoryRow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+directoryColumns+` FROM env_directory WHERE app = ? AND env = ?`, app, env)
	r, err := scanDirectoryRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read the directory entry for %s/%s: %w", app, env, err)
	}
	return &r, nil
}

func (s *store) PutDirectoryEntry(ctx context.Context, r DirectoryRow) error {
	if err := validDirectoryRow(r); err != nil {
		return err
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = time.Now()
	}
	if _, err := s.db.ExecContext(ctx, directoryUpsert, directoryArgs(r)...); err != nil {
		return fmt.Errorf("record %s/%s in the directory: %w", r.App, r.Env, err)
	}
	return nil
}

func (s *store) DeleteDirectoryEntry(ctx context.Context, app, env string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM env_directory WHERE app = ? AND env = ?`, app, env)
	if err != nil {
		return fmt.Errorf("forget %s/%s in the directory: %w", app, env, err)
	}
	return nil
}

func (s *store) ReplaceMachineDirectory(ctx context.Context, machine string, rows []DirectoryRow) error {
	if strings.TrimSpace(machine) == "" {
		return errors.New("replace the directory of a machine: empty machine")
	}
	now := time.Now()
	for i := range rows {
		if rows[i].Machine == "" {
			rows[i].Machine = machine
		}
		if rows[i].Machine != machine {
			return fmt.Errorf("replace the directory of machine %q: %s/%s is on %q",
				machine, rows[i].App, rows[i].Env, rows[i].Machine)
		}
		if err := validDirectoryRow(rows[i]); err != nil {
			return err
		}
		if rows[i].UpdatedAt.IsZero() {
			rows[i].UpdatedAt = now
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace the directory of machine %q: %w", machine, err)
	}

	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM env_directory WHERE machine = ?`, machine); err != nil {
		return fmt.Errorf("replace the directory of machine %q: %w", machine, err)
	}
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, directoryUpsert, directoryArgs(r)...); err != nil {
			return fmt.Errorf("replace the directory of machine %q: %s/%s: %w", machine, r.App, r.Env, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace the directory of machine %q: %w", machine, err)
	}
	return nil
}

const directoryUpsert = `INSERT INTO env_directory (app, env, machine, address, owner, mode, via, hosts, updated_at)
	 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	 ON CONFLICT (app, env) DO UPDATE SET
		machine = excluded.machine, address = excluded.address, owner = excluded.owner,
		mode = excluded.mode, via = excluded.via, hosts = excluded.hosts,
		updated_at = excluded.updated_at`

func directoryArgs(r DirectoryRow) []any {
	return []any{r.App, r.Env, r.Machine, r.Address, r.Owner, r.Mode, r.Via, r.Hosts, formatTime(r.UpdatedAt)}
}

func validDirectoryRow(r DirectoryRow) error {
	switch {
	case strings.TrimSpace(r.App) == "":
		return errors.New("directory entry: empty app")
	case strings.TrimSpace(r.Env) == "":
		return fmt.Errorf("directory entry of app %q: empty environment", r.App)
	case strings.TrimSpace(r.Machine) == "":
		return fmt.Errorf("directory entry for %s/%s: empty machine", r.App, r.Env)
	}
	return nil
}

func scanDirectoryRow(sc scanner) (DirectoryRow, error) {
	var r DirectoryRow
	var updated string
	if err := sc.Scan(&r.App, &r.Env, &r.Machine, &r.Address, &r.Owner, &r.Mode, &r.Via, &r.Hosts, &updated); err != nil {
		return DirectoryRow{}, err
	}
	r.UpdatedAt = parseTime(updated)
	return r, nil
}

const releaseImageColumns = `release_id, service, arch, machine, image_id, built_at`

func (s *store) AddReleaseImage(ctx context.Context, img ReleaseImage) error {
	switch {
	case img.ReleaseID == 0:
		return errors.New("add release image: no release id")
	case strings.TrimSpace(img.Service) == "":
		return fmt.Errorf("add image of release %d: empty service", img.ReleaseID)
	case strings.TrimSpace(img.Arch) == "":
		return fmt.Errorf("add image of release %d service %q: empty architecture", img.ReleaseID, img.Service)
	case strings.TrimSpace(img.Machine) == "":
		return fmt.Errorf("add image of release %d service %q: empty machine", img.ReleaseID, img.Service)
	}
	if img.BuiltAt.IsZero() {
		img.BuiltAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO release_images (release_id, service, arch, machine, image_id, built_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (release_id, service, arch, machine) DO UPDATE SET
			image_id = excluded.image_id, built_at = excluded.built_at`,
		img.ReleaseID, img.Service, img.Arch, img.Machine, img.ImageID, formatTime(img.BuiltAt))
	if err != nil {
		if isForeignKeyConstraint(err) {
			return fmt.Errorf("add image of release %d: no such release", img.ReleaseID)
		}
		return fmt.Errorf("add image of release %d service %q: %w", img.ReleaseID, img.Service, err)
	}
	return nil
}

func (s *store) ReleaseImages(ctx context.Context, releaseID int64) ([]ReleaseImage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+releaseImageColumns+` FROM release_images WHERE release_id = ?
		 ORDER BY arch, service, machine`, releaseID)
	if err != nil {
		return nil, fmt.Errorf("list the images of release %d: %w", releaseID, err)
	}
	defer rows.Close()
	var out []ReleaseImage
	for rows.Next() {
		var img ReleaseImage
		var built string
		if err := rows.Scan(&img.ReleaseID, &img.Service, &img.Arch, &img.Machine, &img.ImageID, &built); err != nil {
			return nil, fmt.Errorf("list the images of release %d: %w", releaseID, err)
		}
		img.BuiltAt = parseTime(built)
		out = append(out, img)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the images of release %d: %w", releaseID, err)
	}
	return out, nil
}

func (s *store) DeleteReleaseImage(ctx context.Context, releaseID int64, service, arch, machine string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM release_images WHERE release_id = ? AND service = ? AND arch = ? AND machine = ?`,
		releaseID, service, arch, machine)
	if err != nil {
		return fmt.Errorf("forget image %s/%s of release %d on %s: %w", service, arch, releaseID, machine, err)
	}
	return nil
}

func (s *store) DeleteMachineImages(ctx context.Context, machine string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM release_images WHERE machine = ?`, machine); err != nil {
		return fmt.Errorf("forget the images held by machine %q: %w", machine, err)
	}
	return nil
}

const joinTokenColumns = `token_hash, created_by, created_at, expires_at, used_by, used_at`

func (s *store) AddJoinToken(ctx context.Context, t JoinToken) error {
	if strings.TrimSpace(t.Hash) == "" {
		return errors.New("add join token: empty hash")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO join_tokens (`+joinTokenColumns+`) VALUES (?, ?, ?, ?, ?, ?)`,
		t.Hash, t.CreatedBy, formatTime(t.CreatedAt), formatTimeOrEmpty(t.ExpiresAt),
		t.UsedBy, formatTimeOrEmpty(t.UsedAt))
	if err != nil {
		if isUniqueConstraint(err) {
			return ErrExists
		}
		return fmt.Errorf("add join token: %w", err)
	}
	return nil
}

func (s *store) JoinToken(ctx context.Context, hash string) (*JoinToken, error) {
	if strings.TrimSpace(hash) == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+joinTokenColumns+` FROM join_tokens WHERE token_hash = ?`, hash)
	var t JoinToken
	var created, expires, used string
	err := row.Scan(&t.Hash, &t.CreatedBy, &created, &expires, &t.UsedBy, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read a join token: %w", err)
	}
	t.CreatedAt, t.ExpiresAt, t.UsedAt = parseTime(created), parseTime(expires), parseTime(used)
	return &t, nil
}

func (s *store) RedeemJoinToken(ctx context.Context, hash, machine string, at time.Time) error {
	if strings.TrimSpace(hash) == "" {
		return ErrNotFound
	}
	if strings.TrimSpace(machine) == "" {
		return errors.New("redeem a join token: empty machine")
	}
	if at.IsZero() {
		at = time.Now()
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE join_tokens SET used_by = ?, used_at = ?
		 WHERE token_hash = ? AND used_by = '' AND used_at = ''`,
		machine, formatTime(at), hash)
	if err != nil {
		return fmt.Errorf("redeem a join token for machine %q: %w", machine, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("redeem a join token for machine %q: %w", machine, err)
	}
	if n == 0 {

		if _, readErr := s.JoinToken(ctx, hash); errors.Is(readErr, ErrNotFound) {
			return ErrNotFound
		}
		return ErrExists
	}
	return nil
}

func (s *store) DeleteExpiredJoinTokens(ctx context.Context, t time.Time) (int, error) {
	if t.IsZero() {
		t = time.Now()
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM join_tokens WHERE used_by = '' AND used_at = '' AND expires_at != '' AND expires_at < ?`,
		formatTime(t))
	if err != nil {
		return 0, fmt.Errorf("forget the expired join tokens: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("forget the expired join tokens: %w", err)
	}
	return int(n), nil
}
