package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup"
	dockersetup "github.com/plytz/caramelo/internal/setup/docker"
)

func init() {
	registerServer(func(a *app) *cobra.Command { return a.serverSetupCmd() })
}

func (a *app) serverSetupCmd() *cobra.Command {
	var (
		configDir     string
		opts          setup.Options
		dryRun        bool
		noPkgs        bool
		target        string
		targetBinary  string
		targetRelease string
		name          string
		peer          string
		noHTTP3       bool
		swap          string
	)
	cfg := serverconfig.Default()

	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Turn this machine into a Caramelo host (run as root)",
		Long: `Set up this machine: create the caramelo user, the directory layout and
config.yaml, install rootless Docker, and run caramelod as a systemd user unit
listening for the SSH API.

Every step checks the machine before it changes it, so running setup again on a
finished box reports no changes. --dry-run reports what would change and does
nothing.

With --target the same setup runs on another machine, from here: the binary is
shipped over ssh, setup runs there as root (root login or passwordless sudo),
the machine is recorded in the commander config and the API is checked from here.`,

		Args: rangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkBinarySource(cmd.Flags(), target, targetBinary, targetRelease); err != nil {
				return err
			}
			opts.InstallPackages = !noPkgs

			cfg.HTTP3 = !noHTTP3
			if cmd.Flags().Changed("swap") {
				parsed, err := parseSwapFlag(swap)
				if err != nil {
					return &usageError{err}
				}
				cfg.Swap = parsed
			}
			if len(args) == 1 {
				if peer == "" || strings.ContainsAny(peer, " \t=,") {
					return &usageError{fmt.Errorf(
						"unexpected argument %q; the only one this command takes is the key of --peer NAME KEY", args[0])}
				}
				peer += " " + args[0]
			}

			spec, err := setup.ParsePeer(peer)
			if err != nil {
				return &usageError{err}
			}
			opts.Peer = spec
			if !opts.Join.Empty() {

				t, err := ticketFrom(cmd.Context(), opts.Join.Token)
				if err != nil {
					return err
				}
				opts.Join.Token = t
			}
			if cfg.Fleet.Private {

				cfg.Edge = true
			}
			if cfg.Fleet.Private && cmd.Flags().Changed("edge") && opts.Join.Empty() {

				return &usageError{errors.New(
					"--edge and --private ask for opposite things: an edge on 80 and 443 is exactly " +
						"the public listener --private says this machine will not have")}
			}
			if target != "" {
				return a.runBootstrap(cmd, bootstrapFlags{
					target: target, name: name, cfg: cfg, configDir: configDir,
					authorizedKeys: opts.AuthorizedKeysFile, yes: opts.Yes, dryRun: dryRun,
					peer: spec, binary: targetBinary, release: targetRelease,
				})
			}
			resolved, err := resolveSetupConfig(cmd, configDir, cfg)
			if err != nil {
				return err
			}
			if resolved.Fleet.Private {
				resolved.Edge = true
			}
			binary, err := os.Executable()
			if err != nil {
				return fmt.Errorf("locate this binary: %w", err)
			}
			env := &setup.Env{
				Config:     resolved,
				ConfigDir:  configDir,
				Opts:       opts,
				Run:        runner.Exec{},
				Log:        a.stderr,
				DryRun:     dryRun,
				Version:    version,
				BinaryPath: binary,
			}
			if !opts.Yes && !dryRun {
				proceed, err := a.confirm(cmd.Context(), setupPlan(env))
				if err != nil {
					return err
				}
				if !proceed {
					return errors.New("cancelled")
				}
			}
			return a.runSetup(cmd.Context(), env, setupSteps())
		},
	}

	f := cmd.Flags()
	f.StringVar(&configDir, "config-dir", serverconfig.DefaultConfigDir, "directory for config.yaml")
	f.StringVar(&cfg.StateDir, "state-dir", cfg.StateDir, "directory for the daemon's state and the user's home")
	f.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "directory for apps and Docker images (the big one)")
	f.StringVar(&cfg.User, "user", cfg.User, "system user everything runs as")
	f.StringVar(&cfg.Group, "group", cfg.Group, "group of that user")
	f.IntVar(&cfg.SSHPort, "ssh-port", cfg.SSHPort, "TCP port for the SSH API")
	f.StringVar(&cfg.Bind, "bind", cfg.Bind, "address the SSH API listens on")
	f.StringVar(&cfg.VPNSubnet, "vpn-subnet", cfg.VPNSubnet, "private range this machine's network hands out addresses from")
	f.StringVar(&cfg.VPNListen, "vpn-listen", cfg.VPNListen, "UDP endpoint of the WireGuard device (the one port that must be reachable)")
	f.StringVar(&cfg.APIListen, "api-listen", cfg.APIListen,
		"where the SSH API answers: "+strings.Join(serverconfig.APIListenValues, ", "))
	f.StringVar(&peer, "peer", "", "admit an identity on the machine's network: 'NAME PUBLIC_KEY'")

	f.BoolVar(&cfg.Edge, "edge", cfg.Edge, "serve public traffic from this machine: ports 80 and 443, certificates, routes")
	f.StringVar(&cfg.ACMEEmail, "acme-email", cfg.ACMEEmail, "address to register with the certificate authority")
	f.StringVar(&cfg.ACMECA, "acme-ca", cfg.ACMECA, "ACME directory URL (default: Let's Encrypt production)")
	f.StringVar(&cfg.TLS, "tls", cfg.TLS, "where certificates come from: "+strings.Join(serverconfig.TLSValues, ", "))
	f.BoolVar(&noHTTP3, "no-http3", false, "do not serve QUIC on UDP 443 (for a firewall that drops UDP)")
	f.StringVar(&opts.AuthorizedKeysFile, "authorized-keys", "", "file of public keys to authorize (default: the invoking user's)")
	f.BoolVar(&opts.Yes, "yes", false, "do not ask for confirmation")
	f.BoolVar(&opts.Force, "force", false, "continue even when the machine is unsupported or too small")
	f.BoolVar(&opts.LowPorts, "low-ports", false, "let containers publish ports below 1024")
	f.StringVar(&swap, "swap", "", "swap this machine gets: a size such as 4G, or off (default 4G)")
	f.BoolVar(&opts.OpenPorts, "open-ports", false,
		"open the ports this machine needs in its own firewall (off by default: caramelo only looks)")
	f.BoolVar(&dryRun, "dry-run", false, "report what would change, change nothing")
	f.BoolVar(&noPkgs, "no-packages", false, "assume Docker is already installed")
	f.StringVar(&target, "target", "", "set up another machine from here: [user@]host[:port] for ssh (default user: yours, port 22)")
	f.StringVar(&name, "name", "", "name to record the --target machine under in the commander config (default: its host)")
	f.StringVar(&targetBinary, "binary", "", "the caramelo binary to ship to --target (default: this one; caramelo-<os>-<arch> beside it; or, when this binary is itself a release, that release downloaded for the target)")
	f.StringVar(&targetRelease, "release", "",
		"ship this release of caramelo to the target instead of this binary: a tag such as v0.0.1, downloaded from GitHub for the target's platform")

	f.StringVar(&opts.Join.Token, "join-token", "", "join a fleet in this run: the ticket from `caramelo machine token` on the hub, or - to read it from standard input")
	f.StringVar(&opts.Join.Name, "join-name", "", "what to call this machine in the fleet (default: its hostname)")
	f.BoolVar(&cfg.Fleet.Private, "private", false,
		"a member with no public listener at all: 80 and 443 on loopback, everything served through the hub")
	return cmd
}

