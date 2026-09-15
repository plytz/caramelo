//go:build integration

package edge

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
	cedge "github.com/plytz/caramelo/internal/edge"
	cenv "github.com/plytz/caramelo/internal/env"
	csetup "github.com/plytz/caramelo/internal/setup"
	"github.com/plytz/caramelo/test/integration/itest"
)

const edgeControlSocket = "/run/caramelo/edge.sock"

var commanderTimeout = itest.Scale(8 * time.Minute)

func begin(t *testing.T) *itest.Machine {
	t.Helper()
	if startErr != nil {
		t.Fatalf("edge suite: %v", startErr)
	}
	if skipReason != "" {
		t.Skip("edge suite: " + skipReason)
	}
	return box
}

func needRepo(t *testing.T) {
	t.Helper()
	if repo == "" {
		t.Skip("edge suite: the sample repository was never created (an earlier test failed)")
	}
}

func commanderEnv(extra ...string) []string {
	return itest.GitEnv(home, append([]string{"CARAMELO_MACHINE=" + machineAddr}, extra...)...)
}

type commanderOpts = itest.CommanderOptions

func opts(o commanderOpts) commanderOpts {
	o.Env = commanderEnv()
	if o.Timeout == 0 {
		o.Timeout = commanderTimeout
	}
	return o
}

func commanderExec(o commanderOpts, args ...string) (itest.Result, error) {
	return itest.RunCommander(context.Background(), opts(o), args...)
}

func commander(t *testing.T, o commanderOpts, args ...string) itest.Result {
	t.Helper()
	return itest.MustRunCommander(t, opts(o), args...)
}

func inRepo(t *testing.T, args ...string) itest.Result {
	t.Helper()
	return commander(t, commanderOpts{Dir: repo}, args...)
}

func mustInRepo(t *testing.T, args ...string) itest.Result {
	t.Helper()
	return itest.CommanderOK(t, opts(commanderOpts{Dir: repo}), args...)
}

func decode[T any](t *testing.T, what, stdout string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &v); err != nil {
		t.Fatalf("%s --json: %v\nstdout: %q", what, err, stdout)
	}
	return v
}

func createEnv(t *testing.T, name string, extra ...string) cenv.Env {
	t.Helper()
	args := append([]string{"env", "create", name, "--json"}, extra...)
	res := mustInRepo(t, args...)
	e := decode[cenv.Env](t, "env create "+name, res.Stdout)
	if e.Status != cenv.StatusReady {
		t.Fatalf("env create %s: status %q, want %q\nstderr:\n%s", name, e.Status, cenv.StatusReady, res.Stderr)
	}
	return e
}

func up(t *testing.T, name string, extra ...string) capi.UpResult {
	t.Helper()
	args := append([]string{"up", name, "--json"}, extra...)
	res := mustInRepo(t, args...)
	t.Logf("[up %s] progress:\n%s", name, res.Stderr)
	return decode[capi.UpResult](t, "up "+name, res.Stdout)
}

func destroyEnv(t *testing.T, name string, extra ...string) {
	t.Helper()
	args := append([]string{"env", "destroy", name, "--yes"}, extra...)
	mustInRepo(t, args...)
}

func showEnv(t *testing.T, name string) capi.EnvDetail {
	t.Helper()
	res := mustInRepo(t, "env", "show", name, "--json")
	return decode[capi.EnvDetail](t, "env show "+name, res.Stdout)
}

func edgeStatus(t *testing.T) cedge.Status {
	t.Helper()
	res := mustInRepo(t, "edge", "status", "--json")
	return decode[cedge.Status](t, "edge status", res.Stdout)
}

func routeOf(t *testing.T, what string, routes []cedge.Route, host string) cedge.Route {
	t.Helper()
	for _, r := range routes {
		if r.Host == host {
			return r
		}
	}
	t.Fatalf("%s: no route for %s in %+v", what, host, routes)
	return cedge.Route{}
}

