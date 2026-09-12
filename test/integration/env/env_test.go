//go:build integration

package env

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	capi "github.com/plytz/caramelo/internal/api"
	cconfig "github.com/plytz/caramelo/internal/config"
	cenv "github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/ports"
	"github.com/plytz/caramelo/test/integration/itest"
)

func TestEnv(t *testing.T) {
	s := start(t)
	for _, step := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"CreateFirstEnv", s.createFirstEnv},
		{"GossEnv", s.gossEnv},
		{"SecondEnv", s.secondEnv},
		{"CreateUsesTheCurrentBranch", s.createUsesTheCurrentBranch},
		{"Exec", s.exec},
		{"ExecRejectsAnUnknownEnv", s.execRejectsAnUnknownEnv},
		{"EnvNetworkAndCacheVolume", s.envNetworkAndCacheVolume},
		{"ExportViews", s.exportViews},
		{"EnvSurvivesAMachineRestart", s.envSurvivesAMachineRestart},
		{"PushToADirtyWorktreeIsRefused", s.pushToADirtyWorktreeIsRefused},
		{"FailedEnvIsRecoverable", s.failedEnvIsRecoverable},
		{"ParallelCreate", s.parallelCreate},
		{"Destroy", s.destroy},
		{"AgentCommitComesBack", s.agentCommitComesBack},
		{"ZZCacheImages", s.zzCacheImages},
	} {
		t.Run(step.name, step.run)
	}
}

func (s *suite) createFirstEnv(t *testing.T) {
	s.repo = s.initSampleRepo(t)

	e := s.createEnv(t, envX, "--from", defaultBranch)
	t.Logf("env: %+v", e)

	if e.App != appName || e.Name != envX || e.Branch != envX {
		t.Errorf("env = app %q name %q branch %q, want %q %q %q", e.App, e.Name, e.Branch, appName, envX, envX)
	}
	if e.Commit != s.mainCommit {
		t.Errorf("commit = %q, want main at %q", e.Commit, s.mainCommit)
	}
	if want := cenv.WorktreePath(dataDir, appName, envX); e.Worktree != want {
		t.Errorf("worktree = %q, want %q", e.Worktree, want)
	}
	if !ports.ValidBase(e.PortBase) || e.PortCount != ports.BlockSize {
		t.Errorf("ports = %d+%d, want a valid base and a block of %d", e.PortBase, e.PortCount, ports.BlockSize)
	}
	t.Logf("created_by = %q", e.CreatedBy)

	block := ports.Block(e.PortBase)
	if got := e.Vars["PORT"]; got != strconv.Itoa(e.PortBase) {
		t.Errorf("PORT = %q, want %d", got, e.PortBase)
	}
	if e.Vars["CARAMELO_APP"] != appName || e.Vars["CARAMELO_ENV"] != envX {
		t.Errorf("CARAMELO_APP/CARAMELO_ENV = %q/%q", e.Vars["CARAMELO_APP"], e.Vars["CARAMELO_ENV"])
	}
	for i, v := range []struct{ name, url string }{
		{"DATABASE_URL", e.Vars["DATABASE_URL"]},
		{"REDIS_URL", e.Vars["REDIS_URL"]},
	} {
		if !strings.Contains(v.url, "127.0.0.1") {
			t.Errorf("%s = %q, want it to point at 127.0.0.1", v.name, v.url)
		}
		if got, want := portIn(t, v.name, v.url), block[i+1]; got != want {
			t.Errorf("%s port = %d, want %d (dep %d of the block)", v.name, got, want, i+1)
		}
	}
	if want := "hello from " + envX + " of " + appName; e.Vars["GREETING"] != want {
		t.Errorf("GREETING = %q, want %q", e.Vars["GREETING"], want)
	}

	refs := itest.GitRefs(t, s.repo, s.clientEnv(), s.remote)
	if refs["refs/heads/"+defaultBranch] != s.mainCommit {
		t.Errorf("ls-remote: %s = %q, want %q", defaultBranch, refs["refs/heads/"+defaultBranch], s.mainCommit)
	}
	if refs["refs/heads/"+envX] != s.mainCommit {
		t.Errorf("ls-remote: %s = %q, want the branch created at %q", envX, refs["refs/heads/"+envX], s.mainCommit)
	}
}

func (s *suite) gossEnv(t *testing.T) {
	s.needRepo(t)
	itest.RunGoss(t, s.m, itest.MustGossSpec(t, "env.yaml"))
}

