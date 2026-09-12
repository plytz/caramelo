//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/setup"
)

func ProvisionForTheLab(m *Machine) error { return provision(m, false) }

func SetupMemberForJoin(m *Machine, private bool) error { return provision(m, private) }

func MemberSetupCommandFor(home string) string {
	return fmt.Sprintf(
		"sudo %s server setup --yes --json --authorized-keys %s/.ssh/authorized_keys "+
			"--private --acme-ca %s --acme-email %s",
		RemoteBin, home, ACMEDirectory, ACMEEmail)
}

func provision(m *Machine, private bool) error {
	budget := m.Budget()
	ctx, cancel := context.WithTimeout(context.Background(), budget.Setup)
	defer cancel()

	bin, err := BinaryFor(ctx, m)
	if err != nil {
		return err
	}
	if _, err := EnsureGossFor(ctx, m); err != nil {
		return err
	}
	if err := m.Copy(ctx, bin, RemoteBin); err != nil {
		return err
	}
	if err := mustSucceedOn(ctx, m, "chmod +x "+RemoteBin); err != nil {
		return err
	}

	cmd := MemberSetupCommandFor(m.Home())
	if !private {
		kp, keyErr := LabPeerKey()
		if keyErr != nil {
			return fmt.Errorf("the lab identity's key: %w", keyErr)
		}
		cmd = SetupCommandFor(m.Home(), kp.Public)
	}
	res, err := m.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("server setup on %s: %w", m.Alias, err)
	}
	var report setup.Report
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &report); err != nil {
		return fmt.Errorf("server setup on %s: exit %d, stdout is not a report: %w\nstdout:\n%s\nstderr:\n%s",
			m.Alias, res.ExitCode, err, res.Stdout, res.Stderr)
	}
	if res.ExitCode != 0 || report.Failed != 0 {
		return fmt.Errorf("server setup on %s: exit %d, %d step(s) failed\n%s",
			m.Alias, res.ExitCode, report.Failed, res.Stdout)
	}
	if private {
		if err := waitForDaemonUser(ctx, m); err != nil {
			return err
		}
	} else if err := WaitForPort(ctx, m, CarameloSSHPort); err != nil {
		return err
	}
	return m.Relogin(ctx)
}

func waitForDaemonUser(ctx context.Context, m *Machine) error {
	cmd := AsUserSession(m, CarameloUser, "systemctl --user is-active caramelod")
	deadline := time.Now().Add(m.Budget().For(2 * time.Minute))
	var last string
	for time.Now().Before(deadline) {
		res, err := m.Run(ctx, cmd)
		if err == nil && res.ExitCode == 0 {
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = strings.TrimSpace(res.Stdout + res.Stderr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("caramelod on %s did not come up: %s", m.Alias, last)
}
