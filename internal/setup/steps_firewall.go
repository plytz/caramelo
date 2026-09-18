package setup

import (
	"context"
	"fmt"
	"strings"

	"github.com/plytz/caramelo/internal/firewall"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
)

const forceHint = "(--force to continue anyway)"

const LocalOnlyCaveat = "checked here only; a security group or the network between can still drop it"

type FirewallStep struct{}

func NewFirewallStep() *FirewallStep { return &FirewallStep{} }

func (s *FirewallStep) Name() string { return "firewall" }

func (s *FirewallStep) Check(ctx context.Context, env *Env) (bool, string, error) {
	want := firewall.Required(env.Config, WantsInbound(env.Config, env.Opts), env.PrivateDoor())
	rep, err := firewall.Check(ctx, env.Run, geteuid(), want)
	if err != nil {
		return false, "", fmt.Errorf("read this machine's firewall: %w", err)
	}
	env.Firewall = &rep

	logf(env, "firewall: %s", rep.Detail)
	for _, p := range rep.Ports {
		logf(env, "%s", portSentence(rep, p))
	}
	llmnrWarning(ctx, env)

	if env.Opts.OpenPorts && len(openable(rep)) > 0 {
		return false, firewallDetail(rep), nil
	}
	if p, stop := deniedTunnelPort(env, rep); stop {
		_, printed, supported := firewall.OpenCommand(rep.Manager, p.PortSpec)
		msg := fmt.Sprintf("%s is active and denies %s, %s: %s",
			rep.Manager, p.PortSpec, p.Why, strOr(p.Rule, rep.Detail))
		if printed != "" {
			msg += fmt.Sprintf(". Open it with '%s', then run setup again", printed)
			if supported {
				msg += " (--open-ports lets setup add that rule itself)"
			}
		}
		if env.Opts.Force {
			logf(env, "warning: %s", msg)
			return true, firewallDetail(rep), nil
		}
		return false, "", fmt.Errorf("%s %s", msg, forceHint)
	}
	return true, firewallDetail(rep), nil
}

func (s *FirewallStep) Apply(ctx context.Context, env *Env) error {
	if !env.Opts.OpenPorts {
		return fmt.Errorf("caramelo reads this machine's firewall and never changes it: " +
			"open the port by hand, or run setup again with --open-ports")
	}
	rep := env.Firewall
	if rep == nil {
		return fmt.Errorf("the firewall was not read, so there is nothing to open")
	}
	for _, p := range rep.Blocked() {
		cmds, printed, supported := firewall.OpenCommand(rep.Manager, p.PortSpec)
		if !supported {
			return fmt.Errorf("--open-ports: caramelo does not write into a %s ruleset, "+
				"where one blind rule can lock you out of this machine; add it by hand: %s", rep.Manager, printed)
		}
		for _, c := range cmds {
			if _, err := mustRun(ctx, env, c); err != nil {
				return fmt.Errorf("open %s: %w", p.PortSpec, err)
			}
		}
		logf(env, "opened %s in %s: %s", p.PortSpec, rep.Manager, printed)
	}
	return nil
}

func WantsInbound(cfg serverconfig.Config, opts Options) bool {
	return cfg.IsHub() && opts.Join.Empty()
}

func openable(rep firewall.Report) []firewall.PortCheck {
	var out []firewall.PortCheck
	for _, p := range rep.Blocked() {
		if _, _, supported := firewall.OpenCommand(rep.Manager, p.PortSpec); supported {
			out = append(out, p)
		}
	}
	return out
}

func deniedTunnelPort(env *Env, rep firewall.Report) (firewall.PortCheck, bool) {
	if !WantsInbound(env.Config, env.Opts) {
		return firewall.PortCheck{}, false
	}
	p, ok := rep.Find(firewall.Tunnel(env.Config))
	if !ok || p.Verdict != firewall.VerdictBlocked {
		return firewall.PortCheck{}, false
	}
	return p, true
}

func portSentence(rep firewall.Report, p firewall.PortCheck) string {
	switch p.Verdict {
	case firewall.VerdictOpen:
		return fmt.Sprintf("this machine is not blocking %s (%s)", p.PortSpec, LocalOnlyCaveat)
	case firewall.VerdictBlocked:
		_, printed, _ := firewall.OpenCommand(rep.Manager, p.PortSpec)
		line := fmt.Sprintf("warning: %s is active and denies %s, %s: %s",
			rep.Manager, p.PortSpec, p.Why, strOr(p.Rule, rep.Detail))
		if printed != "" {
			line += fmt.Sprintf(" — open it with '%s'", printed)
		}
		return line
	}
	return fmt.Sprintf("warning: whether %s is blocked here cannot be worked out: %s",
		p.PortSpec, strOr(p.Rule, rep.Detail))
}

func firewallDetail(rep firewall.Report) string {
	if len(rep.Ports) == 0 {
		return rep.Detail + "; this machine needs no inbound port"
	}
	var parts []string
	for _, p := range rep.Ports {
		parts = append(parts, fmt.Sprintf("%s %s", p.PortSpec, p.Verdict))
	}
	return rep.Detail + " (" + strings.Join(parts, ", ") + ")"
}

func llmnrWarning(ctx context.Context, env *Env) {
	res, err := runCmd(ctx, env, runner.Cmd{Name: "ss", Args: []string{"-lun"}})
	if err != nil || res.ExitCode != 0 {
		return
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if !strings.Contains(line, ":5355") {
			continue
		}
		if !strings.Contains(line, "0.0.0.0:5355") && !strings.Contains(line, "*:5355") &&
			!strings.Contains(line, "[::]:5355") {
			continue
		}
		logf(env, "warning: systemd-resolved answers LLMNR on udp 5355 on every address of this machine; "+
			"it is exposed the moment a firewall in front of it is flushed (caramelo reports it and changes nothing)")
		return
	}
}
