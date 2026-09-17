package edge

import (
	"context"
	"net"
	"path/filepath"
	"time"

	"github.com/plytz/caramelo/internal/edge/certs"
)

func SocketPath(runDir string) string { return filepath.Join(runDir, SocketFile) }

func RoutesPath(stateDir string) string { return filepath.Join(stateDir, RoutesFile) }

func CertsPath(stateDir string) string { return filepath.Join(stateDir, certs.StoreDir) }

type Op string

const (
	OpPushTable Op = "push-table"

	OpStatus Op = "status"

	OpSubscribe Op = "subscribe"

	OpCA Op = "ca"

	OpCounts Op = "counts"

	OpPrune Op = "prune"
)

type Request struct {
	Op Op `json:"op"`

	Table *Table `json:"table,omitempty"`

	Since time.Time `json:"since,omitempty"`

	Prune *certs.PruneRequest `json:"prune,omitempty"`
}

type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`

	Status *Status `json:"status,omitempty"`

	CA *certs.CA `json:"ca,omitempty"`

	Event *Event `json:"event,omitempty"`

	Counts *Counts `json:"counts,omitempty"`

	Pruned *certs.PruneResult `json:"pruned,omitempty"`
}

type Counts struct {
	Since time.Time `json:"since"`
	At    time.Time `json:"at"`

	Hosts []HostCounts `json:"hosts,omitempty"`
}

type HostCounts struct {
	Host string `json:"host"`

	Requests int `json:"requests"`

	Status5xx int `json:"status_5xx"`

	ConnectFailures int `json:"connect_failures"`

	Targets []TargetCounts `json:"targets,omitempty"`

	Unrouted UnroutedCounts `json:"unrouted"`
}

type UnroutedCounts struct {
	Requests        int `json:"requests"`
	Status5xx       int `json:"status_5xx"`
	ConnectFailures int `json:"connect_failures"`
}

func (u UnroutedCounts) Errors() int { return u.Status5xx + u.ConnectFailures }

type TargetCounts struct {
	Replica         int `json:"replica"`
	Requests        int `json:"requests"`
	Status5xx       int `json:"status_5xx"`
	ConnectFailures int `json:"connect_failures"`
}

func (c *Counts) Host(host string) (HostCounts, bool) {
	if c == nil {
		return HostCounts{}, false
	}
	want := NormalizeHost(host)
	for _, h := range c.Hosts {
		if NormalizeHost(h.Host) == want {
			return h, true
		}
	}
	return HostCounts{}, false
}

func (h HostCounts) Errors() int { return h.Status5xx + h.ConnectFailures }

func (h HostCounts) Rate() float64 {
	if h.Requests <= 0 {
		return 0
	}
	return float64(h.Errors()) / float64(h.Requests)
}

func (h HostCounts) Target(replica int) (TargetCounts, bool) {
	for _, t := range h.Targets {
		if t.Replica == replica {
			return t, true
		}
	}
	return TargetCounts{}, false
}

func (t TargetCounts) Errors() int { return t.Status5xx + t.ConnectFailures }

func (t TargetCounts) Rate() float64 {
	if t.Requests <= 0 {
		return 0
	}
	return float64(t.Errors()) / float64(t.Requests)
}

func (h HostCounts) Replicas(replicas ...int) TargetCounts {
	out := TargetCounts{}
	for _, r := range replicas {
		t, ok := h.Target(r)
		if !ok {
			continue
		}
		out.Requests += t.Requests
		out.Status5xx += t.Status5xx
		out.ConnectFailures += t.ConnectFailures
	}
	return out
}

type EventKind string

const (
	EventTarget EventKind = "target"

	EventDrained EventKind = "drained"

	EventCertificate EventKind = "certificate"

	EventTable EventKind = "table"
)

type Event struct {
	Kind EventKind `json:"kind"`

	Host string `json:"host,omitempty"`

	Target *Target `json:"target,omitempty"`

	Deadline bool `json:"deadline,omitempty"`

	Certificate *certs.Certificate `json:"certificate,omitempty"`

	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

type Status struct {
	Running bool   `json:"running"`
	Error   string `json:"error,omitempty"`

	Version   string    `json:"version,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`

	Listeners []string `json:"listeners,omitempty"`

	HTTP3 bool `json:"http3"`

	TLS           certs.Mode `json:"tls,omitempty"`
	ACMEDirectory string     `json:"acme_directory,omitempty"`

	Routes []Route `json:"routes,omitempty"`

	Certificates []certs.Certificate `json:"certificates,omitempty"`

	TableUpdatedAt time.Time `json:"table_updated_at,omitempty"`

	Ingress *IngressStatus `json:"ingress,omitempty"`
}

type Client interface {
	PushTable(ctx context.Context, t Table) error

	Status(ctx context.Context) (*Status, error)

	Subscribe(ctx context.Context, since time.Time, fn func(Event) error) error

	CA(ctx context.Context) (*certs.CA, error)

	Counts(ctx context.Context, since time.Time) (*Counts, error)

	Prune(ctx context.Context, req certs.PruneRequest) (*certs.PruneResult, error)

	Close() error
}

type Handler interface {
	PushTable(ctx context.Context, t Table) error

	Status(ctx context.Context) (*Status, error)

	Subscribe(ctx context.Context, since time.Time, fn func(Event) error) error

	CA(ctx context.Context) (*certs.CA, error)

	Counts(ctx context.Context, since time.Time) (*Counts, error)

	Prune(ctx context.Context, req certs.PruneRequest) (*certs.PruneResult, error)
}

type Server interface {
	Serve(ctx context.Context, ln net.Listener) error

	Close() error
}

const DialTimeout = 5 * time.Second
