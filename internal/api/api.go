package api

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/edge/certs"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/machine"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/state"
)

type Service interface {
	Status(ctx context.Context) (*Status, error)
	Machine(ctx context.Context) (*machine.Record, error)
	Keys(ctx context.Context) ([]state.Key, error)
	AddKey(ctx context.Context, name, options, authorizedKeyLine string) (*state.Key, error)
	RemoveKey(ctx context.Context, name string) error

	CreateEnv(ctx context.Context, req env.CreateRequest, progress io.Writer) (*env.Env, error)
	Envs(ctx context.Context, app string) ([]env.Env, error)
	Env(ctx context.Context, app, name string) (*EnvDetail, error)
	DestroyEnv(ctx context.Context, req env.DestroyRequest, progress io.Writer) error
	ExecEnv(ctx context.Context, req env.ExecRequest) (int, error)

	ExportEnv(ctx context.Context, app, name string, format env.ExportFormat, view config.View, reveal bool) (string, error)
	Apps(ctx context.Context) ([]AppInfo, error)

	Up(ctx context.Context, req env.UpRequest, progress io.Writer) (*UpResult, error)

	Down(ctx context.Context, req env.DownRequest, progress io.Writer) (*DownResult, error)

	Test(ctx context.Context, req env.RunRequest) (int, error)

	Run(ctx context.Context, req env.RunRequest) (int, error)

	Logs(ctx context.Context, req env.LogsRequest) error

	EffectiveConfig(ctx context.Context, app, name string, reveal bool) (*EffectiveConfig, error)

	URLs(ctx context.Context, app, name, target string) ([]env.URL, error)

	AddPeer(ctx context.Context, name, publicKey string) (*state.Peer, error)
	Peers(ctx context.Context) ([]state.Peer, error)

	RemovePeer(ctx context.Context, name string, force bool) error

	VPNStatus(ctx context.Context) (*VPNStatus, error)

	Expose(ctx context.Context, req env.ExposeRequest) (*ExposeResult, error)
	Unexpose(ctx context.Context, req env.UnexposeRequest) (*ExposeResult, error)

	EdgeStatus(ctx context.Context) (*edge.Status, error)

	EdgeCounts(ctx context.Context, since time.Time) (*edge.Counts, error)

	EdgeCA(ctx context.Context) (*certs.CA, error)

	EdgePrune(ctx context.Context, req certs.PruneRequest) (*certs.PruneResult, error)

	ProductionService

	FleetService
}

type ExposeResult struct {
	Env env.Env `json:"env"`

	Routes []edge.Route `json:"routes"`

	Changed []string `json:"changed,omitempty"`

	URL string `json:"url,omitempty"`
}

type VPNStatus struct {
	Enabled bool   `json:"enabled"`
	Error   string `json:"error,omitempty"`

	PublicKey string `json:"public_key,omitempty"`

	Listen   string `json:"listen,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`

	Mode string `json:"mode,omitempty"`

	Subnet  string `json:"subnet,omitempty"`
	Address string `json:"address,omitempty"`

	Reach string `json:"reach,omitempty"`

	Resolver string `json:"resolver,omitempty"`

	APIListen string `json:"api_listen,omitempty"`

	Peers  int `json:"peers"`
	Envs   int `json:"envs"`
	Routes int `json:"routes"`
}

type EnvDetail struct {
	Env env.Env `json:"env"`

	Machine     string         `json:"machine,omitempty"`
	Unreachable bool           `json:"unreachable,omitempty"`
	Deps        []env.DepState `json:"deps"`

	Services []env.Service `json:"services,omitempty"`

	Network string `json:"network,omitempty"`

	Vars map[string]map[string]string `json:"vars,omitempty"`

	Routes []edge.Route `json:"routes,omitempty"`

	Certificates []certs.Certificate `json:"certificates,omitempty"`

	Release *release.Release `json:"release,omitempty"`

	Deploy *env.Deploy `json:"deploy,omitempty"`
	Events []env.Event `json:"events"`
}

type UpResult struct {
	Env      env.Env       `json:"env"`
	Services []env.Service `json:"services"`

	Network string `json:"network,omitempty"`

	Image string `json:"image,omitempty"`

	Stack string `json:"stack,omitempty"`

	Rollouts []env.Rollout `json:"rollouts,omitempty"`

	Routes []edge.Route `json:"routes,omitempty"`

	URL string `json:"url,omitempty"`
}

type DownResult struct {
	Services []env.Service `json:"services"`
}

type Source string

const (
	SourceFile Source = "file"

	SourceDetected Source = "detected"

	SourceDefault Source = "default"
)

type ConfigField struct {
	Key string `json:"key"`

	Value string `json:"value"`

	Source Source `json:"source"`

	Evidence string `json:"evidence,omitempty"`
}

type EffectiveConfig struct {
	App string `json:"app"`

	Env string `json:"env,omitempty"`

	Stack string `json:"stack,omitempty"`

	Config *config.App `json:"config"`

	Fields []ConfigField `json:"fields,omitempty"`
}

type AppInfo struct {
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch,omitempty"`

	Stack    string `json:"stack,omitempty"`
	EnvCount int    `json:"env_count"`

	RepoBytes int64 `json:"repo_bytes"`
}

type Status struct {
	Version            string        `json:"version"`
	Hostname           string        `json:"hostname"`
	Uptime             time.Duration `json:"uptime"`
	StartedAt          time.Time     `json:"started_at"`
	Transport          string        `json:"transport"`
	Machine            string        `json:"machine"`
	Identity           string        `json:"identity,omitempty"`
	Docker             DockerStatus  `json:"docker"`
	HostKeyFingerprint string        `json:"host_key_fingerprint"`
	SSHPort            int           `json:"ssh_port"`
	Paths              Paths         `json:"paths"`

	VPN *VPNStatus `json:"vpn,omitempty"`

	Production *ProductionStatus `json:"production,omitempty"`
}

type ProductionStatus struct {
	Envs    int `json:"envs"`
	Release int `json:"release"`

	Deploying int `json:"deploying"`

	Unhealthy int `json:"unhealthy"`
	Held      int `json:"held"`

	Vault   bool `json:"vault"`
	Secrets int  `json:"secrets"`

	Watching    int   `json:"watching,omitempty"`
	FeedDropped int64 `json:"feed_dropped,omitempty"`
}

type DockerStatus struct {
	Running       bool   `json:"running"`
	Rootless      bool   `json:"rootless"`
	ServerVersion string `json:"server_version,omitempty"`
	Error         string `json:"error,omitempty"`
}

type Paths struct {
	Config string `json:"config"`
	State  string `json:"state"`
	Data   string `json:"data"`
	Socket string `json:"socket"`
}

type Session struct {
	Transport string
	Identity  string
	Machine   string

	Args []string `json:"args,omitempty"`

	Stdin  io.Reader `json:"-"`
	Stdout io.Writer `json:"-"`
	Stderr io.Writer `json:"-"`

	Peer string `json:"peer,omitempty"`

	OnBehalfOf string `json:"on_behalf_of,omitempty"`
}

func (s Session) Author() string {
	if s.FromMachine() && s.OnBehalfOf != "" {
		return s.OnBehalfOf
	}
	return s.Identity
}

func (s Session) FromMachine() bool { return s.Peer != "" }

type ForwardedError struct {
	Machine string

	Code int
}

func (e *ForwardedError) Error() string {
	return fmt.Sprintf("the command ran on machine %s and exited %d", e.Machine, e.Code)
}

func Forwarded(machine string, code int) error { return &ForwardedError{Machine: machine, Code: code} }
