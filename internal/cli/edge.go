package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/edge/certs"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup"

	"github.com/plytz/caramelo/internal/cli/ui"
)

func init() {
	register(func(a *app) *cobra.Command {
		var configDir string
		cmd := &cobra.Command{
			Use:   "edge",
			Short: "The machine's public edge: routes, certificates, targets",
			Long: `A machine with an edge answers ports 80 and 443 for the hostnames its
environments are exposed under: HTTP/1.1, HTTP/2 and HTTP/3, certificates
issued and renewed on their own, WebSockets passed through, and several
replicas of a service behind one name with the old ones drained rather than
killed when they are replaced.

Run without a subcommand, this is the edge process itself: systemd starts it
with the sockets root bound at boot, and it is not something to run by hand.
Use 'caramelo edge status' to ask a machine what its edge is doing.`,
			Args: exactArgs(0),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runEdgeProcess(cmd.Context(), a, configDir)
			},
		}
		cmd.Flags().StringVar(&configDir, "config-dir", serverconfig.DefaultConfigDir,
			"directory holding config.yaml")
		cmd.AddCommand(
			a.edgeStatusCmd(),
			a.edgeCountsCmd(),
			a.edgeEnableCmd(),
			a.edgeDisableCmd(),
			a.edgeCACmd(),
		)
		return cmd
	})
}

func runEdgeProcess(ctx context.Context, a *app, configDir string) error {
	cfg, err := serverconfig.Load(configDir)
	if err != nil {
		return fmt.Errorf("read the machine's configuration: %w", err)
	}
	if !cfg.Edge {
		return fmt.Errorf("%s says this machine has no edge: run 'caramelo edge enable' on it",
			serverconfig.Path(configDir))
	}
	mode, err := cfg.TLSMode()
	if err != nil {
		return fmt.Errorf("%s: %w", serverconfig.Path(configDir), err)
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()

	opts := edge.Options{
		StateDir:  cfg.StateDir,
		RunDir:    cfg.RunDir,
		HTTP3:     cfg.HTTP3,
		TLS:       mode,
		Directory: cfg.ACMEDirectory(),
		Email:     cfg.ACMEEmail,
		Version:   version,
		Log:       a.stderr,
	}
	applyEdgeTestKnobs(&opts, os.Getenv, a.stderr)
	if err := edge.Run(ctx, opts); err != nil {
		return fmt.Errorf("edge: %w", err)
	}
	return nil
}

const (
	envRenewalWindowRatio = "CARAMELO_EDGE_RENEWAL_WINDOW_RATIO"
	envRenewCheckInterval = "CARAMELO_EDGE_RENEW_CHECK_INTERVAL"
	envACMEProfile        = "CARAMELO_EDGE_ACME_PROFILE"
)

func applyEdgeTestKnobs(opts *edge.Options, getenv func(string) string, warn io.Writer) {
	note := func(format string, args ...any) {
		if warn != nil {
			fmt.Fprintf(warn, "caramelo: warning: "+format+"\n", args...)
		}
	}
	if v := strings.TrimSpace(getenv(envACMEProfile)); v != "" {
		opts.ACMEProfile = v
	}
	if v := strings.TrimSpace(getenv(envRenewalWindowRatio)); v != "" {
		ratio, err := strconv.ParseFloat(v, 64)
		switch {
		case err != nil:
			note("%s=%q is not a number; using the default renewal window", envRenewalWindowRatio, v)
		case ratio <= 0 || ratio > 1:
			note("%s=%v is outside (0,1]; using the default renewal window", envRenewalWindowRatio, ratio)
		default:
			opts.RenewalWindowRatio = ratio
		}
	}
	if v := strings.TrimSpace(getenv(envRenewCheckInterval)); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			note("%s=%q is not a duration; using the default check interval", envRenewCheckInterval, v)
		case d <= 0:
			note("%s=%s is not positive; using the default check interval", envRenewCheckInterval, d)
		default:
			opts.RenewCheckInterval = d
		}
	}
}

