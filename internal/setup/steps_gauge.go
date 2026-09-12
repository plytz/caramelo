package setup

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/plytz/caramelo/internal/machine"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
)

const (
	minMemoryBytes = 768 << 20
	minFreeBytes   = 5 * (1 << 30)
)

var gauge = func(ctx context.Context, run runner.Runner, cfg serverconfig.Config, configDir, version string) (*machine.Record, error) {
	return machine.GaugeWith(ctx, run, cfg, machine.Options{ConfigDir: configDir, Version: version})
}

type GaugeStep struct {
	Record *machine.Record
}

func NewGaugeStep() *GaugeStep { return &GaugeStep{} }

func (s *GaugeStep) Name() string { return "gauge" }

func (s *GaugeStep) Check(ctx context.Context, env *Env) (bool, string, error) {
	rec, err := gauge(ctx, env.Run, env.Config, env.ConfigDir, env.Version)
	if err != nil {
		return false, "", fmt.Errorf("gauge machine: %w", err)
	}
	s.Record = rec

	free := rec.DataDir.AvailBytes
	if free == 0 {
		if n, err := availBytes(ctx, env, env.Config.DataDir); err == nil {
			free = n
		}
	}

	detail := describeMachine(rec, free)
	logf(env, "machine: %s", detail)

	var problems []string
	if rec.Memory.TotalBytes > 0 && rec.Memory.TotalBytes < minMemoryBytes {
		problems = append(problems, fmt.Sprintf("%s of RAM (%s needed)", bytesIEC(rec.Memory.TotalBytes), bytesIEC(minMemoryBytes)))
	}
	if free > 0 && free < minFreeBytes {
		problems = append(problems, fmt.Sprintf("%s free for %s (%s needed)", bytesIEC(free), env.Config.DataDir, bytesIEC(minFreeBytes)))
	}
	if len(problems) > 0 {
		if !env.Opts.Force {
			return false, detail, fmt.Errorf("machine too small: %s (--force to continue anyway)", strings.Join(problems, "; "))
		}
		logf(env, "warning: machine too small: %s (continuing because of --force)", strings.Join(problems, "; "))
	}
	return true, detail, nil
}

func (s *GaugeStep) Apply(ctx context.Context, env *Env) error { return nil }

func describeMachine(r *machine.Record, free int64) string {
	parts := []string{
		fmt.Sprintf("%d vCPU", r.CPU.Count),
		bytesIEC(r.Memory.TotalBytes) + " RAM",
	}
	if free > 0 {
		parts = append(parts, fmt.Sprintf("%s free on %s", bytesIEC(free), strOr(r.DataDir.Path, r.Dirs.Data)))
	}
	if os := strings.TrimSpace(r.OS.ID + " " + r.OS.VersionID); os != "" {
		parts = append(parts, os)
	}
	if r.OS.Arch != "" {
		parts = append(parts, r.OS.Arch)
	}
	if r.OS.Virt != "" && r.OS.Virt != "none" {
		parts = append(parts, r.OS.Virt)
	}
	if r.Hostname != "" {
		parts = append(parts, "host "+r.Hostname)
	}
	return strings.Join(parts, ", ")
}

func availBytes(ctx context.Context, env *Env, path string) (int64, error) {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		res, err := runCmd(ctx, env, runner.Cmd{Name: "df", Args: []string{"-B1", "--output=avail", p}})
		if err != nil {
			return 0, err
		}
		if res.ExitCode == 0 {
			return parseDFAvail(res.Stdout)
		}
		if p == "/" || p == "." {
			return 0, fmt.Errorf("df: no filesystem for %s", path)
		}
	}
}

func parseDFAvail(out string) (int64, error) {
	lines := strings.Fields(out)
	if len(lines) < 2 {
		return 0, fmt.Errorf("unexpected df output %q", out)
	}
	n, err := strconv.ParseInt(lines[len(lines)-1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unexpected df output %q", out)
	}
	return n, nil
}

func bytesIEC(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
