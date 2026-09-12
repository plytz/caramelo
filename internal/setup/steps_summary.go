package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"

	gossh "golang.org/x/crypto/ssh"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/vpn"
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

	logf(env, "network: %s on %s (udp), api_listen: %s", cfg.VPNSubnet, cfg.VPNListen, cfg.APIListen)
	logf(env, "fleet: %s", describeFleet(cfg))
	ports := "udp " + vpnPort(cfg.VPNListen)
	if cfg.APIListensPublic() {
		ports += fmt.Sprintf(", tcp %d", cfg.SSHPort)
	}
	logf(env, "make sure these ports reach this machine: %s", ports)

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
		return "a hub (a machine on its own is a fleet of one)"
	}
	s := fmt.Sprintf("a member of %s at %s", cfg.Fleet.Hub.Name, cfg.Fleet.Hub.Endpoint)
	if sub := strings.TrimSpace(cfg.Fleet.Subnet); sub != "" {
		s += ", " + sub
	}
	if cfg.Fleet.Private {
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

func vpnPort(listen string) string {
	if _, port, err := net.SplitHostPort(listen); err == nil {
		return port
	}
	return strconv.Itoa(vpn.DefaultListenPort)
}
