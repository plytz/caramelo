package dockersetup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/runner"
	dockerrt "github.com/plytz/caramelo/internal/runtime/docker"
	"github.com/plytz/caramelo/internal/setup"
)

const StepRootless = "docker-rootless"

const (
	busWait    = 10 * time.Second
	daemonWait = 60 * time.Second
	pollEvery  = 500 * time.Millisecond
)

type rootlessStep struct {
	lookupUID  func(name string) (string, error)
	lookupHome func(name string) (string, error)

	now   func() time.Time
	sleep func(context.Context, time.Duration)
}

func Rootless() setup.Step {
	return &rootlessStep{lookupUID: lookupUID, lookupHome: lookupHome, now: time.Now, sleep: sleepCtx}
}

func (s *rootlessStep) Name() string { return StepRootless }

type dockerUser struct {
	name string
	uid  string
	home string
}

func (s *rootlessStep) user(env *setup.Env) (dockerUser, error) {
	name := env.Config.User
	uid, err := s.lookupUID(name)
	if err != nil {
		return dockerUser{}, err
	}
	home, err := s.lookupHome(name)
	if err != nil {
		return dockerUser{}, err
	}
	return dockerUser{name: name, uid: uid, home: home}, nil
}

func asUser(u dockerUser, name string, args ...string) runner.Cmd {
	return runner.Cmd{Name: name, Args: args, User: u.name}
}

func (s *rootlessStep) Check(ctx context.Context, env *setup.Env) (bool, string, error) {
	u, err := s.user(env)
	if err != nil {

		return false, "user " + env.Config.User + " does not exist yet", nil
	}
	state, err := systemctlState(ctx, env.Run, asUser(u, "systemctl", "--user", "is-enabled", dockerrt.UserUnit))
	if err != nil {
		return false, "", err
	}
	if state != "enabled" && state != "enabled-runtime" {
		return false, "rootless docker user unit " + state, nil
	}
	want, err := dockerrt.RenderDaemonJSON(s.daemonConfig(env))
	if err != nil {
		return false, "", err
	}
	got, err := s.readAsUser(ctx, env, u, dockerrt.DaemonJSONPath(u.home))
	if err != nil {
		return false, "", err
	}
	if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
		return false, "daemon.json differs from the Caramelo configuration", nil
	}
	info, err := s.info(ctx, env, u)
	if err != nil {
		return false, "docker info failed: " + firstLine(err.Error()), nil
	}
	if problem := s.verifyInfo(env, info); problem != "" {
		return false, problem, nil
	}
	return true, s.detail(ctx, env, u, info), nil
}

func (s *rootlessStep) Apply(ctx context.Context, env *setup.Env) error {
	u, err := s.user(env)
	if err != nil {
		return err
	}
	if err := s.waitForBus(ctx, env, u); err != nil {
		return err
	}
	daemonJSON, err := dockerrt.RenderDaemonJSON(s.daemonConfig(env))
	if err != nil {
		return err
	}

	logf(env.Log, "writing %s", dockerrt.DaemonJSONPath(u.home))
	if err := s.writeAsUser(ctx, env, u, dockerrt.DaemonDir(u.home), dockerrt.DaemonJSONPath(u.home), daemonJSON); err != nil {
		return err
	}
	state, err := systemctlState(ctx, env.Run, asUser(u, "systemctl", "--user", "is-enabled", dockerrt.UserUnit))
	if err != nil {
		return err
	}
	if state == "not-found" {
		logf(env.Log, "installing the rootless docker daemon for %s...", u.name)
		args := s.installArgs(ctx, env, u)
		if _, err := run(ctx, env.Run, asUser(u, dockerrt.SetupTool, args...)); err != nil {
			return fmt.Errorf("%s %s: %w", dockerrt.SetupTool, strings.Join(args, " "), err)
		}
		if skipsIptables(args) {
			if err := s.keepIptables(ctx, env, u); err != nil {
				return err
			}
		}
	} else {

		logf(env.Log, "restarting the rootless docker daemon...")
		if _, err := run(ctx, env.Run, asUser(u, "systemctl", "--user", "restart", dockerrt.UserUnit)); err != nil {
			return err
		}
	}
	if _, err := run(ctx, env.Run, asUser(u, "systemctl", "--user", "enable", dockerrt.UserUnit)); err != nil {
		return err
	}
	info, err := s.waitForDaemon(ctx, env, u)
	if err != nil {
		return err
	}
	if problem := s.verifyInfo(env, info); problem != "" {
		return fmt.Errorf("rootless docker is up but %s", problem)
	}
	logf(env.Log, "rootless docker: %s", s.detail(ctx, env, u, info))
	return nil
}