func parseSwapFlag(v string) (serverconfig.Swap, error) {
	s := serverconfig.Swap{Swappiness: serverconfig.DefaultSwappiness}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case serverconfig.SwapOff, "none", "0":
		s.Backend = serverconfig.SwapOff
		return s, nil
	case serverconfig.SwapZram:
		s.Backend = serverconfig.SwapZram
		return s, nil
	}
	size, err := config.ParseMemory(v)
	if err != nil {
		return serverconfig.Swap{}, fmt.Errorf("--swap %q: %w", v, err)
	}
	s.Backend = serverconfig.SwapFile
	s.SizeBytes = size
	return s, nil
}

func resolveSetupConfig(cmd *cobra.Command, configDir string, flags serverconfig.Config) (serverconfig.Config, error) {
	cfg := serverconfig.Default()
	if serverconfig.Exists(configDir) {
		existing, err := serverconfig.Load(configDir)
		if err != nil {
			return cfg, fmt.Errorf("read the existing configuration: %w", err)
		}
		cfg = existing
	}
	for name, apply := range map[string]func(){
		"state-dir": func() { cfg.StateDir = flags.StateDir },
		"data-dir":  func() { cfg.DataDir = flags.DataDir },
		"user":      func() { cfg.User = flags.User },
		"group":     func() { cfg.Group = flags.Group },
		"ssh-port":  func() { cfg.SSHPort = flags.SSHPort },
		"bind":      func() { cfg.Bind = flags.Bind },

		"vpn-subnet": func() { cfg.VPNSubnet = flags.VPNSubnet },
		"vpn-listen": func() { cfg.VPNListen = flags.VPNListen },
		"api-listen": func() { cfg.APIListen = flags.APIListen },

		"edge":       func() { cfg.Edge = flags.Edge },
		"acme-email": func() { cfg.ACMEEmail = flags.ACMEEmail },
		"acme-ca":    func() { cfg.ACMECA = flags.ACMECA },
		"tls":        func() { cfg.TLS = flags.TLS },
		"no-http3":   func() { cfg.HTTP3 = flags.HTTP3 },

		"swap": func() { cfg.Swap = flags.Swap },

		"private": func() { cfg.Fleet.Private = flags.Fleet.Private },
	} {
		if cmd.Flags().Changed(name) {
			apply()
		}
	}
	if err := cfg.Validate(); err != nil {
		return cfg, &usageError{err}
	}
	return cfg, nil
}

