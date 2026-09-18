//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const gossTimeout = 5 * time.Minute

func RunGoss(t testing.TB, m *Machine, specPath string) {
	t.Helper()
	RunGossWith(t, m, specPath, nil)
}

func RunGossWith(t testing.TB, m *Machine, specPath string, extra map[string]string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), m.Budget().For(gossTimeout))
	defer cancel()

	if err := ensureGossOn(ctx, m); err != nil {
		t.Fatalf("goss: %v", err)
	}
	remoteSpec := "/var/tmp/goss-" + filepath.Base(specPath)
	if err := m.Copy(ctx, specPath, remoteSpec); err != nil {
		t.Fatalf("goss: copy spec %s: %v", specPath, err)
	}

	vars, err := gossVars(ctx, m, extra)
	if err != nil {
		t.Fatalf("goss: %v", err)
	}
	cmd := fmt.Sprintf("sudo -n %s --gossfile %s --vars-inline %s validate --format json",
		GossRemotePath, remoteSpec, ShellQuote(vars))
	res, err := m.Run(ctx, cmd)
	if err != nil {
		t.Fatalf("goss: %v", err)
	}
	report, err := ParseGossReport([]byte(res.Stdout))
	if err != nil {
		t.Fatalf("goss %s: %v\nstderr:\n%s", filepath.Base(specPath), err, res.Stderr)
	}
	t.Logf("goss %s: %s", filepath.Base(specPath), report.Summary.SummaryLine)
	if stderr := strings.TrimSpace(res.Stderr); stderr != "" {
		t.Logf("goss %s: stderr: %s", filepath.Base(specPath), stderr)
	}
	for _, f := range report.Failures() {
		t.Errorf("goss %s: %s: %s: %s", filepath.Base(specPath), f.ResourceType, f.ResourceID, f.Describe())
	}
	if len(report.Failures()) == 0 && report.Summary.FailedCount != 0 {
		t.Errorf("goss %s: %d failures reported in the summary but none in the results",
			filepath.Base(specPath), report.Summary.FailedCount)
	}
}

func EnsureGossFor(ctx context.Context, m *Machine) (string, error) {
	arch, err := m.Arch(ctx)
	if err != nil {
		return "", err
	}
	dir, err := CacheDir(arch, "goss")
	if err != nil {
		return "", err
	}
	return EnsureGoss(dir, arch)
}

func ensureGossOn(ctx context.Context, m *Machine) error {
	local, err := EnsureGossFor(ctx, m)
	if err != nil {
		return err
	}
	arch, err := m.Arch(ctx)
	if err != nil {
		return err
	}
	want, err := GossSHA256For(arch)
	if err != nil {
		return err
	}
	res, err := m.Run(ctx, "sha256sum "+GossRemotePath+" 2>/dev/null | cut -d' ' -f1")
	if err != nil {
		return err
	}
	if strings.TrimSpace(res.Stdout) == want {
		return nil
	}
	if err := m.Copy(ctx, local, GossRemotePath); err != nil {
		return fmt.Errorf("copy goss: %w", err)
	}
	if res, err := m.Run(ctx, "chmod +x "+GossRemotePath); err != nil {
		return fmt.Errorf("chmod goss: %w", err)
	} else if res.ExitCode != 0 {
		return fmt.Errorf("chmod goss: exit %d: %s", res.ExitCode, res.Stderr)
	}
	return nil
}

func gossVars(ctx context.Context, m *Machine, extra map[string]string) (string, error) {
	user := UserOf(m)
	res, err := m.Run(ctx, "id -u "+ShellQuote(user))
	if err != nil {
		return "", fmt.Errorf("uid of %s on %s: %w", user, m.Alias, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("uid of %s on %s: exit %d: %s", user, m.Alias, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	arch, err := m.Arch(ctx)
	if err != nil {
		return "", err
	}
	vars := map[string]string{
		"user": user,
		"home": HomeOf(m),
		"uid":  strings.TrimSpace(res.Stdout),
		"arch": arch,
	}
	for k, v := range extra {
		vars[k] = v
	}
	b, err := json.Marshal(vars)
	if err != nil {
		return "", fmt.Errorf("goss vars: %w", err)
	}
	return string(b), nil
}