func (s *suite) secondEnv(t *testing.T) {
	s.needRepo(t)

	x := s.showEnv(t, envX).Env
	y := s.createEnv(t, envY, "--from", featureBranch)

	if y.Commit != s.featureCommit {
		t.Errorf("commit = %q, want feature at %q", y.Commit, s.featureCommit)
	}
	if diff := y.PortBase - x.PortBase; diff > -ports.BlockSize && diff < ports.BlockSize {
		t.Errorf("port blocks overlap: %s at %d, %s at %d", envX, x.PortBase, envY, y.PortBase)
	}
	if x.Vars["DATABASE_URL"] == y.Vars["DATABASE_URL"] {
		t.Errorf("both envs got DATABASE_URL = %q", x.Vars["DATABASE_URL"])
	}

	if res := s.inRepo(t, "env", "exec", envY, "--", "test", "-f", "feature.txt"); res.ExitCode != 0 {
		t.Errorf("feature.txt missing from %s's worktree (exit %d)", envY, res.ExitCode)
	}
	if res := s.inRepo(t, "env", "exec", envX, "--", "test", "-f", "feature.txt"); res.ExitCode == 0 {
		t.Errorf("feature.txt is in %s's worktree, which was created from %s", envX, defaultBranch)
	}

	list := decode[[]cenv.Env](t, "env list", s.mustInRepo(t, "env", "list", "--json").Stdout)
	seen := map[string]int{}
	for _, e := range list {
		seen[e.Name]++
	}
	if seen[envX] != 1 || seen[envY] != 1 {
		t.Errorf("env list = %v, want one %s and one %s", seen, envX, envY)
	}

	detail := s.showEnv(t, envX)
	if len(detail.Deps) != len(depNames) {
		t.Fatalf("env show %s: %d deps, want %d: %+v", envX, len(detail.Deps), len(depNames), detail.Deps)
	}
	for i, dep := range detail.Deps {
		if dep.Name != depNames[i] {
			t.Errorf("dep %d = %q, want %q (config order)", i, dep.Name, depNames[i])
		}
		if dep.Status != cenv.DepRunning {
			t.Errorf("dep %s = %q, want %q", dep.Name, dep.Status, cenv.DepRunning)
		}
		if want := cenv.ContainerName(appName, envX, dep.Name); dep.Container != want {
			t.Errorf("dep %s container = %q, want %q", dep.Name, dep.Container, want)
		}
	}
	if len(detail.Events) == 0 {
		t.Errorf("env show %s: no events; create should leave an audit trail", envX)
	}

	for _, name := range []string{envX, envY} {
		containers := s.dockerNames(t, "label=caramelo.env="+name, true)
		volumes := s.dockerVolumes(t, "label=caramelo.env="+name)
		for _, dep := range depNames {
			want := cenv.ContainerName(appName, name, dep)
			if !contains(containers, want) {
				t.Errorf("container %s missing; docker ps has %v", want, containers)
			}
			if vol := cenv.VolumeName(appName, name, dep); !contains(volumes, vol) {
				t.Errorf("volume %s missing; docker volume ls has %v", vol, volumes)
			}
		}
	}
}

func (s *suite) createUsesTheCurrentBranch(t *testing.T) {
	s.needRepo(t)

	env := s.clientEnv()
	itest.Git(t, s.repo, env, "checkout", featureBranch)
	t.Cleanup(func() { itest.Git(t, s.repo, env, "checkout", defaultBranch) })

	e := s.createEnv(t, envCurrent, "--no-deps")
	t.Cleanup(func() { s.destroyEnv(t, envCurrent, "--delete-branch") })

	if e.Commit != s.featureCommit {
		t.Errorf("commit = %q, want %s at %q: the env was built from the app's default branch, not from the checkout's",
			e.Commit, featureBranch, s.featureCommit)
	}
}

