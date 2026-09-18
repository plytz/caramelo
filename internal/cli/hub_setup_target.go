package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/plytz/caramelo/internal/bootstrap"
	"github.com/plytz/caramelo/internal/edge/certs"
	"github.com/plytz/caramelo/internal/firewall"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup"
	"github.com/plytz/caramelo/internal/vpn"
	"github.com/plytz/caramelo/internal/vpnclient"
)

const bootstrapSSHPort = 22

var (
	newBootstrapShell = func(t remote.Target) (bootstrap.Shell, error) {
		bin, extra, err := remote.SSHClient()
		if err != nil {
			return nil, err
		}
		return bootstrap.OpenSSH{Target: t, Bin: bin, Extra: extra}, nil
	}
	verifyMachine = verifyOverSSH

	bootstrapRun = bootstrap.Run
)

func checkBinarySource(flags *pflag.FlagSet, target, binary, release string) error {
	for _, fl := range []struct{ name, value, wants string }{
		{"binary", binary, "a path to a caramelo binary"},
		{"release", release, "a tag such as v0.0.1"},
	} {
		if flags.Changed(fl.name) && fl.value == "" {
			return &usageError{fmt.Errorf(
				"--%s was given with no value: it wants %s", fl.name, fl.wants)}
		}
	}
	if binary != "" && release != "" {
		return &usageError{errors.New("--binary and --release name two different binaries; give one")}
	}
	if release != "" && !bootstrap.IsReleaseTag(release) {
		return &usageError{fmt.Errorf(
			"--release wants a tag of the form v<major>.<minor>.<patch>, such as v0.0.1: got %q", release)}
	}
	if target == "" && commandTakesATargetFlag(flags) {
		for _, name := range []string{"release", "binary"} {
			if flags.Changed(name) {
				return &usageError{fmt.Errorf(
					"--%s only makes sense with --target: a machine sets itself up with the binary that is running", name)}
			}
		}
	}
	return nil
}

func commandTakesATargetFlag(flags *pflag.FlagSet) bool {
	return flags.Lookup("target") != nil
}

type bootstrapFlags struct {
	target         string
	name           string
	cfg            serverconfig.Config
	configDir      string
	authorizedKeys string

	binary  string
	release string
	yes     bool
	dryRun  bool

	peer setup.PeerSpec

	args []string

	member bool

	report *bootstrapResult
}

var forwardedFlags = []string{
	"config-dir", "state-dir", "data-dir", "user", "group", "ssh-port", "bind",
	"vpn-subnet", "vpn-listen", "api-listen",
	"edge", "acme-email", "acme-ca", "tls", "no-http3", "swap",
	"name", "fleet", "join-token", "join-name", "private",
	"force", "low-ports", "open-ports", "dry-run", "no-packages",
}

type bootstrapResult struct {
	Target string          `json:"target"`
	Probe  bootstrap.Probe `json:"probe"`
	Setup  setup.Report    `json:"setup"`

	Fleet *fleetEntry `json:"fleet,omitempty"`

	Peer *setup.PeerSpec `json:"peer,omitempty"`

	Verified  bool            `json:"verified"`
	Transport string          `json:"transport,omitempty"`
	Status    json.RawMessage `json:"status,omitempty"`

	Reachability *vpnclient.Probe `json:"reachability,omitempty"`
}

type fleetEntry struct {
	Name    string `json:"name"`
	Hub     string `json:"hub"`
	Default bool   `json:"default"`
	Config  string `json:"config"`
}

