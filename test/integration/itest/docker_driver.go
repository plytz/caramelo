//go:build integration

package itest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const dockerHostIP = "127.0.0.1"

type dockerDriver struct {
	m *Machine

	ports   map[string]int
	ip      string
	cpuArch string
}

func newDockerDriver(m *Machine) *dockerDriver {
	d := &dockerDriver{m: m}
	m.drv = d
	return d
}

func (m *Machine) docker() *dockerDriver {
	d, _ := m.drv.(*dockerDriver)
	return d
}

func (d *dockerDriver) target() string { return TargetDocker }

func (d *dockerDriver) user() string { return LoginUser }

func (d *dockerDriver) home() string { return LoginHome }

func (d *dockerDriver) image(ctx context.Context) (string, error) {
	if d.m.Kind == KindLaptop {
		return EnsureImage(ctx, ImageLaptop)
	}
	if d.m.State == StateProvisioned {
		return EnsureProvisionedImage(ctx)
	}
	return EnsureImage(ctx, ImageMachine)
}

func (d *dockerDriver) start(ctx context.Context) error {
	m := d.m
	l := m.lab
	ctx, cancel := context.WithTimeout(ctx, imageBuildTimeout+l.budget.Boot)
	defer cancel()

	image, err := d.image(ctx)
	if err != nil {
		return err
	}
	args := []string{"run", "-d",
		"--name", m.Name,
		"--hostname", m.Hostname,
		"--network", l.Network,
		"--network-alias", m.Alias,
		"--label", label(),
		"--label", SuiteLabel + "=" + l.Suite,
	}
	if m.Hostname != m.Alias {
		args = append(args, "--network-alias", m.Hostname)
	}
	if m.Kind == KindMachine {
		if err := createVolume(ctx, m.Volume); err != nil {
			return err
		}
		if m.State == StateProvisioned {
			if err := restoreProvisionedVolume(ctx, m.Volume); err != nil {
				return err
			}
		}
		args = append(args,
			"--privileged", "--cgroupns=private",
			"--tmpfs", "/run", "--tmpfs", "/run/lock", "--tmpfs", "/tmp:exec,mode=1777,size=512m",
			"-v", m.Volume+":"+DockerDataDir,
		)
		for _, p := range l.ports {
			args = append(args, "-p", dockerHostIP+"::"+p.String())
		}
	}
	args = append(args, image)

	id, err := docker(ctx, args...)
	if err != nil {
		return fmt.Errorf("start %s: %w", m.Name, err)
	}
	m.ID = id
	l.logf("itest: started %s (%s) from %s", m.Name, m.Hostname, image)

	if m.Kind == KindMachine {
		d.ports, err = publishedPorts(ctx, m.Name)
		if err != nil {
			return err
		}
		if err := waitForSystemd(ctx, m, l.budget.Boot); err != nil {
			return err
		}
	}
	ip, err := containerIP(ctx, m.Name, l.Network)
	if err != nil {
		return err
	}
	d.ip = ip
	return nil
}

func (d *dockerDriver) execArgs(user, cmd string) []string {
	home := LoginHome
	if user == "root" {
		home = "/root"
	}
	return []string{"exec",
		"-u", user,
		"-w", home,
		"-e", "HOME=" + home,
		"-e", "USER=" + user,
		"-e", "LANG=C.UTF-8",
		d.m.Name, "sh", "-c", cmd,
	}
}

func (d *dockerDriver) run(ctx context.Context, user, cmd string) (Result, error) {
	m := d.m
	m.lab.logf("[%s] $ %s", m.Alias, cmd)
	start := time.Now()
	res, err := runDocker(ctx, nil, nil, d.execArgs(user, cmd)...)
	out := Result{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode}
	if err != nil {
		return out, fmt.Errorf("docker exec %s: %w", m.Name, err)
	}
	if out.ExitCode != 0 && isDockerFailure(out.Stderr) {
		return out, fmt.Errorf("docker exec %s: %s", m.Name, strings.TrimSpace(out.Stderr))
	}
	m.lab.logf("[%s] exit %d after %s", m.Alias, out.ExitCode, time.Since(start).Round(time.Millisecond))
	return out, nil
}

