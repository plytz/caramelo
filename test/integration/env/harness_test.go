//go:build integration

package env

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	capi "github.com/plytz/caramelo/internal/api"
	cenv "github.com/plytz/caramelo/internal/env"
	cprogress "github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/test/integration/itest"
)

const (
	appName       = "sampleapp"
	defaultBranch = "main"
	featureBranch = "feature"
	brokenBranch  = "broken"

	envX       = "feat-x"
	envY       = "feat-y"
	envCurrent = "feat-c"
	envBad     = "feat-bad"
	envAgent   = "feat-z"

	dataDir = "/mnt/caramelo"
)

var depNames = []string{"db", "cache"}

var commanderTimeout = itest.Scale(5 * time.Minute)

type suite struct {
	lab     *itest.Lab
	m       *itest.Machine
	home    string
	repo    string
	remote  string
	machine string
	tmp     string

	mainCommit    string
	featureCommit string
	brokenCommit  string
}

func start(t *testing.T) *suite {
	t.Helper()
	lab := itest.New(t, itest.Options{Suite: "env", State: itest.StateProvisioned})
	m := lab.Machine(itest.RoleHub)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(5*time.Minute))
	defer cancel()
	if err := itest.WaitForCaramelod(ctx, m); err != nil {
		t.Fatalf("caramelod on %s: %v", m.Alias, err)
	}
	if _, err := itest.EnsureGossFor(ctx, m); err != nil {
		t.Fatalf("goss for %s: %v", m.Alias, err)
	}
	if err := itest.SeedImagesTo(m, itest.DepImages...); err != nil {
		t.Fatalf("seed the dependency images onto %s: %v", m.Alias, err)
	}

	s := &suite{
		lab:     lab,
		m:       m,
		home:    itest.CommanderHomeNoPeer(t, m),
		machine: m.CommanderMachine(t),
		tmp:     t.TempDir(),
	}
	s.remote = m.GitRemote(t, appName)
	t.Logf("machine %s, git remote %s", s.machine, s.remote)
	return s
}

func (s *suite) needRepo(t *testing.T) {
	t.Helper()
	if s.repo == "" {
		t.Skip("env suite: the sample repository was never created (an earlier step failed)")
	}
}

func (s *suite) commanderEnv(extra ...string) []string {
	return itest.GitEnv(s.home, append([]string{"CARAMELO_MACHINE=" + s.machine}, extra...)...)
}

type commanderOpts = itest.CommanderOptions

func (s *suite) opts(o commanderOpts) commanderOpts {
	o.Env = s.commanderEnv()
	if o.Timeout == 0 {
		o.Timeout = commanderTimeout
	}
	return o
}

func (s *suite) commander(t *testing.T, o commanderOpts, args ...string) itest.Result {
	t.Helper()
	return itest.MustRunCommander(t, s.opts(o), args...)
}

func (s *suite) inRepo(t *testing.T, args ...string) itest.Result {
	t.Helper()
	return s.commander(t, commanderOpts{Dir: s.repo}, args...)
}

func (s *suite) mustCommander(t *testing.T, o commanderOpts, args ...string) itest.Result {
	t.Helper()
	return itest.CommanderOK(t, s.opts(o), args...)
}

func (s *suite) mustInRepo(t *testing.T, args ...string) itest.Result {
	t.Helper()
	return s.mustCommander(t, commanderOpts{Dir: s.repo}, args...)
}

func decode[T any](t *testing.T, what, stdout string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &v); err != nil {
		t.Fatalf("%s --json: %v\nstdout: %q", what, err, stdout)
	}
	return v
}

func (s *suite) createEnv(t *testing.T, name string, extra ...string) cenv.Env {
	t.Helper()
	args := append([]string{"env", "create", name, "--json"}, extra...)
	res := s.mustInRepo(t, args...)
	e := decode[cenv.Env](t, "env create "+name, res.Stdout)
	if e.Status != cenv.StatusReady {
		t.Fatalf("env create %s: status %q, want %q\nstderr:\n%s", name, e.Status, cenv.StatusReady, res.Stderr)
	}
	return e
}

