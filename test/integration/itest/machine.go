//go:build integration

package itest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type Machine struct {
	Name     string
	Role     string
	Alias    string
	Kind     string
	State    string
	Hostname string
	Volume   string
	ID       string

	lab *Lab
	drv driver
}

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

func (m *Machine) User() string { return m.drv.user() }

func (m *Machine) Home() string { return m.drv.home() }

func (m *Machine) Target() string { return m.drv.target() }

func (m *Machine) Budget() Budgets { return m.lab.Budget() }

func (m *Machine) Run(ctx context.Context, cmd string) (Result, error) {
	return m.RunAs(ctx, m.User(), cmd)
}

func (m *Machine) RunAsRoot(ctx context.Context, cmd string) (Result, error) {
	return m.RunAs(ctx, "root", cmd)
}

func (m *Machine) RunAs(ctx context.Context, user, cmd string) (Result, error) {
	return m.drv.run(ctx, user, cmd)
}

func (m *Machine) MustRun(t testing.TB, cmd string) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), m.Budget().For(DefaultTimeout))
	defer cancel()
	res, err := m.Run(ctx, cmd)
	if err != nil {
		t.Fatalf("[%s] %s: %v\nstdout:\n%sstderr:\n%s", m.Alias, cmd, err, res.Stdout, res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Fatalf("[%s] %s: exit %d\nstdout:\n%sstderr:\n%s", m.Alias, cmd, res.ExitCode, res.Stdout, res.Stderr)
	}
	return res
}

func (m *Machine) Copy(ctx context.Context, local, remote string) error {
	return m.drv.copyIn(ctx, local, remote, m.User())
}

func (m *Machine) CopyAsRoot(ctx context.Context, local, remote string) error {
	return m.drv.copyIn(ctx, local, remote, "root")
}

func (m *Machine) Fetch(ctx context.Context, remote, local string) error {
	return m.drv.fetch(ctx, remote, local)
}

func (m *Machine) WaitFor(ctx context.Context, cmd string, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	var last string
	for attempt := 1; ; attempt++ {
		res, err := m.Run(ctx, cmd)
		if err == nil && res.ExitCode == 0 {
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for %q on %s: %w after %d attempt(s); last: %s",
				cmd, m.Alias, ctx.Err(), attempt, last)
		case <-time.After(interval):
		}
	}
}

func (m *Machine) CollectLogs(t testing.TB, dir string) error {
	if t != nil {
		t.Helper()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("collect logs for %s: %w", m.Alias, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.Budget().For(DefaultTimeout))
	defer cancel()
	logs, err := m.drv.collectLogs(ctx)
	if err != nil {
		return fmt.Errorf("collect logs for %s: %w", m.Alias, err)
	}
	path := filepath.Join(dir, m.Alias+".journal.log")
	if err := os.WriteFile(path, []byte(logs), 0o644); err != nil {
		return fmt.Errorf("collect logs for %s: %w", m.Alias, err)
	}
	if t != nil {
		t.Logf("[%s] journal saved to %s (%d bytes)", m.Alias, path, len(logs))
	}
	return nil
}

func (m *Machine) Address(ctx context.Context) (string, error) { return m.drv.address(ctx) }

func (m *Machine) MustAddress(t testing.TB) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ip, err := m.Address(ctx)
	if err != nil {
		t.Fatalf("itest: %v", err)
	}
	return ip
}

func (m *Machine) HostIP() string { return m.drv.hostIP() }

func (m *Machine) HostPort(port int) (int, error) { return m.HostPortProto(port, "tcp") }

func (m *Machine) HostPortProto(port int, proto string) (int, error) {
	return m.drv.hostPort(port, proto)
}

func (m *Machine) HostAddrProto(port int, proto string) (string, error) {
	p, err := m.HostPortProto(port, proto)
	if err != nil {
		return "", err
	}
	return m.HostIP() + ":" + strconv.Itoa(p), nil
}

func (m *Machine) MustHostPort(t testing.TB, port int) int {
	t.Helper()
	p, err := m.HostPort(port)
	if err != nil {
		t.Fatalf("itest: %v", err)
	}
	return p
}

func (m *Machine) HostAddr(t testing.TB, port int) string {
	t.Helper()
	return m.HostAddrOf(t, port, "tcp")
}