func (a *app) runBootstrap(cmd *cobra.Command, f bootstrapFlags) error {
	ctx := cmd.Context()
	target, err := remote.ParseTargetWith(f.target, localUser(), bootstrapSSHPort)
	if err != nil {
		return &usageError{fmt.Errorf("--target: %w", err)}
	}
	if err := f.cfg.Validate(); err != nil {
		return &usageError{err}
	}
	if f.authorizedKeys != "" {
		if _, err := os.Stat(f.authorizedKeys); err != nil {
			return &usageError{fmt.Errorf("--authorized-keys names a local file: %w", err)}
		}
	}
	if f.binary != "" {
		if _, err := os.Stat(f.binary); err != nil {
			return &usageError{fmt.Errorf("--binary names a local file: %w", err)}
		}
	}
	name := f.name
	if name == "" {
		name = target.Host
	}
	fleetName := strOr(strings.TrimSpace(f.cfg.Hub.Fleet), defaultMachineName(name))
	address := remote.Target{User: f.cfg.User, Host: target.Host, Port: f.cfg.SSHPort}

	if !f.yes && !f.dryRun {
		proceed, err := a.confirm(ctx, bootstrapPlan(target, f))
		if err != nil {
			return err
		}
		if !proceed {
			return errors.New("cancelled")
		}
	}

	peer, err := a.bootstrapPeer(f, fleetName)
	if err != nil {
		return err
	}
	if f.member {
		peer = setup.PeerSpec{}
	}
	if !peer.Empty() {
		fmt.Fprintf(a.stderr, "[bootstrap] joining as peer %s\n", peer.Name)
	}

	shell, err := newBootstrapShell(target)
	if err != nil {
		return err
	}
	args := f.args
	if args == nil {
		args = setupArgs(cmd.Flags())
	}

	args, joinToken := liftJoinToken(args)
	out, err := bootstrapRun(ctx, shell, bootstrap.Options{
		Binary:         f.binary,
		Release:        f.release,
		Version:        version,
		AuthorizedKeys: f.authorizedKeys,
		SetupArgs:      args,
		JoinToken:      joinToken,
		Peer:           peer,
		Log:            a.stderr,
		RemoteStderr:   a.stderr,
	})
	if err != nil {
		return fmt.Errorf("bootstrap %s: %w", target, err)
	}

	res := bootstrapResult{Target: target.String(), Probe: out.Probe, Setup: out.Report}

	answer := func(r bootstrapResult, verified bool) error {
		if f.report != nil {
			*f.report = r
			return nil
		}
		return a.printBootstrap(r, fleetName, verified)
	}
	if !peer.Empty() {
		res.Peer = &peer
	}
	if out.Report.Failed > 0 || out.ExitCode != 0 {

		_ = answer(res, false)
		return &exitError{ExitError}
	}
	if f.dryRun {
		return answer(res, false)
	}

	if f.member {

		return answer(res, true)
	}
	entry, err := recordFleet(fleetName, address)
	if err != nil {
		_ = answer(res, false)
		return fmt.Errorf("setup finished on %s, but its fleet could not be recorded: %w", target, err)
	}
	res.Fleet = entry

	endpoint := net.JoinHostPort(target.Host, strconv.Itoa(firewall.VPNPort(f.cfg.VPNListen)))
	joined, joinErr := a.join(ctx, fleetName, peer, f, shellControl{shell: shell, sudo: out.Probe.Privilege != bootstrap.PrivilegeRoot})
	if joinErr != nil {
		fmt.Fprintf(a.stderr, "[bootstrap] warning: could not join the machine's network: %v\n", joinErr)
	}

	status, err := verifyMachine(ctx, a, fleetName)
	if err != nil {
		res.Reachability = reachability(fleetName, endpoint, joined, joinErr, res.Transport)
		_ = answer(res, false)
		return fmt.Errorf("setup finished on %s and its fleet is recorded as %q, but the API at %s did not answer from here: %w\n"+
			"%s\ncheck that port %d and udp %s are reachable (firewall, security group), then try: %s",
			target, fleetName, address, err, bootstrapFailureCause(out.Report, res.Reachability),
			address.Port, f.cfg.VPNListen, statusHint(entry.Name, entry.Default))
	}
	res.Verified, res.Status = true, status
	res.Transport = transportOf(status)
	res.Reachability = reachability(fleetName, endpoint, joined, joinErr, res.Transport)
	return answer(res, true)
}

func setupArgs(flags *pflag.FlagSet) []string {
	var args []string
	for _, n := range forwardedFlags {
		fl := flags.Lookup(n)
		if fl == nil || !fl.Changed {
			continue
		}
		if fl.Value.Type() == "bool" {
			if fl.Value.String() == "true" {
				args = append(args, "--"+n)
			}
			continue
		}
		args = append(args, "--"+n+"="+fl.Value.String())
	}
	return args
}