func (d *dockerDriver) copyIn(ctx context.Context, local, remote, owner string) error {
	m := d.m
	m.lab.logf("[%s] cp %s -> %s", m.Alias, local, remote)
	if _, err := docker(ctx, "cp", local, m.Name+":"+remote); err != nil {
		return fmt.Errorf("copy %s to %s:%s: %w", local, m.Name, remote, err)
	}
	res, err := d.run(ctx, "root", "chown -R "+owner+":"+owner+" "+ShellQuote(remote))
	if err != nil {
		return fmt.Errorf("copy %s to %s:%s: %w", local, m.Name, remote, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("copy %s to %s:%s: chown: %s", local, m.Name, remote, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func (d *dockerDriver) fetch(ctx context.Context, remote, local string) error {
	m := d.m
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return fmt.Errorf("fetch %s:%s to %s: %w", m.Name, remote, local, err)
	}
	m.lab.logf("[%s] cp %s <- %s", m.Alias, local, remote)
	if _, err := docker(ctx, "cp", m.Name+":"+remote, local); err != nil {
		return fmt.Errorf("fetch %s:%s to %s: %w", m.Name, remote, local, err)
	}
	return nil
}

func (d *dockerDriver) address(ctx context.Context) (string, error) {
	if d.ip != "" {
		return d.ip, nil
	}
	ip, err := containerIP(ctx, d.m.Name, d.m.lab.Network)
	if err != nil {
		return "", fmt.Errorf("address of %s: %w", d.m.Alias, err)
	}
	d.ip = ip
	return ip, nil
}

func (d *dockerDriver) hostIP() string { return dockerHostIP }

func (d *dockerDriver) hostPort(port int, proto string) (int, error) {
	key := strconv.Itoa(port) + "/" + proto
	if p, ok := d.ports[key]; ok {
		return p, nil
	}
	return 0, fmt.Errorf("%s publishes no %s (published: %s)", d.m.Alias, key, strings.Join(d.portKeys(), ", "))
}

func (d *dockerDriver) portKeys() []string {
	var out []string
	for k := range d.ports {
		out = append(out, k)
	}
	return out
}

func (d *dockerDriver) arch(ctx context.Context) (string, error) {
	if d.cpuArch != "" {
		return d.cpuArch, nil
	}
	res, err := d.run(ctx, LoginUser, "uname -m")
	if err != nil {
		return "", fmt.Errorf("architecture of %s: %w", d.m.Alias, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("architecture of %s: uname exit %d: %s",
			d.m.Alias, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	arch := GoArch(res.Stdout)
	if arch == "" {
		return "", fmt.Errorf("architecture of %s: uname -m said %q", d.m.Alias, strings.TrimSpace(res.Stdout))
	}
	d.cpuArch = arch
	return arch, nil
}

func (d *dockerDriver) ensure(ctx context.Context, state string) error {
	if d.m.ID != "" && d.m.State == state {
		return nil
	}
	return d.reset(ctx, state)
}

func (d *dockerDriver) reset(ctx context.Context, state string) error {
	m := d.m
	if err := removeContainer(ctx, m.Name); err != nil {
		return fmt.Errorf("reset %s: %w", m.Alias, err)
	}
	if m.Volume != "" {
		if err := removeVolume(ctx, m.Volume); err != nil {
			return fmt.Errorf("reset %s: %w", m.Alias, err)
		}
	}
	m.State = state
	m.Hostname = m.Alias
	if state == StateProvisioned {
		m.Hostname = ProvisionedHostname
	}
	m.ID = ""
	d.ports = nil
	d.ip = ""
	d.cpuArch = ""
	if err := d.start(ctx); err != nil {
		return fmt.Errorf("reset %s to %q: %w", m.Alias, state, err)
	}
	return nil
}

func (d *dockerDriver) restart(ctx context.Context) error {
	m := d.m
	if _, err := docker(ctx, "restart", "-t", "30", m.Name); err != nil {
		return fmt.Errorf("restart %s: %w", m.Alias, err)
	}
	ports, err := publishedPorts(ctx, m.Name)
	if err != nil {
		return fmt.Errorf("restart %s: %w", m.Alias, err)
	}
	d.ports = ports
	ip, err := containerIP(ctx, m.Name, m.lab.Network)
	if err != nil {
		return fmt.Errorf("restart %s: %w", m.Alias, err)
	}
	d.ip = ip
	if err := waitForSystemd(ctx, m, m.Budget().Boot); err != nil {
		return fmt.Errorf("restart %s: %w", m.Alias, err)
	}
	return nil
}

func (d *dockerDriver) relogin(ctx context.Context) error { return nil }

func (d *dockerDriver) pty(ctx context.Context, dir string, env []string, line string) (PTYResult, error) {
	m := d.m
	args := []string{"exec", "-t",
		"-u", m.User(),
		"-w", dir,
		"-e", "HOME=" + m.Home(),
		"-e", "USER=" + m.User(),
		"-e", "TERM=xterm-256color",
		"-e", "COLUMNS=" + strconv.Itoa(PTYCols),
		"-e", "LINES=" + strconv.Itoa(PTYRows),
	}
	for _, kv := range env {
		args = append(args, "-e", kv)
	}
	args = append(args, m.Name, "sh", "-c", line)

	m.lab.logf("[%s] pty $ %s", m.Alias, line)
	res, err := runDocker(ctx, nil, nil, args...)
	out := PTYResult{Output: res.Stdout + res.Stderr, ExitCode: res.ExitCode}
	if err != nil {
		return out, fmt.Errorf("docker exec -t %s: %w", m.Name, err)
	}
	if out.ExitCode != 0 && isDockerFailure(res.Stderr) {
		return out, fmt.Errorf("docker exec -t %s: %s", m.Name, strings.TrimSpace(res.Stderr))
	}
	return out, nil
}

func (d *dockerDriver) collectLogs(ctx context.Context) (string, error) {
	res, err := d.run(ctx, "root", "journalctl -b --no-pager")
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("journalctl exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

func (d *dockerDriver) remove(ctx context.Context) error {
	m := d.m
	if err := removeContainer(ctx, m.Name); err != nil {
		return err
	}
	if m.Volume != "" {
		return removeVolume(ctx, m.Volume)
	}
	return nil
}
