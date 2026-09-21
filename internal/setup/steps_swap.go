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
	SwapSysctlFile = "/etc/sysctl.d/80-caramelo-swap.conf"

	swapUnitTemplate = `# Installed by caramelo fleet setup: the machine's swap.
[Unit]
Description=Caramelo swap
Documentation=https://github.com/plytz/caramelo

[Swap]
What=%s

[Install]
WantedBy=swap.target
`

	swapSysctlTemplate = `# Installed by caramelo fleet setup: how early this machine leans on its swap.
vm.swappiness=%d
`
)

const swapAllocationUnit = 1 << 20

type SwapStep struct{}

func NewSwapStep() *SwapStep { return &SwapStep{} }

func (s *SwapStep) Name() string { return "swap" }

func swapUnitContent(path string) string { return fmt.Sprintf(swapUnitTemplate, path) }

func swapSysctlContent(swappiness int) string {
	return fmt.Sprintf(swapSysctlTemplate, swappiness)
}

func swapSizeBytes(cfg serverconfig.Config) int64 {
	return cfg.Swap.SizeBytes / swapAllocationUnit * swapAllocationUnit
}

func SwapUnitPath(ctx context.Context, run runner.Runner, swapfile string) (string, error) {
	if run == nil {
		return "", fmt.Errorf("name the swap unit of %s: no command runner", swapfile)
	}
	res, err := run.Run(ctx, runner.Cmd{Name: "systemd-escape", Args: []string{"-p", "--suffix=swap", swapfile}})
	if err != nil {
		return "", fmt.Errorf("name the swap unit of %s: %w", swapfile, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("name the swap unit of %s: systemd-escape exited %d: %s",
			swapfile, res.ExitCode, firstLine(res.Stderr, res.Stdout))
	}
	name := strings.TrimSpace(res.Stdout)
	if name == "" {
		return "", fmt.Errorf("name the swap unit of %s: systemd-escape said nothing", swapfile)
	}
	return filepath.Join(SystemUnitDir, name), nil
}

func (s *SwapStep) files(cfg serverconfig.Config, unitPath string) []fileSpec {
	return []fileSpec{
		{Path: unitPath, Content: swapUnitContent(cfg.SwapFilePath()), Mode: "0644", Owner: "root", Group: "root"},
		{Path: SwapSysctlFile, Content: swapSysctlContent(cfg.Swap.Swappiness), Mode: "0644", Owner: "root", Group: "root"},
	}
}

const swapOffAlready = "swap off: this machine is set up without swap (--swap 4G gives it some)"

type swapRemains struct {
	Path      string
	UnitPath  string
	Active    bool
	FileThere bool
	UnitThere bool
	Sysctl    bool
}

func (r swapRemains) list() []string {
	var out []string
	if r.Active {
		out = append(out, r.Path+" is swapped on")
	}
	if r.FileThere {
		out = append(out, r.Path+" is still there")
	}
	if r.UnitThere {
		out = append(out, r.UnitPath+" is still there")
	}
	if r.Sysctl {
		out = append(out, SwapSysctlFile+" is still there")
	}
	return out
}

func (s *SwapStep) swapRemains(ctx context.Context, env *Env) (swapRemains, error) {
	path := env.Config.SwapFilePath()
	rem := swapRemains{Path: path}
	info, err := statSwapFile(ctx, env, path)
	if err != nil {
		return rem, err
	}
	rem.FileThere = info.Exists
	entries, _ := readSwapon(ctx, env)
	rem.Active, _ = splitSwapon(entries, path)
	if unitPath, err := SwapUnitPath(ctx, env.Run, path); err == nil {
		st, err := statPath(ctx, env, unitPath)
		if err != nil {
			return rem, err
		}
		rem.UnitPath, rem.UnitThere = unitPath, st.Exists
	}
	st, err := statPath(ctx, env, SwapSysctlFile)
	if err != nil {
		return rem, err
	}
	rem.Sysctl = st.Exists
	return rem, nil
}

func (s *SwapStep) checkOff(ctx context.Context, env *Env) (bool, string, error) {
	rem, err := s.swapRemains(ctx, env)
	if err != nil {
		return false, "", err
	}
	left := rem.list()
	if len(left) == 0 {
		return false, "", Skip{Reason: swapOffAlready}
	}
	return false, "swap off, but " + strings.Join(left, "; "), nil
}

func (s *SwapStep) applyOff(ctx context.Context, env *Env) error {
	rem, err := s.swapRemains(ctx, env)
	if err != nil {
		return err
	}
	active := rem.Active
	if rem.UnitThere {
		if _, err := mustRun(ctx, env, runner.Cmd{
			Name: "systemctl", Args: []string{"disable", "--now", filepath.Base(rem.UnitPath)},
		}); err != nil {
			return fmt.Errorf("take the swap unit of %s out of service: %w", rem.Path, err)
		}
		if active {
			if entries, listed := readSwapon(ctx, env); listed {
				active, _ = splitSwapon(entries, rem.Path)
			}
		}
	}
	if active {
		if _, err := mustRun(ctx, env, runner.Cmd{Name: "swapoff", Args: []string{"--", rem.Path}}); err != nil {
			return fmt.Errorf("swap off %s: %w", rem.Path, err)
		}
	}
	gone := []string{}
	if rem.UnitPath != "" {
		gone = append(gone, rem.UnitPath)
	}
	gone = append(gone, SwapSysctlFile, rem.Path)
	for _, p := range gone {
		if _, err := mustRun(ctx, env, runner.Cmd{Name: "rm", Args: []string{"-f", "--", p}}); err != nil {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	if rem.UnitThere {
		if _, err := mustRun(ctx, env, runner.Cmd{Name: "systemctl", Args: []string{"daemon-reload"}}); err != nil {
			return err
		}
	}
	return nil
}

func (s *SwapStep) Check(ctx context.Context, env *Env) (bool, string, error) {
	cfg := env.Config
	switch cfg.Swap.Backend {
	case serverconfig.SwapOff:
		return s.checkOff(ctx, env)
	case serverconfig.SwapZram:
		return false, "", Skip{Reason: "zram is not implemented yet: use a size such as 4G, or off"}
	case serverconfig.SwapFile:
	default:
		return false, "", Skip{Reason: fmt.Sprintf("swap backend %q: want one of %s",
			cfg.Swap.Backend, strings.Join(serverconfig.SwapBackends, ", "))}
	}
	if inContainer(ctx, env) {
		return false, "", Skip{Reason: "a container shares the host kernel's swap: there is none to allocate here"}
	}

	path := cfg.SwapFilePath()
	want := swapSizeBytes(cfg)
	info, err := statSwapFile(ctx, env, path)
	if err != nil {
		return false, "", err
	}
	entries, listed := readSwapon(ctx, env)
	active, foreign := splitSwapon(entries, path)
	if !info.Exists && len(foreign) > 0 {
		return true, describeSwapEntries(foreign) + ", left alone", nil
	}

	unitPath, err := SwapUnitPath(ctx, env.Run, path)
	if err != nil {
		return false, "", Skip{Reason: err.Error()}
	}

	var problems []string
	switch {
	case !info.Exists:
		problems = append(problems, path+" missing")
	default:
		if info.SizeBytes != want {
			problems = append(problems, fmt.Sprintf("%s is %s, want %s", path, bytesIEC(info.SizeBytes), bytesIEC(want)))
		}
		if info.Sparse() {
			problems = append(problems, path+" is sparse")
		}
		if info.Mode != "0600" {
			problems = append(problems, fmt.Sprintf("%s mode %s, want 0600", path, info.Mode))
		}
		if info.Owner != "root" || info.Group != "root" {
			problems = append(problems, fmt.Sprintf("%s owned by %s:%s, want root:root", path, info.Owner, info.Group))
		}
	}
	for _, f := range s.files(cfg, unitPath) {
		done, detail, err := f.check(ctx, env)
		if err != nil {
			return false, "", err
		}
		if !done {
			problems = append(problems, detail)
		}
	}
	if listed && !active {
		problems = append(problems, path+" is not swapped on")
	}
	if len(problems) == 0 {
		return true, fmt.Sprintf("%s swapfile at %s, vm.swappiness %d", bytesIEC(want), path, cfg.Swap.Swappiness), nil
	}
	if !info.Exists || info.SizeBytes != want || info.Sparse() {
		if reason := swapRefusal(ctx, env, path, want, info); reason != "" {
			return false, "", Skip{Reason: reason}
		}
	}
	return false, strings.Join(problems, "; "), nil
}

func (s *SwapStep) Apply(ctx context.Context, env *Env) error {
	cfg := env.Config
	if cfg.Swap.Backend == serverconfig.SwapOff {
		return s.applyOff(ctx, env)
	}
	path := cfg.SwapFilePath()
	want := swapSizeBytes(cfg)
	unitPath, err := SwapUnitPath(ctx, env.Run, path)
	if err != nil {
		return err
	}
	info, err := statSwapFile(ctx, env, path)
	if err != nil {
		return err
	}
	entries, _ := readSwapon(ctx, env)
	active, _ := splitSwapon(entries, path)

	if !info.Exists || info.SizeBytes != want || info.Sparse() {
		if active {
			if _, err := mustRun(ctx, env, runner.Cmd{Name: "swapoff", Args: []string{"--", path}}); err != nil {
				return fmt.Errorf("swap off %s before it is made again: %w", path, err)
			}
			active = false
		}
		if err := ensureParent(ctx, env, path); err != nil {
			return err
		}
		if err := allocateSwapFile(ctx, env, path, want); err != nil {
			return err
		}
	}
	if _, err := mustRun(ctx, env, runner.Cmd{Name: "chmod", Args: []string{"0600", "--", path}}); err != nil {
		return err
	}
	if _, err := mustRun(ctx, env, runner.Cmd{Name: "chown", Args: []string{"root:root", "--", path}}); err != nil {
		return err
	}
	if !active {
		if _, err := mustRun(ctx, env, runner.Cmd{Name: "mkswap", Args: []string{"--", path}}); err != nil {
			return fmt.Errorf("make swap on %s: %w", path, err)
		}
	}

	sysctlWritten := false
	for _, f := range s.files(cfg, unitPath) {
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
		if f.Path == SwapSysctlFile {
			sysctlWritten = true
		}
	}

	if _, err := mustRun(ctx, env, runner.Cmd{Name: "systemctl", Args: []string{"daemon-reload"}}); err != nil {
		return err
	}
	if _, err := mustRun(ctx, env, runner.Cmd{
		Name: "systemctl", Args: []string{"enable", "--now", filepath.Base(unitPath)},
	}); err != nil {
		return fmt.Errorf("swap on %s: %w", path, err)
	}
	if sysctlWritten {
		if _, err := mustRun(ctx, env, runner.Cmd{
			Name: "sysctl", Args: []string{"--quiet", "--load=" + SwapSysctlFile},
		}); err != nil {
			logf(env, "warning: could not apply %s: %v", SwapSysctlFile, err)
		}
	}
	return nil
}

func allocateSwapFile(ctx context.Context, env *Env, path string, size int64) error {
	res, err := runCmd(ctx, env, runner.Cmd{
		Name: "fallocate", Args: []string{"-l", strconv.FormatInt(size, 10), "--", path},
	})
	if err == nil && res.ExitCode == 0 {
		info, err := statSwapFile(ctx, env, path)
		if err != nil {
			return err
		}
		if info.Exists && info.SizeBytes == size && !info.Sparse() {
			return nil
		}
		logf(env, "warning: fallocate left %s sparse or short: writing it out with dd", path)
	}
	if _, err := mustRun(ctx, env, runner.Cmd{Name: "rm", Args: []string{"-f", "--", path}}); err != nil {
		return err
	}
	if _, err := mustRun(ctx, env, runner.Cmd{Name: "dd", Args: []string{
		"if=/dev/zero", "of=" + path, "bs=1M",
		"count=" + strconv.FormatInt(size/swapAllocationUnit, 10), "status=none",
	}}); err != nil {
		return fmt.Errorf("allocate %s: %w", path, err)
	}
	return nil
}

func swapRefusal(ctx context.Context, env *Env, path string, want int64, info swapFileInfo) string {
	dir := filepath.Dir(path)
	if fstype, source, ok := mountOf(ctx, env, dir); ok {
		switch {
		case fstype == "btrfs":
			return fmt.Sprintf("%s is on btrfs, where a swapfile has to be made NOCOW by hand before it holds anything: "+
				"make one yourself, or run with --swap off", dir)
		case fstype == "zfs":
			return fmt.Sprintf("%s is on ZFS, where a swapfile can deadlock the machine under the very pressure it exists for: "+
				"give the machine a swap partition, or run with --swap off", dir)
		case strings.HasPrefix(source, "/dev/mmcblk"), strings.HasPrefix(source, "/dev/mtd"):
			return fmt.Sprintf("a swapfile on flash storage %s would wear the card out: a board needs zram, "+
				"which caramelo does not make yet, so run with --swap off", source)
		}
	}
	avail, err := availBytes(ctx, env, dir)
	if err != nil || avail <= 0 {
		return ""
	}
	free := avail + info.AllocatedBytes()
	if free >= want+minFreeBytes {
		return ""
	}
	fits := (free - minFreeBytes) / swapAllocationUnit * swapAllocationUnit
	advice := "run with --swap off"
	if fits >= serverconfig.MinSwapSizeBytes {
		advice = fmt.Sprintf("run with --swap %dM, or --swap off", fits/swapAllocationUnit)
	}
	return fmt.Sprintf("%s free on %s: %s of swap plus the %s caramelo keeps free needs %s, so %s",
		bytesIEC(free), dir, bytesIEC(want), bytesIEC(minFreeBytes), bytesIEC(want+minFreeBytes), advice)
}

type swapFileInfo struct {
	Exists     bool
	Owner      string
	Group      string
	Mode       string
	SizeBytes  int64
	Blocks     int64
	BlockBytes int64
}

func (i swapFileInfo) AllocatedBytes() int64 { return i.Blocks * i.BlockBytes }

func (i swapFileInfo) Sparse() bool {
	return i.Exists && i.BlockBytes > 0 && i.AllocatedBytes() < i.SizeBytes
}

func statSwapFile(ctx context.Context, env *Env, path string) (swapFileInfo, error) {
	res, err := runCmd(ctx, env, runner.Cmd{Name: "stat", Args: []string{"-c", "%U:%G:%a:%s:%b:%B", "--", path}})
	if err != nil {
		return swapFileInfo{}, err
	}
	if res.ExitCode != 0 {
		return swapFileInfo{}, nil
	}
	fields := strings.Split(strings.TrimSpace(res.Stdout), ":")
	if len(fields) != 6 {
		return swapFileInfo{}, fmt.Errorf("stat %s: unexpected output %q", path, res.Stdout)
	}
	info := swapFileInfo{Exists: true, Owner: fields[0], Group: fields[1], Mode: normalMode(fields[2])}
	for i, into := range []*int64{&info.SizeBytes, &info.Blocks, &info.BlockBytes} {
		n, err := strconv.ParseInt(fields[3+i], 10, 64)
		if err != nil {
			return swapFileInfo{}, fmt.Errorf("stat %s: unexpected output %q", path, res.Stdout)
		}
		*into = n
	}
	return info, nil
}

type probeResult struct {
	Answered bool
	OK       bool
	Out      string
}

func probe(ctx context.Context, env *Env, c runner.Cmd) probeResult {
	if env.Run == nil {
		return probeResult{}
	}
	res, err := env.Run.Run(ctx, c)
	if err != nil {
		return probeResult{}
	}
	return probeResult{Answered: true, OK: res.ExitCode == 0, Out: res.Stdout}
}

func inContainer(ctx context.Context, env *Env) bool {
	if env.Run == nil {
		return false
	}
	if p := probe(ctx, env, runner.Cmd{Name: "systemd-detect-virt", Args: []string{"--container"}}); p.Answered {
		return p.OK
	}
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if p := probe(ctx, env, runner.Cmd{Name: "test", Args: []string{"-e", marker}}); p.Answered && p.OK {
			return true
		}
	}
	return false
}

func readSwapon(ctx context.Context, env *Env) ([]machine.SwapEntry, bool) {
	if env.Run == nil {
		return nil, false
	}
	p := probe(ctx, env, machine.SwaponCmd())
	if !p.Answered || !p.OK {
		return nil, false
	}
	return machine.ParseSwapon(p.Out), true
}

func splitSwapon(entries []machine.SwapEntry, path string) (ours bool, foreign []machine.SwapEntry) {
	for _, e := range entries {
		if e.Name == path {
			ours = true
			continue
		}
		foreign = append(foreign, e)
	}
	return ours, foreign
}

func describeSwapEntries(entries []machine.SwapEntry) string {
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.SizeBytes > 0 {
			parts = append(parts, bytesIEC(e.SizeBytes)+" on "+e.Name)
			continue
		}
		parts = append(parts, e.Name)
	}
	return "swap already: " + strings.Join(parts, ", ")
}

func mountOf(ctx context.Context, env *Env, path string) (fstype, source string, ok bool) {
	p := probe(ctx, env, runner.Cmd{Name: "findmnt", Args: []string{"-n", "-o", "FSTYPE,SOURCE", "-T", path}})
	if !p.Answered || !p.OK {
		return "", "", false
	}
	fields := strings.Fields(p.Out)
	if len(fields) < 2 {
		return "", "", false
	}
	return fields[0], fields[1], true
}