func (s *suite) exec(t *testing.T) {
	s.needRepo(t)

	vars := s.exportEnv(t, envX)
	if vars["DATABASE_URL"] == "" || vars["PORT"] == "" {
		t.Fatalf("env export: %v", vars)
	}

	t.Run("the child sees the env's variables", func(t *testing.T) {
		res := s.mustInRepo(t, "env", "exec", envX, "--", "sh", "-c", "echo $DATABASE_URL $PORT")
		want := vars["DATABASE_URL"] + " " + vars["PORT"]
		if got := strings.TrimSpace(res.Stdout); got != want {
			t.Errorf("exec printed %q, want %q", got, want)
		}
	})

	t.Run("the dependency really answers", func(t *testing.T) {
		res := s.inRepo(t, "env", "exec", envX, "--",
			"docker", "exec", cenv.ContainerName(appName, envX, "db"),
			"psql", "-U", "postgres", "-c", "select 1")
		if res.ExitCode != 0 {
			t.Errorf("psql in %s: exit %d\nstdout:\n%sstderr:\n%s",
				cenv.ContainerName(appName, envX, "db"), res.ExitCode, res.Stdout, res.Stderr)
		}
	})

	t.Run("the exit code passes through", func(t *testing.T) {
		if res := s.inRepo(t, "env", "exec", envX, "--", "sh", "-c", "exit 3"); res.ExitCode != 3 {
			t.Errorf("exit = %d, want 3\nstderr:\n%s", res.ExitCode, res.Stderr)
		}
	})

	t.Run("stdin round-trips", func(t *testing.T) {
		const payload = "caramelo reads stdin\n"
		res := s.mustClient(t, clientOpts{Dir: s.repo, Stdin: strings.NewReader(payload)},
			"env", "exec", envX, "--", "cat")
		if res.Stdout != payload {
			t.Errorf("stdout = %q, want %q", res.Stdout, payload)
		}
	})

	t.Run("a silent command is not cut off", func(t *testing.T) {
		started := time.Now()
		res := s.client(t, clientOpts{Dir: s.repo, Timeout: itest.Scale(5 * time.Minute)},
			"env", "exec", envX, "--", "sleep", "90")
		if res.ExitCode != 0 {
			t.Fatalf("sleep 90: exit %d after %s\nstderr:\n%s", res.ExitCode, time.Since(started), res.Stderr)
		}
		if elapsed := time.Since(started); elapsed < 90*time.Second {
			t.Errorf("sleep 90 returned after %s; it cannot have run", elapsed)
		}
	})
}

func (s *suite) execRejectsAnUnknownEnv(t *testing.T) {
	s.needRepo(t)

	res := s.inRepo(t, "env", "exec", "feat-nope", "--", "true")
	if res.ExitCode == 0 {
		t.Fatalf("env exec into an env that was never created succeeded\nstdout:\n%s", res.Stdout)
	}
	if res.ExitCode == 3 {
		t.Errorf("exit = 3, which is a child's own exit code, not a refusal")
	}
	if !strings.Contains(res.Stderr, "feat-nope") {
		t.Errorf("the refusal does not name the env, so nobody can tell what is wrong:\n%s", res.Stderr)
	}
	if res.Stdout != "" {
		t.Errorf("a refused exec wrote %q to stdout; stdout belongs to the child alone", res.Stdout)
	}
}

func (s *suite) envNetworkAndCacheVolume(t *testing.T) {
	s.needRepo(t)

	for _, name := range []string{envX, envY} {
		network := cenv.NetworkName(appName, name)
		if got := s.dockerNetworks(t, "name="+network); !contains(got, network) {
			t.Errorf("network %s missing after create; docker network ls has %v", network, got)
		}
		volume := cenv.CacheVolumeName(appName, name)
		if got := s.dockerVolumes(t, "name="+volume); !contains(got, volume) {
			t.Errorf("cache volume %s missing after create; docker volume ls has %v", volume, got)
		}
	}

	network := cenv.NetworkName(appName, envX)
	res := s.asCaramelo(t, "docker network inspect -f '{{range .Containers}}{{.Name}} {{end}}' "+network)
	if res.ExitCode != 0 {
		t.Fatalf("docker network inspect %s: exit %d: %s", network, res.ExitCode, res.Stderr)
	}
	for _, dep := range depNames {
		if want := cenv.ContainerName(appName, envX, dep); !strings.Contains(res.Stdout, want) {
			t.Errorf("network %s does not hold %s: %q", network, want, res.Stdout)
		}
	}
}