func recordFleet(name string, address remote.Target) (*fleetEntry, error) {
	if !remote.ValidFleetName(name) {
		return nil, fmt.Errorf(
			"fleet %q: a fleet's name is a slug — lowercase letters, digits and dashes", name)
	}
	path, err := remote.CommanderConfigPath()
	if err != nil {
		return nil, err
	}
	cfg, err := remote.LoadCommanderConfigFrom(path)
	if err != nil {
		return nil, err
	}
	f := cfg.Commander.Fleets[name]
	f.Hub = address.String()
	cfg.SetFleet(name, f)
	if err := remote.SaveCommanderConfigTo(path, cfg); err != nil {
		return nil, err
	}
	return &fleetEntry{Name: name, Hub: f.Hub, Default: cfg.Commander.DefaultFleet == name, Config: path}, nil
}

func verifyOverSSH(ctx context.Context, a *app, fleet string) (json.RawMessage, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
		var stdout, stderr bytes.Buffer
		sub := &app{stdout: &stdout, stderr: &stderr, fleet: fleet, args: []string{"status", "--json"}}
		code, err := forwardImpl(ctx, sub)
		if err != nil {
			return nil, err
		}
		if code == 255 {
			lastErr = fmt.Errorf("ssh: %s", strings.TrimSpace(stderr.String()))
			continue
		}
		if code != 0 {
			return nil, fmt.Errorf("status exited %d: %s", code, strings.TrimSpace(stderr.String()))
		}
		raw := bytes.TrimSpace(stdout.Bytes())
		if !json.Valid(raw) {
			return nil, fmt.Errorf("status answered with something that is not JSON: %q", raw)
		}
		return json.RawMessage(raw), nil
	}
	return nil, lastErr
}

func (a *app) printBootstrap(res bootstrapResult, fleet string, verified bool) error {
	return a.printer().Result(res, func(w io.Writer) error {

		if res.Probe.OS != "" || res.Probe.Arch != "" {
			if _, err := fmt.Fprintf(w, "%s is %s/%s\n", res.Target,
				strOr(res.Probe.OS, "?"), strOr(res.Probe.Arch, "?")); err != nil {
				return err
			}
		}
		if err := writeBootstrapSummary(w, res.Setup); err != nil {
			return err
		}
		if res.Fleet != nil {
			how := "fleet %s (hub %s) recorded in %s\n"
			if _, err := fmt.Fprintf(w, how, res.Fleet.Name, res.Fleet.Hub, res.Fleet.Config); err != nil {
				return err
			}
		}
		if res.Reachability != nil {
			if _, err := fmt.Fprintln(w, bootstrapReachLine(res.Reachability)); err != nil {
				return err
			}
		}
		if !verified {
			return nil
		}
		try := statusHint(fleet, false)
		if res.Fleet != nil {
			try = statusHint(res.Fleet.Name, res.Fleet.Default)
		}
		_, err := fmt.Fprintf(w, "API verified from here; try: %s\n", try)
		return err
	})
}

func statusHint(fleet string, isDefault bool) string {
	if isDefault {
		return "caramelo status"
	}
	return "caramelo --fleet " + fleet + " status"
}

func writeBootstrapSummary(w io.Writer, r setup.Report) error {
	var total time.Duration
	for _, res := range r.Results {
		total += res.Duration
	}
	verb := "changed"
	if r.DryRun {
		verb = "would change"
	}
	_, err := fmt.Fprintf(w, "%d steps, %d %s, %d failed in %s\n",
		len(r.Results), countStatus(r, verb == "changed"), verb, r.Failed, total.Round(time.Millisecond))
	return err
}

