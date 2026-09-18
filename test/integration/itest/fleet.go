//go:build integration

package itest

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/serverconfig"
)

func PrepareMembers(l *Lab, state string) error {
	members := l.Members()
	if len(members) == 0 {
		return fmt.Errorf("no machine plays the %q role in suite %s", RoleMember, l.Suite)
	}
	for _, m := range members {
		if err := Reset(m, state); err != nil {
			return fmt.Errorf("reset %s to %q: %w", m.Alias, state, err)
		}
	}
	return nil
}

func ReleaseMembers(l *Lab) error { return PrepareMembers(l, StateClean) }

func SSHTarget(ctx context.Context, m *Machine) (string, error) {
	addr, err := m.Address(ctx)
	if err != nil {
		return "", err
	}
	return UserOf(m) + "@" + addr, nil
}

func HubEndpoint(ctx context.Context, hub *Machine) (string, error) {
	addr, err := hub.Address(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d", addr, VPNPort), nil
}

func MemberNames(n int) []string {
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf("m%d", i))
	}
	return out
}

func FleetTimeout(d time.Duration) time.Duration { return Scale(d) }

func WaitForCaramelod(ctx context.Context, m *Machine) error {
	if public, err := apiListensPublicly(ctx, m); err == nil && !public {
		return waitForDaemonUser(ctx, m)
	}
	if err := WaitForPort(ctx, m, CarameloSSHPort); err != nil {
		return fmt.Errorf("caramelod on %s: %w", m.Alias, err)
	}
	return nil
}

func apiListensPublicly(ctx context.Context, m *Machine) (bool, error) {
	res, err := m.Run(ctx, "sudo -n sed -n 's/^ *api_listen: *//p' /etc/caramelo/config.yaml 2>/dev/null || true")
	if err != nil {
		return false, fmt.Errorf("read api_listen of %s: %w", m.Alias, err)
	}
	got := strings.Trim(strings.TrimSpace(res.Stdout), `"'`)
	if got == "" {
		return false, fmt.Errorf("%s does not say where its API listens", m.Alias)
	}
	return got == serverconfig.APIListenPublic || got == serverconfig.APIListenBoth, nil
}

func ServerFleet(ctx context.Context, m *Machine) (string, error) {
	res, err := m.Run(ctx, "sudo -n sed -n 's/^ *fleet: *//p' /etc/caramelo/config.yaml 2>/dev/null || true")
	if err != nil {
		return "", fmt.Errorf("read the fleet of %s: %w", m.Alias, err)
	}
	return strings.Trim(strings.TrimSpace(res.Stdout), `"'`), nil
}

func FleetRole(ctx context.Context, m *Machine) (string, error) {
	res, err := m.Run(ctx, "sudo -n sed -n 's/^ *role: *//p' /etc/caramelo/config.yaml 2>/dev/null || true")
	if err != nil {
		return "", fmt.Errorf("read the fleet role of %s: %w", m.Alias, err)
	}
	return strings.TrimSpace(res.Stdout), nil
}

func WaitForApt(ctx context.Context, m *Machine) error {
	limit := Scale(3 * time.Minute)
	deadline := time.Now().Add(limit)
	const busy = "pgrep -a -x '(apt|apt-get|dpkg|unattended-upgr)' 2>/dev/null | " +
		"grep -v unattended-upgrade-shutdown | grep -q ."
	for {
		res, err := m.Run(ctx, busy)
		if err != nil {
			return fmt.Errorf("look for a package manager on %s: %w", m.Alias, err)
		}
		if res.ExitCode != 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("something is still installing packages on %s after %s", m.Alias, limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
