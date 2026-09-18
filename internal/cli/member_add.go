package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup"
)

func (a *app) runMachineAdd(cmd *cobra.Command, target string, spec machineAddSpec) error {
	ctx := cmd.Context()
	name := spec.Name
	ticket, err := a.takeTicket(ctx)
	if err != nil {
		return err
	}
	ticket.Token = a.hubReachableFrom(ticket.Token)

	if name == "" {
		name = defaultMachineName(target)
	}
	cfg := serverconfig.Default()
	cfg.Name, cfg.Hub.Fleet = name, name
	cfg.Edge = spec.Edge || spec.Private
	if spec.ACMEEmail != "" {
		cfg.ACMEEmail = spec.ACMEEmail
	}
	if spec.ACMECA != "" {
		cfg.ACMECA = spec.ACMECA
	}
	if spec.TLS != "" {
		cfg.TLS = spec.TLS
	}
	var report bootstrapResult
	if err := a.runBootstrap(cmd, bootstrapFlags{
		target: target, name: name, cfg: cfg, configDir: serverconfig.ConfigDir(),
		binary: spec.Binary, release: spec.Release, yes: true, member: true, args: setupArgsForJoin(ticket.Token, name, spec),
		report: &report,
	}); err != nil {
		return err
	}

	res, err := a.confirmJoined(ctx, name, target, report)
	if err != nil {
		return err
	}

	res.Setup = report

	res.Joined = joinChanged(report)
	return a.printer().Result(res, func(w io.Writer) error {
		return machineAddedView(res, time.Now()).Write(w)
	})
}

func (a *app) takeTicket(ctx context.Context) (*api.MachineTokenResult, error) {
	var res api.MachineTokenResult
	if err := a.askHub(ctx, &res, "member", "token", "--json"); err != nil {
		return nil, fmt.Errorf("member add: take a join token from the hub: %w", err)
	}
	return &res, nil
}

func (a *app) askHub(ctx context.Context, out any, argv ...string) error {
	var stdout, stderr bytes.Buffer
	sub := &app{stdout: &stdout, stderr: &stderr, machine: a.machine, json: true, args: argv}
	code, err := forward(ctx, sub)
	if err != nil {
		return err
	}
	if code != 0 {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = fmt.Sprintf("exit %d", code)
		}
		return errors.New(detail)
	}
	raw := bytes.TrimSpace(stdout.Bytes())
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("read the hub's answer to %q: %w", strings.Join(argv, " "), err)
	}
	return nil
}

func setupArgsForJoin(token, name string, spec machineAddSpec) []string {
	args := []string{"--join-token=" + token, "--join-name=" + name}
	if spec.Edge {
		args = append(args, "--edge")
	}
	if spec.Private {
		args = append(args, "--private")
	}
	for flag, value := range map[string]string{
		"acme-email": spec.ACMEEmail,
		"acme-ca":    spec.ACMECA,
		"tls":        spec.TLS,
	} {
		if value != "" {
			args = append(args, "--"+flag+"="+value)
		}
	}
	sort.Strings(args)
	return args
}

func (a *app) confirmJoined(ctx context.Context, name, target string, report bootstrapResult) (*api.MachineAddResult, error) {
	var machines []fleet.Machine
	if err := a.askHub(ctx, &machines, "member", "list", "--json"); err != nil {
		return nil, fmt.Errorf("member add: read the fleet back: %w", err)
	}
	m, ok := fleet.Find(machines, name)
	if !ok {
		if joinSkipped(report) {
			return nil, fmt.Errorf("member add: %q is not in `caramelo member list`, and the box did not "+
				"try to join: its configuration already names this hub, which this hub has forgotten. "+
				"Run `sudo caramelo member leave` on %s and add it again", name, target)
		}
		return nil, fmt.Errorf("member add: setup finished on the box but %q is not in `caramelo member list`: "+
			"the join did not land (is this hub's UDP 4021 reachable from it?)", name)
	}
	return &api.MachineAddResult{Machine: m, Joined: true}, nil
}

func joinSkipped(report bootstrapResult) bool {
	for _, r := range report.Setup.Results {
		if r.Step == setup.JoinStepName {
			return r.Status == setup.StatusOK
		}
	}
	return false
}

func defaultMachineName(target string) string {
	host := target
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	if i := strings.Index(host, "."); i > 0 {
		host = host[:i]
	}
	return strOr(machineNameSlug(host), "member")
}

func machineNameSlug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func (a *app) hubReachableFrom(token string) string {
	host := a.hubHost()
	if host == "" {
		return token
	}
	t, err := fleet.ParseTicket(token)
	if err != nil {
		return token
	}
	_, port, err := net.SplitHostPort(t.Endpoint)
	if err != nil || port == "" {
		return token
	}
	if h, _, err := net.SplitHostPort(t.Endpoint); err == nil && h == host {
		return token
	}
	t.Endpoint = net.JoinHostPort(host, port)
	out, err := t.Encode()
	if err != nil {
		return token
	}
	return out
}

func (a *app) hubHost() string {
	if m := strings.TrimSpace(a.machine); m != "" {
		t, err := remote.ParseTarget(m)
		if err != nil {
			return ""
		}
		return t.Host
	}
	commander, err := remote.LoadCommanderConfig()
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(a.fleet)
	if name == "" {
		name = commander.Commander.DefaultFleet
	}
	if name == "" {
		if names := commander.FleetNames(); len(names) == 1 {
			name = names[0]
		}
	}
	t, err := commander.FleetTarget(name)
	if err != nil {
		return ""
	}
	return t.Host
}

func joinChanged(report bootstrapResult) bool {
	for _, r := range report.Setup.Results {
		if r.Step != setup.JoinStepName {
			continue
		}
		return r.Status == setup.StatusChanged
	}
	return false
}
