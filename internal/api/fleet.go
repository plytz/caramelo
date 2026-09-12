package api

import (
	"context"
	"io"
	"time"

	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/machine"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/vault"
)

type FleetService interface {
	MachineAdd(ctx context.Context, req MachineAddRequest, progress io.Writer) (*MachineAddResult, error)

	MachineToken(ctx context.Context, req MachineTokenRequest) (*MachineTokenResult, error)

	MachineJoin(ctx context.Context, req MachineJoinRequest, progress io.Writer) (*MachineJoinResult, error)

	Machines(ctx context.Context) ([]fleet.Machine, error)

	MachineInfo(ctx context.Context, name string) (*MachineDetail, error)

	MachineRemove(ctx context.Context, req MachineRemoveRequest, progress io.Writer) error

	EnvHandoff(ctx context.Context, req HandoffRequest) (*env.Env, error)

	EnvsMine(ctx context.Context, app string) ([]env.Env, error)

	EnvsAll(ctx context.Context, app string) ([]fleet.DirectoryEntry, error)

	EnvExpose(ctx context.Context, req ExposeRequest) (*ExposeResult, error)

	MachineAnnounce(ctx context.Context, a fleet.Announcement) (*AnnounceResult, error)

	VaultBundle(ctx context.Context, req BundleRequest) (*vault.Bundle, error)

	ImageTransfer(ctx context.Context, req ImageTransferRequest, out io.Writer) (*ImageTransferResult, error)

	MachineRedeem(ctx context.Context, req RedeemRequest) (*RedeemResult, error)

	MachineRemoved(ctx context.Context) error

	EnvWorktreeStatus(ctx context.Context, app, name string) (*env.WorktreeStatus, error)

	ReleaseOfTree(ctx context.Context, app, tree string) (*release.Release, error)

	ReleaseRecord(ctx context.Context, req ReleaseRecordRequest) error

	EnvSync(ctx context.Context, app, name string) (*env.Env, error)

	GitPrecheck(ctx context.Context, app string, branches []string) error
}

type ReleaseRecordRequest struct {
	Release release.Release `json:"release"`

	Images release.Images `json:"images,omitempty"`
}

type RedeemRequest struct {
	Secret string `json:"secret"`

	Name string `json:"name"`

	PublicKey string `json:"public_key"`

	Arch string `json:"arch,omitempty"`
	OS   string `json:"os,omitempty"`

	Private bool `json:"private,omitempty"`

	Endpoint string `json:"endpoint,omitempty"`
}

type RedeemResult struct {
	Machine fleet.Machine `json:"machine"`

	Hub fleet.Machine `json:"hub"`

	Endpoint string `json:"endpoint,omitempty"`

	Changed bool `json:"changed"`
}

type Resolver interface {
	ResolveEnv(ctx context.Context, app, name string) (Location, error)

	ResolveMachine(ctx context.Context, name string) (Location, error)
}

type Location struct {
	Machine string `json:"machine,omitempty"`

	Local bool `json:"local"`

	Address string `json:"address,omitempty"`

	Reachable bool `json:"reachable"`

	LastSeen time.Time `json:"last_seen,omitempty"`
}

type Forwarder interface {
	Forward(ctx context.Context, loc Location, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)
}

type MachineAddRequest struct {
	Target string `json:"target"`

	Name string `json:"name,omitempty"`

	Edge bool `json:"edge,omitempty"`

	Private bool `json:"private,omitempty"`

	Binary string `json:"binary,omitempty"`
}

type MachineAddResult struct {
	Machine fleet.Machine `json:"machine"`

	Setup any `json:"setup,omitempty"`

	Joined bool `json:"joined"`
}

type MachineTokenRequest struct {
	TTL time.Duration `json:"ttl,omitempty"`
}

type MachineTokenResult struct {
	Token string `json:"token"`

	Hub      string `json:"hub"`
	Endpoint string `json:"endpoint,omitempty"`

	PublicKey string    `json:"public_key,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

type MachineJoinRequest struct {
	Hub string `json:"hub"`

	Token string `json:"token"`

	Name string `json:"name,omitempty"`

	Private bool `json:"private,omitempty"`
}

type MachineJoinResult struct {
	Machine fleet.Machine `json:"machine"`

	Hub fleet.Machine `json:"hub"`

	Changed bool `json:"changed"`
}

type MachineDetail struct {
	Machine fleet.Machine `json:"machine"`

	Gauge *machine.Record `json:"gauge,omitempty"`

	Envs []fleet.DirectoryEntry `json:"envs,omitempty"`

	Unreachable bool `json:"unreachable,omitempty"`
}

type MachineRemoveRequest struct {
	Name string `json:"name"`

	Force bool `json:"force,omitempty"`
}

type HandoffRequest struct {
	App  string `json:"app"`
	Name string `json:"name"`

	To string `json:"to"`
}

type ExposeRequest struct {
	env.ExposeRequest
}

type AnnounceResult struct {
	Machine fleet.Machine `json:"machine"`

	Hub fleet.Machine `json:"hub"`

	Accepted int `json:"accepted"`

	Removed int `json:"removed,omitempty"`
}

type BundleRequest struct {
	App string `json:"app"`
	Env string `json:"env"`

	Machine string `json:"machine,omitempty"`

	Reason string `json:"reason,omitempty"`
}

type ImageTransferRequest struct {
	Release int64    `json:"release"`
	Refs    []string `json:"refs"`

	Arch string `json:"arch"`

	Send bool `json:"send"`

	Archive io.Reader `json:"-"`
}

type ImageTransferResult struct {
	Images []release.Image `json:"images,omitempty"`

	Bytes    int64         `json:"bytes,omitempty"`
	Duration time.Duration `json:"duration,omitempty"`
}
