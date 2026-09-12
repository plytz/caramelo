package vpn

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"
)

const (
	DefaultSubnet = "10.86.0.0/16"

	DefaultListenPort = 4021

	DefaultListen = "0.0.0.0:4021"

	ResolverPort = 53

	MTU = 1280

	KeyFile = "vpn/private.key"
)

type Protocol string

const (
	TCP Protocol = "tcp"
	UDP Protocol = "udp"
)

func (p Protocol) Valid() bool { return p == TCP || p == UDP }

func (p Protocol) Network() string { return string(p) }

type AddressKind string

const (
	KindMachine AddressKind = "machine"

	KindPeer AddressKind = "peer"

	KindEnv AddressKind = "env"
)

type Address struct {
	IP   netip.Addr  `json:"ip"`
	Kind AddressKind `json:"kind"`

	Owner string `json:"owner"`

	Names []string `json:"names,omitempty"`
}

type Peer struct {
	Name string `json:"name"`

	PublicKey string `json:"public_key"`

	IP netip.Addr `json:"ip"`

	AddedBy string `json:"added_by,omitempty"`

	CreatedAt time.Time `json:"created_at"`

	LastHandshake time.Time `json:"last_handshake,omitempty"`
}

type Route struct {
	IP netip.Addr `json:"ip"`

	Port int `json:"port"`

	Protocol Protocol `json:"protocol"`

	Target string `json:"target"`

	Allow netip.Prefix `json:"allow,omitempty"`

	App  string `json:"app,omitempty"`
	Env  string `json:"env,omitempty"`
	Name string `json:"name,omitempty"`
}

func (r Route) Allows(src netip.Addr) bool {
	return !r.Allow.IsValid() || r.Allow.Contains(src)
}

func (r Route) AddrPort() netip.AddrPort {
	return netip.AddrPortFrom(r.IP, uint16(r.Port))
}

type Forward struct {
	Name string `json:"name"`

	Machine string `json:"machine,omitempty"`

	To netip.AddrPort `json:"to"`

	Port int `json:"port"`
}

func (f Forward) Addr() string {
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(f.Port)).String()
}

type Status struct {
	Up bool `json:"up"`

	PublicKey string `json:"public_key,omitempty"`

	Listen string `json:"listen,omitempty"`

	Subnet netip.Prefix `json:"subnet"`

	IP netip.Addr `json:"ip"`

	Resolver string `json:"resolver,omitempty"`

	Peers  int `json:"peers"`
	Routes int `json:"routes"`

	Machines int `json:"machines,omitempty"`
	Forwards int `json:"forwards,omitempty"`

	Range netip.Prefix `json:"range,omitempty"`
	Relay bool         `json:"relay,omitempty"`
}

type Device interface {
	Up(ctx context.Context) error

	Down(ctx context.Context) error

	Status(ctx context.Context) (Status, error)

	AddPeer(ctx context.Context, p Peer) error

	RemovePeer(ctx context.Context, name string) error

	Peers(ctx context.Context) ([]Peer, error)

	AddMachinePeer(ctx context.Context, m MachinePeer) error

	RemoveMachinePeer(ctx context.Context, name string) error

	MachinePeers(ctx context.Context) ([]MachinePeer, error)

	MachineAt(ctx context.Context, ip netip.Addr) (MachinePeer, bool)

	AddForward(ctx context.Context, f Forward) (Forward, error)

	RemoveForward(ctx context.Context, name string) error

	Forwards(ctx context.Context) ([]Forward, error)

	SetFleetNames(ctx context.Context, addrs []Address) error

	SetAddresses(ctx context.Context, addrs []Address) error

	AddAddress(ctx context.Context, a Address) error

	RemoveAddress(ctx context.Context, ip netip.Addr) error

	Addresses(ctx context.Context) ([]Address, error)

	SetRoutes(ctx context.Context, routes []Route) error

	SetAddressRoutes(ctx context.Context, ip netip.Addr, routes []Route) error

	Routes(ctx context.Context) ([]Route, error)

	Listen(ctx context.Context, at netip.AddrPort) (net.Listener, error)

	PeerAt(ctx context.Context, ip netip.Addr) (Peer, bool)

	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type Resolver interface {
	Resolve(ctx context.Context, name string) ([]netip.Addr, error)

	SetZone(ctx context.Context, addrs []Address) error
}

type Allocator interface {
	AllocatePeer(ctx context.Context, name string) (netip.Addr, error)

	AllocateEnv(ctx context.Context, envID int64) (netip.Addr, error)

	Release(ctx context.Context, ip netip.Addr) error
}

var (
	ErrNXDOMAIN = errors.New("no such name")

	ErrExhausted = errors.New("no free address in the range")

	ErrNotImplemented = errors.New("not implemented")
)

type Options struct {
	Subnet netip.Prefix

	Listen string

	PrivateKeyPath string

	MTU int

	FleetRange netip.Prefix

	Relay bool

	Forward netip.AddrPort

	Log interface{ Write([]byte) (int, error) }
}
