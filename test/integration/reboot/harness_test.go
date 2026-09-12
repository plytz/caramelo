//go:build integration

package reboot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	capi "github.com/plytz/caramelo/internal/api"
	cenv "github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/test/integration/itest"
)

const (
	webContainer = "web"
	webImage     = "nginx:alpine"
	webPort      = 20080
)

const (
	envApp       = "rebootapp"
	envName      = "pre-reboot"
	envService   = "web"
	envDomain    = "reboot.test"
	envHost      = envName + "." + envDomain
	envPublicURL = "https://" + envHost
)

const (
	edgeControlSocket = "/run/caramelo/edge.sock"
	vaultKeyPath      = "/var/lib/caramelo/vault.key"
)

const peerName = itest.LabPeerName

var envDeps = []string{"db", "cache"}

const rebootAppConfig = `name: ` + envApp + `
domain: ` + envDomain + `
deps:
  db: ` + itest.PostgresImage + `
  cache: ` + itest.RedisImage + `
services:
  ` + envService + `:
    image: ` + itest.PythonImage + `
    run: python -m http.server $PORT
env:
  DATABASE_URL: postgres://postgres:caramelo@${deps.db.host}:${deps.db.port}/postgres
  REDIS_URL: redis://${deps.cache.host}:${deps.cache.port}
`

var suiteImages = append(append([]string{}, itest.RunImages...), webImage, itest.PebbleImage)

var (
	recoveryBudget = itest.Scale(30 * time.Second)
	settleBudget   = itest.Scale(4 * time.Minute)
	stepBudget     = itest.Scale(30 * time.Second)
)

func begin(t *testing.T) *itest.Machine {
	t.Helper()
	if startErr != nil {
		t.Fatalf("reboot suite: %v", startErr)
	}
	if skipReason != "" {
		t.Skip("reboot suite: " + skipReason)
	}
	return box
}

func hasEdge() bool { return internet != nil }

func api(t *testing.T, timeout time.Duration, args ...string) itest.Result {
	t.Helper()
	opts := itest.SSHAPIFor(t, box)
	opts.Timeout = timeout
	return itest.SSHAPIRun(t, opts, args...)
}

func apiTry(t *testing.T, timeout time.Duration, args ...string) itest.Result {
	t.Helper()
	opts := itest.SSHAPIFor(t, box)
	opts.Timeout = timeout
	opts.AllowTimeout = true
	return itest.SSHAPIRun(t, opts, args...)
}

func clientEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	if _, err := box.HostPortProto(itest.VPNPort, "udp"); err != nil {
		t.Fatalf("reboot suite: %v", err)
	}
	return itest.GitEnv(home, append([]string{"CARAMELO_MACHINE=" + box.HostIP()}, extra...)...)
}

func powerCycle(t *testing.T) time.Time {
	t.Helper()
	t.Logf("power cycling %s", box.Alias)
	itest.MustRestart(t, box)
	settle(t, box)
	return time.Now()
}

func settle(t *testing.T, m *itest.Machine) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), settleBudget)
	defer cancel()
	start := time.Now()
	res, err := m.RunAsRoot(ctx, "systemctl is-system-running --wait")
	if err != nil {
		t.Fatalf("wait for systemd on %s to finish starting: %v", m.Alias, err)
	}
	t.Logf("systemd on %s finished starting after %s (%s)", m.Alias,
		time.Since(start).Round(time.Second), strings.TrimSpace(res.Stdout))
}

func atLeast(deadline time.Time, budget time.Duration) time.Time {
	if later := time.Now().Add(budget); later.After(deadline) {
		return later
	}
	return deadline
}

func refreshClient(t *testing.T) {
	t.Helper()
	if err := itest.WriteClientHome(home, box); err != nil {
		t.Fatalf("rewrite the client home after the power cycle: %v", err)
	}
}

func waitUntil(t *testing.T, deadline time.Time, what string, check func() error) {
	t.Helper()
	start := time.Now()
	var last error
	for {
		if last = check(); last == nil {
			t.Logf("%s after %s", what, time.Since(start).Round(time.Millisecond))
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("%s not ready within the recovery budget: %v", what, last)
			return
		}
		time.Sleep(2 * time.Second)
	}
}

func runOK(m *itest.Machine, cmd string) error {
	ctx, cancel := context.WithTimeout(context.Background(), stepBudget)
	defer cancel()
	res, err := m.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr+res.Stdout))
	}
	return nil
}

func rootOK(m *itest.Machine, cmd string) error {
	ctx, cancel := context.WithTimeout(context.Background(), stepBudget)
	defer cancel()
	res, err := m.RunAsRoot(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr+res.Stdout))
	}
	return nil
}

func containerRunning(m *itest.Machine, container string) error {
	return runOK(m, itest.AsUserSession(m, itest.CarameloUser,
		"docker inspect -f '{{.State.Running}}' "+container+" | grep -qx true"))
}

func ensureContainer(t *testing.T, m *itest.Machine) {
	t.Helper()
	if err := containerRunning(m, webContainer); err == nil {
		return
	}
	itest.SeedImages(t, m, webImage)
	cmd := fmt.Sprintf("docker rm -f %s >/dev/null 2>&1; "+
		"docker run -d --name %s --restart unless-stopped -p 127.0.0.1:%d:80 %s",
		webContainer, webContainer, webPort, webImage)
	itest.MustRunAsUser(t, m, itest.CarameloUser, cmd)
}

func envIsReady(t *testing.T) bool {
	t.Helper()
	res := apiTry(t, itest.Scale(30*time.Second), "env", "show", envName, "--app", envApp, "--json")
	if res.ExitCode != 0 {
		return false
	}
	var detail capi.EnvDetail
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &detail); err != nil {
		return false
	}
	if detail.Env.Status != cenv.StatusReady || len(detail.Deps) != len(envDeps) {
		return false
	}
	for _, dep := range detail.Deps {
		if dep.Status != cenv.DepRunning {
			return false
		}
	}
	return true
}

func serviceURLOf(t *testing.T) string {
	t.Helper()
	res := api(t, itest.Scale(30*time.Second), "env", "url", envName, "--app", envApp, "--json")
	if res.ExitCode != 0 {
		t.Fatalf("env url %s: exit %d\nstderr:%s", envName, res.ExitCode, res.Stderr)
	}
	var list []cenv.URL
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &list); err != nil {
		t.Fatalf("env url --json: %v (%q)", err, res.Stdout)
	}
	for _, u := range list {
		if u.Name == envService {
			return u.URL
		}
	}
	return ""
}

func errExit(code int, stderr string) error {
	return fmt.Errorf("exit %d: %s", code, strings.TrimSpace(stderr))
}
