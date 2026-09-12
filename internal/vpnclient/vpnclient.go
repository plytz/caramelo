package vpnclient

import (
	"context"
	"errors"
	"github.com/plytz/caramelo/internal/userdir"
	"net"
	"net/netip"
	"path/filepath"
	"time"

	"github.com/plytz/caramelo/internal/vpn"
)

type Mode string

const (
	ModeUserspace Mode = "userspace"

	ModeTransparent Mode = "transparent"

	ModeOff Mode = "off"
)

const KeyDir = "vpn"

type State struct {
	Mode Mode `json:"mode"`

	Machine string `json:"machine"`

	Endpoint string `json:"endpoint,omitempty"`

	PublicKey string `json:"public_key,omitempty"`
	PeerName  string `json:"peer_name,omitempty"`

	IP     netip.Addr   `json:"ip,omitempty"`
	Subnet netip.Prefix `json:"subnet,omitempty"`

	Resolver string `json:"resolver,omitempty"`

	LastHandshake time.Time `json:"last_handshake,omitempty"`

	Installed bool `json:"installed"`
}

type KeyPair struct {
	Private string `json:"-"`
	Public  string `json:"public_key"`
}

type KeyStore interface {
	Ensure(machine string) (kp KeyPair, created bool, err error)

	Load(machine string) (KeyPair, error)

	Remove(machine string) error

	Path(machine string) string
}

type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)

	Resolve(ctx context.Context, name string) ([]netip.Addr, error)

	Close() error
}

type UpRequest struct {
	Machine string `json:"machine"`

	PeerName string `json:"peer_name,omitempty"`

	Transparent bool `json:"transparent,omitempty"`
}

type Client interface {
	Up(ctx context.Context, req UpRequest) (*State, error)

	Down(ctx context.Context, machine string) error

	Status(ctx context.Context, machine string) (*State, error)

	Dialer(ctx context.Context, machine string) (Dialer, error)

	Config(ctx context.Context, machine string) (string, error)
}

type Listener struct {
	Name string `json:"name"`

	Kind string `json:"kind"`

	Protocol vpn.Protocol `json:"protocol"`

	Local string `json:"local"`

	Remote string `json:"remote"`

	Host string `json:"host"`

	URL string `json:"url,omitempty"`
}

type ConnectRequest struct {
	Machine string `json:"machine"`
	App     string `json:"app"`
	Env     string `json:"env"`

	Listen string `json:"listen,omitempty"`

	Only []string `json:"only,omitempty"`
}

type Connector interface {
	Open(ctx context.Context, req ConnectRequest) ([]Listener, error)

	Close() error
}

type InstallOptions struct {
	Machine string `json:"machine"`

	Binary string `json:"binary,omitempty"`

	Interface string `json:"interface,omitempty"`
}

type Installer interface {
	Install(ctx context.Context, opts InstallOptions) error

	Uninstall(ctx context.Context) error

	Installed(ctx context.Context) (bool, error)
}

var (
	ErrNoKey = errors.New("no key for this machine; run 'caramelo vpn up'")

	ErrNotInstalled = errors.New("the transparent-mode service is not installed; run 'sudo caramelo vpn install'")

	ErrNotImplemented = errors.New("not implemented")
)

func KeyPath(machine string) (string, error) {
	dir, err := userdir.Config()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "caramelo", KeyDir, KeyFileName(machine)), nil
}

func KeyFileName(machine string) string {
	b := []byte(machine)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.':
		default:
			b[i] = '-'
		}
	}
	name := string(b)
	if name == "" || name == "." || name == ".." {
		name = "machine"
	}
	return name + ".key"
}

var DefaultControl Control

func New() (Client, error) {
	if DefaultControl == nil {
		return nil, errors.New("vpnclient: this build has no way to reach a machine")
	}
	return NewWith(Options{Control: DefaultControl})
}
