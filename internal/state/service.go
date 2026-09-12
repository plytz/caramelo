package state

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	ServiceStarting = "starting"

	ServiceRunning = "running"

	ServiceFailed = "failed"

	ServiceStopped = "stopped"
)

const (
	ResourceNetwork = "network"

	ResourceImage = "image"

	ResourceCache = "cache"
)

type EnvService struct {
	ID    int64 `json:"id"`
	EnvID int64 `json:"env_id"`

	Name string `json:"name"`

	Image string `json:"image,omitempty"`

	Container string `json:"container,omitempty"`

	Port int `json:"port,omitempty"`

	ContainerPort int `json:"container_port,omitempty"`

	Protocol string `json:"protocol,omitempty"`

	CommandJSON string `json:"command_json,omitempty"`
	VarsJSON    string `json:"vars_json,omitempty"`
	HealthJSON  string `json:"health_json,omitempty"`

	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`

	Replicas int `json:"replicas,omitempty"`
}

type ServiceStore interface {
	Services(ctx context.Context, envID int64) ([]EnvService, error)

	PutService(ctx context.Context, s EnvService) error

	DeleteService(ctx context.Context, envID int64, name string) error

	SetAppStack(ctx context.Context, name, stack string) error
}

const serviceColumns = `id, env_id, name, image, container, port, container_port, protocol,
	command_json, vars_json, health_json, status, updated_at, replicas`

func (s *store) Services(ctx context.Context, envID int64) ([]EnvService, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+serviceColumns+` FROM env_services WHERE env_id = ? ORDER BY id`, envID)
	if err != nil {
		return nil, fmt.Errorf("list services of env %d: %w", envID, err)
	}
	defer rows.Close()
	var out []EnvService
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, fmt.Errorf("list services of env %d: %w", envID, err)
		}
		out = append(out, svc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list services of env %d: %w", envID, err)
	}
	return out, nil
}

func (s *store) PutService(ctx context.Context, svc EnvService) error {
	switch {
	case svc.EnvID == 0:
		return errors.New("record service: no env id")
	case svc.Name == "":
		return errors.New("record service: empty name")
	}

	if svc.Status == "" {
		svc.Status = ServiceStarting
	}
	if svc.Protocol == "" {
		svc.Protocol = "tcp"
	}

	if svc.Replicas <= 0 {
		svc.Replicas = 1
	}
	if svc.UpdatedAt.IsZero() {
		svc.UpdatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO env_services (env_id, name, image, container, port, container_port, protocol,
			command_json, vars_json, health_json, status, updated_at, replicas)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (env_id, name) DO UPDATE SET
			image = excluded.image,
			container = excluded.container,
			port = excluded.port,
			container_port = excluded.container_port,
			protocol = excluded.protocol,
			command_json = excluded.command_json,
			vars_json = excluded.vars_json,
			health_json = excluded.health_json,
			status = excluded.status,
			updated_at = excluded.updated_at,
			replicas = excluded.replicas`,
		svc.EnvID, svc.Name, svc.Image, svc.Container, svc.Port, svc.ContainerPort, svc.Protocol,
		svc.CommandJSON, svc.VarsJSON, svc.HealthJSON, svc.Status, formatTime(svc.UpdatedAt),
		svc.Replicas)
	if err != nil {
		if isForeignKeyConstraint(err) {
			return fmt.Errorf("record service %q: no such env %d", svc.Name, svc.EnvID)
		}
		return fmt.Errorf("record service %q of env %d: %w", svc.Name, svc.EnvID, err)
	}
	return nil
}

func (s *store) DeleteService(ctx context.Context, envID int64, name string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM env_services WHERE env_id = ? AND name = ?`, envID, name); err != nil {
		return fmt.Errorf("delete service %q of env %d: %w", name, envID, err)
	}
	return nil
}

func (s *store) SetAppStack(ctx context.Context, name, stack string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE apps SET stack = ? WHERE name = ?`, stack, name)
	if err != nil {
		return fmt.Errorf("set stack of app %q: %w", name, err)
	}
	return affectedOne(res, fmt.Sprintf("set stack of app %q", name))
}

func scanService(sc scanner) (EnvService, error) {
	var svc EnvService
	var updated string
	if err := sc.Scan(&svc.ID, &svc.EnvID, &svc.Name, &svc.Image, &svc.Container, &svc.Port,
		&svc.ContainerPort, &svc.Protocol, &svc.CommandJSON, &svc.VarsJSON, &svc.HealthJSON,
		&svc.Status, &updated, &svc.Replicas); err != nil {
		return EnvService{}, err
	}
	svc.UpdatedAt = parseTime(updated)
	return svc, nil
}
