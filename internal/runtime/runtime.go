package runtime

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/plytz/caramelo/internal/runner"
)

var ErrNotFound = errors.New("not found")

const (
	StatusRunning = "running"
	StatusExited  = "exited"
	StatusCreated = "created"
)

const RestartUnlessStopped = "unless-stopped"

type Driver interface {
	Pull(ctx context.Context, image string) error

	CreateVolume(ctx context.Context, name string, labels map[string]string) error

	RemoveVolume(ctx context.Context, name string) error

	Run(ctx context.Context, spec ContainerSpec) (string, error)

	Inspect(ctx context.Context, name string) (ContainerState, error)

	Exec(ctx context.Context, name string, argv []string) (runner.Result, error)

	LogTail(ctx context.Context, name string, tail int) (string, error)

	Remove(ctx context.Context, name string, force bool) error

	Stop(ctx context.Context, name string, timeout time.Duration) error

	Restart(ctx context.Context, name string, timeout time.Duration) error

	Build(ctx context.Context, spec BuildSpec) (string, error)

	CreateNetwork(ctx context.Context, name string, labels map[string]string) error

	RemoveNetwork(ctx context.Context, name string) error

	Connect(ctx context.Context, network, container string, aliases []string) error

	RunAttached(ctx context.Context, spec ContainerSpec, streams Streams) (int, error)

	Logs(ctx context.Context, name string, opts LogOptions, stdout, stderr io.Writer) error

	ImageExists(ctx context.Context, ref string) (bool, error)

	RemoveImage(ctx context.Context, ref string) error

	ListByLabel(ctx context.Context, labels map[string]string) ([]ContainerState, error)

	ListVolumesByLabel(ctx context.Context, labels map[string]string) ([]string, error)
}

type BuildSpec struct {
	Context string

	Dockerfile string

	Tag string

	Labels map[string]string

	NoCache bool

	Progress io.Writer
}

type Streams struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type LogOptions struct {
	Follow bool

	Since string

	Tail int

	Timestamps bool
}

type ContainerSpec struct {
	Name string

	Image string

	Env map[string]string

	Labels map[string]string

	Publish []PortMap

	Volumes []VolumeMount

	Restart string

	Binds []BindMount

	Network string
	Aliases []string

	Command []string

	WorkDir string

	User string

	AutoRemove bool

	TTY bool

	EnvFile string

	Memory int64
	CPU    float64
}

const (
	LogMaxSize = "10m"

	LogMaxFiles = 5
)

type BindMount struct {
	Host     string `json:"host"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

type PortMap struct {
	HostIP        string `json:"host_ip"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`

	Protocol string `json:"protocol,omitempty"`
}

type VolumeMount struct {
	Volume   string `json:"volume"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

type ContainerState struct {
	Name   string            `json:"name"`
	ID     string            `json:"id"`
	Status string            `json:"status"`
	Image  string            `json:"image"`
	Labels map[string]string `json:"labels,omitempty"`
	Ports  []PortMap         `json:"ports,omitempty"`

	Restarts int `json:"restarts,omitempty"`
}

func (c ContainerState) Running() bool { return c.Status == StatusRunning }