const (
	nftablesModulePath = "/sys/module/nf_tables"
	netfilterSysctlDir = "/proc/sys/net/netfilter"
)

func (s *rootlessStep) installArgs(ctx context.Context, env *setup.Env, u dockerUser) []string {
	if !s.nftablesBuiltIn(ctx, env, u) {
		return []string{"install"}
	}
	logf(env.Log, "nf_tables is built into this kernel (%s is absent, %s is there):"+
		" installing with --skip-iptables", nftablesModulePath, netfilterSysctlDir)
	return []string{"install", skipIptablesArg}
}

const (
	skipIptablesArg = "--skip-iptables"
	iptablesOffFlag = "--iptables=false"
)

func skipsIptables(args []string) bool {
	for _, a := range args {
		if a == skipIptablesArg {
			return true
		}
	}
	return false
}

func userUnitPath(home string) string {
	return filepath.Join(home, ".config", "systemd", "user", dockerrt.UserUnit+".service")
}

func (s *rootlessStep) keepIptables(ctx context.Context, env *setup.Env, u dockerUser) error {
	path := userUnitPath(u.home)
	unit, err := s.readAsUser(ctx, env, u, path)
	if err != nil {
		return err
	}
	if !bytes.Contains(unit, []byte(iptablesOffFlag)) {
		return nil
	}
	logf(env.Log, "%s put %s in %s because its own nf_tables check cannot see a built-in module;"+
		" removing it so published ports reach containers", dockerrt.SetupTool, iptablesOffFlag, path)
	fixed := bytes.ReplaceAll(unit, []byte(" "+iptablesOffFlag), nil)
	fixed = bytes.ReplaceAll(fixed, []byte(iptablesOffFlag), nil)
	if err := s.writeAsUser(ctx, env, u, filepath.Dir(path), path, fixed); err != nil {
		return err
	}
	if _, err := run(ctx, env.Run, asUser(u, "systemctl", "--user", "daemon-reload")); err != nil {
		return err
	}
	if _, err := run(ctx, env.Run, asUser(u, "systemctl", "--user", "restart", dockerrt.UserUnit)); err != nil {
		return fmt.Errorf("restart rootless docker without %s: %w", iptablesOffFlag, err)
	}
	return nil
}

func (s *rootlessStep) nftablesBuiltIn(ctx context.Context, env *setup.Env, u dockerUser) bool {
	if _, err := env.Run.Run(ctx, runner.Cmd{Name: "modprobe", Args: []string{"nf_tables"}}); err != nil {
		logf(env.Log, "modprobe nf_tables: %v", err)
	}
	if s.pathExists(ctx, env, u, nftablesModulePath) {
		return false
	}
	return s.pathExists(ctx, env, u, netfilterSysctlDir)
}

func (s *rootlessStep) pathExists(ctx context.Context, env *setup.Env, u dockerUser, path string) bool {
	res, err := env.Run.Run(ctx, asUser(u, "test", "-e", path))
	return err == nil && res.ExitCode == 0
}

func (s *rootlessStep) daemonConfig(env *setup.Env) dockerrt.DaemonConfig {
	return dockerrt.DaemonConfig{DataRoot: env.Config.DockerDataRoot(), UserlandProxy: false}
}

func (s *rootlessStep) verifyInfo(env *setup.Env, info dockerrt.Info) string {
	switch {
	case !info.Rootless():
		return "the daemon is not rootless"
	case info.DataRoot != env.Config.DockerDataRoot():
		return fmt.Sprintf("data root is %s, want %s", info.DataRoot, env.Config.DockerDataRoot())
	case !info.OverlayStorage():
		return fmt.Sprintf("storage driver is %s, want an overlay driver", info.StorageDriver)
	case !info.CgroupV2():
		return fmt.Sprintf("cgroup version is %s, want 2", info.CgroupVersion)
	case info.CgroupDriver != "systemd":
		return fmt.Sprintf("cgroup driver is %s, want systemd", info.CgroupDriver)
	case len(info.Warnings) > 0:
		return "the daemon reports: " + strings.Join(cleanWarnings(info.Warnings), "; ") + hint(info.Warnings)
	}
	return ""
}

func cleanWarnings(ws []string) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, strings.TrimPrefix(strings.TrimSpace(w), "WARNING: "))
	}
	return out
}