func (a *app) edgeStatusCmd() *cobra.Command {
	return clientCmd(&cobra.Command{
		Use:   "status",
		Short: "Show the machine's routes, targets and certificates",
		Long: `Reports what the edge is serving right now: every route with the environment
and service behind it, each target with its state (starting, active, draining,
stopped, and since M7 unhealthy for a replica the health loop took out and held
for an old pool a deploy is keeping) and how many requests it is holding, and
each certificate with the CA that issued it and when it expires.

On a machine whose edge is switched off this says so and exits 0.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := a.service.EdgeStatus(cmd.Context())
			if err != nil {
				return err
			}
			return a.printer().Result(st, func(w io.Writer) error {
				return writeEdgeStatus(w, st)
			})
		},
	})
}

func writeEdgeStatus(w io.Writer, st *edge.Status) error { return edgeStatusView(st).Write(w) }

func edgeStatusView(st *edge.Status) *ui.View {
	if st == nil {
		return ui.NewView().Text("no edge")
	}
	state := "running"
	if !st.Running {
		state = "not running"
	}
	head := "edge " + state
	if st.Version != "" {
		head += ", version " + st.Version
	}
	if !st.StartedAt.IsZero() {
		head += fmt.Sprintf(", up %s", time.Since(st.StartedAt).Round(time.Second))
	}
	v := ui.NewView().Text("%s", head)
	if st.Error != "" {
		v.Text("  %s", st.Error)
	}

	f := ui.NewFields("")
	if st.TLS != "" {
		ca := string(st.TLS)
		if st.ACMEDirectory != "" {
			ca += " (" + st.ACMEDirectory + ")"
		}
		f.Add("certificates", "%s", ca)
	}
	f.Add("http/3", "%v", st.HTTP3)
	if len(st.Listeners) > 0 {
		f.Add("listening", "%s", strings.Join(st.Listeners, ", "))
	}
	if line := describeIngress(st.Ingress); line != "" {
		f.Add("ingress", "%s", line)
	}
	v.Fields(f)

	if len(st.Routes) == 0 {
		v.Blank().Text("no routes: nothing on this machine is exposed")
	} else {
		v.Section(edgeRoutesTable(st.Routes))

		if line := describeTargetStates(st.Routes); line != "" {
			v.Blank().Text("%s", line)
		}
	}
	return v.Section(edgeCertificatesTable(st.Certificates))
}

func describeIngress(in *edge.IngressStatus) string {
	if in == nil || (!in.Enabled && in.Error == "") {
		return ""
	}
	if !in.Enabled {
		return "off: " + in.Error
	}
	s := strOr(in.Addr, "enabled")
	if in.Error != "" {
		return s + ", not listening: " + in.Error
	}
	s += fmt.Sprintf(", %d request(s) from the hub", in.Requests)
	if in.Inflight > 0 {
		s += fmt.Sprintf(", %d in flight", in.Inflight)
	}
	return s
}

func edgeRoutesTable(routes []edge.Route) *ui.Table {
	if len(routes) == 0 {
		return nil
	}
	via := routesHaveVia(routes)
	head := []string{"HOST", "ENV", "SERVICE", "REPLICA", "STATE", "PORT", "IN-FLIGHT"}
	if via {
		head = append(head, "VIA")
	}
	t := ui.NewTable(head...)
	row := func(r edge.Route, cells ...string) {
		if via {

			cells = append(cells, strOrDash(r.Via))
		}
		t.Row(cells...)
	}
	for _, r := range routes {
		where := r.Env
		if r.App != "" {
			where = r.App + "/" + r.Env
		}
		if len(r.Targets) == 0 {
			row(r, r.Host, strOrDash(where), strOrDash(r.Service), "-", "-", "-", "-")
			continue
		}
		for _, g := range r.Targets {

			replica := fmt.Sprint(g.Replica)
			if r.Kind == edge.KindVia {
				replica = "-"
			}
			row(r, r.Host, strOrDash(where), strOrDash(r.Service), replica,
				string(g.State), fmt.Sprint(g.Port), fmt.Sprint(g.Inflight))
		}
	}
	return t
}

func edgeCertificatesTable(list []certs.Certificate) *ui.Table {
	if len(list) == 0 {
		return nil
	}
	t := ui.NewTable("CERTIFICATE", "ISSUER", "EXPIRES", "MANAGED")
	now := time.Now()
	for _, c := range list {
		expires := "-"
		if !c.NotAfter.IsZero() {
			expires = fmt.Sprintf("%s (%s)", c.NotAfter.Local().Format(time.RFC3339),
				c.NotAfter.Sub(now).Round(time.Minute))
			if c.Expired(now) {
				expires = c.NotAfter.Local().Format(time.RFC3339) + " (expired)"
			}
		}
		t.Row(c.Host, strOrDash(c.Issuer), expires, fmt.Sprintf("%v", c.Managed))
	}
	return t
}

func (a *app) edgeEnableCmd() *cobra.Command {
	var (
		configDir string
		acmeEmail string
		acmeCA    string
		tls       string
		noHTTP3   bool
	)
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Turn the edge on for this machine (run as root)",
		Long: `Installs the socket and service units, creates the storage directories and
writes the edge keys into config.yaml — exactly what 'caramelo server setup
--edge' does, on a machine that was set up without it.

Ports 80, 443/tcp and 443/udp have to be reachable; Caramelo never touches a
firewall and says so instead.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadEdgeConfig(configDir)
			if err != nil {
				return err
			}
			cfg.Edge = true
			for name, apply := range map[string]func(){
				"acme-email": func() { cfg.ACMEEmail = acmeEmail },
				"acme-ca":    func() { cfg.ACMECA = acmeCA },
				"tls":        func() { cfg.TLS = tls },
				"no-http3":   func() { cfg.HTTP3 = !noHTTP3 },
			} {
				if cmd.Flags().Changed(name) {
					apply()
				}
			}
			if err := cfg.Validate(); err != nil {
				return &usageError{err}
			}
			return a.runEdgeSetup(cmd.Context(), cfg, configDir)
		},
	}
	f := cmd.Flags()
	f.StringVar(&configDir, "config-dir", serverconfig.DefaultConfigDir, "directory holding config.yaml")
	f.StringVar(&acmeEmail, "acme-email", "", "address to register with the certificate authority")
	f.StringVar(&acmeCA, "acme-ca", "", "ACME directory URL (default: Let's Encrypt production)")
	f.StringVar(&tls, "tls", "", "where certificates come from: "+tlsValuesHelp())
	f.BoolVar(&noHTTP3, "no-http3", false, "do not serve QUIC on UDP 443")
	return cmd
}

