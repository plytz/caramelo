//go:build integration

package itest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func DeployBinary(t testing.TB, m *Machine) {
	t.Helper()
	if err := DeployBinaryTo(m); err != nil {
		t.Fatalf("itest: %v", err)
	}
}

func DeployBinaryTo(m *Machine) error {
	ctx, cancel := context.WithTimeout(context.Background(), m.Budget().For(3*time.Minute))
	defer cancel()
	if err := InstallBinaryOn(ctx, m); err != nil {
		return fmt.Errorf("deploy the binary onto %s: %w", m.Alias, err)
	}
	if err := mustSucceedOn(ctx, m, AsUserSession(m, CarameloUser, "systemctl --user restart caramelod")); err != nil {
		return fmt.Errorf("restart caramelod on %s: %w", m.Alias, err)
	}
	return WaitForCaramelod(ctx, m)
}

func AsUserSession(m *Machine, user, cmd string) string {
	uid := "$(id -u " + user + ")"
	home := "$(getent passwd " + user + " | cut -d: -f6)"
	return fmt.Sprintf("sudo -n -u %s env HOME=%s USER=%s LOGNAME=%s XDG_RUNTIME_DIR=/run/user/%s "+
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%s/bus DOCKER_HOST=unix:///run/user/%s/docker.sock "+
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin sh -c %s",
		user, home, user, user, uid, uid, uid, ShellQuote(cmd))
}

func MustRunAsUser(t testing.TB, m *Machine, user, cmd string) Result {
	t.Helper()
	return m.MustRun(t, AsUserSession(m, user, cmd))
}

func RunAsUser(ctx context.Context, m *Machine, user, cmd string) (Result, error) {
	return m.Run(ctx, AsUserSession(m, user, cmd))
}

func mustSucceedOn(ctx context.Context, m *Machine, cmd string) error {
	res, err := m.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s: exit %d: %s", cmd, res.ExitCode,
			strings.TrimSpace(res.Stderr+res.Stdout))
	}
	return nil
}

type SSHAPIOptions struct {
	Key          string
	User         string
	Host         string
	Port         int
	Extra        []string
	Stdin        io.Reader
	Timeout      time.Duration
	AllowTimeout bool
}

const SSHTimedOut = -1

func SSHAPI(t testing.TB, keyPath, host string, port int, args ...string) Result {
	t.Helper()
	return SSHAPIRun(t, SSHAPIOptions{Key: keyPath, Host: host, Port: port}, args...)
}

func SSHAPIOn(t testing.TB, m *Machine, args ...string) Result {
	t.Helper()
	return SSHAPIRun(t, SSHAPIFor(t, m), args...)
}

func SSHAPIFor(t testing.TB, m *Machine) SSHAPIOptions {
	t.Helper()
	key, _, err := LabSSHKey()
	if err != nil {
		t.Fatalf("itest: %v", err)
	}
	return SSHAPIOptions{
		Key:  key,
		User: CarameloUser,
		Host: m.HostIP(),
		Port: m.MustHostPort(t, CarameloSSHPort),
	}
}

func SSHAPIRun(t testing.TB, o SSHAPIOptions, args ...string) Result {
	t.Helper()
	if o.Key == "" || o.Host == "" {
		t.Fatalf("itest: SSHAPI needs a key and a host (got %+v)", o)
	}
	if o.User == "" {
		o.User = CarameloUser
	}
	if o.Port == 0 {
		o.Port = CarameloSSHPort
	}
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Second
	}
	known := filepath.Join(t.TempDir(), "known_hosts")

	sshArgs := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=" + known,
		"-o", "LogLevel=ERROR",
		"-o", "IdentitiesOnly=yes",
		"-o", "ConnectTimeout=10",
		"-i", o.Key,
		"-p", strconv.Itoa(o.Port),
	}
	sshArgs = append(sshArgs, o.Extra...)
	sshArgs = append(sshArgs, o.User+"@"+o.Host)
	sshArgs = append(sshArgs, args...)

	ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
	defer cancel()
	t.Logf("[api] ssh -p %d %s@%s %s", o.Port, o.User, o.Host, strings.Join(args, " "))
	c := exec.CommandContext(ctx, "ssh", sshArgs...)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr, c.Stdin = &stdout, &stderr, o.Stdin
	err := c.Run()

	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		t.Fatalf("itest: ssh could not run: %v", err)
	}
	if ctx.Err() != nil {
		if !o.AllowTimeout {
			t.Fatalf("itest: ssh %v timed out after %s (stderr: %s)", args, o.Timeout, strings.TrimSpace(res.Stderr))
		}
		t.Logf("[api] still connected after %s", o.Timeout)
		res.ExitCode = SSHTimedOut
		return res
	}
	t.Logf("[api] exit %d", res.ExitCode)
	return res
}
