package fleet

import (
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/machine"
)

type Role string

const (
	RoleHub Role = "hub"

	RoleMember Role = "member"
)

var Roles = []Role{RoleHub, RoleMember}

func ParseRole(s string) (Role, error) {
	switch r := Role(strings.ToLower(strings.TrimSpace(s))); r {
	case "", RoleHub:
		return RoleHub, nil
	case RoleMember:
		return RoleMember, nil
	default:
		return "", fmt.Errorf("unknown fleet role %q: want %s or %s", s, RoleHub, RoleMember)
	}
}

func (r Role) String() string {
	if r == "" {
		return string(RoleHub)
	}
	return string(r)
}

func (r Role) IsHub() bool { return r == "" || r == RoleHub }

type Machine struct {
	Name string `json:"name"`

	Role Role `json:"role"`

	PublicKey string `json:"public_key"`

	Subnet netip.Prefix `json:"subnet"`

	Arch string `json:"arch,omitempty"`
	OS   string `json:"os,omitempty"`

	Endpoint string `json:"endpoint,omitempty"`

	Private bool `json:"private,omitempty"`

	JoinedAt time.Time `json:"joined_at"`
	LastSeen time.Time `json:"last_seen,omitempty"`

	Gauge *machine.Record `json:"gauge,omitempty"`

	Envs int `json:"envs"`
}

const UnreachableAfter = 100 * time.Second

const HeartbeatInterval = 30 * time.Second

func (m Machine) Address() netip.Addr {
	if !m.Subnet.IsValid() {
		return netip.Addr{}
	}
	base := m.Subnet.Masked().Addr()
	if !base.Is4() {
		return netip.Addr{}
	}
	b := base.As4()
	b[3] = 1
	return netip.AddrFrom4(b)
}

func (m Machine) Reachable(now time.Time) bool {
	if m.Role.IsHub() {
		return true
	}
	if m.LastSeen.IsZero() {
		return false
	}
	return now.Sub(m.LastSeen) <= UnreachableAfter
}

type Announcement struct {
	Machine string          `json:"machine"`
	Arch    string          `json:"arch,omitempty"`
	OS      string          `json:"os,omitempty"`
	Gauge   *machine.Record `json:"gauge,omitempty"`

	Envs []DirectoryEntry `json:"envs,omitempty"`

	Full bool      `json:"full,omitempty"`
	At   time.Time `json:"at"`
}

type DirectoryEntry struct {
	App     string `json:"app"`
	Env     string `json:"env"`
	Machine string `json:"machine"`

	Address string `json:"address,omitempty"`

	Owner string `json:"owner,omitempty"`

	Mode string `json:"mode,omitempty"`

	Via string `json:"via,omitempty"`

	Hosts []ViaHost `json:"hosts,omitempty"`

	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

func (d DirectoryEntry) Key() (string, string) { return d.App, d.Env }

type ViaHost struct {
	Host    string `json:"host"`
	Service string `json:"service,omitempty"`

	Drain time.Duration `json:"drain,omitempty"`
}