func (a *app) edgeDisableCmd() *cobra.Command {
	var configDir string
	cmd := &cobra.Command{
		Use:   "disable",
		Short: "Turn the edge off for this machine (run as root)",
		Long: `Stops and disables the edge's units, so nothing answers ports 80 and 443.
Certificates, the route table and every environment stay exactly as they are:
'caramelo edge enable' brings the same names back without a new certificate.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadEdgeConfig(configDir)
			if err != nil {
				return err
			}
			cfg.Edge = false
			return a.runEdgeSetup(cmd.Context(), cfg, configDir)
		},
	}
	cmd.Flags().StringVar(&configDir, "config-dir", serverconfig.DefaultConfigDir,
		"directory holding config.yaml")
	return cmd
}

func loadEdgeConfig(configDir string) (serverconfig.Config, error) {
	if !serverconfig.Exists(configDir) {
		return serverconfig.Config{}, fmt.Errorf(
			"%s is not a Caramelo machine (no %s): run 'caramelo server setup --edge' instead",
			configDir, serverconfig.ConfigFile)
	}
	cfg, err := serverconfig.Load(configDir)
	if err != nil {
		return serverconfig.Config{}, fmt.Errorf("read the machine's configuration: %w", err)
	}
	return cfg, nil
}

func (a *app) runEdgeSetup(ctx context.Context, cfg serverconfig.Config, configDir string) error {
	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate this binary: %w", err)
	}
	env := &setup.Env{
		Config:     cfg,
		ConfigDir:  configDir,
		Opts:       setup.Options{Yes: true, InstallPackages: false},
		Run:        runner.Exec{},
		Log:        a.stderr,
		Version:    version,
		BinaryPath: binary,
	}
	return a.runSetup(ctx, env, []setup.Step{
		setup.NewDirsStep(), setup.NewEdgeStep(), setup.NewCaramelodStep(),
	})
}

func (a *app) edgeCACmd() *cobra.Command {
	return clientCmd(&cobra.Command{
		Use:   "ca",
		Short: "Print the machine's internal CA root, to trust it",
		Long: `On a machine running 'tls: internal' — one whose hostnames have no public
DNS — the edge issues certificates from a CA it generated itself. This prints
that CA's root certificate, which is what a browser, a curl or a test client
has to trust:

  caramelo edge ca > caramelo-root.crt
  curl --cacert caramelo-root.crt https://feat-x.shop.internal/

A machine issuing from a public CA has nothing to trust by hand and says so.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			ca, err := a.service.EdgeCA(cmd.Context())
			if err != nil {
				return err
			}
			if ca == nil {
				return errors.New("this machine has no internal CA")
			}

			return a.printer().Result(ca, func(w io.Writer) error {
				return a.writeCA(w, ca)
			})
		},
	})
}

