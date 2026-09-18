package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/vpn"
	"github.com/plytz/caramelo/internal/vpnclient"

	"github.com/plytz/caramelo/internal/cli/ui"
)

func init() {

	vpnclient.InstallTunnelDialer(vpnclient.TunnelOptions{Records: pinnedRecords{}})

	register(func(a *app) *cobra.Command {
		cmd := &cobra.Command{
			Use:   "vpn",
			Short: "Join a machine's private network",
			Long: `Every Caramelo machine is a private network: the box, each of its
environments and each authorized peer has an address on it, and nothing else on
the internet gets an answer.

By default nothing is installed and nothing needs root: the CLI carries the
tunnel in process, and 'caramelo connect' lends it to other programs as local
ports. 'caramelo vpn install' is the one-time sudo that turns on transparent
mode, where names like db.feat-x.shop.internal work in every program.`,
		}
		asGroup(cmd)
		cmd.AddCommand(
			a.vpnUpCmd(),
			a.vpnDownCmd(),
			a.vpnStatusCmd(),
			a.vpnInstallCmd(),
			a.vpnUninstallCmd(),
			a.vpnConfigCmd(),
			a.vpnServiceCmd(),
		)
		return cmd
	})
}

func (a *app) vpnClient() (vpnclient.Client, error) {
	return vpnclient.NewWith(vpnclient.Options{
		Control: fleetControl{a: a},
		Records: pinnedRecords{},
		Log:     a.stderr,
	})
}

type fleetControl struct{ a *app }

var _ vpnclient.Control = fleetControl{}

func (c fleetControl) Run(ctx context.Context, fleet string, argv []string, stdout, stderr io.Writer) (int, error) {
	sub := &app{stdout: stdout, stderr: stderr, args: argv, fleet: fleet}
	if m := strings.TrimSpace(c.a.machine); m != "" && m != remote.MachineLocal {
		sub.fleet, sub.machine = "", m
	}
	return forward(ctx, sub)
}

type pinnedRecords struct{ store vpnclient.FileRecordStore }

var _ vpnclient.RecordStore = pinnedRecords{}

func (p pinnedRecords) Path(fleet string) string { return p.store.Path(fleet) }

func (p pinnedRecords) List() ([]vpnclient.Record, error) { return p.store.List() }

func (p pinnedRecords) Remove(fleet string) error { return p.store.Remove(fleet) }

func (p pinnedRecords) Load(fleet string) (vpnclient.Record, error) {
	rec, err := p.store.Load(fleet)
	if err != nil {
		return rec, err
	}
	cfg, err := remote.LoadCommanderConfig()
	if err != nil {
		return vpnclient.Record{}, err
	}
	if !isKnownFleet(cfg, rec.Fleet) {
		return rec, nil
	}
	if _, err := cfg.Pin(rec.Fleet, rec.MachineKey); err != nil {
		return vpnclient.Record{}, err
	}
	return rec, nil
}

func (p pinnedRecords) Save(rec vpnclient.Record) error {
	path, err := remote.CommanderConfigPath()
	if err != nil {
		return err
	}
	cfg, err := remote.LoadCommanderConfigFrom(path)
	if err != nil {
		return err
	}
	if isKnownFleet(cfg, rec.Fleet) {
		pinned, err := cfg.Pin(rec.Fleet, rec.MachineKey)
		if err != nil {
			return err
		}
		if pinned {
			if err := remote.SaveCommanderConfigTo(path, cfg); err != nil {
				return err
			}
		}
	}
	return p.store.Save(rec)
}

func isKnownFleet(cfg remote.CommanderConfig, name string) bool {
	_, ok := cfg.Commander.Fleets[name]
	return ok
}

func commanderPeerName(flag string) string {
	if n := strings.TrimSpace(flag); n != "" {
		return n
	}
	cfg, err := remote.LoadCommanderConfig()
	if err != nil {
		return ""
	}
	return cfg.MachineName()
}

