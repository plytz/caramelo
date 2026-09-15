//go:build integration

package prod

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	capi "github.com/plytz/caramelo/internal/api"
	cedge "github.com/plytz/caramelo/internal/edge"
	cenv "github.com/plytz/caramelo/internal/env"
	cprogress "github.com/plytz/caramelo/internal/progress"
	crelease "github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/test/integration/itest"
)

const (
	edgeControlSocket = "/run/caramelo/edge.sock"
	vaultKeyPath      = "/var/lib/caramelo/vault.key"
	secretsRunDir     = "/run/caramelo/secrets"
	straySecretsDir   = "/mnt/caramelo/secrets"
)

var clientTimeout = itest.Scale(10 * time.Minute)

func begin(t *testing.T) *itest.Machine {
	t.Helper()
	if startErr != nil {
		t.Fatalf("prod suite: %v", startErr)
	}
	if skipReason != "" {
		t.Skip("prod suite: " + skipReason)
	}
	return box
}

func needRepo(t *testing.T) {
	t.Helper()
	if repo == "" {
		t.Skip("prod suite: the sample repository was never created (an earlier test failed)")
	}
}

func needProduction(t *testing.T) {
	t.Helper()
	needRepo(t)
	if firstRelease == "" {
		t.Skip("prod suite: production has never been deployed (an earlier test failed)")
	}
}

func clientEnv(extra ...string) []string {
	return itest.GitEnv(home, append([]string{"CARAMELO_MACHINE=" + machineAddr}, extra...)...)
}

func reconnect(t *testing.T, m *itest.Machine) {
	t.Helper()
	machineAddr = m.ClientMachine(t)
	if err := itest.WriteClientHomeNoPeer(home, m); err != nil {
		t.Fatalf("point the client at %s again: %v", m.Alias, err)
	}
	t.Logf("the client is pointed at %s again", machineAddr)
}