func (m *Machine) HostAddrOf(t testing.TB, port int, proto string) string {
	t.Helper()
	addr, err := m.HostAddrProto(port, proto)
	if err != nil {
		t.Fatalf("itest: %v", err)
	}
	return addr
}

func (m *Machine) Relogin(ctx context.Context) error { return m.drv.relogin(ctx) }

func (m *Machine) Arch(ctx context.Context) (string, error) { return m.drv.arch(ctx) }

func (m *Machine) MustArch(t testing.TB) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	arch, err := m.Arch(ctx)
	if err != nil {
		t.Fatalf("itest: %v", err)
	}
	return arch
}

func (m *Machine) ClockOffset(ctx context.Context) (time.Duration, error) {
	before := time.Now()
	res, err := m.Run(ctx, "date -u +%s.%N")
	after := time.Now()
	if err != nil {
		return 0, fmt.Errorf("itest: read the clock of %s: %w", m.Alias, err)
	}
	if res.ExitCode != 0 {
		return 0, fmt.Errorf("itest: read the clock of %s: exit %d: %s",
			m.Alias, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	remote, err := parseEpoch(res.Stdout)
	if err != nil {
		return 0, fmt.Errorf("itest: read the clock of %s: %w", m.Alias, err)
	}
	here := before.Add(after.Sub(before) / 2)
	return remote.Sub(here), nil
}

func (m *Machine) ClockOffsetOrZero(t testing.TB, ctx context.Context) time.Duration {
	t.Helper()
	offset, err := m.ClockOffset(ctx)
	if err != nil {
		t.Logf("%v; the two clocks are treated as one", err)
		return 0
	}
	return offset
}

func parseEpoch(out string) (time.Time, error) {
	text := strings.TrimSpace(out)
	secText, fracText, _ := strings.Cut(text, ".")
	sec, err := strconv.ParseInt(strings.TrimSpace(secText), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a unix time: %w", text, err)
	}
	nsec := int64(0)
	if fracText = strings.TrimSpace(fracText); fracText != "" {
		if len(fracText) > 9 {
			fracText = fracText[:9]
		}
		for len(fracText) < 9 {
			fracText += "0"
		}
		if nsec, err = strconv.ParseInt(fracText, 10, 64); err != nil {
			return time.Time{}, fmt.Errorf("%q is not a unix time: %w", text, err)
		}
	}
	return time.Unix(sec, nsec).UTC(), nil
}

func HomeOf(m *Machine) string { return m.Home() }

func UserOf(m *Machine) string { return m.User() }

func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func AsUser(m *Machine, user, cmd string) string {
	uid := "$(id -u " + user + ")"
	home := "$(getent passwd " + user + " | cut -d: -f6)"
	return fmt.Sprintf("sudo -n -u %s env HOME=%s USER=%s LOGNAME=%s XDG_RUNTIME_DIR=/run/user/%s "+
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%s/bus DOCKER_HOST=unix:///run/user/%s/docker.sock "+
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin sh -c %s",
		user, home, user, user, uid, uid, uid, ShellQuote(cmd))
}

func waitForSystemd(ctx context.Context, m *Machine, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		res, err := m.RunAsRoot(ctx, "systemctl is-system-running")
		if err == nil {
			state := strings.TrimSpace(res.Stdout)
			if state == "running" {
				return nil
			}
			if state == "degraded" {
				failed, _ := m.RunAsRoot(ctx, "systemctl --failed --no-legend --no-pager")
				m.lab.logf("[%s] systemd is degraded; failed units: %s",
					m.Alias, strings.Join(strings.Fields(failed.Stdout), " "))
				return nil
			}
			last = state
			if last == "" {
				last = strings.TrimSpace(res.Stderr)
			}
		} else {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("systemd on %s: %w (last: %s)", m.Alias, ctx.Err(), last)
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("systemd on %s never came up within %s (last: %s)", m.Alias, timeout, last)
}

func WaitForPort(ctx context.Context, m *Machine, port int) error {
	cmd := fmt.Sprintf("ss -lnt 2>/dev/null | grep -q ':%d ' || ss -lnu 2>/dev/null | grep -q ':%d '", port, port)
	return m.WaitFor(ctx, cmd, time.Second)
}
