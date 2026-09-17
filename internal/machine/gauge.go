package machine

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
)

var (
	readFile    = os.ReadFile
	lookupUser  = user.Lookup
	currentUser = user.Current
	geteuid     = os.Geteuid
	hostname    = os.Hostname
	now         = time.Now
)

type Options struct {
	ConfigDir string

	Version string
}

func Gauge(ctx context.Context, run runner.Runner, cfg serverconfig.Config, version string) (*Record, error) {
	return GaugeWith(ctx, run, cfg, Options{Version: version})
}

func GaugeWith(ctx context.Context, run runner.Runner, cfg serverconfig.Config, opts Options) (*Record, error) {
	if run == nil {
		return nil, fmt.Errorf("gauge: no runner")
	}
	configDir := opts.ConfigDir
	if configDir == "" {
		configDir = serverconfig.DefaultConfigDir
	}

	r := &Record{
		GaugedAt: now().UTC(),
		Dirs:     Dirs{Config: configDir, State: cfg.StateDir, Data: cfg.DataDir},
		Reserved: Reserved{MemoryBytes: cfg.Reserve.MemoryBytes, CPU: cfg.Reserve.CPU},
		Caramelo: Caramelo{Version: opts.Version, User: cfg.User, SSHPort: cfg.SSHPort},
	}

	out, err := output(ctx, run, runner.Cmd{Name: "nproc"})
	if err != nil {
		return nil, fmt.Errorf("gauge cpu count: %w", err)
	}
	if r.CPU.Count, err = parseNProc(out); err != nil {
		return nil, fmt.Errorf("gauge cpu count: %w", err)
	}

	b, err := readFile("/proc/meminfo")
	if err != nil {
		return nil, fmt.Errorf("gauge memory: %w", err)
	}
	if r.Memory, err = parseMeminfo(string(b)); err != nil {
		return nil, fmt.Errorf("gauge memory: %w", err)
	}
	if total, managed := ReadSwap(ctx, run, cfg.SwapFilePath()); total > 0 {
		r.Memory.SwapTotalBytes, r.Memory.SwapManaged = total, managed
	}

	b, err = readFile("/etc/os-release")
	if err != nil {
		return nil, fmt.Errorf("gauge os: %w", err)
	}
	osr := parseOSRelease(string(b))
	r.OS.ID = osr["ID"]
	r.OS.VersionID = osr["VERSION_ID"]
	r.OS.Codename = osr["VERSION_CODENAME"]

	if s, err := readFile("/proc/cpuinfo"); err == nil {
		r.CPU.Model = parseCPUModel(string(s))
	}
	if s, err := readFile("/etc/machine-id"); err == nil {
		r.MachineID = strings.TrimSpace(string(s))
	}
	r.Hostname = gaugeHostname(ctx, run)
	if s, err := output(ctx, run, runner.Cmd{Name: "uname", Args: []string{"-r"}}); err == nil {
		r.OS.Kernel = strings.TrimSpace(s)
	}

	if s, err := output(ctx, run, runner.Cmd{Name: "uname", Args: []string{"-m"}}); err == nil {
		r.OS.Hardware = strings.TrimSpace(s)
		r.OS.Arch = GOARCH(r.OS.Hardware)
	}

	if s, err := readFile("/proc/loadavg"); err == nil {
		r.Load = parseLoadavg(string(s))
	}

	if res, err := run.Run(ctx, runner.Cmd{Name: "systemd-detect-virt"}); err == nil {
		if v := strings.TrimSpace(res.Stdout); v != "" {
			r.OS.Virt = v
		}
	}
	if s, err := output(ctx, run, runner.Cmd{Name: "stat", Args: []string{"-fc", "%T", "/sys/fs/cgroup"}}); err == nil {
		r.Cgroup.Version = parseCgroupVersion(s)
	}

	r.DataDir = gaugeDataDir(ctx, run, cfg.DataDir)
	if s, err := output(ctx, run, runner.Cmd{Name: "lsblk", Args: []string{"-J", "-b", "-o", "NAME,TYPE,SIZE,MOUNTPOINTS,FSTYPE,ROTA,MODEL"}}); err == nil {
		if disks, err := parseLsblk(s); err == nil {
			r.Disks = disks
		}
	}
	r.Network = gaugeNetwork(ctx, run)

	if u, err := lookupUser(cfg.User); err == nil {
		if uid, err := strconv.Atoi(u.Uid); err == nil {
			r.Caramelo.UID = uid
			r.Cgroup.Controllers = gaugeControllers(uid)
		}
	}

	r.Docker = gaugeDocker(ctx, run, cfg.User)
	return r, nil
}