func (s *suite) exportViews(t *testing.T) {
	s.needRepo(t)

	host := s.exportEnv(t, envX)
	if !strings.Contains(host["DATABASE_URL"], "127.0.0.1") {
		t.Errorf("default export: DATABASE_URL = %q, want the host view (127.0.0.1)", host["DATABASE_URL"])
	}

	res := s.mustInRepo(t, "env", "export", envX, "--format", "json", "--view", string(cconfig.ViewNetwork))
	network := decode[map[string]string](t, "env export --view network", res.Stdout)
	if !strings.Contains(network["DATABASE_URL"], "@db:5432") {
		t.Errorf("network view: DATABASE_URL = %q, want the dependency's alias and its own port", network["DATABASE_URL"])
	}
	if !strings.Contains(network["REDIS_URL"], "cache:6379") {
		t.Errorf("network view: REDIS_URL = %q, want cache:6379", network["REDIS_URL"])
	}
	if host["GREETING"] != network["GREETING"] {
		t.Errorf("GREETING differs between views: %q vs %q", host["GREETING"], network["GREETING"])
	}
}

func (s *suite) envSurvivesAMachineRestart(t *testing.T) {
	s.needRepo(t)

	before := s.showEnv(t, envX)
	marker := cenv.WorktreePath(dataDir, appName, envX) + "/restart.txt"
	s.mustInRepo(t, "env", "exec", envX, "--", "sh", "-c", "echo 'written before the power cycle' > restart.txt")

	itest.MustRestart(t, s.m)
	s.refresh(t)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(5*time.Minute))
	defer cancel()
	if err := itest.WaitForCaramelod(ctx, s.m); err != nil {
		t.Fatalf("caramelod did not come back after the power cycle: %v", err)
	}

	after := s.showEnv(t, envX)
	if after.Env.PortBase != before.Env.PortBase {
		t.Errorf("port base = %d after the power cycle, want the allocation to be durable at %d",
			after.Env.PortBase, before.Env.PortBase)
	}
	if after.Env.Commit != before.Env.Commit {
		t.Errorf("commit = %q after the power cycle, want %q", after.Env.Commit, before.Env.Commit)
	}
	if got := strings.TrimSpace(s.mustInRepo(t, "env", "exec", envX, "--", "cat", "restart.txt").Stdout); got != "written before the power cycle" {
		t.Errorf("%s came back as %q; the worktree lives on the machine's disk and must survive a reboot", marker, got)
	}

	deadline := time.Now().Add(itest.Scale(3 * time.Minute))
	for {
		after = s.showEnv(t, envX)
		running := 0
		for _, dep := range after.Deps {
			if dep.Status == cenv.DepRunning {
				running++
			}
		}
		if running == len(depNames) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d dependencies came back after the power cycle: %+v",
				running, len(depNames), after.Deps)
		}
		time.Sleep(3 * time.Second)
	}

	for _, dep := range depNames {
		want := cenv.ContainerName(appName, envX, dep)
		if got := s.dockerNames(t, "label=caramelo.env="+envX, false); !contains(got, want) {
			t.Errorf("container %s is not running after the power cycle; docker ps has %v", want, got)
		}
	}
}

func (s *suite) refresh(t *testing.T) {
	t.Helper()
	s.machine = s.m.ClientMachine(t)
	s.remote = s.m.GitRemote(t, appName)
	if err := itest.WriteClientHomeNoPeer(s.home, s.m); err != nil {
		t.Fatalf("rewrite the client home after the power cycle: %v", err)
	}
	t.Logf("machine %s, git remote %s", s.machine, s.remote)
}

func (s *suite) pushToADirtyWorktreeIsRefused(t *testing.T) {
	s.needRepo(t)

	env := s.clientEnv()
	name := "feat-dirty"
	s.createEnv(t, name, "--from", defaultBranch, "--no-deps")
	t.Cleanup(func() { s.destroyEnv(t, name, "--delete-branch") })

	itest.Git(t, s.repo, env, "checkout", defaultBranch)
	if err := os.WriteFile(filepath.Join(s.repo, "pushed.txt"), []byte("from the laptop\n"), 0o644); err != nil {
		t.Fatalf("write pushed.txt: %v", err)
	}
	itest.GitCommitAll(t, s.repo, env, "sampleapp: something to push")
	s.mustInRepo(t, "env", "exec", name, "--", "sh", "-c", "echo 'the agent was here' >> main.txt")

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
	defer cancel()
	res, err := itest.GitRun(ctx, s.repo, env, "push", s.remote, "HEAD:"+name)
	if err != nil {
		t.Fatalf("git push: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("git push into %s succeeded with a dirty worktree; uncommitted work must never be overwritten\nstdout:\n%sstderr:\n%s",
			name, res.Stdout, res.Stderr)
	}
	out := res.Stdout + res.Stderr
	if worktree := cenv.WorktreePath(dataDir, appName, name); !strings.Contains(out, worktree) {
		t.Errorf("the refusal does not name the worktree %s, so nobody can tell what to fix:\n%s", worktree, out)
	}
	if got := strings.TrimSpace(s.mustInRepo(t, "env", "exec", name, "--", "cat", "main.txt").Stdout); !strings.Contains(got, "the agent was here") {
		t.Errorf("main.txt in the worktree is %q; the agent's edit is gone", got)
	}
}

