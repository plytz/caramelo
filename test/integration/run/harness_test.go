//go:build integration

package run

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
	cserverconfig "github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/test/integration/itest"
)

const (
	appName = "sampleapp"

	defaultBranch  = "main"
	dockerBranch   = "docker"
	crashBranch    = "crash"
	replicasBranch = "replicas"
	m7Branch       = "m7"

	envX         = "feat-x"
	envY         = "feat-y"
	envZ         = "feat-z"
	envCrash     = "feat-crash"
	envReplicas  = "feat-r"
	envM7        = "feat-m7"
	envM7Release = "rel-m7"

	webService  = "web"
	echoService = "echo"

	dataDir   = cserverconfig.DefaultDataDir
	stateDir  = cserverconfig.DefaultStateDir
	configDir = cserverconfig.DefaultConfigDir
	runDir    = cserverconfig.DefaultRunDir
)

var (
	commanderTimeout = itest.Scale(8 * time.Minute)
	curlTimeout      = itest.Scale(15 * time.Second)
	udpBudget        = itest.Scale(30 * time.Second)
)

type suite struct {
	lab     *itest.Lab
	m       *itest.Machine
	home    string
	machine string
	repo    string
	tmp     string

	mainCommit   string
	dockerCommit string
}

func start(t *testing.T) *suite {
	t.Helper()
	lab := itest.New(t, itest.Options{Suite: "run", State: itest.StateProvisioned})
	m := lab.Machine(itest.RoleHub)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(5*time.Minute))
	defer cancel()
	if err := itest.WaitForCaramelod(ctx, m); err != nil {
		t.Fatalf("caramelod on %s: %v", m.Alias, err)
	}
	if _, err := itest.EnsureGossFor(ctx, m); err != nil {
		t.Fatalf("goss for %s: %v", m.Alias, err)
	}
	if m.Target() == itest.TargetDocker {
		if err := itest.SeedImagesTo(m, itest.RunImages...); err != nil {
			t.Fatalf("seed the run images onto %s: %v", m.Alias, err)
		}
	} else {
		t.Logf("%s is not a container: it pulls the run images itself rather than take them over the wire", m.Alias)
	}
	itest.EnsureCurl(t, m)

	s := &suite{
		lab:     lab,
		m:       m,
		home:    itest.CommanderHomeNoPeer(t, m),
		machine: m.CommanderMachine(t),
		tmp:     t.TempDir(),
	}
	t.Logf("machine %s", s.machine)
	return s
}

func (s *suite) refresh(t *testing.T) {
	t.Helper()
	s.machine = s.m.CommanderMachine(t)
	if err := itest.WriteCommanderHomeNoPeer(s.home, s.m); err != nil {
		t.Fatalf("rewrite the commander home after the power cycle: %v", err)
	}
	t.Logf("machine %s", s.machine)
}

