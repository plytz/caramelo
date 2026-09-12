//go:build integration && e2e

package itest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/plytz/caramelo/test/e2e/inventory"
	"github.com/plytz/caramelo/test/e2e/sshrun"
)

type sshDriver struct {
	m       *Machine
	entry   inventory.Machine
	path    string
	hook    []string
	dir     string
	session *sshrun.Session

	loginHome string
	hostname  string
	cpuArch   string
	prepared  bool
	onBox     string
}

var (
	sshBoxesMu sync.Mutex
	sshBoxes   = map[string]*sshDriver{}
)

func newSSHDriver(m *Machine, entry inventory.Machine, path string, hook []string) (*sshDriver, error) {
	sshBoxesMu.Lock()
	defer sshBoxesMu.Unlock()
	if d, ok := sshBoxes[entry.Name]; ok {
		d.adopt(m, entry, path, hook)
		return d, nil
	}
	dir, err := os.MkdirTemp("", "caramelo-e2e-"+sanitize(entry.Name)+"-")
	if err != nil {
		return nil, fmt.Errorf("create the ssh work directory for %s: %w", entry.Name, err)
	}
	d := &sshDriver{m: m, entry: entry, path: path, hook: hook, dir: dir}
	m.drv = d
	sshBoxes[entry.Name] = d
	return d, nil
}

func (d *sshDriver) adopt(m *Machine, entry inventory.Machine, path string, hook []string) {
	changed := entry != d.entry
	d.m = m
	d.path = path
	d.hook = hook
	m.drv = d
	if changed {
		d.entry = entry
		d.onBox = ""
		d.prepared = false
		d.hostname = ""
		d.disconnect()
		return
	}
	if d.hostname != "" {
		m.Hostname = d.hostname
	}
}

func (d *sshDriver) host() sshrun.Host {
	return sshrun.Host{
		Name:    d.entry.Name,
		Addr:    d.entry.Host,
		Port:    d.entry.Port,
		User:    d.entry.User,
		Key:     d.entry.Key,
		HostKey: d.entry.HostKey,
		Arch:    d.entry.Arch,
	}
}

func (d *sshDriver) target() string { return TargetSSH }

func (d *sshDriver) user() string { return d.entry.User }

func (d *sshDriver) home() string {
	if d.loginHome != "" {
		return d.loginHome
	}
	return "/home/" + d.entry.User
}

func (d *sshDriver) hostIP() string { return d.entry.Host }

func (d *sshDriver) hostPort(port int, proto string) (int, error) { return port, nil }

func (d *sshDriver) address(ctx context.Context) (string, error) { return d.entry.Host, nil }