func waitForClient(t *testing.T) {
	t.Helper()
	budget := itest.Scale(3 * time.Minute)
	deadline := time.Now().Add(budget)
	var last string
	for attempt := 1; ; attempt++ {
		res, err := clientExec(clientOpts{Dir: repo, Timeout: itest.Scale(time.Minute)}, "status", "--json")
		switch {
		case err == nil && res.ExitCode == 0:
			t.Logf("the client reached the machine again on attempt %d", attempt)
			return
		case err != nil:
			last = err.Error()
		default:
			last = fmt.Sprintf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		if time.Now().After(deadline) {
			t.Fatalf("the client never reached the machine again within %s (%d attempt(s)); last: %s",
				budget, attempt, last)
		}
		time.Sleep(2 * time.Second)
	}
}

type clientOpts = itest.ClientOptions

func opts(o clientOpts) clientOpts {
	o.Env = clientEnv()
	if o.Timeout == 0 {
		o.Timeout = clientTimeout
	}
	return o
}

func clientExec(o clientOpts, args ...string) (itest.Result, error) {
	return itest.RunClient(context.Background(), opts(o), args...)
}

func client(t *testing.T, o clientOpts, args ...string) itest.Result {
	t.Helper()
	return itest.MustRunClient(t, opts(o), args...)
}

func inRepo(t *testing.T, args ...string) itest.Result {
	t.Helper()
	return client(t, clientOpts{Dir: repo}, args...)
}

func mustInRepo(t *testing.T, args ...string) itest.Result {
	t.Helper()
	return itest.ClientOK(t, opts(clientOpts{Dir: repo}), args...)
}

func destroyEnv(t *testing.T, name string) {
	t.Helper()
	res, err := clientExec(clientOpts{Dir: repo, Timeout: itest.Scale(4 * time.Minute)},
		"env", "destroy", name, "--yes")
	if err != nil {
		t.Logf("env destroy %s: %v", name, err)
		return
	}
	if res.ExitCode != 0 {
		t.Logf("env destroy %s: exit %d\n%s", name, res.ExitCode, res.Stderr)
		return
	}
	t.Logf("%s was destroyed so the rest of the suite runs on a box that is not holding it", name)
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

func buildRelease(t *testing.T, name string, extra ...string) capi.BuildResult {
	t.Helper()
	args := append([]string{"build", name, "--json"}, extra...)
	res := mustInRepo(t, args...)
	return decode[capi.BuildResult](t, "build "+name, res.Stdout)
}

func tryDeploy(t *testing.T, name string, extra ...string) (capi.DeployResult, itest.Result) {
	t.Helper()
	args := append([]string{"deploy", name, "--json"}, extra...)
	res := client(t, clientOpts{Dir: repo, Timeout: itest.Scale(12 * time.Minute)}, args...)
	t.Logf("[deploy %s] progress:\n%s", name, res.Stderr)
	if strings.TrimSpace(res.Stdout) == "" {
		return capi.DeployResult{}, res
	}
	return decode[capi.DeployResult](t, "deploy "+name, res.Stdout), res
}

func deploy(t *testing.T, name string, extra ...string) capi.DeployResult {
	t.Helper()
	out, res := tryDeploy(t, name, extra...)
	if res.ExitCode != 0 {
		t.Fatalf("deploy %s: exit %d\nstdout:\n%sstderr:\n%s", name, res.ExitCode, res.Stdout, res.Stderr)
	}
	if out.Deploy == nil {
		t.Fatalf("deploy %s answered no deploy row:\n%s", name, res.Stdout)
	}
	return out
}

func promote(t *testing.T, name string, extra ...string) capi.DeployResult {
	t.Helper()
	args := append([]string{"promote", name, "--json"}, extra...)
	res := client(t, clientOpts{Dir: repo, Timeout: itest.Scale(6 * time.Minute)}, args...)
	if res.ExitCode != 0 {
		t.Fatalf("promote %s: exit %d\nstderr:\n%s", name, res.ExitCode, res.Stderr)
	}
	return decode[capi.DeployResult](t, "promote "+name, res.Stdout)
}

func rollback(t *testing.T, name string, extra ...string) (capi.DeployResult, itest.Result) {
	t.Helper()
	args := append([]string{"rollback", name, "--json"}, extra...)
	res := client(t, clientOpts{Dir: repo, Timeout: itest.Scale(10 * time.Minute)}, args...)
	t.Logf("[rollback %s] progress:\n%s", name, res.Stderr)
	if strings.TrimSpace(res.Stdout) == "" {
		return capi.DeployResult{}, res
	}
	return decode[capi.DeployResult](t, "rollback "+name, res.Stdout), res
}

func releases(t *testing.T, name string, extra ...string) capi.ReleasesResult {
	t.Helper()
	args := append([]string{"releases", name, "--json"}, extra...)
	res := mustInRepo(t, args...)
	return decode[capi.ReleasesResult](t, "releases "+name, res.Stdout)
}

func secretsSet(t *testing.T, args ...string) capi.VaultResult {
	t.Helper()
	res := mustInRepo(t, append([]string{"secrets", "set"}, append(args, "--json")...)...)
	return decode[capi.VaultResult](t, "secrets set", res.Stdout)
}

func secretsList(t *testing.T, args ...string) capi.VaultResult {
	t.Helper()
	res := mustInRepo(t, append([]string{"secrets", "list"}, append(args, "--json")...)...)
	return decode[capi.VaultResult](t, "secrets list", res.Stdout)
}

func secretsExport(t *testing.T, name string, extra ...string) capi.VaultExportResult {
	t.Helper()
	args := append([]string{"secrets", "export", name, "--json"}, extra...)
	res := mustInRepo(t, args...)
	return decode[capi.VaultExportResult](t, "secrets export "+name, res.Stdout)
}

func events(t *testing.T, args ...string) []cprogress.Event {
	t.Helper()
	res := mustInRepo(t, append([]string{"events"}, append(args, "--json")...)...)
	return parseEvents(t, "events", res.Stdout)
}

func parseEvents(t *testing.T, what, stream string) []cprogress.Event {
	t.Helper()
	var out []cprogress.Event
	sc := bufio.NewScanner(strings.NewReader(stream))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var e cprogress.Event
		if err := json.Unmarshal([]byte(text), &e); err != nil {
			t.Fatalf("%s: line %d is not an event: %v\n%s", what, line, err, text)
		}
		out = append(out, e)
	}
	return out
}

type follower struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	mu     sync.Mutex
	lines  []string
	done   chan struct{}
}

