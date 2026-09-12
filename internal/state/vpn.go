package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Peer struct {
	Name string `json:"name"`

	PublicKey string `json:"public_key"`

	IP string `json:"ip"`

	AddedBy string `json:"added_by,omitempty"`

	CreatedAt time.Time `json:"created_at"`

	LastHandshake time.Time `json:"last_handshake,omitempty"`
}

type VPN struct {
	PrivateKeyPath string `json:"private_key_path"`

	Subnet string `json:"subnet"`

	Listen string `json:"listen"`

	UpdatedAt time.Time `json:"updated_at"`
}

type VPNStore interface {
	Peers(ctx context.Context) ([]Peer, error)

	Peer(ctx context.Context, name string) (*Peer, error)

	AddPeer(ctx context.Context, p Peer) error

	SetPeerKey(ctx context.Context, name, publicKey string) error

	RemovePeer(ctx context.Context, name string) error

	SetPeerHandshake(ctx context.Context, name string, at time.Time) error

	EnvVPNIP(ctx context.Context, envID int64) (string, error)

	SetEnvVPNIP(ctx context.Context, envID int64, ip string) error

	TakenVPNIPs(ctx context.Context) ([]string, error)

	VPN(ctx context.Context) (*VPN, error)

	SetVPN(ctx context.Context, v VPN) error
}

const peerColumns = `name, public_key, ip, added_by, created_at, last_handshake`

func (s *store) Peers(ctx context.Context) ([]Peer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+peerColumns+` FROM peers ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list peers: %w", err)
	}
	defer rows.Close()
	var out []Peer
	for rows.Next() {
		p, err := scanPeer(rows)
		if err != nil {
			return nil, fmt.Errorf("list peers: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list peers: %w", err)
	}
	return out, nil
}

func (s *store) Peer(ctx context.Context, name string) (*Peer, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+peerColumns+` FROM peers WHERE name = ?`, name)
	p, err := scanPeer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read peer %q: %w", name, err)
	}
	return &p, nil
}

func (s *store) AddPeer(ctx context.Context, p Peer) error {
	switch {
	case p.Name == "":
		return errors.New("add peer: empty name")
	case p.PublicKey == "":
		return fmt.Errorf("add peer %q: empty public key", p.Name)
	case p.IP == "":
		return fmt.Errorf("add peer %q: empty address", p.Name)
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO peers (name, public_key, ip, added_by, created_at, last_handshake)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		p.Name, p.PublicKey, p.IP, p.AddedBy, formatTime(p.CreatedAt), formatOptionalTime(p.LastHandshake))
	if err != nil {

		if isUniqueConstraint(err) {
			return ErrExists
		}
		return fmt.Errorf("add peer %q: %w", p.Name, err)
	}
	return nil
}

func (s *store) SetPeerKey(ctx context.Context, name, publicKey string) error {
	if publicKey == "" {
		return fmt.Errorf("rotate the key of peer %q: empty public key", name)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE peers SET public_key = ? WHERE name = ?`, publicKey, name)
	if err != nil {

		if isUniqueConstraint(err) {
			return ErrExists
		}
		return fmt.Errorf("rotate the key of peer %q: %w", name, err)
	}
	return affectedOne(res, fmt.Sprintf("rotate the key of peer %q", name))
}

func (s *store) RemovePeer(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM peers WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("remove peer %q: %w", name, err)
	}

	return affectedOne(res, fmt.Sprintf("remove peer %q", name))
}

func (s *store) SetPeerHandshake(ctx context.Context, name string, at time.Time) error {

	if _, err := s.db.ExecContext(ctx, `UPDATE peers SET last_handshake = ? WHERE name = ?`,
		formatOptionalTime(at), name); err != nil {
		return fmt.Errorf("record the handshake of peer %q: %w", name, err)
	}
	return nil
}

func scanPeer(sc scanner) (Peer, error) {
	var p Peer
	var created, handshake string
	if err := sc.Scan(&p.Name, &p.PublicKey, &p.IP, &p.AddedBy, &created, &handshake); err != nil {
		return Peer{}, err
	}
	p.CreatedAt, p.LastHandshake = parseTime(created), parseTime(handshake)
	return p, nil
}

func (s *store) EnvVPNIP(ctx context.Context, envID int64) (string, error) {
	var ip string
	err := s.db.QueryRowContext(ctx, `SELECT vpn_ip FROM envs WHERE id = ?`, envID).Scan(&ip)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read the address of env %d: %w", envID, err)
	}
	return ip, nil
}

func (s *store) SetEnvVPNIP(ctx context.Context, envID int64, ip string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE envs SET vpn_ip = ?, updated_at = ? WHERE id = ?`,
		ip, formatTime(time.Now()), envID)
	if err != nil {

		if isUniqueConstraint(err) {
			return ErrExists
		}
		return fmt.Errorf("set the address of env %d: %w", envID, err)
	}
	return affectedOne(res, fmt.Sprintf("set the address of env %d", envID))
}

func (s *store) TakenVPNIPs(ctx context.Context) ([]string, error) {

	rows, err := s.db.QueryContext(ctx,
		`SELECT ip FROM peers UNION SELECT vpn_ip FROM envs WHERE vpn_ip <> '' ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("list the allocated addresses: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return nil, fmt.Errorf("list the allocated addresses: %w", err)
		}
		if ip != "" {
			out = append(out, ip)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the allocated addresses: %w", err)
	}
	return out, nil
}

func (s *store) VPN(ctx context.Context) (*VPN, error) {
	var v VPN
	var updated string
	err := s.db.QueryRowContext(ctx,
		`SELECT private_key, subnet, listen, updated_at FROM vpn WHERE id = 1`).
		Scan(&v.PrivateKeyPath, &v.Subnet, &v.Listen, &updated)
	if errors.Is(err, sql.ErrNoRows) {

		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read the vpn row: %w", err)
	}
	v.UpdatedAt = parseTime(updated)
	return &v, nil
}

func (s *store) SetVPN(ctx context.Context, v VPN) error {
	switch {
	case v.PrivateKeyPath == "":
		return errors.New("set vpn: empty private key path")
	case v.Subnet == "":
		return errors.New("set vpn: empty subnet")
	case v.Listen == "":
		return errors.New("set vpn: empty listen address")
	}
	if v.UpdatedAt.IsZero() {
		v.UpdatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO vpn (id, private_key, subnet, listen, updated_at) VALUES (1, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET private_key = excluded.private_key,
			subnet = excluded.subnet, listen = excluded.listen, updated_at = excluded.updated_at`,
		v.PrivateKeyPath, v.Subnet, v.Listen, formatTime(v.UpdatedAt))
	if err != nil {
		return fmt.Errorf("write the vpn row: %w", err)
	}
	return nil
}

func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return formatTime(t)
}