func (s *suite) needRepo(t *testing.T) {
	t.Helper()
	if s.repo == "" {
		t.Skip("run suite: the sample repository was never created (an earlier step failed)")
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

func (s *suite) up(t *testing.T, name string, extra ...string) capi.UpResult {
	t.Helper()
	args := append([]string{"up", name, "--json"}, extra...)
	res := s.mustInRepo(t, args...)
	t.Logf("[up %s] progress:\n%s", name, res.Stderr)
	return decode[capi.UpResult](t, "up "+name, res.Stdout)
}

func service(t *testing.T, what string, services []cenv.Service, name string) cenv.Service {
	t.Helper()
	for _, svc := range services {
		if svc.Name == name {
			return svc
		}
	}
	t.Fatalf("%s: no service %q in %+v", what, name, services)
	return cenv.Service{}
}

func (s *suite) showEnv(t *testing.T, name string) capi.EnvDetail {
	t.Helper()
	res := s.mustInRepo(t, "env", "show", name, "--json")
	return decode[capi.EnvDetail](t, "env show "+name, res.Stdout)
}

func (s *suite) urls(t *testing.T, name string) []cenv.URL {
	t.Helper()
	res := s.mustInRepo(t, "env", "url", name, "--json")
	return decode[[]cenv.URL](t, "env url "+name, res.Stdout)
}

func (s *suite) urlOf(t *testing.T, name, target string) cenv.URL {
	t.Helper()
	for _, u := range s.urls(t, name) {
		if u.Name == target {
			return u
		}
	}
	t.Fatalf("env url %s: nothing called %q", name, target)
	return cenv.URL{}
}

func (s *suite) destroyEnv(t *testing.T, name string, extra ...string) itest.Result {
	t.Helper()
	args := append([]string{"env", "destroy", name, "--yes"}, extra...)
	return s.mustInRepo(t, args...)
}

func (s *suite) onBox(t *testing.T, cmd string) itest.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
	defer cancel()
	res, err := s.m.Run(ctx, cmd)
	if err != nil {
		t.Fatalf("[%s] %s: %v", s.m.Alias, cmd, err)
	}
	return res
}

func (s *suite) asCaramelo(t *testing.T, cmd string) itest.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
	defer cancel()
	res, err := itest.RunAsUser(ctx, s.m, itest.CarameloUser, cmd)
	if err != nil {
		t.Fatalf("[%s] %s: %v", s.m.Alias, cmd, err)
	}
	return res
}

func (s *suite) get(t *testing.T, url string) string {
	t.Helper()
	res := s.getOr(t, url)
	if res.ExitCode != 0 {
		t.Fatalf("curl %s on the box: exit %d\nstdout:\n%sstderr:\n%s", url, res.ExitCode, res.Stdout, res.Stderr)
	}
	return res.Stdout
}

func (s *suite) getOr(t *testing.T, url string) itest.Result {
	t.Helper()
	return s.onBox(t, fmt.Sprintf("curl -fsS --max-time %d %s",
		int(curlTimeout.Seconds()), itest.ShellQuote(url)))
}

func (s *suite) getWithin(t *testing.T, url string, budget time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		res := s.getOr(t, url)
		if res.ExitCode == 0 {
			return res.Stdout
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never answered within %s: curl exit %d: %s",
				url, budget, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		time.Sleep(2 * time.Second)
	}
}

