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
	cprogress "github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/test/integration/itest"
)

const (
	relApp     = "rebootrel"
	relEnv     = "production"
	relService = "web"
)

const relAppConfig = `name: ` + relApp + `
services:
  ` + relService + `:
    image: ` + itest.PythonImage + `
    run: python -m http.server $PORT
    health: /
deploy:
  watch: 20s
  keep: 3
`

func TestZZProductionComesBack(t *testing.T) {
	m := begin(t)

	if !hasVault(m) {
		t.Skip("reboot suite: the provisioned machine has no vault key, so it predates M7")
	}
	refreshCommander(t)

	repo := pushReleaseApp(t, m)

	res := api(t, itest.Scale(8*time.Minute), "env", "create", relEnv,
		"--app", relApp, "--release", "--from", "main", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("env create %s --release: exit %d\nstdout:%s\nstderr:%s",
			relEnv, res.ExitCode, res.Stdout, res.Stderr)
	}

	first := mustDeploy(t, "--no-push")
	if first.Status != cenv.DeployPromoted {
		t.Fatalf("the first deploy ended %q, want %q", first.Status, cenv.DeployPromoted)
	}
	release := first.Release.Short()
	t.Logf("%s/%s is running release %s", relApp, relEnv, release)

	t.Run("a deploy interrupted in its watch is finished by the daemon", func(t *testing.T) {
		bumpReleaseApp(t, repo, "a second release")
		second := mustDeploy(t, "--no-push", "--no-watch")
		if second.Release == nil || second.Release.Short() == release {
			t.Fatalf("the second deploy reused release %v", second.Release)
		}
		if second.Status.Done() {
			t.Skipf("the deploy had already finished (%q) before the daemon could be restarted; "+
				"the resume path needs a watch still open", second.Status)
		}
		restartDaemon(t, m)

		deadline := time.Now().Add(itest.Scale(3 * time.Minute))
		for {
			hist := history(t)
			if len(hist.Deploys) > 0 && hist.Deploys[0].Done() {
				t.Logf("the interrupted deploy ended %q", hist.Deploys[0].Status)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the interrupted deploy is still %v three minutes after the restart",
					describeHistory(hist))
			}
			time.Sleep(5 * time.Second)
		}
		detail := showRelEnv(t)
		if detail.Deploy != nil {
			t.Errorf("env show still names a deploy in progress: %+v", detail.Deploy)
		}
		if detail.Release == nil {
			t.Fatal("the environment has no release after the resume")
		}
		release = detail.Release.Short()
	})

	before := showRelEnv(t)
	if len(before.Services) == 0 {
		t.Fatal("the release environment runs nothing before the reboot")
	}

	t.Logf("%s carries a release environment into the power cycle", m.Alias)
	powerCycle(t)

	t.Run("the release environment is serving the same release", func(t *testing.T) {
		deadline := time.Now().Add(itest.Scale(2 * time.Minute))
		waitUntil(t, deadline, "the release environment is back", func() error {
			detail := showRelEnvOr(t)
			if detail == nil {
				return fmt.Errorf("env show does not answer yet")
			}
			if detail.Release == nil {
				return fmt.Errorf("no release on the environment")
			}
			if got := detail.Release.Short(); got != release {
				return fmt.Errorf("release %s, want %s: nothing may rebuild over a reboot", got, release)
			}
			if len(detail.Services) == 0 {
				return fmt.Errorf("no services")
			}
			for _, s := range detail.Services {
				if s.Status != cenv.ServiceRunning {
					return fmt.Errorf("service %s is %q", s.Name, s.Status)
				}
			}
			return nil
		})
	})

	t.Run("no deploy is left in progress", func(t *testing.T) {
		st := status(t)
		if st.Production == nil {
			t.Fatal("status has no production section: this daemon predates M7")
		}
		t.Logf("production: %d env(s), %d in release mode, %d deploying, %d held, %d unhealthy",
			st.Production.Envs, st.Production.Release, st.Production.Deploying,
			st.Production.Held, st.Production.Unhealthy)
		if st.Production.Deploying != 0 {
			t.Errorf("%d deploy(s) in progress after a reboot: a deploy the daemon cannot "+
				"resume must be failed, not left open", st.Production.Deploying)
		}
		if !st.Production.Vault {
			t.Error("status says the machine has no vault after a reboot")
		}
	})

	t.Run("the health loop is back", func(t *testing.T) {
		deadline := time.Now().Add(itest.Scale(90 * time.Second))
		for time.Now().Before(deadline) {
			list := feed(t)
			if len(list) > 0 {
				t.Logf("%d event(s) in %s/%s's feed; the newest is %s %s %q",
					len(list), relApp, relEnv, list[len(list)-1].Action,
					list[len(list)-1].Status, list[len(list)-1].Detail)
				return
			}
			time.Sleep(5 * time.Second)
		}
		t.Log("the feed is quiet, which a machine with nothing wrong is allowed to be")
	})
}