func caDescription(ca *certs.CA) string {
	parts := []string{strOrDash(ca.Subject)}
	if ca.Fingerprint != "" {
		parts = append(parts, "sha256:"+ca.Fingerprint)
	}
	if !ca.NotAfter.IsZero() {
		parts = append(parts, "expires "+ca.NotAfter.Local().Format(time.RFC3339))
	}
	return strings.Join(parts, ", ")
}

func tlsValuesHelp() string {
	out := ""
	for i, v := range serverconfig.TLSValues {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
}

func (a *app) writeCA(w io.Writer, ca *certs.CA) error {
	if ca == nil {
		return errors.New("this machine has no internal CA")
	}
	fmt.Fprintf(a.stderr, "%s\n", caDescription(ca))
	pem := ca.PEM
	if !strings.HasSuffix(pem, "\n") {
		pem += "\n"
	}
	return ui.NewView().Raw(pem).Write(w)
}

func describeTargetStates(routes []edge.Route) string {
	var held, unhealthy, draining int
	for _, r := range routes {
		for _, t := range r.Targets {
			switch t.State {
			case edge.TargetHeld:
				held++
			case edge.TargetUnhealthy:
				unhealthy++
			case edge.TargetDraining:
				draining++
			}
		}
	}
	var parts []string
	if held > 0 {
		parts = append(parts, fmt.Sprintf("%d replica(s) held by a deploy's watch "+
			"(caramelo rollback is one pushed table away)", held))
	}
	if draining > 0 {
		parts = append(parts, fmt.Sprintf("%d draining", draining))
	}
	if unhealthy > 0 {
		parts = append(parts, fmt.Sprintf("%d unhealthy, out of the pool until a probe passes", unhealthy))
	}
	return strings.Join(parts, "; ")
}

func (a *app) edgeCountsCmd() *cobra.Command {
	var since time.Duration
	cmd := clientCmd(&cobra.Command{
		Use:   "counts",
		Short: "Requests, errors and connection failures per hostname and replica",
		Long: `counts reports what the edge has served since --since ago (default: since it
started): per hostname and per replica, how many requests, how many answered 5xx
and how many connections to the replica could not be made at all.

These are the numbers a deploy's watch window decides on. A failure the edge
counts is a failure a client saw, which is why a connection that could not be
made counts as one: from the client's side there is no difference.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {

			var at time.Time
			if since > 0 {
				at = time.Now().Add(-since)
			}
			counts, err := a.service.EdgeCounts(cmd.Context(), at)
			if err != nil {
				return err
			}
			return a.printer().Result(counts, func(w io.Writer) error {
				return writeEdgeCounts(w, counts)
			})
		},
	})
	cmd.Flags().DurationVar(&since, "since", 0, "only what was served this recently (default: since the edge started)")
	return cmd
}

func writeEdgeCounts(w io.Writer, c *edge.Counts) error {
	if c == nil {
		_, err := fmt.Fprintln(w, "no counts")
		return err
	}
	window := "since the edge started"
	if !c.Since.IsZero() {
		window = fmt.Sprintf("since %s", c.Since.Local().Format(time.RFC3339))
		if !c.At.IsZero() {
			window += fmt.Sprintf(" (%s)", c.At.Sub(c.Since).Round(time.Second))
		}
	}
	if _, err := fmt.Fprintf(w, "edge counts %s\n", window); err != nil {
		return err
	}
	if len(c.Hosts) == 0 {
		_, err := fmt.Fprintln(w, "\nnothing has been served: no route has had a request")
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tREPLICA\tREQUESTS\t5XX\tREFUSED\tERROR RATE")
	for _, h := range c.Hosts {
		for _, t := range h.Targets {
			fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\n",
				h.Host, t.Replica, t.Requests, t.Status5xx, t.ConnectFailures, percent(t.Rate()))
		}
		fmt.Fprintf(tw, "%s\ttotal\t%d\t%d\t%d\t%s\n",
			h.Host, h.Requests, h.Status5xx, h.ConnectFailures, percent(h.Rate()))
	}
	return tw.Flush()
}

func percent(rate float64) string {
	if rate <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", rate*100)
}
