package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/plytz/caramelo/internal/bootstrap"
	"github.com/plytz/caramelo/internal/edge/certs"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup"
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

func checkBinarySource(binary, release string) error {
	if binary != "" && release != "" {
		return &usageError{errors.New("--binary and --release name two different binaries; give one")}
	}
	if release != "" && !bootstrap.IsReleaseTag(release) {
		return &usageError{fmt.Errorf(
			"--release wants a tag of the form v<major>.<minor>.<patch>, such as v0.0.1: got %q", release)}
	}
	return nil
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
	"edge", "acme-email", "acme-ca", "tls", "no-http3",
	"join-token", "join-name", "private",
	"force", "low-ports", "dry-run", "no-packages",
}

type bootstrapResult struct {
	Target string          `json:"target"`
	Probe  bootstrap.Probe `json:"probe"`
	Setup  setup.Report    `json:"setup"`

	Machine *machineEntry `json:"machine,omitempty"`

	Peer *setup.PeerSpec `json:"peer,omitempty"`

	Verified  bool            `json:"verified"`
	Transport string          `json:"transport,omitempty"`
	Status    json.RawMessage `json:"status,omitempty"`
}

type machineEntry struct {
	Name    string `json:"name"`
	Address string `json:"address"`
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

	peer, err := a.bootstrapPeer(f, name)
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
		return a.printBootstrap(r, name, verified)
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
	entry, err := recordMachine(name, address)
	if err != nil {
		_ = answer(res, false)
		return fmt.Errorf("setup finished on %s, but the machine could not be recorded: %w", target, err)
	}
	res.Machine = entry

	if err := a.join(ctx, name, peer, f, shellControl{shell: shell, sudo: out.Probe.Privilege != bootstrap.PrivilegeRoot}); err != nil {
		fmt.Fprintf(a.stderr, "[bootstrap] warning: could not join the machine's network: %v\n", err)
	}

	status, err := verifyMachine(ctx, a, name)
	if err != nil {
		_ = answer(res, false)
		return fmt.Errorf("setup finished on %s and it is recorded as machine %q, but the API at %s did not answer from here: %w\n"+
			"check that port %d and udp %s are reachable (firewall, security group), then try: caramelo --machine %s status",
			target, name, address, err, address.Port, f.cfg.VPNListen, name)
	}
	res.Verified, res.Status = true, status
	res.Transport = transportOf(status)
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

func recordMachine(name string, address remote.Target) (*machineEntry, error) {
	path, err := remote.CommanderConfigPath()
	if err != nil {
		return nil, err
	}
	cfg, err := remote.LoadCommanderConfigFrom(path)
	if err != nil {
		return nil, err
	}
	if cfg.Machines == nil {
		cfg.Machines = map[string]string{}
	}
	cfg.Machines[name] = address.String()
	if cfg.DefaultMachine == "" {
		cfg.DefaultMachine = name
	}
	if err := remote.SaveCommanderConfigTo(path, cfg); err != nil {
		return nil, err
	}
	return &machineEntry{Name: name, Address: address.String(), Default: cfg.DefaultMachine == name, Config: path}, nil
}

func verifyOverSSH(ctx context.Context, a *app, address string) (json.RawMessage, error) {
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
		sub := &app{stdout: &stdout, stderr: &stderr, machine: address, args: []string{"status", "--json"}}
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

func (a *app) printBootstrap(res bootstrapResult, name string, verified bool) error {
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
		if res.Machine != nil {
			how := "machine %s (%s) recorded in %s\n"
			if _, err := fmt.Fprintf(w, how, res.Machine.Name, res.Machine.Address, res.Machine.Config); err != nil {
				return err
			}
		}
		if !verified {
			return nil
		}
		try := "caramelo --machine " + name + " status"
		if res.Machine != nil && res.Machine.Default {
			try = "caramelo status"
		}
		_, err := fmt.Fprintf(w, "API verified from here; try: %s\n", try)
		return err
	})
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
	fmt.Fprintf(&b, "caramelo server setup will change %s (over ssh, as root):\n", target)

	binary := "this one; or caramelo-<os>-<arch> beside it, or the same release downloaded for the target, " +
		"when the target is another platform"
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
	fmt.Fprintf(&b, "  user      %s:%s, system user with rootless Docker\n", cfg.User, cfg.Group)
	fmt.Fprintf(&b, "  API       ssh on %s:%d\n", cfg.Bind, cfg.SSHPort)
	if cfg.Edge {

		fmt.Fprintf(&b, "  edge      ports 80 and 443, certificates from %s\n", edgeSource(cfg))
	}
	fmt.Fprintf(&b, "  keys      %s\n", keys)
	fmt.Fprintf(&b, "  commander records the machine as %q in the commander config\n", nameOr(f.name, target.Host))
	return b.String()
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

	joinMachine = func(ctx context.Context, machine string, ctl vpnclient.Control) error {

		c, err := vpnclient.NewWith(vpnclient.Options{Control: ctl, Log: io.Discard})
		if err != nil {
			return err
		}
		_, err = c.Up(ctx, vpnclient.UpRequest{Machine: machine})
		return err
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

func (a *app) bootstrapPeer(f bootstrapFlags, machine string) (setup.PeerSpec, error) {
	if !f.peer.Empty() {
		return f.peer, nil
	}
	if f.dryRun || commanderPeerKey == nil {
		return setup.PeerSpec{}, nil
	}
	peer, err := commanderPeerKey(machine)
	if err != nil {

		fmt.Fprintf(a.stderr, "[bootstrap] warning: no key for the commander: %v\n", err)
		return setup.PeerSpec{}, nil
	}
	return peer, nil
}

func (a *app) join(ctx context.Context, machine string, peer setup.PeerSpec, f bootstrapFlags, ctl vpnclient.Control) error {
	if joinMachine == nil || peer.Empty() || !f.peer.Empty() {
		return nil
	}
	return joinMachine(ctx, machine, ctl)
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
