//go:build integration

package itest

import (
	"context"
	"os"
	"strings"
	"testing"
)

const (
	InventoryEnv = "CARAMELO_E2E_INVENTORY"
	ResetEnv     = "CARAMELO_E2E_RESET"
)

type driver interface {
	target() string
	user() string
	home() string
	start(ctx context.Context) error
	run(ctx context.Context, user, cmd string) (Result, error)
	copyIn(ctx context.Context, local, remote, owner string) error
	fetch(ctx context.Context, remote, local string) error
	address(ctx context.Context) (string, error)
	hostIP() string
	hostPort(port int, proto string) (int, error)
	arch(ctx context.Context) (string, error)
	ensure(ctx context.Context, state string) error
	reset(ctx context.Context, state string) error
	restart(ctx context.Context) error
	relogin(ctx context.Context) error
	pty(ctx context.Context, dir string, env []string, line string) (PTYResult, error)
	collectLogs(ctx context.Context) (string, error)
	remove(ctx context.Context) error
}

type driverFactory func(t testing.TB, l *Lab, want []*Machine) ([]*Machine, error)

var inventoryDriver driverFactory

func OverSSH() bool {
	return strings.TrimSpace(os.Getenv(InventoryEnv)) != "" && inventoryDriver != nil
}

func Relogin(ctx context.Context, m *Machine) error { return m.Relogin(ctx) }

func bindsToInventory(m *Machine) bool {
	return m.Kind == KindMachine && (m.Role == RoleHub || m.Role == RoleMember)
}
