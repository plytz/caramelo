package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"

	gossh "golang.org/x/crypto/ssh"

	"github.com/plytz/caramelo/internal/firewall"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
)

type SummaryStep struct{}

func NewSummaryStep() *SummaryStep { return &SummaryStep{} }

func (s *SummaryStep) Name() string { return "summary" }

func (s *SummaryStep) Check(ctx context.Context, env *Env) (bool, string, error) {
	cfg := env.Config

	if fp, err := hostKeyFingerprint(ctx, env); err != nil {
		logf(env, "warning: %v", err)
	} else if fp != "" {
		logf(env, "host key: %s", fp)
	}

	names, err := authorizedKeyNames(ctx, env)
	if err != nil {
		return false, "", err
	}
	switch len(names) {
	case 0:
		logf(env, "authorized keys: none yet — add one with 'caramelo key add --name NAME < key.pub'")
	default:
		logf(env, "authorized keys: %s", strings.Join(names, ", "))
	}

	logf(env, "vault: %s", describeVaultKey(ctx, env))

	ip := primaryIP(ctx, env)
	target := fmt.Sprintf("%s@%s", cfg.User, strOr(ip, "<this-machine>"))

	logf(env, "network: %s on %s (udp), vpn_mode: %s, api_listen: %s",
		cfg.VPNSubnet, cfg.VPNListen, cfg.VPNMode, cfg.APIListen)
	logf(env, "fleet: %s", describeFleet(cfg))
	s.reportPorts(ctx, env, ip)

	if !cfg.APIListensPublic() {
		logf(env, "the API answers inside the tunnel only: add a peer with "+
			"'caramelo peer add NAME PUBLIC_KEY', then 'caramelo vpn up --machine %s' from there", strOr(ip, "<this-machine>"))
		return true, fmt.Sprintf("peer add, then vpn up --machine %s", strOr(ip, "<this-machine>")), nil
	}
	logf(env, "try: ssh -p %d %s status", cfg.SSHPort, target)
	return true, fmt.Sprintf("ssh -p %d %s", cfg.SSHPort, target), nil
}

func describeFleet(cfg serverconfig.Config) string {
	if !cfg.IsMember() {
		return fmt.Sprintf("the hub of the fleet %s (a machine on its own is a fleet of one)", cfg.FleetName())
	}
	s := fmt.Sprintf("a member of the fleet %s at %s", cfg.FleetName(), cfg.Member.Hub.Endpoint)
	if sub := strings.TrimSpace(cfg.Member.Subnet); sub != "" {
		s += ", " + sub
	}
	if cfg.Member.Private {
		s += ", private: it is served through its hub and listens on nothing public"
	}
	return s
}

func describeVaultKey(ctx context.Context, env *Env) string {
	path := env.Config.VaultKeyPath()
	st, err := statPath(ctx, env, path)
	switch {
	case err != nil:
		return fmt.Sprintf("%s: %v", path, err)
	case !st.Exists:
		return path + " missing: this machine cannot hold a secret until setup runs again"
	case st.Mode != vaultKeyMode:
		return fmt.Sprintf("%s mode %s, want %s", path, st.Mode, vaultKeyMode)
	}
	return fmt.Sprintf("%s (%s %s:%s) — 'caramelo secrets set --app NAME=VALUE' from a checkout",
		path, st.Mode, st.Owner, st.Group)
}

func (s *SummaryStep) Apply(ctx context.Context, env *Env) error { return nil }

func hostKeyFingerprint(ctx context.Context, env *Env) (string, error) {
	path := env.Config.HostKeyPath() + ".pub"
	content, exists, err := readFile(ctx, env, path)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("no host key at %s yet", path)
	}
	key, _, _, _, err := gossh.ParseAuthorizedKey([]byte(content))
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	return gossh.FingerprintSHA256(key), nil
}

func authorizedKeyNames(ctx context.Context, env *Env) ([]string, error) {
	content, _, err := readFile(ctx, env, env.Config.AuthorizedKeysPath())
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range keyLines(content) {
		blob, comment, keyType, err := parseKeyLine(line)
		if err != nil {
			continue
		}
		if comment == "" {
			comment = keyType + " " + blob[:min(12, len(blob))]
		}
		names = append(names, comment)
	}
	return names, nil
}

func primaryIP(ctx context.Context, env *Env) string {
	res, err := runCmd(ctx, env, runner.Cmd{Name: "ip", Args: []string{"-j", "route", "get", "1.1.1.1"}})
	if err == nil && res.ExitCode == 0 {
		var routes []struct {
			PrefSrc string `json:"prefsrc"`
		}
		if err := json.Unmarshal([]byte(res.Stdout), &routes); err == nil {
			for _, r := range routes {
				if r.PrefSrc != "" {
					return r.PrefSrc
				}
			}
		}
	}
	if out, err := mustRun(ctx, env, runner.Cmd{Name: "hostname", Args: []string{"-I"}}); err == nil {
		if fields := strings.Fields(out); len(fields) > 0 {
			return fields[0]
		}
	}
	return ""
}

func (s *SummaryStep) reportPorts(ctx context.Context, env *Env, ip string) {
	cfg := env.Config
	rep := env.Firewall
	if rep == nil {
		want := firewall.Required(cfg, WantsInbound(cfg, env.Opts), env.PrivateDoor())
		if len(want) > 0 {
			logf(env, "this machine needs %s; the firewall was not read in this run", firewall.List(want))
		}
		return
	}
	if len(rep.Ports) == 0 {
		logf(env, "this machine needs no inbound port: it dials its hub and is served through it")
		return
	}
	for _, p := range rep.Ports {
		logf(env, "port %s (%s): %s", p.PortSpec, p.Why, portVerdict(*rep, p))
	}
	logf(env, "checked on this machine only. %s", s.probeLine(ctx, env, ip))
}

func portVerdict(rep firewall.Report, p firewall.PortCheck) string {
	switch p.Verdict {
	case firewall.VerdictOpen:
		return "this machine is not blocking it"
	case firewall.VerdictBlocked:
		return fmt.Sprintf("%s denies it: %s", rep.Manager, strOr(p.Rule, rep.Detail))
	}
	return "cannot tell from here: " + strOr(p.Rule, rep.Detail)
}

func (s *SummaryStep) probeLine(ctx context.Context, env *Env, ip string) string {
	endpoint := net.JoinHostPort(strOr(ip, "<this-machine>"),
		strconv.Itoa(firewall.VPNPort(env.Config.VPNListen)))
	line := "From a computer outside: caramelo hub probe " + endpoint
	if pub, err := (&VPNStep{}).publicKey(ctx, env); err == nil {
		line += " --key " + pub
	}
	return line + " (that computer's key must be admitted: 'caramelo peer add NAME KEY' here)"
}