func bootstrapPlan(target remote.Target, f bootstrapFlags) string {
	cfg := f.cfg
	keys := "the keys already authorized for " + target.User + " on " + target.Host
	if f.authorizedKeys != "" {
		keys = f.authorizedKeys + " (local file)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "caramelo hub setup will change %s (over ssh, as root):\n", target)

	binary := defaultBinaryPlan(version)
	if f.release != "" {
		binary = "release " + f.release + ", downloaded for the target's platform"
	}
	if f.binary != "" {
		binary = f.binary + ", shipped there"
	}
	fmt.Fprintf(&b, "  binary    %s\n", binary)
	fmt.Fprintf(&b, "  config    %s\n", f.configDir)
	fmt.Fprintf(&b, "  state     %s (home of the %s user)\n", cfg.StateDir, cfg.User)
	fmt.Fprintf(&b, "  data      %s (apps and Docker images)\n", cfg.DataDir)
	fmt.Fprintf(&b, "  swap      %s\n", swapPlanLine(cfg))
	fmt.Fprintf(&b, "  user      %s:%s, system user with rootless Docker\n", cfg.User, cfg.Group)
	fmt.Fprintf(&b, "  API       ssh on %s:%d\n", cfg.Bind, cfg.SSHPort)
	if cfg.Edge {

		fmt.Fprintf(&b, "  edge      ports 80 and 443, certificates from %s\n", edgeSource(cfg))
	}
	fmt.Fprintf(&b, "  keys      %s\n", keys)
	fmt.Fprintf(&b, "  commander records the fleet as %q in the commander config\n",
		nameOr(strings.TrimSpace(cfg.Hub.Fleet), nameOr(f.name, target.Host)))
	return b.String()
}

func defaultBinaryPlan(version string) string {
	if bootstrap.IsReleaseTag(version) {
		return "this one; if the target is another platform, caramelo-<os>-<arch> beside it, " +
			"or release " + version + " downloaded for the target"
	}
	return "this one; if the target is another platform, caramelo-<os>-<arch> beside it — " +
		"this build is not a release, so nothing can be fetched for it: give --binary <path> or --release <tag>"
}

func edgeSource(cfg serverconfig.Config) string {
	if mode, err := cfg.TLSMode(); err == nil && mode == certs.ModeInternal {
		return "an internal CA of its own"
	}
	return cfg.ACMEDirectory()
}

func nameOr(name, fallback string) string {
	if name != "" {
		return name
	}
	return fallback
}

func localUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "root"
}

var (
	commanderPeerKey = ensurePeerKey

	joinMachine = func(ctx context.Context, fleet string, ctl vpnclient.Control) (*vpnclient.State, error) {

		c, err := vpnclient.NewWith(vpnclient.Options{
			Control: ctl, Records: pinnedRecords{}, Log: io.Discard,
		})
		if err != nil {
			return nil, err
		}
		return c.Up(ctx, vpnclient.UpRequest{Machine: fleet, PeerName: commanderPeerName("")})
	}
)

type shellControl struct {
	shell bootstrap.Shell
	sudo  bool
}

var _ vpnclient.Control = shellControl{}

func (c shellControl) Run(ctx context.Context, _ string, argv []string, stdout, stderr io.Writer) (int, error) {
	line := remote.Quote(serverconfig.BinaryPath) + " " + strings.Join(remote.QuoteArgs(argv), " ")
	if c.sudo {
		line = "sudo -n " + line
	}
	return c.shell.Run(ctx, bootstrap.Cmd{Line: line, Stdout: stdout, Stderr: stderr})
}

func (a *app) bootstrapPeer(f bootstrapFlags, fleet string) (setup.PeerSpec, error) {
	if !f.peer.Empty() {
		return f.peer, nil
	}
	if f.dryRun || commanderPeerKey == nil {
		return setup.PeerSpec{}, nil
	}
	peer, err := commanderPeerKey(fleet)
	if err != nil {

		fmt.Fprintf(a.stderr, "[bootstrap] warning: no key for the commander: %v\n", err)
		return setup.PeerSpec{}, nil
	}
	return peer, nil
}

func (a *app) join(ctx context.Context, fleet string, peer setup.PeerSpec, f bootstrapFlags, ctl vpnclient.Control) (*vpnclient.State, error) {
	if joinMachine == nil || peer.Empty() || !f.peer.Empty() {
		return nil, nil
	}
	return joinMachine(ctx, fleet, ctl)
}

