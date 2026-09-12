package setup

import (
	"context"
	"fmt"
	"strings"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
)

const (
	EdgeSocketUnit  = "caramelo-edge.socket"
	EdgeServiceUnit = "caramelo-edge.service"

	SystemUnitDir = "/etc/systemd/system"
)

const (
	EdgeSocketUnitPath  = SystemUnitDir + "/" + EdgeSocketUnit
	EdgeServiceUnitPath = SystemUnitDir + "/" + EdgeServiceUnit
)

const EdgeSysctlPath = "/etc/sysctl.d/60-caramelo-edge.conf"

const EdgeSysctlContent = `# Installed by caramelo server setup --edge.
# QUIC (HTTP/3) reads and writes in large bursts; the kernel's default 208 KiB
# cap is far below what quic-go asks for, and it logs a warning on every start
# until this is raised.
net.core.rmem_max = 7500000
net.core.wmem_max = 7500000
`

const EdgeSocketUnitTemplate = `# Installed by caramelo server setup --edge.
[Unit]
Description=Caramelo edge sockets (%s)
Documentation=https://github.com/plytz/caramelo

[Socket]
%s%sBindIPv6Only=both
Backlog=1024
Accept=no
Service=%s
# systemd stops a socket unit whose activations come too fast (20 in 2 s by
# default), and a stopped socket unit is a machine that answers nothing at all.
# A crash-looping edge must cost requests, never the ports.
TriggerLimitIntervalSec=0

[Install]
WantedBy=sockets.target
`

const EdgeServiceUnitTemplate = `# Installed by caramelo server setup --edge.
[Unit]
Description=Caramelo edge
Documentation=https://github.com/plytz/caramelo
Requires=%s
After=network.target
StartLimitIntervalSec=0

[Service]
Type=exec
User=%s
Group=%s
ExecStart=%s
Restart=always
RestartSec=100ms
KillSignal=SIGTERM
TimeoutStopSec=5s
ReadWritePaths=%s
# Ports 80 and 443 are already open when this process starts: root's socket
# unit bound them and handed the descriptors over. So the edge needs no
# privilege at all, and an empty bounding set is what says so — in particular
# it does not hold CAP_NET_BIND_SERVICE, which is the whole point of activating
# it from a socket (ADR 0002 is untouched).
CapabilityBoundingSet=
AmbientCapabilities=
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true

[Install]
WantedBy=multi-user.target
`

func EdgeSocketUnitContent(cfg serverconfig.Config) string {
	host, where := "", "80, 443"
	if cfg.Fleet.Private {
		host, where = "127.0.0.1:", "127.0.0.1 only — a private member is served through its hub"
	}
	tcp := fmt.Sprintf("ListenStream=%s80\nListenStream=%s443\n", host, host)
	udp := ""
	if cfg.HTTP3 {
		udp = fmt.Sprintf("ListenDatagram=%s443\n", host)
	}
	return fmt.Sprintf(EdgeSocketUnitTemplate, where, tcp, udp, EdgeServiceUnit)
}

func EdgeServiceUnitContent(cfg serverconfig.Config, configDir string) string {
	execStart := serverconfig.BinaryPath + " edge"
	if configDir != "" && configDir != serverconfig.DefaultConfigDir {
		execStart += " --config-dir " + configDir
	}

	writable := cfg.EdgeDir() + " " + cfg.RunDir
	return fmt.Sprintf(EdgeServiceUnitTemplate,
		EdgeSocketUnit, cfg.User, cfg.Group, execStart, writable)
}

type EdgeStep struct{}

func NewEdgeStep() *EdgeStep { return &EdgeStep{} }

func (s *EdgeStep) Name() string { return "edge" }

func (s *EdgeStep) files(env *Env) []fileSpec {
	return []fileSpec{
		{Path: EdgeSocketUnitPath, Content: EdgeSocketUnitContent(env.Config),
			Mode: "0644", Owner: "root", Group: "root"},
		{Path: EdgeServiceUnitPath, Content: EdgeServiceUnitContent(env.Config, env.ConfigDir),
			Mode: "0644", Owner: "root", Group: "root"},
		{Path: EdgeSysctlPath, Content: EdgeSysctlContent,
			Mode: "0644", Owner: "root", Group: "root"},
	}
}

func (s *EdgeStep) dirs(env *Env) []dirSpec {
	cfg := env.Config
	return []dirSpec{
		{Path: cfg.EdgeDir(), Mode: "0750", Owner: cfg.User, Group: cfg.Group, AsUser: cfg.User},
		{Path: cfg.EdgeCertsDir(), Mode: "0700", Owner: cfg.User, Group: cfg.Group, AsUser: cfg.User},
	}
}

func (s *EdgeStep) Check(ctx context.Context, env *Env) (bool, string, error) {
	if !env.Config.Edge {
		return s.checkOff(ctx, env)
	}
	var problems []string
	for _, f := range s.files(env) {
		ok, detail, err := f.check(ctx, env)
		if err != nil {
			return false, "", err
		}
		if !ok {
			problems = append(problems, detail)
		}
	}
	for _, d := range s.dirs(env) {
		ok, detail, err := d.check(ctx, env)
		if err != nil {
			return false, "", err
		}
		if !ok {
			problems = append(problems, detail)
		}
	}

	userThere, err := userExists(ctx, env, env.Config.User)
	if err != nil {
		return false, "", err
	}
	if !userThere {
		problems = append(problems, "user "+env.Config.User+" does not exist yet")
	} else {
		for _, unit := range []string{EdgeSocketUnit, EdgeServiceUnit} {
			active, err := s.unitActive(ctx, env, unit)
			if err != nil {
				return false, "", err
			}
			if !active {
				problems = append(problems, unit+" not active")
			}
		}
	}
	if len(problems) > 0 {
		return false, strings.Join(problems, "; "), nil
	}
	return true, s.detail(env), nil
}