func hint(ws []string) string {
	joined := strings.ToLower(strings.Join(ws, " "))
	switch {
	case strings.Contains(joined, "br_netfilter"):
		return " (load br_netfilter and re-run: the host-config step does that)"
	case strings.Contains(joined, "cpuset") || strings.Contains(joined, "io.max") ||
		strings.Contains(joined, "io.weight") || strings.Contains(joined, "cpu "):
		return " (cgroup delegation missing: the host-config step writes the" +
			" user@.service Delegate drop-in; re-run setup)"
	}
	return ""
}

func (s *rootlessStep) detail(ctx context.Context, env *setup.Env, u dockerUser, info dockerrt.Info) string {
	parts := []string{"rootless " + info.ServerVersion, info.StorageDriver, "data-root " + info.DataRoot}
	if v, err := s.version(ctx, env, u); err == nil {
		if d := v.Drivers(); d != "" {
			parts = append(parts, "rootlesskit "+v.RootlessKit+" "+d)
		}
	}
	return strings.Join(parts, ", ")
}

func (s *rootlessStep) info(ctx context.Context, env *setup.Env, u dockerUser) (dockerrt.Info, error) {
	res, err := run(ctx, env.Run, asUser(u, "docker", "info", "--format", "json"))
	if err != nil {
		return dockerrt.Info{}, err
	}
	return dockerrt.ParseInfo([]byte(res.Stdout))
}

func (s *rootlessStep) version(ctx context.Context, env *setup.Env, u dockerUser) (dockerrt.Version, error) {
	res, err := run(ctx, env.Run, asUser(u, "docker", "version"))
	if err != nil {
		return dockerrt.Version{}, err
	}
	return dockerrt.ParseVersion(res.Stdout), nil
}

func (s *rootlessStep) readAsUser(ctx context.Context, env *setup.Env, u dockerUser, path string) ([]byte, error) {
	res, err := env.Run.Run(ctx, asUser(u, "cat", path))
	if err != nil {
		return nil, fmt.Errorf("read %s as %s: %w", path, u.name, err)
	}
	if res.ExitCode != 0 {
		return nil, nil
	}
	return []byte(res.Stdout), nil
}

func (s *rootlessStep) writeAsUser(ctx context.Context, env *setup.Env, u dockerUser, dir, path string, content []byte) error {
	if _, err := run(ctx, env.Run, asUser(u, "mkdir", "-p", dir)); err != nil {
		return err
	}
	c := asUser(u, "sh", "-c", "umask 022 && cat > "+shellQuote(path))
	c.Stdin = bytes.NewReader(content)
	if _, err := run(ctx, env.Run, c); err != nil {
		return fmt.Errorf("write %s as %s: %w", path, u.name, err)
	}
	return nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (s *rootlessStep) waitForBus(ctx context.Context, env *setup.Env, u dockerUser) error {
	bus := dockerrt.BusPath(u.uid)
	if s.waitForSocket(ctx, env, u, bus, busWait) {
		return nil
	}

	logf(env.Log, "restarting the systemd user session of %s...", u.name)
	if _, err := run(ctx, env.Run, runner.Cmd{
		Name: "systemctl", Args: []string{"restart", "user@" + u.uid + ".service"},
	}); err != nil {
		return err
	}
	if s.waitForSocket(ctx, env, u, bus, busWait) {
		return nil
	}
	return fmt.Errorf("%s did not appear: is linger enabled for %s?", bus, u.name)
}

func (s *rootlessStep) waitForSocket(ctx context.Context, env *setup.Env, u dockerUser, path string, limit time.Duration) bool {
	deadline := s.now().Add(limit)
	for {
		res, err := env.Run.Run(ctx, asUser(u, "test", "-S", path))
		if err == nil && res.ExitCode == 0 {
			return true
		}
		if s.now().After(deadline) || ctx.Err() != nil {
			return false
		}
		s.sleep(ctx, pollEvery)
	}
}

func (s *rootlessStep) waitForDaemon(ctx context.Context, env *setup.Env, u dockerUser) (dockerrt.Info, error) {
	sock := dockerrt.SocketPath(u.uid)
	if !s.waitForSocket(ctx, env, u, sock, daemonWait) {
		return dockerrt.Info{}, fmt.Errorf("%s did not appear within %s: check"+
			" `systemctl --user -M %s@ status %s`", sock, daemonWait, u.name, dockerrt.UserUnit)
	}
	deadline := s.now().Add(daemonWait)
	var last error
	for {
		info, err := s.info(ctx, env, u)
		if err == nil {
			return info, nil
		}
		last = err
		if s.now().After(deadline) {
			return dockerrt.Info{}, fmt.Errorf("rootless docker did not become ready: %w", last)
		}
		if ctx.Err() != nil {
			return dockerrt.Info{}, errors.Join(ctx.Err(), last)
		}
		s.sleep(ctx, pollEvery)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
