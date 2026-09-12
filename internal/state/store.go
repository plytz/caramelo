package state

import (
	"context"
	"time"

	"github.com/plytz/caramelo/internal/machine"
	"github.com/plytz/caramelo/internal/progress"
)

type Key struct {
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	PublicKey   string    `json:"public_key"`
	Fingerprint string    `json:"fingerprint"`
	Options     string    `json:"options,omitempty"`
	AddedAt     time.Time `json:"added_at"`
}

type SetupRun struct {
	RunID     string        `json:"run_id"`
	Step      string        `json:"step"`
	Status    string        `json:"status"`
	Detail    string        `json:"detail,omitempty"`
	Error     string        `json:"error,omitempty"`
	StartedAt time.Time     `json:"started_at"`
	Duration  time.Duration `json:"duration"`
	Version   string        `json:"version"`
}

type Store interface {
	Machine(ctx context.Context) (*machine.Record, error)
	SaveMachine(ctx context.Context, r *machine.Record) error

	Setting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error

	Keys(ctx context.Context) ([]Key, error)
	AddKey(ctx context.Context, k Key) error
	RemoveKey(ctx context.Context, name string) error

	RecordSetupRun(ctx context.Context, r SetupRun) error
	SetupRuns(ctx context.Context, limit int) ([]SetupRun, error)

	EnvStore

	ServiceStore

	VPNStore

	EdgeStore

	ReleaseStore
	DeployStore
	VaultStore

	FleetStore

	SetNotifier(n progress.Notifier)

	Close() error
}

var (
	ErrNotFound = errorString("not found")
	ErrExists   = errorString("already exists")

	ErrNotImplemented = errorString("not implemented")
)

type errorString string

func (e errorString) Error() string { return string(e) }