func (d *sshDriver) arch(ctx context.Context) (string, error) {
	if d.cpuArch != "" {
		return d.cpuArch, nil
	}
	res, err := d.run(ctx, d.user(), "uname -m")
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

func (d *sshDriver) start(ctx context.Context) error {
	if err := d.connect(ctx); err != nil {
		return err
	}
	if d.hostname == "" {
		if err := d.describe(ctx); err != nil {
			return err
		}
	}
	d.m.Hostname = d.hostname
	if !d.prepared {
		if err := d.prepare(ctx); err != nil {
			return err
		}
	}
	return d.ensure(ctx, d.m.State)
}

func (d *sshDriver) connect(ctx context.Context) error {
	if d.session != nil {
		return nil
	}
	h := d.host()
	waitCtx, cancel := context.WithTimeout(ctx, d.budget().Boot)
	defer cancel()
	if err := sshrun.WaitForSSH(waitCtx, d.dir, h, 0); err != nil {
		return fmt.Errorf("reach %s over ssh: %w", d.m.Alias, err)
	}
	session, err := sshrun.Open(ctx, d.dir, h)
	if err != nil {
		return fmt.Errorf("open a session to %s: %w", d.m.Alias, err)
	}
	session.Rows, session.Cols = PTYRows, PTYCols
	d.session = session
	return nil
}

func (d *sshDriver) relogin(ctx context.Context) error {
	d.disconnect()
	if err := d.connect(ctx); err != nil {
		return fmt.Errorf("log in to %s again so the login user carries the groups it was just given: %w", d.m.Alias, err)
	}
	return nil
}

func (d *sshDriver) disconnect() {
	if d.session == nil {
		return
	}
	if err := d.session.Close(); err != nil {
		d.m.lab.logf("[%s] close the ssh session: %v", d.m.Alias, err)
	}
	d.session = nil
}

func (d *sshDriver) describe(ctx context.Context) error {
	d.loginHome = ""
	d.hostname = ""
	d.cpuArch = ""

	home, err := d.readLine(ctx, `echo "$HOME"`)
	if err != nil {
		return fmt.Errorf("the home directory of %s on %s: %w", d.user(), d.m.Alias, err)
	}
	d.loginHome = home
	name, err := d.readLine(ctx, "hostname")
	if err != nil {
		return fmt.Errorf("the hostname of %s: %w", d.m.Alias, err)
	}
	d.hostname = name
	d.m.Hostname = name
	if _, err := d.arch(ctx); err != nil {
		return err
	}
	return nil
}

func (d *sshDriver) readLine(ctx context.Context, cmd string) (string, error) {
	res, err := d.run(ctx, d.user(), cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%s: exit %d: %s", cmd, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		return "", fmt.Errorf("%s: no output", cmd)
	}
	return out, nil
}

func (d *sshDriver) prepare(ctx context.Context) error {
	if err := WaitForApt(ctx, d.m); err != nil {
		return fmt.Errorf("prepare %s: %w", d.m.Alias, err)
	}
	if err := EnsureCurlOn(d.m); err != nil {
		return fmt.Errorf("prepare %s: %w", d.m.Alias, err)
	}
	d.prepared = true
	return nil
}

func (d *sshDriver) budget() Budgets { return d.m.lab.budget }

func (d *sshDriver) withCeiling(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d.budget().For(DefaultTimeout))
}

func (d *sshDriver) run(ctx context.Context, user, cmd string) (Result, error) {
	if d.session == nil {
		return Result{}, fmt.Errorf("run on %s: there is no ssh session", d.m.Alias)
	}
	ctx, cancel := d.withCeiling(ctx)
	defer cancel()
	d.m.lab.logf("[%s] $ %s", d.m.Alias, cmd)
	start := time.Now()
	res, err := d.session.RunAs(ctx, user, cmd)
	out := Result{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode}
	if err != nil {
		return out, fmt.Errorf("ssh %s: %w", d.m.Alias, err)
	}
	d.m.lab.logf("[%s] exit %d after %s", d.m.Alias, out.ExitCode, time.Since(start).Round(time.Millisecond))
	return out, nil
}

