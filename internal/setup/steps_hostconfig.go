package setup

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/plytz/caramelo/internal/runner"
)

const (
	ModulesFile  = "/etc/modules-load.d/caramelo.conf"
	DelegateFile = "/etc/systemd/system/user@.service.d/caramelo-delegate.conf"
	TmpfilesFile = "/etc/tmpfiles.d/caramelo.conf"
	SysctlFile   = "/etc/sysctl.d/80-caramelo-lowports.conf"

	modulesContent = "# Installed by caramelo: rootless Docker with userland-proxy off needs it.\nbr_netfilter\n"

	delegateContent = "# Installed by caramelo: let the rootless Docker daemon set resource limits.\n[Service]\nDelegate=cpu cpuset io memory pids\n"

	lowPortsContent = "# Installed by caramelo (--low-ports): let containers publish ports below 1024.\nnet.ipv4.ip_unprivileged_port_start=80\n"
)

type HostConfigStep struct{}

func NewHostConfigStep() *HostConfigStep { return &HostConfigStep{} }

func (s *HostConfigStep) Name() string { return "host-config" }

func (s *HostConfigStep) files(env *Env) []fileSpec {
	cfg := env.Config
	files := []fileSpec{
		{Path: ModulesFile, Content: modulesContent, Mode: "0644", Owner: "root", Group: "root"},
		{Path: DelegateFile, Content: delegateContent, Mode: "0644", Owner: "root", Group: "root"},
		{Path: TmpfilesFile, Content: tmpfilesContent(cfg.RunDir, cfg.User, cfg.Group), Mode: "0644", Owner: "root", Group: "root"},
	}
	if env.Opts.LowPorts {
		files = append(files, fileSpec{Path: SysctlFile, Content: lowPortsContent, Mode: "0644", Owner: "root", Group: "root"})
	}
	return files
}

func tmpfilesContent(runDir, user, group string) string {
	return fmt.Sprintf("# Installed by caramelo: runtime directory for the caramelod API socket.\nd %s 0750 %s %s -\n",
		runDir, user, group)
}

const bridgeNFCallPath = "/proc/sys/net/bridge/bridge-nf-call-iptables"

func brNetfilterLoaded(ctx context.Context, env *Env) (bool, error) {
	probes := []runner.Cmd{
		{Name: "test", Args: []string{"-d", "/sys/module/br_netfilter"}},
		{Name: "test", Args: []string{"-e", bridgeNFCallPath}},
	}
	for _, probe := range probes {
		found, err := succeeds(ctx, env, probe)
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

func (s *HostConfigStep) Check(ctx context.Context, env *Env) (bool, string, error) {
	var problems []string
	for _, f := range s.files(env) {
		done, detail, err := f.check(ctx, env)
		if err != nil {
			return false, "", err
		}
		if !done {
			problems = append(problems, detail)
		}
	}
	loaded, err := brNetfilterLoaded(ctx, env)
	if err != nil {
		return false, "", err
	}
	if !loaded {
		problems = append(problems, "br_netfilter not loaded")
	}
	runDir, err := statPath(ctx, env, env.Config.RunDir)
	if err != nil {
		return false, "", err
	}
	if !runDir.Exists {
		problems = append(problems, env.Config.RunDir+" missing")
	}
	if len(problems) > 0 {
		return false, strings.Join(problems, "; "), nil
	}
	detail := "br_netfilter, cgroup delegation, " + env.Config.RunDir
	if env.Opts.LowPorts {
		detail += ", low ports"
	}
	return true, detail, nil
}

func (s *HostConfigStep) Apply(ctx context.Context, env *Env) error {
	var (
		delegateWritten bool
		daemonReload    bool
	)
	for _, f := range s.files(env) {
		done, _, err := f.check(ctx, env)
		if err != nil {
			return err
		}
		if done {
			continue
		}
		if err := ensureParent(ctx, env, f.Path); err != nil {
			return err
		}
		if err := f.apply(ctx, env); err != nil {
			return fmt.Errorf("write %s: %w", f.Path, err)
		}
		switch f.Path {
		case DelegateFile:
			delegateWritten, daemonReload = true, true
		case SysctlFile:
			if _, err := mustRun(ctx, env, runner.Cmd{Name: "sysctl", Args: []string{"--system"}}); err != nil {
				return fmt.Errorf("apply %s: %w", SysctlFile, err)
			}
		}
	}

	if loaded, err := brNetfilterLoaded(ctx, env); err != nil {
		return err
	} else if !loaded {
		if _, err := mustRun(ctx, env, runner.Cmd{Name: "modprobe", Args: []string{"br_netfilter"}}); err != nil {
			return fmt.Errorf("load br_netfilter: %w", err)
		}
	}

	if daemonReload {
		if _, err := mustRun(ctx, env, runner.Cmd{Name: "systemctl", Args: []string{"daemon-reload"}}); err != nil {
			return err
		}
	}

	if _, err := mustRun(ctx, env, runner.Cmd{Name: "systemd-tmpfiles", Args: []string{"--create", TmpfilesFile}}); err != nil {
		return fmt.Errorf("create %s: %w", env.Config.RunDir, err)
	}

	if delegateWritten {
		if err := s.restartUserSession(ctx, env); err != nil {
			return err
		}
	}
	return nil
}

func (s *HostConfigStep) restartUserSession(ctx context.Context, env *Env) error {
	uid, err := uidOf(ctx, env, env.Config.User)
	if err != nil {

		return nil
	}
	unit := fmt.Sprintf("user@%d.service", uid)
	active, err := succeeds(ctx, env, runner.Cmd{Name: "systemctl", Args: []string{"is-active", "--quiet", unit}})
	if err != nil {
		return err
	}
	if !active {
		return nil
	}
	if _, err := mustRun(ctx, env, runner.Cmd{Name: "systemctl", Args: []string{"restart", unit}}); err != nil {
		return fmt.Errorf("restart %s: %w", unit, err)
	}
	return waitPath(ctx, env, fmt.Sprintf("/run/user/%d", uid), sessionTimeout)
}

func ensureParent(ctx context.Context, env *Env, path string) error {
	dir := filepath.Dir(path)
	if dir == "/" || dir == "." {
		return nil
	}
	_, err := mustRun(ctx, env, runner.Cmd{Name: "mkdir", Args: []string{"-p", "--", dir}})
	return err
}
