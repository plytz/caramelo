package env

import (
	"io"
	"time"
)

const MountPath = "/app"

const DefaultUpTimeout = 120 * time.Second

type ServiceStatus string

const (
	ServiceStarting ServiceStatus = "starting"

	ServiceRunning ServiceStatus = "running"

	ServiceFailed ServiceStatus = "failed"

	ServiceStopped ServiceStatus = "stopped"

	ServiceExited ServiceStatus = "exited"

	ServiceMissing ServiceStatus = "missing"
)

type HealthStatus string

const (
	HealthNone HealthStatus = "none"

	HealthWaiting HealthStatus = "waiting"

	HealthOK HealthStatus = "ok"

	HealthFailed HealthStatus = "failed"
)

type Change string

const (
	ChangeCreated Change = "created"

	ChangeRecreated Change = "recreated"

	ChangeUnchanged Change = "unchanged"

	ChangeRemoved Change = "removed"
)

type Service struct {
	Name string `json:"name"`

	Container string `json:"container"`

	ID string `json:"id,omitempty"`

	Image string `json:"image,omitempty"`

	Command []string `json:"command,omitempty"`

	Port int `json:"port,omitempty"`

	ContainerPort int `json:"container_port,omitempty"`

	Protocol string `json:"protocol,omitempty"`

	URL string `json:"url,omitempty"`

	Status ServiceStatus `json:"status"`

	Health HealthStatus `json:"health,omitempty"`

	Change Change `json:"change,omitempty"`

	Detail string `json:"detail,omitempty"`

	Vars map[string]string `json:"vars,omitempty"`

	SecretsDigest string    `json:"secrets_digest,omitempty"`
	UpdatedAt     time.Time `json:"updated_at,omitempty"`

	Replicas []Replica `json:"replicas,omitempty"`

	Expose string `json:"expose,omitempty"`

	Host      string `json:"host,omitempty"`
	PublicURL string `json:"public_url,omitempty"`
}

type UpRequest struct {
	App  string `json:"app"`
	Name string `json:"name"`

	Services []string `json:"services,omitempty"`

	Build bool `json:"build,omitempty"`

	Timeout time.Duration `json:"timeout,omitempty"`

	NoWait bool `json:"no_wait,omitempty"`
}

type DownRequest struct {
	App  string `json:"app"`
	Name string `json:"name"`

	Services []string `json:"services,omitempty"`

	Force bool `json:"force,omitempty"`
}

type RunRequest struct {
	App  string `json:"app"`
	Name string `json:"name"`

	Service string `json:"service,omitempty"`

	Test bool     `json:"test,omitempty"`
	Argv []string `json:"argv,omitempty"`

	Timeout time.Duration `json:"timeout,omitempty"`

	Stdin  io.Reader `json:"-"`
	Stdout io.Writer `json:"-"`
	Stderr io.Writer `json:"-"`
}

type LogsRequest struct {
	App  string `json:"app"`
	Name string `json:"name"`

	Services []string `json:"services,omitempty"`

	Deps bool `json:"deps,omitempty"`

	Follow bool `json:"follow,omitempty"`

	Since string `json:"since,omitempty"`

	Tail int `json:"tail,omitempty"`

	Edge bool `json:"edge,omitempty"`

	JSON bool `json:"json,omitempty"`

	Stdout io.Writer `json:"-"`
	Stderr io.Writer `json:"-"`
}

type LogLine struct {
	Service string `json:"service"`

	Stream string `json:"stream"`

	TS string `json:"ts,omitempty"`

	Line string `json:"line"`
}

type URLKind string

const (
	KindService URLKind = "service"
	KindDep     URLKind = "dep"
)

type URL struct {
	Name string `json:"name"`

	Kind URLKind `json:"kind"`

	URL string `json:"url"`

	Host string `json:"host"`
	Port int    `json:"port"`

	Protocol string `json:"protocol,omitempty"`

	InternalHost string `json:"internal_host,omitempty"`

	InternalPort int `json:"internal_port,omitempty"`

	InternalAddress string `json:"internal_address,omitempty"`

	InternalURL string `json:"internal_url,omitempty"`
}