func (a *app) vpnFleet() (string, error) {
	machine := strings.TrimSpace(a.machine)
	fleet := strings.TrimSpace(a.fleet)
	if machine != "" && fleet != "" {
		return "", &usageError{fmt.Errorf(
			"--machine %s and --fleet %s ask for two different things: --fleet names a fleet of the "+
				"commander config and --machine a raw ssh target that is in no fleet yet", machine, fleet)}
	}
	if machine != "" && machine != remote.MachineLocal {
		return machine, nil
	}
	if fleet != "" {
		return fleet, nil
	}
	cfg, err := remote.LoadCommanderConfig()
	if err != nil {
		return "", err
	}
	if d := strings.TrimSpace(cfg.Commander.DefaultFleet); d != "" {
		return d, nil
	}
	if names := cfg.FleetNames(); len(names) == 1 {
		return names[0], nil
	}
	path, _ := remote.CommanderConfigPath()
	return "", &usageError{fmt.Errorf(
		"no fleet to join: this commander knows %s in %s; pass --fleet NAME (or CARAMELO_FLEET), "+
			"make one the default with 'caramelo fleet default NAME', or point at a box that is in "+
			"no fleet yet with --machine <user@host>", cfg.FleetList(), path)}
}

func (a *app) vpnUpCmd() *cobra.Command {
	var peerName string
	var transparent bool
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Join the machine's network from the commander",
		Long: `Generates a key for the commander if it has none, registers it with the
machine as a peer, and verifies a session through the tunnel. Idempotent: a
machine already joined is simply re-verified.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			machine, err := a.vpnFleet()
			if err != nil {
				return err
			}
			client, err := a.vpnClient()
			if err != nil {
				return err
			}
			st, err := client.Up(cmd.Context(), vpnclient.UpRequest{
				Machine:     machine,
				PeerName:    commanderPeerName(peerName),
				Transparent: transparent,
			})
			if err != nil {
				return err
			}
			return a.printer().Result(st, func(w io.Writer) error {
				fmt.Fprintf(w, "joined %s as %s (%s)\n", st.Machine, st.PeerName, st.IP)
				if err := renderVPNState(w, st); err != nil {
					return err
				}

				_, err := fmt.Fprintf(w, "for a git push by hand: GIT_SSH_COMMAND=%q %s\n",
					gitSSHCommandTunnel(thisBinary()), gitSSHVariant)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&peerName, "name", "",
		"identity to register the commander under (default: the commander's own name)")
	cmd.Flags().BoolVar(&transparent, "transparent", false,
		"bring the installed background service's interface up instead of using the in-process tunnel")
	return available(cmd, onCommander)
}

func (a *app) vpnDownCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Leave the machine's network (transparent mode only)",
		Long: `Stops the tunnel interface owned by the background service. In the
default userspace mode there is nothing to tear down and this succeeds.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			machine, err := a.vpnFleet()
			if err != nil {
				return err
			}
			client, err := a.vpnClient()
			if err != nil {
				return err
			}
			if err := client.Down(cmd.Context(), machine); err != nil {
				return err
			}
			st, err := client.Status(cmd.Context(), machine)
			if err != nil {
				return err
			}
			return a.printer().Result(st, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "left the network of %s\n", machine)
				return err
			})
		},
	}
	return available(cmd, onCommander)
}

func (a *app) vpnStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the mode, address, last handshake and resolver",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			machine, err := a.vpnFleet()
			if err != nil {
				return err
			}
			client, err := a.vpnClient()
			if err != nil {
				return err
			}
			st, err := client.Status(cmd.Context(), machine)
			if err != nil {
				return err
			}
			return a.printer().Result(st, func(w io.Writer) error {
				if st.Mode == vpnclient.ModeOff {
					_, err := fmt.Fprintf(w,
						"not joined to %s; run 'caramelo vpn up' to join it\n", machine)
					return err
				}
				fmt.Fprintf(w, "%s: %s as %s (%s)\n", st.Machine, st.Mode, st.PeerName, st.IP)
				return renderVPNState(w, st)
			})
		},
	}
	return available(cmd, onCommander)
}

func renderVPNState(w io.Writer, st *vpnclient.State) error {

	return vpnStateView(st).Write(w)
}

func vpnStateView(st *vpnclient.State) *ui.View {
	f := ui.NewFields("")
	f.Add("mode", "%s", st.Mode)
	f.Add("address", "%s", strOrDash(addrString(st)))
	f.Add("subnet", "%s", strOrDash(prefixString(st)))
	f.Add("endpoint", "%s", strOrDash(st.Endpoint))
	f.Add("resolver", "%s", strOrDash(st.Resolver))
	f.Add("handshake", "%s", handshakeAge(st.LastHandshake))
	f.Add("transparent", "%s", installedWord(st.Installed))
	v := ui.NewView().Fields(f)
	if st.Mode == vpnclient.ModeUserspace {
		v.Text("names under .%s resolve inside this process only; "+
			"'caramelo connect ENV' opens local ports for other programs", vpn.Domain)
	}
	return v
}

