package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup"
	"github.com/plytz/caramelo/internal/vpn"
	"github.com/plytz/caramelo/internal/vpnclient"
)

const joinTimeout = 60 * time.Second

func (a *app) runMachineJoin(ctx context.Context, configDir, hub, token, name string, private bool) error {
	ticket, err := fleet.ParseTicket(token)
	if err != nil {
		return &usageError{err}
	}
	if err := ticket.Validate(); err != nil {
		return &usageError{err}
	}

	if h := strings.TrimSpace(hub); h != "" {
		ticket.Endpoint = withDefaultPort(h, ticket.Endpoint)
	}
	pre, err := joinPreflightOf(configDir)
	if err != nil {
		return err
	}
	cfg := pre.cfg
	if pre.loaded && cfg.IsMember() {
		sameHub := cfg.Member.Hub.PublicKey == ticket.PublicKey
		switch {
		case sameHub && cfg.FleetName() == ticket.Fleet:

			return a.printJoined(&api.MachineJoinResult{
				Machine: fleet.Machine{
					Name: cfg.Name, Role: fleet.RoleMember, Private: cfg.Member.Private,
					Subnet: prefixOrZero(cfg.Member.Subnet),
				},
				Hub:     fleet.Machine{Name: cfg.HubName(), Role: fleet.RoleHub, Endpoint: cfg.Member.Hub.Endpoint},
				Changed: false,
			})
		case sameHub:
			return fmt.Errorf(
				"this machine is a member of the fleet %s and that token offers the fleet %s under the same hub key: "+
					"a machine belongs to one fleet, so run `caramelo member leave` here first",
				cfg.FleetName(), ticket.Fleet)
		}
		return fmt.Errorf(
			"this machine is already a member of the fleet %s; remove it there "+
				"(`caramelo member remove %s` on its hub) and `caramelo member leave` here before joining %s",
			cfg.FleetName(), cfg.Name, ticket.Fleet)
	}
	if err := pre.err(); err != nil {
		return err
	}
	key, err := vpn.ReadPrivateKey(cfg.VPNKeyPath())
	if err != nil {
		return fmt.Errorf("read this machine's WireGuard key at %s: %w "+
			"(has `caramelo hub setup` run here?)", cfg.VPNKeyPath(), err)
	}
	pub, err := key.Public()
	if err != nil {
		return fmt.Errorf("derive this machine's public key: %w", err)
	}
	if name = strings.TrimSpace(name); name == "" {
		host, _ := os.Hostname()
		name = vpnclient.Slug(host)
	}
	if private || cfg.Member.Private {
		private = true
	}

	ctx, cancel := context.WithTimeout(ctx, joinTimeout)
	defer cancel()
	res, err := redeem(ctx, ticket, api.RedeemRequest{
		Secret: ticket.Secret, Fleet: ticket.Fleet, Name: name, PublicKey: pub.Base64(),
		Arch: runtime.GOARCH, OS: runtime.GOOS, Private: private,
	}, a.tunnelLog())
	if err != nil {
		return err
	}
	if err := writeMemberConfig(configDir, cfg, ticket, res); err != nil {
		return err
	}

	if err := restartDaemon(ctx, cfg.User); err != nil {
		fmt.Fprintf(a.stderr, "caramelo: joined, but caramelod did not restart: %v\n"+
			"run `sudo systemctl restart caramelod` to finish\n", err)
	}
	return a.printJoined(&api.MachineJoinResult{
		Machine: res.Machine, Hub: res.Hub, Changed: res.Changed,
	})
}

func (a *app) printJoined(res *api.MachineJoinResult) error {
	return a.printer().Result(res, func(w io.Writer) error {
		return machineJoinedView(res).Write(w)
	})
}

func redeem(ctx context.Context, t fleet.Ticket, req api.RedeemRequest, logw io.Writer) (*api.RedeemResult, error) {
	rec, err := bootstrapRecord(t)
	if err != nil {
		return nil, err
	}
	boot := vpn.KeyFromSecret(t.Secret)
	dialer, err := vpnclient.OneOff(rec, boot.Base64(), logw)
	if err != nil {
		return nil, fmt.Errorf("open a tunnel to the hub %s at %s: %w", t.Hub, t.Endpoint, err)
	}
	defer func() { _ = dialer.Close() }()

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode the join: %w", err)
	}
	tr := &remote.ConnTransport{
		KindName: remote.KindTunnel,
		Dialer:   dialer,
		Network:  "tcp",
		Address:  rec.APIAddr(),
		User:     api.ForwardUser,
		Label:    t.Hub + " (" + rec.APIAddr() + ")",
	}
	var out, errs strings.Builder
	code, err := tr.Run(ctx, []string{"member", "redeem", "--json"}, remote.Streams{
		Stdin: strings.NewReader(string(body)), Stdout: &out, Stderr: &errs,
	})
	switch {
	case err != nil:
		return nil, fmt.Errorf("reach the hub %s at %s: %w "+
			"(only the hub's UDP port has to be open, and this machine has to be able to dial it)",
			t.Hub, t.Endpoint, err)
	case code != 0:
		detail := strings.TrimSpace(errs.String())
		if detail == "" {
			detail = fmt.Sprintf("exit %d", code)
		}
		return nil, fmt.Errorf("the hub %s refused the join: %s", t.Hub, detail)
	}
	var res api.RedeemResult
	if err := json.Unmarshal([]byte(out.String()), &res); err != nil {
		return nil, fmt.Errorf("read the hub %s's answer: %w", t.Hub, err)
	}
	if !res.Machine.Subnet.IsValid() {
		return nil, fmt.Errorf("the hub %s answered with no subnet for this machine", t.Hub)
	}
	return &res, nil
}