func gaugeHostname(ctx context.Context, run runner.Runner) string {
	if s, err := output(ctx, run, runner.Cmd{Name: "hostname"}); err == nil {
		if h := strings.TrimSpace(s); h != "" {
			return h
		}
	}
	h, _ := hostname()
	return h
}

func gaugeDataDir(ctx context.Context, run runner.Runner, dataDir string) Mount {
	m := Mount{Path: dataDir}
	if dataDir == "" {
		return m
	}
	if s, err := output(ctx, run, runner.Cmd{Name: "findmnt", Args: []string{"-J", "-T", dataDir}}); err == nil {
		if source, fstype, _, err := parseFindmntT(s); err == nil {
			m.Source, m.FSType = source, fstype
		}
	}

	if res, err := run.Run(ctx, runner.Cmd{Name: "findmnt", Args: []string{"-M", dataDir}}); err == nil && res.ExitCode == 0 {
		m.OwnMountPoint = true
	}
	if s, err := output(ctx, run, runner.Cmd{Name: "df", Args: []string{"-B1", "--output=source,fstype,size,used,avail,target", dataDir}}); err == nil {
		if d, err := parseDF(s); err == nil {
			m.SizeBytes, m.AvailBytes = d.SizeBytes, d.AvailBytes
			if m.Source == "" {
				m.Source = d.Source
			}
			if m.FSType == "" {
				m.FSType = d.FSType
			}
		}
	}
	return m
}

func gaugeNetwork(ctx context.Context, run runner.Runner) Network {
	var n Network
	if s, err := output(ctx, run, runner.Cmd{Name: "ip", Args: []string{"-j", "route", "get", "1.1.1.1"}}); err == nil {
		if iface, ip, err := parseRouteGet(s); err == nil {
			n.PrimaryIface, n.PrimaryIP = iface, ip
		}
	}
	if s, err := output(ctx, run, runner.Cmd{Name: "ip", Args: []string{"-j", "-4", "addr"}}); err == nil {
		if addrs, err := parseAddrs(s); err == nil {
			n.Addresses = addrs
		}
	}
	return n
}

func gaugeControllers(uid int) []string {
	p := filepath.Join("/sys/fs/cgroup/user.slice",
		fmt.Sprintf("user-%d.slice", uid),
		fmt.Sprintf("user@%d.service", uid),
		"cgroup.controllers")
	b, err := readFile(p)
	if err != nil {
		return nil
	}
	return parseControllers(string(b))
}

func gaugeDocker(ctx context.Context, run runner.Runner, asUser string) Docker {
	var d Docker
	cmdUser := dockerUser(asUser)
	res, err := run.Run(ctx, runner.Cmd{User: cmdUser, Name: "docker", Args: []string{"info", "--format", "json"}})
	if err != nil {
		return d
	}
	if strings.TrimSpace(res.Stdout) == "" {

		d.Installed = true
		if msg := strings.TrimSpace(res.Stderr); msg != "" {
			d.Warnings = append(d.Warnings, firstLine(msg))
		}
		return d
	}
	d, perr := parseDockerInfo(res.Stdout)
	if perr != nil {
		return Docker{Installed: true, Warnings: []string{perr.Error()}}
	}
	if d.ServerVersion == "" && strings.TrimSpace(res.Stderr) != "" {
		d.Warnings = append(d.Warnings, firstLine(strings.TrimSpace(res.Stderr)))
	}
	if s, err := output(ctx, run, runner.Cmd{User: cmdUser, Name: "docker", Args: []string{"version"}}); err == nil {
		d.NetDriver, d.PortDriver = parseDockerVersionDrivers(s)
	}
	return d
}

func dockerUser(asUser string) string {
	if asUser == "" {
		return ""
	}
	if geteuid() == 0 {
		return asUser
	}
	if u, err := currentUser(); err == nil && u.Username == asUser {
		return asUser
	}
	return ""
}

func output(ctx context.Context, run runner.Runner, c runner.Cmd) (string, error) {
	res, err := run.Run(ctx, c)
	if err != nil {
		return "", fmt.Errorf("run %s: %w", c.Name, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%s exited %d: %s", c.Name, res.ExitCode, firstLine(strings.TrimSpace(res.Stderr)))
	}
	return res.Stdout, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