func (s *suite) echoUDP(t *testing.T, port int, payload string) string {
	t.Helper()
	script := fmt.Sprintf(
		"bash -c 'exec 3<>/dev/udp/127.0.0.1/%d; printf %%s %s >&3; timeout 5 head -c %d <&3'",
		port, payload, len(payload))
	deadline := time.Now().Add(udpBudget)
	for attempt := 1; ; attempt++ {
		res := s.onBox(t, script)
		if res.ExitCode == 0 {
			if attempt > 1 {
				t.Logf("the udp service answered on attempt %d", attempt)
			}
			return res.Stdout
		}
		if time.Now().After(deadline) {
			t.Fatalf("the udp service on the box loopback port %d answered nothing in %s "+
				"(%d datagram(s), last exit %d)\nstderr:\n%s",
				port, udpBudget, attempt, res.ExitCode, res.Stderr)
		}
		time.Sleep(time.Second)
	}
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

func (s *suite) networkMembers(t *testing.T, network string) []string {
	t.Helper()
	res := s.asCaramelo(t, "docker network inspect -f '{{range .Containers}}{{.Name}} {{end}}' "+network)
	if res.ExitCode != 0 {
		t.Fatalf("docker network inspect %s: exit %d: %s", network, res.ExitCode, res.Stderr)
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

func (s *suite) inspect(t *testing.T, container, format string) string {
	t.Helper()
	res := s.asCaramelo(t, fmt.Sprintf("docker inspect --format %s %s",
		itest.ShellQuote(format), itest.ShellQuote(container)))
	if res.ExitCode != 0 {
		t.Fatalf("docker inspect %s: exit %d: %s", container, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout)
}

func (s *suite) psql(t *testing.T, env, password, statement string) itest.Result {
	t.Helper()
	container := cenv.ContainerName(appName, env, "db")
	cmd := fmt.Sprintf("docker exec -e PGPASSWORD=%s %s psql -U postgres -tAc %s",
		itest.ShellQuote(password), itest.ShellQuote(container), itest.ShellQuote(statement))
	return s.asCaramelo(t, cmd)
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func (s *suite) initSampleRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(s.tmp, "sampleapp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create the sample checkout: %v", err)
	}
	t.Logf("sample checkout: %s", dir)
	env := s.commanderEnv()

	copyIn(t, dir, "app.py", "behaviour.py", "greeting.py", "version.py", "health.py", "echo.py",
		"test_sampleapp.py", "caramelo.yaml")
	copyAs(t, dir, "gitignore", ".gitignore")
	itest.Git(t, dir, env, "init", "-b", defaultBranch)
	s.mainCommit = itest.GitCommitAll(t, dir, env, "sampleapp: the app, two services and its tests")

	itest.Git(t, dir, env, "checkout", "-b", dockerBranch)
	copyIn(t, dir, "Dockerfile")
	copyAs(t, dir, "caramelo.docker.yaml", "caramelo.yaml")
	s.dockerCommit = itest.GitCommitAll(t, dir, env, "sampleapp: a Dockerfile and nothing else to say")

	itest.Git(t, dir, env, "checkout", "-b", crashBranch, defaultBranch)
	copyIn(t, dir, "crash.py")
	copyAs(t, dir, "caramelo.crash.yaml", "caramelo.yaml")
	itest.GitCommitAll(t, dir, env, "sampleapp: a service that cannot start")

	itest.Git(t, dir, env, "checkout", "-b", replicasBranch, defaultBranch)
	copyAs(t, dir, "caramelo.replicas.yaml", "caramelo.yaml")
	itest.GitCommitAll(t, dir, env, "sampleapp: two replicas of the web service")

	itest.Git(t, dir, env, "checkout", defaultBranch)
	return dir
}

func (s *suite) initM7Branch(t *testing.T) {
	t.Helper()
	env := s.commanderEnv()
	itest.Git(t, s.repo, env, "checkout", "-b", m7Branch, defaultBranch)
	copyIn(t, s.repo, "pgwire.py", "migrate.py", "smoke.py")
	copyAs(t, s.repo, "greeting.secret.py", "greeting.py")
	copyAs(t, s.repo, "caramelo.m7.yaml", "caramelo.yaml")
	itest.GitCommitAll(t, s.repo, env, "sampleapp: resources, a vault password and a deploy block")
	itest.Git(t, s.repo, env, "checkout", defaultBranch)
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

func replaceInFile(t *testing.T, dir, name, old, replacement string) {
	t.Helper()
	path := filepath.Join(dir, name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := string(b)
	if !strings.Contains(body, old) {
		t.Fatalf("%s does not contain %q", name, old)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(body, old, replacement, 1)), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func (s *suite) envExists(t *testing.T, name string) bool {
	t.Helper()
	list := decode[[]cenv.Env](t, "env list --all",
		s.mustInRepo(t, "env", "list", "--all", "--json").Stdout)
	for _, e := range list {
		if e.Name == name {
			return true
		}
	}
	return false
}

func (s *suite) secretsSet(t *testing.T, args ...string) capi.VaultResult {
	t.Helper()
	res := s.mustInRepo(t, append([]string{"secrets", "set"}, append(args, "--json")...)...)
	return decode[capi.VaultResult](t, "secrets set", res.Stdout)
}

func (s *suite) secretsExport(t *testing.T, name string, extra ...string) capi.VaultExportResult {
	t.Helper()
	args := append([]string{"secrets", "export", name, "--json"}, extra...)
	res := s.mustInRepo(t, args...)
	return decode[capi.VaultExportResult](t, "secrets export "+name, res.Stdout)
}