func hasVault(m *itest.Machine) bool {
	return rootOK(m, "test -s "+vaultKeyPath) == nil
}

func pushReleaseApp(t *testing.T, m *itest.Machine) string {
	t.Helper()
	itest.SeedImages(t, m, itest.PythonImage)
	repo := filepath.Join(tmpDir, relApp)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("create the release checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "caramelo.yaml"), []byte(relAppConfig), 0o644); err != nil {
		t.Fatalf("write caramelo.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "index.html"), []byte("<h1>release 1</h1>\n"), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	gitEnv := itest.GitEnv(home)
	itest.Git(t, repo, gitEnv, "init", "-b", "main")
	itest.GitCommitAll(t, repo, gitEnv, relApp+": first commit")
	itest.Git(t, repo, gitEnv, "push", m.GitRemote(t, relApp), "main")
	return repo
}

func bumpReleaseApp(t *testing.T, repo, message string) {
	t.Helper()
	gitEnv := itest.GitEnv(home)
	if err := os.WriteFile(filepath.Join(repo, "index.html"),
		[]byte("<h1>"+message+"</h1>\n"), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	itest.GitCommitAll(t, repo, gitEnv, relApp+": "+message)
	itest.Git(t, repo, gitEnv, "push", box.GitRemote(t, relApp), "HEAD:"+relEnv)
}

func mustDeploy(t *testing.T, extra ...string) *cenv.Deploy {
	t.Helper()
	args := append([]string{"deploy", relEnv, "--app", relApp, "--json"}, extra...)
	res := api(t, itest.Scale(12*time.Minute), args...)
	if res.ExitCode != 0 {
		t.Fatalf("deploy %s: exit %d\nstdout:%s\nstderr:%s",
			relEnv, res.ExitCode, res.Stdout, res.Stderr)
	}
	var out capi.DeployResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &out); err != nil {
		t.Fatalf("deploy --json: %v (%q)", err, res.Stdout)
	}
	if out.Deploy == nil {
		t.Fatalf("deploy answered no row: %q", res.Stdout)
	}
	return out.Deploy
}

func restartDaemon(t *testing.T, m *itest.Machine) {
	t.Helper()
	if err := runOK(m, itest.AsUserSession(m, itest.CarameloUser, "systemctl --user restart caramelod")); err != nil {
		t.Fatalf("restart caramelod: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
	defer cancel()
	if err := itest.WaitForCaramelod(ctx, m); err != nil {
		t.Fatalf("caramelod did not come back: %v", err)
	}
	waitUntil(t, time.Now().Add(itest.Scale(time.Minute)), "the API answers after the restart", func() error {
		res := apiTry(t, itest.Scale(10*time.Second), "status", "--json")
		if res.ExitCode != 0 {
			return fmt.Errorf("exit %d", res.ExitCode)
		}
		return nil
	})
}

func history(t *testing.T) capi.ReleasesResult {
	t.Helper()
	res := api(t, itest.Scale(time.Minute), "releases", relEnv, "--app", relApp, "--json")
	if res.ExitCode != 0 {
		t.Fatalf("releases: exit %d\nstderr:%s", res.ExitCode, res.Stderr)
	}
	var out capi.ReleasesResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &out); err != nil {
		t.Fatalf("releases --json: %v (%q)", err, res.Stdout)
	}
	return out
}

func describeHistory(h capi.ReleasesResult) string {
	var parts []string
	for _, d := range h.Deploys {
		parts = append(parts, fmt.Sprintf("%s/%s", d.Kind, d.Status))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func showRelEnv(t *testing.T) capi.EnvDetail {
	t.Helper()
	detail := showRelEnvOr(t)
	if detail == nil {
		t.Fatalf("env show %s/%s does not answer", relApp, relEnv)
	}
	return *detail
}

func showRelEnvOr(t *testing.T) *capi.EnvDetail {
	t.Helper()
	res := apiTry(t, itest.Scale(time.Minute), "env", "show", relEnv, "--app", relApp, "--json")
	if res.ExitCode != 0 {
		return nil
	}
	var detail capi.EnvDetail
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &detail); err != nil {
		return nil
	}
	return &detail
}

func status(t *testing.T) capi.Status {
	t.Helper()
	res := api(t, itest.Scale(time.Minute), "status", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("status: exit %d\nstderr:%s", res.ExitCode, res.Stderr)
	}
	var st capi.Status
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &st); err != nil {
		t.Fatalf("status --json: %v (%q)", err, res.Stdout)
	}
	return st
}

func feed(t *testing.T) []cprogress.Event {
	t.Helper()
	res := api(t, itest.Scale(time.Minute), "events", relEnv, "--app", relApp, "--limit", "100", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("events: exit %d\nstderr:%s", res.ExitCode, res.Stderr)
	}
	var out []cprogress.Event
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e cprogress.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("events --json: a line is not an event: %v\n%s", err, line)
		}
		out = append(out, e)
	}
	return out
}