func serviceOf(t *testing.T, what string, services []cenv.Service, name string) cenv.Service {
	t.Helper()
	for _, s := range services {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("%s: no service %q in %+v", what, name, services)
	return cenv.Service{}
}

func rolloutOf(t *testing.T, what string, rollouts []cenv.Rollout, service string) cenv.Rollout {
	t.Helper()
	for _, r := range rollouts {
		if r.Service == service {
			return r
		}
	}
	t.Fatalf("%s: no rollout for %q in %+v", what, service, rollouts)
	return cenv.Rollout{}
}

func steps(r cenv.Rollout) []string {
	out := make([]string, 0, len(r.Steps))
	for _, s := range r.Steps {
		out = append(out, fmt.Sprintf("%s/%d:%s=%s", s.Service, s.Replica, s.Step, s.Status))
	}
	return out
}

func hasStep(r cenv.Rollout, name cenv.RolloutStepName) bool {
	for _, s := range r.Steps {
		if s.Step == name {
			return true
		}
	}
	return false
}

func onBox(t *testing.T, m *itest.Machine, cmd string) itest.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
	defer cancel()
	res, err := m.Run(ctx, cmd)
	if err != nil {
		t.Fatalf("[%s] %s: %v", m.Alias, cmd, err)
	}
	return res
}

func asCaramelo(t *testing.T, m *itest.Machine, cmd string) itest.Result {
	t.Helper()
	return onBox(t, m, itest.AsUser(m, itest.CarameloUser, cmd))
}

func dockerNames(t *testing.T, m *itest.Machine, filter string, all bool) []string {
	t.Helper()
	flag := ""
	if all {
		flag = "-a "
	}
	res := asCaramelo(t, m, fmt.Sprintf("docker ps %s--filter %s --format '{{.Names}}'", flag, filter))
	if res.ExitCode != 0 {
		t.Fatalf("docker ps: exit %d: %s", res.ExitCode, res.Stderr)
	}
	return strings.Fields(res.Stdout)
}

func restartEdge(t *testing.T, m *itest.Machine) {
	t.Helper()
	res := onBox(t, m, "sudo systemctl restart "+csetup.EdgeServiceUnit)
	if res.ExitCode != 0 {
		t.Fatalf("restart %s: exit %d: %s", csetup.EdgeServiceUnit, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	deadline := time.Now().Add(itest.Scale(30 * time.Second))
	for {
		if onBox(t, m, "test -S "+edgeControlSocket).ExitCode == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the edge's control socket did not come back after a restart:\n%s",
				onBox(t, m, "sudo systemctl status "+csetup.EdgeServiceUnit+" --no-pager").Stdout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func field(body, name string) string {
	for _, line := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), name+" "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func versionOf(body string) string { return field(body, "version") }

func replicaOf(body string) string { return field(body, "replica") }

var sampleDir = filepath.Join("..", "run", "testdata", "sampleapp")

func initSampleRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(tmpDir, "sampleapp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create the sample checkout: %v", err)
	}
	t.Logf("sample checkout: %s", dir)
	env := commanderEnv()

	copyIn(t, dir, "app.py", "behaviour.py", "greeting.py", "version.py", "health.py")
	copyAs(t, dir, "caramelo.edge.yaml", "caramelo.yaml")
	copyAs(t, dir, "gitignore", ".gitignore")
	itest.Git(t, dir, env, "init", "-b", branch)
	firstCommit = itest.GitCommitAll(t, dir, env, "sampleapp: published, two replicas")
	return dir
}

func copyIn(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		copyAs(t, dir, name, name)
	}
}

func copyAs(t *testing.T, dir, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(sampleDir, src))
	if err != nil {
		t.Fatalf("read the sample app: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, dst), b, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func setVersion(t *testing.T, dir, want string) string {
	t.Helper()
	path := filepath.Join(dir, "version.py")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read version.py: %v", err)
	}
	body := string(b)
	old := fmt.Sprintf("VERSION = %q", currentVersion)
	if !strings.Contains(body, old) {
		t.Fatalf("version.py does not contain %s:\n%s", old, body)
	}
	body = strings.Replace(body, old, fmt.Sprintf("VERSION = %q", want), 1)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write version.py: %v", err)
	}
	currentVersion = want
	return itest.GitCommitAll(t, dir, commanderEnv(), "sampleapp: version "+want)
}
