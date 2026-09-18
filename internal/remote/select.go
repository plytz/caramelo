package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/plytz/caramelo/internal/serverconfig"
)

var ErrNoFleet = errors.New("no caramelod to talk to")

const MachineLocal = "local"

type Selection struct {
	Machine string

	Fleet string

	App func() string

	Config CommanderConfig

	SocketPath string
	SocketUser string

	SocketExists func(path string) bool

	Tunnel func(ctx context.Context, fleet string, t Target) (Dialer, error)
}

type Choice struct {
	Kind string

	SocketPath string
	User       string

	Target Target

	Fleet string

	Dialer Dialer

	Why string
}

type Plan struct {
	Kind string

	Target Target

	Fleet string

	Why string
}

func Select(ctx context.Context, s Selection) (Choice, error) {
	p, err := s.Plan()
	if err != nil {
		return Choice{}, err
	}
	if p.Kind == KindSocket {
		return Choice{Kind: KindSocket, SocketPath: s.SocketPath, User: s.SocketUser, Why: p.Why}, nil
	}
	return s.remote(ctx, p.Target, p.Fleet, p.Why)
}

func (s Selection) Plan() (Plan, error) {
	exists := s.SocketExists
	if exists == nil {
		exists = SocketExists
	}
	socket := func(why string) Plan {
		return Plan{Kind: KindSocket, Why: why}
	}

	machine := strings.TrimSpace(s.Machine)
	fleet := strings.TrimSpace(s.Fleet)
	if machine != "" && fleet != "" {
		return Plan{}, fmt.Errorf(
			"--machine %s and --fleet %s ask for two different things: --fleet names a fleet of the "+
				"commander config and --machine a raw ssh target that is in no fleet yet", machine, fleet)
	}

	if machine != "" {
		if machine == MachineLocal {
			return socket("--machine " + MachineLocal), nil
		}
		target, err := ParseTarget(machine)
		if err != nil {
			return Plan{}, err
		}
		return Plan{Kind: KindSSH, Target: target, Why: "--machine " + machine}, nil
	}

	if fleet != "" {
		return s.planFleet(fleet, "--fleet "+fleet)
	}

	if exists(s.SocketPath) {
		return socket("local daemon socket"), nil
	}

	if app := s.app(); app != "" {
		if name := s.Config.FleetForApp(app); name != "" {
			return s.planFleet(name, "the fleet recorded for app "+app)
		}
	}

	if name := strings.TrimSpace(s.Config.Commander.DefaultFleet); name != "" {
		return s.planFleet(name, "commander.default_fleet")
	}

	names := s.Config.FleetNames()
	switch len(names) {
	case 0:
		return Plan{}, ErrNoFleet
	case 1:
		return s.planFleet(names[0], "the only fleet in the commander config")
	}
	return Plan{}, fmt.Errorf(
		"this commander knows %d fleets (%s) and nothing here says which one to talk to: "+
			"pass --fleet NAME (or CARAMELO_FLEET), or make one the default with "+
			"'caramelo fleet default NAME'", len(names), s.Config.FleetList())
}

func (s Selection) planFleet(name, why string) (Plan, error) {
	target, err := s.Config.FleetTarget(name)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Kind: KindSSH, Target: target, Fleet: name, Why: why}, nil
}

func (s Selection) app() string {
	if s.App == nil {
		return ""
	}
	return strings.TrimSpace(s.App())
}

func (s Selection) remote(ctx context.Context, target Target, fleet, why string) (Choice, error) {
	tunnel := s.Tunnel
	if tunnel == nil {
		tunnel = Tunnel
	}
	d, err := tunnel(ctx, fleet, target)
	switch {
	case err == nil && d != nil:
		return Choice{Kind: KindTunnel, Target: target, Fleet: fleet, Dialer: d,
			Why: why + ", peer key for this machine"}, nil
	case err == nil, errors.Is(err, ErrNoTunnel):

		return Choice{Kind: KindSSH, Target: target, Fleet: fleet, Why: why}, nil
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