func addrString(st *vpnclient.State) string {
	if !st.IP.IsValid() {
		return ""
	}
	return st.IP.String()
}

func prefixString(st *vpnclient.State) string {
	if !st.Subnet.IsValid() {
		return ""
	}
	return st.Subnet.String()
}

func installedWord(installed bool) string {
	if installed {
		return "installed"
	}
	return "not installed ('sudo caramelo vpn install' turns it on)"
}

func handshakeAge(at time.Time) string {
	if at.IsZero() {
		return "never"
	}
	d := time.Since(at).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%s ago", d)
}

func (a *app) vpnInstallCmd() *cobra.Command {
	var iface string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install transparent mode: a background service and split DNS (needs root)",
		Long: `Installs this same binary as a small background service that owns a real
tunnel interface, and points .internal at the machine's resolver. This is the
only command that ever needs root on the commander, and it needs it once.
Everything works without it; transparent mode is what makes names and addresses
work in browsers, psql and everything else.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			machine, err := a.vpnFleet()
			if err != nil {
				return err
			}
			installer := vpnclient.NewInstaller()
			opts := vpnclient.InstallOptions{Machine: machine, Interface: iface}
			if err := installer.Install(cmd.Context(), opts); err != nil {
				return installError(err)
			}
			out := struct {
				Machine   string `json:"machine"`
				Interface string `json:"interface"`
				Installed bool   `json:"installed"`
			}{machine, strOr(iface, vpnclient.DefaultInterface), true}
			return a.printer().Result(out, func(w io.Writer) error {
				_, err := fmt.Fprintf(w,
					"transparent mode installed for %s on %s; "+
						"names under .%s now work in every program\n",
					machine, out.Interface, vpn.Domain)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&iface, "interface", "", "tunnel interface name (default: caramelo0)")
	return available(cmd, onCommander)
}

func installError(err error) error {
	if errors.Is(err, vpnclient.ErrUnsupported) {
		return fmt.Errorf("%w; userspace mode works here and needs no installation", err)
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("%w (this command needs root: try 'sudo caramelo vpn install')", err)
	}
	return err
}

func (a *app) vpnUninstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove transparent mode's service and DNS entry (needs root)",
		Long: `Stops and removes the background service and the split-DNS entry. Keys and
commander configuration are left alone, so the default userspace mode keeps
working.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			installer := vpnclient.NewInstaller()
			if err := installer.Uninstall(cmd.Context()); err != nil {
				return installError(err)
			}
			out := struct {
				Installed bool `json:"installed"`
			}{false}
			return a.printer().Result(out, func(w io.Writer) error {
				_, err := fmt.Fprintln(w, "transparent mode removed; userspace mode still works")
				return err
			})
		},
	}
	return available(cmd, onCommander)
}

func (a *app) vpnConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Print a wg-quick configuration for a third-party WireGuard client",
		Long: `Renders a standard wg-quick file for the commander's peer. Nothing in
Caramelo needs it — it is the escape hatch for anyone who would rather use the
WireGuard client they already have. It contains the commander's private key.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			machine, err := a.vpnFleet()
			if err != nil {
				return err
			}
			client, err := a.vpnClient()
			if err != nil {
				return err
			}
			conf, err := client.Config(cmd.Context(), machine)
			if err != nil {
				return err
			}
			out := struct {
				Machine string `json:"machine"`
				Config  string `json:"config"`
			}{machine, conf}
			return a.printer().Result(out, func(w io.Writer) error {
				_, err := io.WriteString(w, conf)
				return err
			})
		},
	}
	return available(cmd, onCommander)
}

func (a *app) vpnServiceCmd() *cobra.Command {
	var iface string
	cmd := &cobra.Command{
		Use:    "service",
		Short:  "Run the transparent-mode tunnel (used by the installed service)",
		Hidden: true,
		Long: `Owns the tunnel interface for one machine and answers the local control
socket. It is started by the unit 'caramelo vpn install' wrote; running it by
hand is only useful for debugging transparent mode.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			machine, err := a.vpnFleet()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return vpnclient.RunService(ctx, vpnclient.ServiceOptions{
				Machine:   machine,
				Interface: iface,
				Log:       a.stderr,
			})
		},
	}
	cmd.Flags().StringVar(&iface, "interface", "", "tunnel interface name (default: caramelo0)")
	return available(cmd, onCommander)
}