func (d *sshDriver) copyIn(ctx context.Context, local, remote, owner string) error {
	if d.session == nil {
		return fmt.Errorf("copy %s to %s: there is no ssh session", local, d.m.Alias)
	}
	if owner == "" {
		owner = d.user()
	}
	d.m.lab.logf("[%s] cp %s -> %s (%s)", d.m.Alias, local, remote, owner)
	info, err := os.Stat(local)
	if err != nil {
		return fmt.Errorf("copy %s to %s: %w", local, d.m.Alias, err)
	}
	mode := "0" + strconv.FormatUint(uint64(info.Mode().Perm()), 8)
	staged := filepath.Join(sshrun.StagingDir, "caramelo-itest-"+stagingID()+"-"+filepath.Base(local))
	if err := d.session.Copy(ctx, local, staged); err != nil {
		return fmt.Errorf("copy %s to %s: %w", local, d.m.Alias, err)
	}
	install := fmt.Sprintf("install -m %s -o %s -g \"$(id -gn %s)\" %s %s; rc=$?; rm -f %s; exit $rc",
		mode, owner, owner, sshrun.Quote(staged), sshrun.Quote(remote), sshrun.Quote(staged))
	res, err := d.run(ctx, "root", install)
	if err != nil {
		return fmt.Errorf("copy %s to %s:%s: %w", local, d.m.Alias, remote, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("copy %s to %s:%s: install: exit %d: %s",
			local, d.m.Alias, remote, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func stagingID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano()%1e8, 16)
	}
	return hex.EncodeToString(b[:])
}

func (d *sshDriver) fetch(ctx context.Context, remote, local string) error {
	if d.session == nil {
		return fmt.Errorf("fetch %s from %s: there is no ssh session", remote, d.m.Alias)
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return fmt.Errorf("fetch %s:%s to %s: %w", d.m.Alias, remote, local, err)
	}
	d.m.lab.logf("[%s] cp %s <- %s", d.m.Alias, local, remote)
	if err := d.session.Fetch(ctx, remote, local); err != nil {
		return fmt.Errorf("fetch %s:%s to %s: %w", d.m.Alias, remote, local, err)
	}
	return nil
}

func (d *sshDriver) restart(ctx context.Context) error {
	m := d.m
	m.lab.logf("[%s] rebooting", m.Alias)
	start := time.Now()
	was, err := d.bootID(ctx)
	if err != nil {
		m.lab.logf("[%s] the boot id is unreadable, so the reboot is only watched over ssh: %v", m.Alias, err)
		was = ""
	}
	if _, err := d.run(ctx, "root", "systemctl reboot"); err != nil && !unreachable(err) {
		return fmt.Errorf("reboot %s: %w", m.Alias, err)
	}
	d.disconnect()
	if err := d.waitForBoot(ctx, was); err != nil {
		return fmt.Errorf("reboot %s: %w", m.Alias, err)
	}
	m.lab.logf("[%s] back after %s", m.Alias, time.Since(start).Round(time.Second))
	return waitForSystemd(ctx, m, d.budget().Boot)
}

func (d *sshDriver) bootID(ctx context.Context) (string, error) {
	res, err := d.run(ctx, d.user(), "cat /proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("read the boot id of %s: exit %d: %s",
			d.m.Alias, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	id := strings.TrimSpace(res.Stdout)
	if id == "" {
		return "", fmt.Errorf("read the boot id of %s: no output", d.m.Alias)
	}
	return id, nil
}

func (d *sshDriver) waitForBoot(ctx context.Context, leaving string) error {
	h := d.host()
	deadline := time.Now().Add(d.budget().Boot)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
		waitCtx, cancel := context.WithDeadline(ctx, deadline)
		err := sshrun.WaitForSSH(waitCtx, d.dir, h, 0)
		cancel()
		if err != nil {
			return err
		}
		if err := d.connect(ctx); err != nil {
			return err
		}
		if leaving == "" {
			break
		}
		id, err := d.bootID(ctx)
		if err == nil && id != leaving {
			break
		}
		if err != nil {
			d.m.lab.logf("[%s] answered but the boot id is not readable yet: %v", d.m.Alias, err)
		} else {
			d.m.lab.logf("[%s] answered from the boot it was asked to leave; waiting for the new one", d.m.Alias)
		}
		d.disconnect()
		if time.Now().After(deadline) {
			return fmt.Errorf("%s never left boot %s within %s", d.m.Alias, leaving, d.budget().Boot)
		}
	}
	return d.describe(ctx)
}

func (d *sshDriver) pty(ctx context.Context, dir string, env []string, line string) (PTYResult, error) {
	if d.session == nil {
		return PTYResult{}, fmt.Errorf("pty on %s: there is no ssh session", d.m.Alias)
	}
	script := line
	if dir != "" {
		script = "cd " + sshrun.Quote(dir) + " && " + line
	}
	d.m.lab.logf("[%s] pty $ %s", d.m.Alias, script)
	res, err := d.session.RunTTY(ctx, ptyEnv(append([]string{}, env...)), script)
	out := PTYResult{Output: res.Stdout, ExitCode: res.ExitCode}
	if err != nil {
		return out, fmt.Errorf("ssh -tt %s: %w", d.m.Alias, err)
	}
	return out, nil
}

func (d *sshDriver) collectLogs(ctx context.Context) (string, error) {
	res, err := d.run(ctx, "root", "journalctl -b --no-pager")
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("journalctl exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

func (d *sshDriver) remove(ctx context.Context) error { return nil }