func (s *suite) failedEnvIsRecoverable(t *testing.T) {
	s.needRepo(t)

	started := time.Now()
	res := s.client(t, clientOpts{Dir: s.repo, Timeout: itest.Scale(4 * time.Minute)},
		"env", "create", envBad, "--from", brokenBranch, "--timeout", "30s", "--json")
	if res.ExitCode != 1 {
		t.Fatalf("env create %s: exit %d, want 1\nstdout:\n%sstderr:\n%s",
			envBad, res.ExitCode, res.Stdout, res.Stderr)
	}
	if elapsed := time.Since(started); elapsed > itest.Scale(3*time.Minute) {
		t.Errorf("env create %s took %s; --timeout 30s should have ended it much sooner", envBad, elapsed)
	}
	t.Logf("env create %s failed as it should after %s:\n%s", envBad, time.Since(started).Round(time.Second), res.Stderr)

	detail := s.showEnv(t, envBad)
	if detail.Env.Status != cenv.StatusFailed {
		t.Errorf("status = %q, want %q", detail.Env.Status, cenv.StatusFailed)
	}
	container := cenv.ContainerName(appName, envBad, "db")
	if got := s.dockerNames(t, "label=caramelo.env="+envBad, true); !contains(got, container) {
		t.Errorf("container %s is gone; a failed env is left in place for inspection (have %v)", container, got)
	}

	env := s.clientEnv()
	itest.Git(t, s.repo, env, "checkout", brokenBranch)
	copyIn(t, s.repo, "caramelo.yaml")
	s.brokenCommit = itest.GitCommitAll(t, s.repo, env, "sampleapp: fix the readiness command")
	itest.Git(t, s.repo, env, "checkout", defaultBranch)

	fixed := s.createEnv(t, envBad, "--from", brokenBranch)
	if fixed.Commit != s.brokenCommit {
		t.Errorf("commit = %q, want the fixed commit %q", fixed.Commit, s.brokenCommit)
	}
	detail = s.showEnv(t, envBad)
	for _, dep := range detail.Deps {
		if dep.Status != cenv.DepRunning {
			t.Errorf("after the retry, dep %s is %q", dep.Name, dep.Status)
		}
	}

	s.destroyEnv(t, envBad, "--delete-branch")
}

func (s *suite) parallelCreate(t *testing.T) {
	s.needRepo(t)

	names := []string{"par-a", "par-b", "par-c", "par-d"}
	t.Cleanup(func() {
		for _, name := range names {
			s.destroyEnv(t, name)
		}
	})

	var wg sync.WaitGroup
	results := make([]itest.Result, len(names))
	errs := make([]error, len(names))
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			results[i], errs[i] = itest.RunClient(context.Background(),
				s.opts(clientOpts{Dir: s.repo, Timeout: itest.Scale(8 * time.Minute)}),
				"env", "create", name, "--from", defaultBranch, "--no-push", "--no-deps", "--json")
		}(i, name)
	}
	wg.Wait()

	bases := map[int]string{}
	for i, name := range names {
		res := results[i]
		if errs[i] != nil {
			t.Errorf("env create %s: %v\nstderr:\n%s", name, errs[i], res.Stderr)
			continue
		}
		if res.ExitCode != 0 {
			t.Errorf("env create %s: exit %d\nstderr:\n%s", name, res.ExitCode, res.Stderr)
			continue
		}
		e := decode[cenv.Env](t, "env create "+name, res.Stdout)
		if e.Status != cenv.StatusReady {
			t.Errorf("env %s: status %q, want ready", name, e.Status)
		}
		if other, ok := bases[e.PortBase]; ok {
			t.Errorf("envs %s and %s were both given port base %d", other, name, e.PortBase)
		}
		bases[e.PortBase] = name
	}
	t.Logf("four parallel envs got port bases %v", bases)
}

