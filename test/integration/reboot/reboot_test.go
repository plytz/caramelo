//go:build integration

package reboot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	capi "github.com/plytz/caramelo/internal/api"
	cenv "github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/vpn"
	"github.com/plytz/caramelo/test/integration/itest"
)

const dataProof = "written before the power cycle"

func TestRebootRecovery(t *testing.T) {
	m := begin(t)

	ensureContainer(t, m)
	ensureEnv(t, m)
	seedDependencyData(t, m)

	back := powerCycle(t)
	t.Logf("%s is a machine again", m.Alias)
	deadline := back.Add(recoveryBudget)

	t.Run("api answers", func(t *testing.T) {
		waitUntil(t, deadline, "tcp:4022 serving status", func() error {
			res := apiTry(t, itest.Scale(10*time.Second), "status", "--json")
			if res.ExitCode != 0 {
				return fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
			}
			var st capi.Status
			if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &st); err != nil {
				return fmt.Errorf("status --json: %w (%q)", err, res.Stdout)
			}
			if st.Transport != "ssh" {
				return fmt.Errorf("transport = %q", st.Transport)
			}
			return nil
		})
	})

	t.Run("rootless docker is up", func(t *testing.T) {
		waitUntil(t, deadline, "docker user unit active", func() error {
			return runOK(m, itest.AsUserSession(m, itest.CarameloUser, "systemctl --user is-active docker"))
		})
	})

	t.Run("container is running", func(t *testing.T) {
		waitUntil(t, deadline, webContainer+" running", func() error {
			return containerRunning(m, webContainer)
		})
	})

	t.Run("the env's dependencies are running", func(t *testing.T) {
		for _, dep := range envDeps {
			container := cenv.ContainerName(envApp, envName, dep)
			waitUntil(t, deadline, container+" running", func() error {
				return containerRunning(m, container)
			})
		}
	})

	t.Run("the env's service is serving again", func(t *testing.T) {
		container := cenv.ReplicaContainerName(envApp, envName, envService, 1)
		waitUntil(t, deadline, container+" running", func() error {
			return containerRunning(m, container)
		})
		waitUntil(t, deadline, "the service answers on "+serviceURL, func() error {
			return runOK(m, "curl -fsS --max-time 5 "+serviceURL+"/")
		})
	})

	t.Run("env show agrees", func(t *testing.T) {
		waitUntil(t, deadline, "env show reports the deps running", func() error {
			res := apiTry(t, itest.Scale(20*time.Second), "env", "show", envName, "--app", envApp, "--json")
			if res.ExitCode != 0 {
				return fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
			}
			var detail capi.EnvDetail
			if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &detail); err != nil {
				return fmt.Errorf("env show --json: %w (%q)", err, res.Stdout)
			}
			if len(detail.Deps) != len(envDeps) {
				return fmt.Errorf("%d deps, want %d", len(detail.Deps), len(envDeps))
			}
			for _, dep := range detail.Deps {
				if dep.Status != cenv.DepRunning {
					return fmt.Errorf("dep %s is %q", dep.Name, dep.Status)
				}
			}
			for _, s := range detail.Services {
				if s.Status != cenv.ServiceRunning {
					return fmt.Errorf("service %s is %q", s.Name, s.Status)
				}
			}
			if len(detail.Services) != 1 {
				return fmt.Errorf("%d service(s), want 1", len(detail.Services))
			}
			return nil
		})
	})

	t.Run("the dependency's data survived the power cycle", func(t *testing.T) {
		db := cenv.ContainerName(envApp, envName, "db")
		waitUntil(t, deadline, db+" answers a query again", func() error {
			return runOK(m, psql(m, db, "select 1"))
		})
		ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(time.Minute))
		defer cancel()
		res, err := m.Run(ctx, psql(m, db, "select note from reboot_proof"))
		if err != nil {
			t.Fatalf("read %s back out of %s: %v", dataProof, db, err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("read %s back out of %s: exit %d: %s",
				dataProof, db, res.ExitCode, strings.TrimSpace(res.Stderr+res.Stdout))
		}
		if got := strings.TrimSpace(res.Stdout); got != dataProof {
			t.Errorf("%s answers %q, want %q: a dependency's volume is on disk and must be reattached, "+
				"not recreated, when the machine comes back", db, got, dataProof)
		}
	})

	t.Run("no caramelo unit failed across the power cycle", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(time.Minute))
		defer cancel()
		res, err := m.RunAsRoot(ctx, "systemctl --failed --no-legend --no-pager --plain")
		if err != nil {
			t.Fatalf("list the failed units on %s: %v", m.Alias, err)
		}
		if out := strings.TrimSpace(res.Stdout); out != "" {
			t.Logf("failed system units after the power cycle:\n%s", out)
		}
		for _, unit := range failedUnits(res.Stdout) {
			if strings.HasPrefix(unit, "caramelo") {
				t.Errorf("%s failed at boot: a machine nobody logs into owes its own units", unit)
			}
		}
		user, err := m.Run(ctx, itest.AsUserSession(m, itest.CarameloUser,
			"systemctl --user --failed --no-legend --no-pager --plain"))
		if err != nil {
			t.Fatalf("list the failed user units on %s: %v", m.Alias, err)
		}
		if units := failedUnits(user.Stdout); len(units) != 0 {
			t.Errorf("the %s user's units failed at boot: %s; nothing of caramelo's may need a login",
				itest.CarameloUser, strings.Join(units, " "))
		}
	})

	t.Run("the public edge is back", func(t *testing.T) {
		if !hasEdge() {
			t.Skip("this machine has no edge")
		}
		waitUntil(t, deadline, "the edge's sockets are bound again", func() error {
			return runOK(m, "ss -H -ltn | grep -q ':443 ' && ss -H -lun | grep -q ':443 '")
		})
		if err := refreshEdgeTrust(); err != nil {
			t.Fatalf("%v", err)
		}
		var served string
		waitUntil(t, atLeast(deadline, recoveryBudget), envPublicURL+" answers", func() error {
			ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(20*time.Second))
			defer cancel()
			res, err := internet.Get(ctx, envPublicURL+"/")
			if err != nil {
				return err
			}
			if res.Status != 200 {
				return fmt.Errorf("HTTP %d", res.Status)
			}
			served = res.Serial()
			return nil
		})
		if served == "" {
			return
		}
		if served != certSerial {
			t.Errorf("the certificate for %s changed from %s to %s across the reboot; the store is on disk "+
				"and a machine that reissued on every boot would spend a real CA's rate limits on nothing",
				envHost, certSerial, served)
		} else {
			t.Logf("%s answers again with the same certificate %s", envPublicURL, served)
		}
	})

	refreshCommander(t)

	t.Run("a session through the tunnel works", func(t *testing.T) {
		waitUntil(t, atLeast(deadline, recoveryBudget), "a session through the tunnel", func() error {
			res, err := itest.RunCommander(context.Background(),
				itest.CommanderOptions{Env: commanderEnv(t), Timeout: itest.Scale(30 * time.Second)}, "status", "--json")
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
			}
			var st capi.Status
			if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &st); err != nil {
				return fmt.Errorf("status --json: %w (%q)", err, res.Stdout)
			}
			if st.Transport != remote.KindTunnel {
				return fmt.Errorf("transport = %q", st.Transport)
			}
			if st.Identity != peerName {
				return fmt.Errorf("identity = %q, want %q", st.Identity, peerName)
			}
			return nil
		})
	})

	t.Run("the env's address answers", func(t *testing.T) {
		waitUntil(t, atLeast(deadline, recoveryBudget), "the env's address answers on its native ports", func() error {
			ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(30*time.Second))
			defer cancel()
			session, err := itest.Connect(ctx,
				itest.CommanderOptions{Env: commanderEnv(t), Timeout: itest.Scale(30 * time.Second)},
				envName, "--app", envApp, "cache")
			if err != nil {
				return err
			}
			defer session.Close()
			local, ok := session.Local("cache", vpn.TCP)
			if !ok {
				return fmt.Errorf("no tcp port for cache in the port map")
			}
			got, err := itest.RedisPing(local)
			if err != nil {
				return err
			}
			if got != "+PONG" {
				return fmt.Errorf("redis answered %q", got)
			}
			return nil
		})
	})

	t.Logf("recovery complete %s after the machine came back", time.Since(back).Round(time.Second))
}

