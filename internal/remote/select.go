package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"

	"github.com/plytz/caramelo/internal/serverconfig"
)

var ErrNoMachine = errors.New("no caramelod to talk to")

type Selection struct {
	Machine string

	Config CommanderConfig

	SocketPath string
	SocketUser string

	SocketExists func(path string) bool

	Tunnel func(ctx context.Context, t Target) (Dialer, error)
}

type Choice struct {
	Kind string

	SocketPath string
	User       string

	Target Target

	Dialer Dialer

	Why string
}

func Select(ctx context.Context, s Selection) (Choice, error) {
	exists := s.SocketExists
	if exists == nil {
		exists = SocketExists
	}
	socket := func(why string) Choice {
		return Choice{Kind: KindSocket, SocketPath: s.SocketPath, User: s.SocketUser, Why: why}
	}

	if s.Machine != "" {
		if s.Machine == "local" {
			return socket("--machine local"), nil
		}
		target, err := s.Config.Resolve(s.Machine)
		if err != nil {
			return Choice{}, err
		}
		return s.remote(ctx, target, "--machine "+s.Machine)
	}

	if exists(s.SocketPath) {
		return socket("local daemon socket"), nil
	}

	if s.Config.Commander.DefaultMachine != "" {
		target, err := s.Config.Resolve(s.Config.Commander.DefaultMachine)
		if err != nil {
			return Choice{}, err
		}
		return s.remote(ctx, target, "commander.default_machine in the commander config")
	}

	return Choice{}, ErrNoMachine
}

func (s Selection) remote(ctx context.Context, target Target, why string) (Choice, error) {
	tunnel := s.Tunnel
	if tunnel == nil {
		tunnel = Tunnel
	}
	d, err := tunnel(ctx, target)
	switch {
	case err == nil && d != nil:
		return Choice{Kind: KindTunnel, Target: target, Dialer: d, Why: why + ", peer key for this machine"}, nil
	case err == nil, errors.Is(err, ErrNoTunnel):

		return Choice{Kind: KindSSH, Target: target, Why: why}, nil
	default:

		return Choice{}, fmt.Errorf("reach %s through its tunnel: %w", target, err)
	}
}

func SocketExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

func TunnelTransport(d Dialer, t Target) *ConnTransport {
	addr := net.JoinHostPort(TunnelMachineHost(d), strconv.Itoa(t.Port))
	return &ConnTransport{
		KindName: KindTunnel,
		Dialer:   d,
		Network:  "tcp",
		Address:  addr,
		User:     t.User,
		Label:    t.Host + " (" + addr + ")",
	}
}

type TunnelMachineAddr interface {
	MachineAddr() string
}

func TunnelMachineHost(d Dialer) string {
	if m, ok := d.(TunnelMachineAddr); ok {
		if addr := m.MachineAddr(); addr != "" {
			return addr
		}
	}
	return defaultMachineHost()
}

func defaultMachineHost() string {
	p, err := netip.ParsePrefix(serverconfig.DefaultVPNSubnet)
	if err != nil {
		return ""
	}
	ip, err := serverconfig.MachineIP(p)
	if err != nil {
		return ""
	}
	return ip.String()
}