func (s *EdgeStep) detail(env *Env) string {
	cfg := env.Config
	ports := "80, 443/tcp"
	if cfg.HTTP3 {
		ports += ", 443/udp"
	}
	where := cfg.TLS
	if cfg.TLS == serverconfig.DefaultTLS {
		where += " via " + cfg.ACMEDirectory()
	}
	return fmt.Sprintf("%s and %s active on %s, certificates: %s, store %s",
		EdgeSocketUnit, EdgeServiceUnit, ports, where, cfg.EdgeCertsDir())
}

func (s *EdgeStep) checkOff(ctx context.Context, env *Env) (bool, string, error) {
	var present []string
	for _, path := range []string{EdgeSocketUnitPath, EdgeServiceUnitPath, EdgeSysctlPath} {
		st, err := statPath(ctx, env, path)
		if err != nil {
			return false, "", err
		}
		if st.Exists {
			present = append(present, path)
		}
	}
	if len(present) == 0 {
		return false, "", Skip{Reason: "no --edge"}
	}
	return false, "the edge is installed and the configuration says it is off: " +
		strings.Join(present, ", ") + " to remove", nil
}

func (s *EdgeStep) Apply(ctx context.Context, env *Env) error {
	if !env.Config.Edge {
		return s.remove(ctx, env)
	}
	for _, d := range s.dirs(env) {
		done, _, err := d.check(ctx, env)
		if err != nil {
			return err
		}
		if done {
			continue
		}
		if err := d.apply(ctx, env); err != nil {
			return fmt.Errorf("create %s: %w", d.Path, err)
		}
	}

	changed := map[string]bool{}
	for _, f := range s.files(env) {
		done, _, err := f.check(ctx, env)
		if err != nil {
			return err
		}
		if done {
			continue
		}
		if err := f.apply(ctx, env); err != nil {
			return fmt.Errorf("write %s: %w", f.Path, err)
		}
		changed[f.Path] = true
	}
	if changed[EdgeSysctlPath] {

		if _, err := mustRun(ctx, env, runner.Cmd{
			Name: "sysctl", Args: []string{"--quiet", "--load=" + EdgeSysctlPath},
		}); err != nil {

			logf(env, "warning: could not apply %s: %v", EdgeSysctlPath, err)
		}
	}

	if _, err := mustRun(ctx, env, s.systemctl("daemon-reload")); err != nil {
		return err
	}

	socketWasActive, err := s.unitActive(ctx, env, EdgeSocketUnit)
	if err != nil {
		return err
	}
	if _, err := mustRun(ctx, env, s.systemctl("enable", "--now", EdgeSocketUnit)); err != nil {
		return fmt.Errorf("start %s: %w", EdgeSocketUnit, err)
	}
	if socketWasActive && changed[EdgeSocketUnitPath] {
		if _, err := mustRun(ctx, env, s.systemctl("restart", EdgeSocketUnit)); err != nil {
			return fmt.Errorf("restart %s: %w", EdgeSocketUnit, err)
		}
	}
	serviceWasActive, err := s.unitActive(ctx, env, EdgeServiceUnit)
	if err != nil {
		return err
	}
	if _, err := mustRun(ctx, env, s.systemctl("enable", "--now", EdgeServiceUnit)); err != nil {
		return fmt.Errorf("start %s: %w", EdgeServiceUnit, err)
	}
	if serviceWasActive && (changed[EdgeServiceUnitPath] || changed[EdgeSocketUnitPath]) {
		if _, err := mustRun(ctx, env, s.systemctl("restart", EdgeServiceUnit)); err != nil {
			return fmt.Errorf("restart %s: %w", EdgeServiceUnit, err)
		}
	}

	logf(env, "the edge answers ports 80 and 443 as %s", env.Config.User)
	s.firewallNote(env)
	return nil
}

func (s *EdgeStep) firewallNote(env *Env) {
	ports := "tcp 80, tcp 443"
	if env.Config.HTTP3 {
		ports += ", udp 443"
	}
	logf(env, "make sure these ports reach this machine: %s", ports)
	if mode, err := env.Config.TLSMode(); err == nil && string(mode) == serverconfig.DefaultTLS {
		logf(env, "the certificate authority has to reach tcp 80 and tcp 443 from the internet, "+
			"and every exposed hostname has to resolve here")
	}
}

func (s *EdgeStep) remove(ctx context.Context, env *Env) error {

	for _, unit := range []string{EdgeServiceUnit, EdgeSocketUnit} {
		st, err := statPath(ctx, env, SystemUnitDir+"/"+unit)
		if err != nil {
			return err
		}
		if !st.Exists {
			continue
		}
		if _, err := mustRun(ctx, env, s.systemctl("disable", "--now", unit)); err != nil {
			return fmt.Errorf("stop %s: %w", unit, err)
		}
	}
	for _, path := range []string{EdgeServiceUnitPath, EdgeSocketUnitPath, EdgeSysctlPath} {
		if _, err := mustRun(ctx, env, runner.Cmd{Name: "rm", Args: []string{"-f", "--", path}}); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	if _, err := mustRun(ctx, env, s.systemctl("daemon-reload")); err != nil {
		return err
	}
	logf(env, "the edge is off; certificates and routes are kept at %s", env.Config.EdgeDir())
	return nil
}

func (s *EdgeStep) systemctl(args ...string) runner.Cmd {
	return runner.Cmd{Name: "systemctl", Args: args}
}

func (s *EdgeStep) unitActive(ctx context.Context, env *Env, unit string) (bool, error) {
	return succeeds(ctx, env, s.systemctl("is-active", "--quiet", unit))
}
