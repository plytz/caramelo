package env

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/progress"
)

type Status string

const (
	StatusCreating   Status = "creating"
	StatusReady      Status = "ready"
	StatusFailed     Status = "failed"
	StatusDestroying Status = "destroying"
)

type Env struct {
	ID int64 `json:"id"`

	App string `json:"app"`

	Name string `json:"name"`

	Branch string `json:"branch"`

	Commit string `json:"commit,omitempty"`

	Worktree string `json:"worktree"`

	PortBase  int `json:"port_base"`
	PortCount int `json:"port_count"`

	VPNIP string `json:"vpn_ip,omitempty"`

	Status Status `json:"status"`

	Mode Mode `json:"mode"`

	Protected bool `json:"protected,omitempty"`

	ReleaseID int64 `json:"release_id,omitempty"`
	DeployID  int64 `json:"deploy_id,omitempty"`

	Config json.RawMessage `json:"config,omitempty"`

	Vars map[string]string `json:"vars,omitempty"`

	Owner string `json:"owner,omitempty"`

	Via config.Via `json:"via,omitempty"`

	Machine string `json:"machine,omitempty"`

	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (e Env) Port() int { return e.PortBase }

type ResourceKind string

const (
	KindBranch    ResourceKind = "branch"
	KindWorktree  ResourceKind = "worktree"
	KindVolume    ResourceKind = "volume"
	KindContainer ResourceKind = "container"

	KindNetwork ResourceKind = "network"
	KindImage   ResourceKind = "image"
	KindCache   ResourceKind = "cache"
)

type Resource struct {
	ID    int64        `json:"id"`
	EnvID int64        `json:"env_id"`
	Kind  ResourceKind `json:"kind"`

	Name string `json:"name"`

	Dep string `json:"dep,omitempty"`

	Service string `json:"service,omitempty"`

	Port      int       `json:"port,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Event = progress.Event

type CreateRequest struct {
	App string `json:"app"`

	Name string `json:"name"`

	From string `json:"from,omitempty"`

	Reset bool `json:"reset,omitempty"`

	NoDeps bool `json:"no_deps,omitempty"`

	Timeout time.Duration `json:"timeout,omitempty"`

	On string `json:"on,omitempty"`

	Release bool `json:"release,omitempty"`

	Production bool `json:"production,omitempty"`

	Protected bool `json:"protected,omitempty"`

	SecretsFrom string `json:"secrets_from,omitempty"`

	Secrets map[string]string `json:"secrets,omitempty"`
}

func (r CreateRequest) Mode() Mode {
	if r.Production || r.Release {
		return ModeRelease
	}
	return ModeDev
}

func (r CreateRequest) IsProtected() bool { return r.Protected || r.Production }

type DestroyRequest struct {
	App  string `json:"app"`
	Name string `json:"name"`

	DeleteBranch bool `json:"delete_branch,omitempty"`

	Force bool `json:"force,omitempty"`
}

type ExecRequest struct {
	App  string   `json:"app"`
	Name string   `json:"name"`
	Argv []string `json:"argv"`

	Stdin  io.Reader `json:"-"`
	Stdout io.Writer `json:"-"`
	Stderr io.Writer `json:"-"`
}

type DepStatus string

const (
	DepRunning DepStatus = "running"
	DepExited  DepStatus = "exited"
	DepMissing DepStatus = "missing"
)

type DepState struct {
	Name string `json:"name"`

	Container string `json:"container"`

	Status DepStatus `json:"status"`

	Port int `json:"port"`
}

type ExportFormat string

const (
	FormatShell ExportFormat = "shell"

	FormatDotenv ExportFormat = "dotenv"

	FormatJSON ExportFormat = "json"
)

func ParseExportFormat(s string) (ExportFormat, error) {
	switch ExportFormat(s) {
	case "":
		return FormatShell, nil
	case FormatShell:
		return FormatShell, nil
	case FormatDotenv:
		return FormatDotenv, nil
	case FormatJSON:
		return FormatJSON, nil
	}
	return "", fmt.Errorf("unknown format %q: want %s, %s or %s", s, FormatShell, FormatDotenv, FormatJSON)
}

type identityKey struct{}

func WithIdentity(ctx context.Context, identity string) context.Context {
	return context.WithValue(ctx, identityKey{}, identity)
}

func IdentityFrom(ctx context.Context) string {
	s, _ := ctx.Value(identityKey{}).(string)
	return s
}
