package edge

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	SocketFile = "edge.sock"

	RoutesFile = "edge/routes.json"

	StateDirName = "edge"

	TargetHost = "127.0.0.1"
)

const (
	IngressPort = 8443
)

const (
	HTTPPort  = 80
	HTTPSPort = 443
)

var ErrNotImplemented = errors.New("not implemented")

type Kind string

const (
	KindHTTPS Kind = "https"

	KindTLS Kind = "tls"

	KindTLSPassthrough Kind = "tls-passthrough"

	KindTCP Kind = "tcp"
	KindUDP Kind = "udp"

	KindVia Kind = "via"
)

var Kinds = []Kind{KindHTTPS, KindTLS, KindTLSPassthrough, KindTCP, KindUDP, KindVia}

func (k Kind) Supported() bool { return k == KindHTTPS || k == KindVia }

func (k Kind) Known() bool {
	for _, kk := range Kinds {
		if kk == k {
			return true
		}
	}
	return false
}

func (k Kind) String() string { return string(k) }

type TargetState string

const (
	TargetStarting TargetState = "starting"

	TargetActive TargetState = "active"

	TargetDraining TargetState = "draining"

	TargetStopped TargetState = "stopped"

	TargetUnhealthy TargetState = "unhealthy"

	TargetHeld TargetState = "held"
)

var TargetStates = []TargetState{
	TargetStarting, TargetActive, TargetDraining, TargetStopped, TargetUnhealthy, TargetHeld,
}

func (s TargetState) Routable() bool { return s == TargetActive }

func (s TargetState) Valid() bool {
	for _, k := range TargetStates {
		if k == s {
			return true
		}
	}
	return false
}

func (s TargetState) String() string { return string(s) }

type Target struct {
	Replica int `json:"replica"`

	Port int `json:"port"`

	State TargetState `json:"state"`

	Inflight int `json:"inflight"`

	Since time.Time `json:"since,omitempty"`

	Detail string `json:"detail,omitempty"`
}

func (t Target) Addr() string {
	return net.JoinHostPort(TargetHost, strconv.Itoa(t.Port))
}

type Route struct {
	Host string `json:"host"`

	Kind Kind `json:"kind"`

	App     string `json:"app,omitempty"`
	Env     string `json:"env"`
	Service string `json:"service"`

	Targets []Target `json:"targets,omitempty"`

	Via string `json:"via,omitempty"`

	Drain time.Duration `json:"drain,omitempty"`

	CreatedAt time.Time `json:"created_at,omitempty"`
}

func (r Route) Target(replica int) (Target, bool) {
	for _, t := range r.Targets {
		if t.Replica == replica {
			return t, true
		}
	}
	return Target{}, false
}

func (r Route) Active() []Target {
	out := make([]Target, 0, len(r.Targets))
	for _, t := range r.Targets {
		if t.State.Routable() {
			out = append(out, t)
		}
	}
	return out
}

func (r Route) Inflight() int {
	n := 0
	for _, t := range r.Targets {
		n += t.Inflight
	}
	return n
}

type Table struct {
	Routes []Route `json:"routes"`

	Ingress *Ingress `json:"ingress,omitempty"`

	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

type Ingress struct {
	Enabled bool `json:"enabled"`

	Port int `json:"port,omitempty"`

	Trusted []string `json:"trusted,omitempty"`
}

type IngressStatus struct {
	Enabled bool `json:"enabled"`

	Addr string `json:"addr,omitempty"`

	Requests int64 `json:"requests,omitempty"`
	Inflight int   `json:"inflight,omitempty"`

	Error string `json:"error,omitempty"`
}

func (t Table) Route(host string) (Route, bool) {
	host = NormalizeHost(host)
	for _, r := range t.Routes {
		if r.Host == host {
			return r, true
		}
	}
	return Route{}, false
}

func (t Table) Has(host string) bool {
	_, ok := t.Route(host)
	return ok
}

func (t Table) Hosts() []string {
	out := make([]string, 0, len(t.Routes))
	for _, r := range t.Routes {
		out = append(out, r.Host)
	}
	sort.Strings(out)
	return out
}

func (t *Table) Put(r Route) {
	r.Host = NormalizeHost(r.Host)
	for i := range t.Routes {
		if t.Routes[i].Host == r.Host {
			t.Routes[i] = r
			return
		}
	}
	t.Routes = append(t.Routes, r)
}

func (t *Table) Remove(host string) bool {
	host = NormalizeHost(host)
	for i := range t.Routes {
		if t.Routes[i].Host != host {
			continue
		}
		t.Routes = append(t.Routes[:i], t.Routes[i+1:]...)
		return true
	}
	return false
}

func (t Table) Clone() Table {
	out := Table{UpdatedAt: t.UpdatedAt}
	if t.Ingress != nil {
		ing := *t.Ingress
		ing.Trusted = append([]string(nil), t.Ingress.Trusted...)
		out.Ingress = &ing
	}
	if t.Routes == nil {
		return out
	}
	out.Routes = make([]Route, len(t.Routes))
	for i, r := range t.Routes {
		r.Targets = append([]Target(nil), r.Targets...)
		out.Routes[i] = r
	}
	return out
}

func (t Table) Validate() error {
	seen := make(map[string]bool, len(t.Routes))
	for _, r := range t.Routes {
		host := NormalizeHost(r.Host)
		switch {
		case host == "":
			return errors.New("edge: a route needs a host")
		case seen[host]:
			return fmt.Errorf("edge: duplicate route for %q", host)
		case !r.Kind.Known():
			return fmt.Errorf("edge: route %s: unknown kind %q", host, r.Kind)
		case !r.Kind.Supported():
			return fmt.Errorf("edge: route %s: %q is not supported yet", host, r.Kind)
		case r.Kind == KindVia && r.Via == "":
			return fmt.Errorf("edge: route %s: a %q route must name the machine it is served by", host, KindVia)
		case r.Kind != KindVia && r.Via != "":
			return fmt.Errorf("edge: route %s: only a %q route is served by another machine, and this one is %q",
				host, KindVia, r.Kind)
		}
		seen[host] = true
		replicas := make(map[int]bool, len(r.Targets))
		for _, tg := range r.Targets {
			switch {
			case tg.Replica < 1:
				return fmt.Errorf("edge: route %s: replica index %d is not a replica", host, tg.Replica)
			case replicas[tg.Replica]:
				return fmt.Errorf("edge: route %s: replica %d twice", host, tg.Replica)
			case tg.Port < 1 || tg.Port > 65535:
				return fmt.Errorf("edge: route %s: replica %d: port %d is out of range (1-65535)", host, tg.Replica, tg.Port)
			case !tg.State.Valid():
				return fmt.Errorf("edge: route %s: replica %d: unknown state %q", host, tg.Replica, tg.State)
			}
			replicas[tg.Replica] = true
		}
	}
	return nil
}

func NormalizeHost(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	s = strings.TrimSuffix(s, ".")
	return strings.ToLower(s)
}

type MarkKind string

const (
	MarkStart MarkKind = "start"

	MarkDone MarkKind = "done"

	MarkFail MarkKind = "fail"

	MarkState MarkKind = "state"
)

type Mark struct {
	Replica int `json:"replica"`

	Kind MarkKind `json:"kind"`

	State TargetState `json:"state,omitempty"`

	At time.Time `json:"at,omitempty"`

	Detail string `json:"detail,omitempty"`
}

const FailureWindow = 5 * time.Second

type Pool interface {
	Pick(skip ...int) (target Target, ok bool)

	Mark(m Mark) error

	Counts() []Target
}
