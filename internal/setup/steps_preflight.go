package setup

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"github.com/plytz/caramelo/internal/runner"
)

var minKernel = [2]int{5, 11}

var supportedOS = map[string][]string{
	"debian": {"12", "13"},
	"ubuntu": {"22.04", "24.04"},
}

type PreflightStep struct{}

func NewPreflightStep() *PreflightStep { return &PreflightStep{} }

func (s *PreflightStep) Name() string { return "preflight" }

func (s *PreflightStep) Check(ctx context.Context, env *Env) (bool, string, error) {
	if geteuid() != 0 {
		return false, "", fmt.Errorf("must run as root: try 'sudo caramelo server setup'")
	}

	var facts, warnings []string

	osr, _, err := readFile(ctx, env, "/etc/os-release")
	if err != nil {
		return false, "", err
	}
	rel := parseOSRelease(osr)
	id, versionID := rel["ID"], rel["VERSION_ID"]
	switch versions, known := supportedOS[id]; {
	case !known:
		msg := fmt.Sprintf("unsupported distribution %q: setup installs Docker with apt and expects Debian or Ubuntu", strOr(id, "unknown"))
		if !env.Opts.Force {
			return false, "", fmt.Errorf("%s (--force to try anyway)", msg)
		}
		warnings = append(warnings, msg)
	case !contains(versions, versionID):
		warnings = append(warnings, fmt.Sprintf("%s %s is not a tested version (tested: %s)",
			id, strOr(versionID, "?"), strings.Join(versions, ", ")))
	}
	facts = append(facts, strings.TrimSpace(id+" "+versionID))

	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return false, "", fmt.Errorf("unsupported architecture %s: amd64 or arm64 required", runtime.GOARCH)
	}
	facts = append(facts, runtime.GOARCH)

	systemd, err := succeeds(ctx, env, runner.Cmd{Name: "test", Args: []string{"-d", "/run/systemd/system"}})
	if err != nil {
		return false, "", err
	}
	if !systemd {
		return false, "", fmt.Errorf("systemd is not running (/run/systemd/system missing): caramelod and rootless Docker are systemd units")
	}

	fsType, err := mustRun(ctx, env, runner.Cmd{Name: "stat", Args: []string{"-fc", "%T", "/sys/fs/cgroup"}})
	if err != nil {
		return false, "", fmt.Errorf("check cgroup version: %w", err)
	}
	if strings.TrimSpace(fsType) != "cgroup2fs" {
		return false, "", fmt.Errorf("cgroup v2 required, /sys/fs/cgroup is %q: boot with systemd.unified_cgroup_hierarchy=1", strings.TrimSpace(fsType))
	}
	facts = append(facts, "cgroup2")

	kernel, err := mustRun(ctx, env, runner.Cmd{Name: "uname", Args: []string{"-r"}})
	if err != nil {
		return false, "", err
	}
	major, minor, err := parseKernelVersion(kernel)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("could not read the kernel version: %v", err))
	} else if major < minKernel[0] || (major == minKernel[0] && minor < minKernel[1]) {
		return false, "", fmt.Errorf("kernel %s is too old: %d.%d or newer required for rootless Docker",
			strings.TrimSpace(kernel), minKernel[0], minKernel[1])
	}
	facts = append(facts, "kernel "+strings.TrimSpace(kernel))

	apt, err := succeeds(ctx, env, runner.Cmd{Name: "sh", Args: []string{"-c", "command -v apt-get"}})
	if err != nil {
		return false, "", err
	}
	if !apt {
		if env.Opts.InstallPackages {
			return false, "", fmt.Errorf("apt-get not found: setup installs Docker from Debian's and Docker's apt repositories (--no-packages to skip that)")
		}
		warnings = append(warnings, "apt-get not found; packages are assumed to be installed already")
	}

	if running, err := succeeds(ctx, env, runner.Cmd{Name: "systemctl", Args: []string{"is-active", "--quiet", "docker.service"}}); err == nil && running {
		warnings = append(warnings, "a rootful docker.service is running; setup will disable it in favour of the rootless daemon")
	}

	for _, w := range warnings {
		logf(env, "warning: %s", w)
	}
	detail := strings.Join(facts, ", ")
	if len(warnings) > 0 {
		detail += fmt.Sprintf(" (%d warning(s))", len(warnings))
	}
	return true, detail, nil
}

func (s *PreflightStep) Apply(ctx context.Context, env *Env) error {
	return fmt.Errorf("preflight problems must be fixed by hand")
}

func parseOSRelease(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out
}

func parseKernelVersion(s string) (major, minor int, err error) {
	s = strings.TrimSpace(s)
	fields := strings.SplitN(s, ".", 3)
	if len(fields) < 2 {
		return 0, 0, fmt.Errorf("unexpected kernel version %q", s)
	}
	major, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("unexpected kernel version %q", s)
	}
	minorField := fields[1]
	for i, r := range minorField {
		if r < '0' || r > '9' {
			minorField = minorField[:i]
			break
		}
	}
	minor, err = strconv.Atoi(minorField)
	if err != nil {
		return 0, 0, fmt.Errorf("unexpected kernel version %q", s)
	}
	return major, minor, nil
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func strOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