func startFollower(t *testing.T, args ...string) *follower {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	argv := append([]string{"events", "--follow", "--json"}, args...)
	cmd := exec.CommandContext(ctx, itest.BinaryPath(t), argv...)
	cmd.Dir = repo
	cmd.Env = clientEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("events --follow: %v", err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("events --follow: %v", err)
	}
	f := &follower{cmd: cmd, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(f.done)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			f.mu.Lock()
			f.lines = append(f.lines, sc.Text())
			f.mu.Unlock()
		}
	}()
	t.Cleanup(func() { f.Stop(t) })
	return f
}

func (f *follower) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.lines)
}

func (f *follower) WaitFor(n int, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if f.Count() >= n {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("the feed produced %d line(s) in %s, want %d", f.Count(), budget, n)
}

func (f *follower) Stop(t *testing.T) []cprogress.Event {
	t.Helper()
	if f.cmd != nil && f.cmd.Process != nil {
		_ = syscall.Kill(-f.cmd.Process.Pid, syscall.SIGKILL)
	}
	f.cancel()
	select {
	case <-f.done:
	case <-time.After(itest.Scale(10 * time.Second)):
		t.Logf("events --follow did not close its stream; reading the %d line(s) it gave", f.Count())
	}
	f.mu.Lock()
	stream := strings.Join(f.lines, "\n")
	f.mu.Unlock()
	if stream == "" {
		return nil
	}
	return parseEvents(t, "events --follow", stream)
}

func eventMatching(list []cprogress.Event, ok func(cprogress.Event) bool) (cprogress.Event, bool) {
	for _, e := range list {
		if ok(e) {
			return e, true
		}
	}
	return cprogress.Event{}, false
}

func waitForEvent(t *testing.T, name string, budget time.Duration, ok func(cprogress.Event) bool) cprogress.Event {
	t.Helper()
	deadline := time.Now().Add(budget)
	var last []cprogress.Event
	for time.Now().Before(deadline) {
		last = events(t, name, "--limit", "400")
		if e, found := eventMatching(last, ok); found {
			return e
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("no matching event in %s's feed within %s; the last %d were:\n%s",
		name, budget, len(last), describeEvents(last))
	return cprogress.Event{}
}

func describeEvents(list []cprogress.Event) string {
	var b strings.Builder
	for _, e := range list {
		fmt.Fprintf(&b, "  %s %s/%s %s/%d %s %s %s %s\n",
			e.At.Format(time.RFC3339), e.App, e.Env, e.Service, e.Replica,
			e.Action, e.Step, e.Status, e.Detail)
	}
	return b.String()
}

func showEnv(t *testing.T, name string) capi.EnvDetail {
	t.Helper()
	res := mustInRepo(t, "env", "show", name, "--json")
	return decode[capi.EnvDetail](t, "env show "+name, res.Stdout)
}

func listEnvs(t *testing.T) []cenv.Env {
	t.Helper()
	res := mustInRepo(t, "env", "list", "--all", "--json")
	return decode[[]cenv.Env](t, "env list --all", res.Stdout)
}

func envNamed(t *testing.T, list []cenv.Env, name string) cenv.Env {
	t.Helper()
	for _, e := range list {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no environment %q in %v", name, envNames(list))
	return cenv.Env{}
}

func envNames(list []cenv.Env) []string {
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, fmt.Sprintf("%s(%s)", e.Name, e.Mode))
	}
	return out
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

func targetsInState(routes []cedge.Route, state cedge.TargetState) int {
	n := 0
	for _, r := range routes {
		for _, target := range r.Targets {
			if target.State == state {
				n++
			}
		}
	}
	return n
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

func stepOf(d *cenv.Deploy, name cenv.DeployStepName) (cenv.DeployStep, bool) {
	if d == nil {
		return cenv.DeployStep{}, false
	}
	out, found := cenv.DeployStep{}, false
	for _, s := range d.Steps {
		if s.Step == name {
			out, found = s, true
		}
	}
	return out, found
}

func stepNames(d *cenv.Deploy) []string {
	if d == nil {
		return nil
	}
	out := make([]string, 0, len(d.Steps))
	for _, s := range d.Steps {
		out = append(out, fmt.Sprintf("%s=%s", s.Step, s.Status))
	}
	return out
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

func inspect(t *testing.T, m *itest.Machine, container, format string) string {
	t.Helper()
	res := asCaramelo(t, m, fmt.Sprintf("docker inspect --format %s %s",
		itest.ShellQuote(format), itest.ShellQuote(container)))
	if res.ExitCode != 0 {
		t.Fatalf("docker inspect %s: exit %d: %s", container, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout)
}

func imageExists(t *testing.T, m *itest.Machine, ref string) bool {
	t.Helper()
	res := asCaramelo(t, m, "docker image inspect "+itest.ShellQuote(ref)+" >/dev/null 2>&1")
	return res.ExitCode == 0
}

func psql(t *testing.T, m *itest.Machine, env, password, statement string) itest.Result {
	t.Helper()
	container := cenv.ContainerName(appName, env, dbDep)
	cmd := fmt.Sprintf("docker exec -e PGPASSWORD=%s %s psql -h %s -U postgres -tAc %s",
		itest.ShellQuote(password), itest.ShellQuote(container),
		itest.ShellQuote(dbDep), itest.ShellQuote(statement))
	return asCaramelo(t, m, cmd)
}

func secretsOnDisk(t *testing.T, m *itest.Machine) []string {
	t.Helper()
	res := onBox(t, m, "sudo ls -1A "+secretsRunDir+" 2>/dev/null || true")
	return strings.Fields(res.Stdout)
}

func straySecretsOnDisk(t *testing.T, m *itest.Machine) string {
	t.Helper()
	res := onBox(t, m, "sudo ls -ld "+straySecretsDir+" 2>/dev/null || true")
	return strings.TrimSpace(res.Stdout)
}

func buildsOnDisk(t *testing.T, m *itest.Machine, env string) []string {
	t.Helper()
	dir := fmt.Sprintf("/mnt/caramelo/apps/%s/envs/%s/builds", appName, env)
	res := onBox(t, m, "sudo ls -1A "+dir+" 2>/dev/null || true")
	return strings.Fields(res.Stdout)
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
	env := clientEnv()

	copyIn(t, dir, "app.py", "version.py", "health.py", "behaviour.py",
		"pgwire.py", "migrate.py", "smoke.py", "smoke_fail.py", "hog.py")
	copyAs(t, dir, "greeting.secret.py", "greeting.py")
	copyAs(t, dir, "caramelo.prod.yaml", "caramelo.yaml")
	copyAs(t, dir, "gitignore", ".gitignore")
	itest.Git(t, dir, env, "init", "-b", branch)
	firstCommit = itest.GitCommitAll(t, dir, env, "sampleapp: run for real")
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

func setVersion(t *testing.T, want string) string {
	t.Helper()
	path := filepath.Join(repo, "version.py")
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
	return itest.GitCommitAll(t, repo, clientEnv(), "sampleapp: version "+want)
}

func servingVersion(t *testing.T, url string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
	defer cancel()
	res, err := internet.GetWithin(ctx, url+"/", itest.Scale(90*time.Second))
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return versionOf(res.Body)
}

func commitAll(t *testing.T, message string) string {
	t.Helper()
	return itest.GitCommitAll(t, repo, clientEnv(), message)
}

func keptReleases(hist capi.ReleasesResult, keep int) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range hist.Deploys {
		if d.Release == nil {
			continue
		}
		name := crelease.ShortTree(d.Release.Tree)
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
		if keep > 0 && len(out) == keep {
			break
		}
	}
	return out
}

func liveReplica(t *testing.T, m *itest.Machine, env, service string) string {
	t.Helper()
	names := liveReplicas(t, m, env, service)
	if len(names) == 0 {
		t.Fatalf("no running container of %s/%s", env, service)
	}
	return names[0]
}

func liveReplicas(t *testing.T, m *itest.Machine, env, service string) []string {
	t.Helper()
	names := dockerNames(t, m, "label="+cenv.LabelService+"="+service+" --filter label="+cenv.LabelEnv+"="+env, false)
	sort.Strings(names)
	return names
}

func replicaIndexOf(t *testing.T, container string) int {
	t.Helper()
	i := strings.LastIndex(container, "-")
	if i < 0 {
		t.Fatalf("%q does not end in a replica index", container)
	}
	n, err := strconv.Atoi(container[i+1:])
	if err != nil || n <= 0 {
		t.Fatalf("%q does not end in a replica index: %v", container, err)
	}
	return n
}

func whoAmI(t *testing.T) string {
	t.Helper()
	res := mustInRepo(t, "status", "--json")
	return decode[capi.Status](t, "status", res.Stdout).Identity
}