func reachability(machine, endpoint string, st *vpnclient.State, joinErr error, transport string) *vpnclient.Probe {
	if st == nil && joinErr == nil {
		return nil
	}
	p := &vpnclient.Probe{Machine: machine, Endpoint: endpoint, Admitted: true}
	if st != nil {
		p.Handshake = st.LastHandshake
		if st.Endpoint != "" {
			p.Endpoint = st.Endpoint
		}
	}
	port := portOfEndpoint(p.Endpoint)
	switch {
	case st == nil || st.LastHandshake.IsZero():
		p.Result = vpnclient.ProbeNoAnswer
		p.Detail = fmt.Sprintf("nothing from here got an answer on udp %s", port)
	case transport == "tunnel":
		p.Result = vpnclient.ProbeReached
		p.Detail = fmt.Sprintf("a packet from here arrived on udp %s and was answered, "+
			"and the API then answered through that tunnel", port)
	default:
		p.Result = vpnclient.ProbeUnproven
		p.Detail = fmt.Sprintf("a handshake came back on udp %s, so this machine answered a packet "+
			"from here; %s, so the tunnel is not proven end to end", port, apiWentWhere(transport))
	}
	return p
}

func apiWentWhere(transport string) string {
	if transport == "" {
		return "the API did not answer"
	}
	return "the API answered over " + transport + ", not through that tunnel"
}

func portOfEndpoint(endpoint string) string {
	if _, port, err := net.SplitHostPort(endpoint); err == nil {
		return port
	}
	return strconv.Itoa(vpn.DefaultListenPort)
}

func bootstrapReachLine(p *vpnclient.Probe) string {
	port := portOfEndpoint(p.Endpoint)
	switch p.Result {
	case vpnclient.ProbeReached:
		return fmt.Sprintf("udp %s reached from here", port)
	case vpnclient.ProbeNoAnswer:
		return fmt.Sprintf("udp %s did not answer from here: setup finished, "+
			"but nothing outside can reach this machine's tunnel", port)
	}
	return fmt.Sprintf("udp %s answered a handshake from here, "+
		"but the tunnel is not proven end to end: the API was not confirmed through it", port)
}

func bootstrapFailureCause(r setup.Report, p *vpnclient.Probe) string {
	if p != nil && !p.Handshake.IsZero() {
		return fmt.Sprintf("udp %s answered a handshake from here, so the way in to the tunnel is open: "+
			"the API is what did not answer through it", portOfEndpoint(p.Endpoint))
	}
	return remoteFirewallLine(r)
}

func remoteFirewallLine(r setup.Report) string {
	for _, res := range r.Results {
		if res.Step != "firewall" {
			continue
		}
		switch {
		case res.Status == setup.StatusFailed:
			return "the machine's own firewall stopped setup: " + res.Error
		case strings.Contains(res.Detail, string(firewall.VerdictBlocked)):
			return "the machine read its own firewall and found a port it needs blocked: " + res.Detail
		case strings.Contains(res.Detail, string(firewall.VerdictUnknown)):
			return "the machine could not read its own firewall (" + res.Detail +
				"), so nothing here says where the packets are going"
		}
		return "the machine read its own firewall and is not the one blocking the way in (" + res.Detail +
			"): look at the security group or the network in front of it"
	}
	return "the machine did not report on its own firewall"
}

func ensurePeerKey(machine string) (setup.PeerSpec, error) {
	kp, _, err := (&vpnclient.FileKeyStore{}).Ensure(machine)
	if err != nil {
		return setup.PeerSpec{}, err
	}
	return setup.PeerSpec{Name: vpnclient.DefaultPeerName(), PublicKey: kp.Public}, nil
}

func transportOf(status json.RawMessage) string {
	var st struct {
		Transport string `json:"transport"`
	}
	if json.Unmarshal(status, &st) != nil {
		return ""
	}
	return st.Transport
}

func liftJoinToken(args []string) ([]string, string) {
	const flag = "--join-token="
	out, token := make([]string, 0, len(args)), ""
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, flag); ok && strings.TrimSpace(v) != "-" {
			token = v
			continue
		}
		out = append(out, a)
	}
	return out, token
}