func (s *suite) showEnv(t *testing.T, name string) capi.EnvDetail {
	t.Helper()
	res := s.mustInRepo(t, "env", "show", name, "--json")
	return decode[capi.EnvDetail](t, "env show "+name, res.Stdout)
}

func (s *suite) events(t *testing.T, args ...string) []cprogress.Event {
	t.Helper()
	res := s.mustInRepo(t, append([]string{"events"}, append(args, "--json")...)...)
	return itest.ParseEvents(t, "events", res.Stdout)
}

func (s *suite) exportEnv(t *testing.T, name string) map[string]string {
	t.Helper()
	res := s.mustInRepo(t, "env", "export", name, "--format", "json")
	return decode[map[string]string](t, "env export "+name, res.Stdout)
}

func (s *suite) destroyEnv(t *testing.T, name string, extra ...string) itest.Result {
	t.Helper()
	args := append([]string{"env", "destroy", name, "--yes"}, extra...)
	return s.mustInRepo(t, args...)
}

func (s *suite) asCaramelo(t *testing.T, cmd string) itest.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(time.Minute))
	defer cancel()
	res, err := itest.RunAsUser(ctx, s.m, itest.CarameloUser, cmd)
	if err != nil {
		t.Fatalf("[%s] %s: %v", s.m.Alias, cmd, err)
	}
	return res
}

func (s *suite) dockerNames(t *testing.T, filter string, all bool) []string {
	t.Helper()
	flag := ""
	if all {
		flag = "-a "
	}
	res := s.asCaramelo(t, fmt.Sprintf("docker ps %s--filter %s --format '{{.Names}}'", flag, filter))
	if res.ExitCode != 0 {
		t.Fatalf("docker ps: exit %d: %s", res.ExitCode, res.Stderr)
	}
	return strings.Fields(res.Stdout)
}

func (s *suite) dockerNetworks(t *testing.T, filter string) []string {
	t.Helper()
	res := s.asCaramelo(t, "docker network ls --format '{{.Name}}' --filter "+filter)
	if res.ExitCode != 0 {
		t.Fatalf("docker network ls: exit %d: %s", res.ExitCode, res.Stderr)
	}
	return strings.Fields(res.Stdout)
}

func (s *suite) dockerVolumes(t *testing.T, filter string) []string {
	t.Helper()
	res := s.asCaramelo(t, "docker volume ls -q --filter "+filter)
	if res.ExitCode != 0 {
		t.Fatalf("docker volume ls: exit %d: %s", res.ExitCode, res.Stderr)
	}
	return strings.Fields(res.Stdout)
}

func portIn(t *testing.T, what, url string) int {
	t.Helper()
	i := strings.LastIndex(url, ":")
	if i < 0 {
		t.Fatalf("%s = %q: no port in it", what, url)
	}
	rest := url[i+1:]
	if j := strings.IndexAny(rest, "/?"); j >= 0 {
		rest = rest[:j]
	}
	port, err := strconv.Atoi(rest)
	if err != nil {
		t.Fatalf("%s = %q: %v", what, url, err)
	}
	return port
}

func (s *suite) initSampleRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(s.tmp, "sampleapp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create the sample checkout: %v", err)
	}
	t.Logf("sample checkout: %s", dir)
	env := s.commanderEnv()

	copyIn(t, dir, "caramelo.yaml", "main.txt")
	itest.Git(t, dir, env, "init", "-b", defaultBranch)
	s.mainCommit = itest.GitCommitAll(t, dir, env, "sampleapp: first commit")

	itest.Git(t, dir, env, "checkout", "-b", featureBranch)
	copyIn(t, dir, "feature.txt")
	s.featureCommit = itest.GitCommitAll(t, dir, env, "sampleapp: the feature branch")

	itest.Git(t, dir, env, "checkout", "-b", brokenBranch, defaultBranch)
	copyAs(t, dir, "caramelo.broken.yaml", "caramelo.yaml")
	s.brokenCommit = itest.GitCommitAll(t, dir, env, "sampleapp: a readiness command that never passes")

	itest.Git(t, dir, env, "checkout", defaultBranch)
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
	b, err := os.ReadFile(filepath.Join("testdata", "sampleapp", src))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, dst), b, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
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