func (a *app) runSetup(ctx context.Context, env *setup.Env, steps []setup.Step) error {
	report := setup.Execute(ctx, steps, env, nil)
	if err := a.printer().Result(report, func(w io.Writer) error {
		return writeSetupReport(w, report)
	}); err != nil {
		return err
	}
	if report.Failed > 0 {

		return &exitError{ExitError}
	}
	return nil
}

func setupSteps() []setup.Step {
	before, after := setup.HostSteps()
	steps := append([]setup.Step{}, before...)
	steps = append(steps, dockersetup.Packages(), dockersetup.Rootless())
	return append(steps, after...)
}

func writeSetupReport(w io.Writer, r setup.Report) error {
	var total time.Duration
	for _, res := range r.Results {
		total += res.Duration
	}
	verb := "changed"
	if r.DryRun {
		verb = "would change"
	}
	if _, err := fmt.Fprintf(w, "%d steps, %d %s, %d failed in %s\n",
		len(r.Results), countStatus(r, verb == "changed"), verb, r.Failed, total.Round(time.Millisecond)); err != nil {
		return err
	}
	if r.Failed == 0 && !r.DryRun {
		if summary := lastDetail(r, "summary"); summary != "" {
			_, err := fmt.Fprintf(w, "try: %s status\n", summary)
			return err
		}
	}
	return nil
}

func countStatus(r setup.Report, changed bool) int {
	want := setup.StatusWouldChange
	if changed {
		want = setup.StatusChanged
	}
	n := 0
	for _, res := range r.Results {
		if res.Status == want {
			n++
		}
	}
	return n
}

func lastDetail(r setup.Report, step string) string {
	for _, res := range r.Results {
		if res.Step == step {
			return res.Detail
		}
	}
	return ""
}

var errNeedsYes = errors.New("setup changes this machine: re-run with --yes to confirm, or --dry-run to see what it would do")

func setupPlan(env *setup.Env) string {
	cfg := env.Config
	keys := env.Opts.AuthorizedKeysFile
	if keys == "" {
		if u := os.Getenv("SUDO_USER"); u != "" {
			keys = "the SSH keys of " + u
		} else {
			keys = "none (no SUDO_USER; use --authorized-keys)"
		}
	}
	packages := "docker-ce and friends from Docker's apt repository"
	if !env.Opts.InstallPackages {
		packages = "none (--no-packages)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "caramelo server setup will change this machine:\n")
	fmt.Fprintf(&b, "  config    %s\n", env.ConfigDir)
	fmt.Fprintf(&b, "  state     %s (home of the %s user)\n", cfg.StateDir, cfg.User)
	fmt.Fprintf(&b, "  data      %s (apps and Docker images)\n", cfg.DataDir)
	fmt.Fprintf(&b, "  swap      %s\n", swapPlanLine(cfg))
	fmt.Fprintf(&b, "  user      %s:%s, system user with rootless Docker\n", cfg.User, cfg.Group)
	fmt.Fprintf(&b, "  API       ssh on %s:%d (%s), socket %s\n", cfg.Bind, cfg.SSHPort, cfg.APIListen, cfg.SocketPath())
	fmt.Fprintf(&b, "  network   %s, wireguard on %s (udp)\n", cfg.VPNSubnet, cfg.VPNListen)
	if !env.Opts.Peer.Empty() {
		fmt.Fprintf(&b, "  peer      %s admitted on the network\n", env.Opts.Peer.Name)
	}
	if env.Opts.OpenPorts {
		fmt.Fprintf(&b, "  firewall  opens the ports this machine needs in ufw or firewalld\n")
	}
	fmt.Fprintf(&b, "  keys      %s\n", keys)
	fmt.Fprintf(&b, "  packages  %s\n", packages)
	return b.String()
}

func swapPlanLine(cfg serverconfig.Config) string {
	if cfg.Swap.Backend != serverconfig.SwapFile {
		return "none (swap: " + cfg.Swap.Backend + ")"
	}
	return fmt.Sprintf("%s at %s, vm.swappiness %d",
		fmtBytesIEC(cfg.Swap.SizeBytes), cfg.SwapFilePath(), cfg.Swap.Swappiness)
}

func isTerminal(r io.Reader) bool {
	f, isFile := r.(*os.File)
	if !isFile {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