func failedUnits(out string) []string {
	var units []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "●" || fields[0] == "*" {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			continue
		}
		units = append(units, fields[0])
	}
	return units
}

func psql(m *itest.Machine, container, query string) string {
	return itest.AsUserSession(m, itest.CarameloUser,
		"docker exec "+container+" psql -U postgres -t -A -v ON_ERROR_STOP=1 -c "+itest.ShellQuote(query))
}

func seedDependencyData(t *testing.T, m *itest.Machine) {
	t.Helper()
	db := cenv.ContainerName(envApp, envName, "db")
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
	defer cancel()
	if err := m.WaitFor(ctx, psql(m, db, "select 1"), 2*time.Second); err != nil {
		t.Fatalf("%s never answered a query before the power cycle: %v", db, err)
	}
	write := "create table if not exists reboot_proof(note text); " +
		"delete from reboot_proof; insert into reboot_proof values ('" + dataProof + "')"
	if err := runOK(m, psql(m, db, write)); err != nil {
		t.Fatalf("write a row into %s before the power cycle: %v", db, err)
	}
	t.Logf("%s holds a row written before the power cycle", db)
}

func ensureEnv(t *testing.T, m *itest.Machine) {
	t.Helper()
	if !envIsReady(t) {
		repo := filepath.Join(tmpDir, envApp)
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatalf("create the sample checkout: %v", err)
		}
		if err := os.WriteFile(filepath.Join(repo, "caramelo.yaml"), []byte(rebootAppConfig), 0o644); err != nil {
			t.Fatalf("write caramelo.yaml: %v", err)
		}
		gitEnv := itest.GitEnv(home)
		itest.Git(t, repo, gitEnv, "init", "-b", "main")
		itest.GitCommitAll(t, repo, gitEnv, envApp+": first commit")
		itest.Git(t, repo, gitEnv, "push", m.GitRemote(t, envApp), "main")

		res := api(t, itest.Scale(8*time.Minute), "env", "create", envName, "--app", envApp, "--from", "main", "--json")
		if res.ExitCode != 0 {
			t.Fatalf("env create %s: exit %d\nstdout:%s\nstderr:%s", envName, res.ExitCode, res.Stdout, res.Stderr)
		}
		if !envIsReady(t) {
			t.Fatalf("env %s is not ready after creating it", envName)
		}
	}

	res := api(t, itest.Scale(8*time.Minute), "up", envName, "--app", envApp, "--no-push", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("up %s: exit %d\nstdout:%s\nstderr:%s", envName, res.ExitCode, res.Stdout, res.Stderr)
	}
	serviceURL = serviceURLOf(t)
	if serviceURL == "" {
		t.Fatalf("env url %s: no address for the service %q", envName, envService)
	}
	if err := runOK(m, "curl -fsS --max-time 10 "+serviceURL+"/"); err != nil {
		t.Fatalf("%s does not answer before the reboot: %v", serviceURL, err)
	}
	t.Logf("the service answers on %s before the reboot", serviceURL)

	if !hasEdge() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
	defer cancel()
	res2, err := internet.GetWithin(ctx, envPublicURL+"/", itest.Scale(2*time.Minute))
	if err != nil {
		t.Fatalf("%s does not answer before the reboot: %v", envPublicURL, err)
	}
	certSerial = res2.Serial()
	if certSerial == "" {
		t.Fatalf("%s answered without a certificate", envPublicURL)
	}
	t.Logf("%s answers before the reboot with certificate %s", envPublicURL, certSerial)
}