func (s *suite) destroy(t *testing.T) {
	s.needRepo(t)

	s.destroyEnv(t, envX)

	if got := s.dockerNames(t, "label=caramelo.env="+envX, true); len(got) != 0 {
		t.Errorf("containers left after destroy: %v", got)
	}
	if got := s.dockerVolumes(t, "label=caramelo.env="+envX); len(got) != 0 {
		t.Errorf("volumes left after destroy: %v", got)
	}
	if network := cenv.NetworkName(appName, envX); contains(s.dockerNetworks(t, "name="+network), network) {
		t.Errorf("network %s left after destroy", network)
	}
	if volume := cenv.CacheVolumeName(appName, envX); contains(s.dockerVolumes(t, "name="+volume), volume) {
		t.Errorf("cache volume %s left after destroy", volume)
	}
	if res := s.asCaramelo(t, "test -e "+cenv.EnvDir(dataDir, appName, envX)); res.ExitCode == 0 {
		t.Errorf("%s still exists after destroy", cenv.EnvDir(dataDir, appName, envX))
	}
	if refs := itest.GitRefs(t, s.repo, s.clientEnv(), s.remote); refs["refs/heads/"+envX] == "" {
		t.Errorf("branch %s was deleted by destroy; only --delete-branch may do that", envX)
	}
	if res := s.inRepo(t, "env", "destroy", envX, "--yes"); res.ExitCode != 0 {
		t.Errorf("second destroy: exit %d, want 0\nstderr:\n%s", res.ExitCode, res.Stderr)
	}

	s.destroyEnv(t, envY, "--delete-branch")
	if refs := itest.GitRefs(t, s.repo, s.clientEnv(), s.remote); refs["refs/heads/"+envY] != "" {
		t.Errorf("--delete-branch left branch %s behind", envY)
	}

	apps := decode[[]capi.AppInfo](t, "app list", s.mustInRepo(t, "app", "list", "--json").Stdout)
	var found bool
	for _, a := range apps {
		if a.Name != appName {
			continue
		}
		found = true
		if a.EnvCount != 0 {
			t.Errorf("app %s has %d env(s) left, want 0", appName, a.EnvCount)
		}
		if a.DefaultBranch != defaultBranch {
			t.Errorf("app %s default branch = %q, want %q", appName, a.DefaultBranch, defaultBranch)
		}
		if a.RepoBytes <= 0 {
			t.Errorf("app %s repo_bytes = %d", appName, a.RepoBytes)
		}
	}
	if !found {
		t.Errorf("app list does not mention %s: %+v", appName, apps)
	}
}

func (s *suite) agentCommitComesBack(t *testing.T) {
	s.needRepo(t)

	s.createEnv(t, envAgent, "--from", defaultBranch, "--no-deps")
	t.Cleanup(func() { s.destroyEnv(t, envAgent, "--delete-branch") })

	s.mustInRepo(t, "env", "exec", envAgent, "--", "sh", "-c",
		"echo 'written by the agent' > agent.txt && "+
			"git -c user.name=agent -c user.email=agent@caramelo.invalid add agent.txt && "+
			"git -c user.name=agent -c user.email=agent@caramelo.invalid commit -q -m 'the agent did some work'")
	remoteHead := strings.TrimSpace(s.mustInRepo(t, "env", "exec", envAgent, "--", "git", "rev-parse", "HEAD").Stdout)
	if remoteHead == "" || remoteHead == s.mainCommit {
		t.Fatalf("HEAD in the env is %q; the commit did not happen", remoteHead)
	}

	env := s.clientEnv()
	itest.Git(t, s.repo, env, "fetch", s.remote, envAgent+":refs/remotes/caramelo/"+envAgent)
	got := strings.TrimSpace(itest.Git(t, s.repo, env, "rev-parse", "refs/remotes/caramelo/"+envAgent).Stdout)
	if got != remoteHead {
		t.Errorf("fetched %q, want the agent's commit %q", got, remoteHead)
	}
	if body := itest.Git(t, s.repo, env, "show", remoteHead+":agent.txt").Stdout; !strings.Contains(body, "written by the agent") {
		t.Errorf("agent.txt came back as %q", body)
	}
}

func (s *suite) zzCacheImages(t *testing.T) {
	if s.m.Target() != itest.TargetDocker {
		t.Skipf("the image cache only pays for itself when the machine and the docker host share a disk; %s is driven over %s, where seeding means uploading %s to a box that pulls them itself",
			s.m.Alias, s.m.Target(), strings.Join(itest.DepImages, " and "))
	}
	itest.ExportImages(t, s.m, itest.DepImages...)
}