func bootstrapRecord(t fleet.Ticket) (vpnclient.Record, error) {
	peer, err := netip.ParseAddr(t.Peer)
	if err != nil {
		return vpnclient.Record{}, fmt.Errorf("the ticket's reserved address %q: %w", t.Peer, err)
	}
	hubIP, err := netip.ParseAddr(t.Address)
	if err != nil {
		return vpnclient.Record{}, fmt.Errorf("the ticket's hub address %q: %w", t.Address, err)
	}
	rng, err := t.RangePrefix()
	if err != nil {
		return vpnclient.Record{}, err
	}
	return vpnclient.Record{
		Fleet: t.Fleet, MachineName: t.Hub, Endpoint: t.Endpoint, MachineKey: t.PublicKey,
		Subnet: rng, MachineIP: hubIP,
		PeerName: "joining", IP: peer,
		APIPort: serverconfig.DefaultSSHPort,
	}, nil
}

func writeMemberConfig(dir string, cfg serverconfig.Config, t fleet.Ticket, res *api.RedeemResult) error {
	subnet := res.Machine.Subnet.String()
	endpoint := res.Endpoint
	if strings.TrimSpace(endpoint) == "" {
		endpoint = t.Endpoint
	}
	hubAddr := t.Address
	if a := res.Hub.Address(); a.IsValid() {
		hubAddr = a.String()
	}
	fleetName := strings.TrimSpace(res.Fleet)
	if fleetName == "" {
		fleetName = t.Fleet
	}
	hubName := strings.TrimSpace(res.Hub.Name)
	if hubName == "" {
		hubName = t.Hub
	}
	cfg.VPNSubnet = subnet
	cfg.Name = res.Machine.Name
	cfg.Role = serverconfig.RoleMember
	cfg.Hub = serverconfig.Hub{}
	cfg.Member = serverconfig.Member{
		Fleet:   fleetName,
		Subnet:  subnet,
		Private: res.Machine.Private,
		Hub: serverconfig.MemberHub{
			Name: hubName, Endpoint: endpoint, Address: hubAddr, PublicKey: t.PublicKey,
		},
	}
	return saveConfigKeepingOwner(dir, cfg)
}

func saveConfigKeepingOwner(dir string, cfg serverconfig.Config) error {
	path := serverconfig.Path(dir)
	uid, gid := -1, -1
	if info, err := os.Stat(path); err == nil {
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
		}
	}
	if err := serverconfig.Save(dir, cfg, 0o640); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if uid < 0 || gid < 0 {
		return nil
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("restore the owner of %s: %w", path, err)
	}
	return nil
}

func restartDaemon(ctx context.Context, user string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil
	}
	if strings.TrimSpace(user) == "" {
		user = serverconfig.DefaultUser
	}
	r := runner.Exec{}
	res, err := r.Run(ctx, runner.Cmd{
		Name: "systemctl", Args: []string{"--user", "restart", setup.UserUnit}, User: user,
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("systemctl --user restart %s as %s: exit %d: %s",
			setup.UserUnit, user, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func withDefaultPort(host, like string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	port := "4021"
	if _, p, err := net.SplitHostPort(like); err == nil && p != "" {
		port = p
	}
	return net.JoinHostPort(host, port)
}

func prefixOrZero(s string) netip.Prefix {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}
	}
	return p
}

func (a *app) tunnelLog() io.Writer {
	if a.json {
		return nil
	}
	return a.stderr
}

func (a *app) runMachineLeave(ctx context.Context, configDir string, force bool) error {
	cfg, err := serverconfig.Load(configDir)
	if err != nil {
		return fmt.Errorf("read %s: %w (run this on the machine that is leaving, as root)",
			serverconfig.Path(configDir), err)
	}
	if !cfg.IsMember() {
		return a.printer().Result(
			map[string]any{"left": false, "hub": ""},
			func(w io.Writer) error {
				_, err := fmt.Fprintln(w, "this machine is a member of no fleet; nothing to leave")
				return err
			})
	}
	hub := cfg.FleetName()
	if !force {

		fmt.Fprintf(a.stderr,
			"caramelo: leaving the fleet %s. Remove it there too (`caramelo member remove %s`), "+
				"or the hub will keep placing work here.\n", hub, cfg.Name)
	}
	cfg.Role = serverconfig.RoleHub
	cfg.Member = serverconfig.Member{}
	cfg.Hub = serverconfig.Hub{Fleet: cfg.Name}
	if err := saveConfigKeepingOwner(configDir, cfg); err != nil {
		return err
	}
	if err := restartDaemon(ctx, cfg.User); err != nil {
		fmt.Fprintf(a.stderr, "caramelo: left %s, but caramelod did not restart: %v\n"+
			"run `sudo systemctl restart caramelod` to finish\n", hub, err)
	}
	return a.printer().Result(
		map[string]any{"left": true, "hub": hub},
		func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "left the fleet %s; this machine is one of one again\n", hub)
			return err
		})
}
