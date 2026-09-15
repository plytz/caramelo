//go:build integration

package run

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	capi "github.com/plytz/caramelo/internal/api"
	cconfig "github.com/plytz/caramelo/internal/config"
	cenv "github.com/plytz/caramelo/internal/env"
	cvpn "github.com/plytz/caramelo/internal/vpn"
	"github.com/plytz/caramelo/test/integration/itest"
)

func TestRun(t *testing.T) {
	s := start(t)
	for _, step := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"UpStartsTheServices", s.upStartsTheServices},
		{"GossRun", s.gossRun},
		{"SecondUpChangesNothing", s.secondUpChangesNothing},
		{"TwoEnvsAreIsolated", s.twoEnvsAreIsolated},
		{"TestAndRun", s.testAndRun},
		{"CodeUpdate", s.codeUpdate},
		{"Logs", s.logs},
		{"DockerStack", s.dockerStack},
		{"ServiceThatCannotStart", s.serviceThatCannotStart},
		{"DownKeepsTheData", s.downKeepsTheData},
		{"ConfigShow", s.configShow},
		{"ParallelUp", s.parallelUp},
		{"Replicas", s.replicas},
		{"RestartKeepsTheAppServing", s.restartKeepsTheAppServing},
		{"ZZCacheImages", s.zzCacheImages},
		{"ZZResourcesAndLogRotation", s.zzResourcesAndLogRotation},
		{"ZZSecretsRollOutOnUp", s.zzSecretsRollOutOnUp},
		{"ZZDevAndReleaseSideBySide", s.zzDevAndReleaseSideBySide},
	} {
		t.Run(step.name, step.run)
	}
}

func (s *suite) upStartsTheServices(t *testing.T) {
	s.repo = s.initSampleRepo(t)

	e := s.createEnv(t, envX, "--from", defaultBranch)
	if e.Commit != s.mainCommit {
		t.Fatalf("env create %s: commit %q, want main at %q", envX, e.Commit, s.mainCommit)
	}

	res := s.up(t, envX)
	t.Logf("up %s: %+v", envX, res.Services)

	if want := cenv.NetworkName(appName, envX); res.Network != want {
		t.Errorf("network = %q, want %q", res.Network, want)
	}
	if len(res.Services) != 2 {
		t.Fatalf("up %s started %d service(s), want web and echo: %+v", envX, len(res.Services), res.Services)
	}

	web := service(t, "up "+envX, res.Services, webService)
	if web.Status != cenv.ServiceRunning {
		t.Errorf("web is %q, want %q (detail: %s)", web.Status, cenv.ServiceRunning, web.Detail)
	}
	if web.Health != cenv.HealthOK {
		t.Errorf("web health = %q, want %q: caramelo.yaml points health: at /healthz", web.Health, cenv.HealthOK)
	}
	if web.Change != cenv.ChangeCreated {
		t.Errorf("web change = %q, want %q on the first up", web.Change, cenv.ChangeCreated)
	}
	if want := cenv.ReplicaContainerName(appName, envX, webService, 1); web.Container != want {
		t.Errorf("web container = %q, want %q", web.Container, want)
	}
	if web.Port == 0 || !strings.Contains(web.URL, "127.0.0.1") {
		t.Errorf("web url = %q port %d, want an address on the box's loopback", web.URL, web.Port)
	}

	echo := service(t, "up "+envX, res.Services, echoService)
	if echo.Status != cenv.ServiceRunning {
		t.Errorf("echo is %q, want %q (detail: %s)", echo.Status, cenv.ServiceRunning, echo.Detail)
	}
	if echo.Health != cenv.HealthNone && echo.Health != "" {
		t.Errorf("echo health = %q, want none: a UDP service has nothing to connect to", echo.Health)
	}
	if echo.Protocol != string(cconfig.ProtocolUDP) {
		t.Errorf("echo protocol = %q, want udp", echo.Protocol)
	}
	if echo.ContainerPort != 9001 {
		t.Errorf("echo container port = %d, want 9001 (caramelo.yaml says so)", echo.ContainerPort)
	}

	webURL := s.urlOf(t, envX, webService)
	if webURL.Kind != cenv.KindService || webURL.Port != web.Port {
		t.Errorf("env url %s web = %+v, want the service's own port %d", envX, webURL, web.Port)
	}

	t.Run("env url carries the address on the machine's network", func(t *testing.T) {
		detail := s.showEnv(t, envX)
		if detail.Env.VPNIP == "" {
			t.Fatalf("%s has no address on the machine's network: %+v", envX, detail.Env)
		}
		for _, c := range []struct {
			target string
			port   int
			scheme string
		}{
			{webService, web.ContainerPort, "http://"},
			{echoService, 9001, "udp://"},
			{"db", 5432, "tcp://"},
			{"cache", 6379, "tcp://"},
		} {
			u := s.urlOf(t, envX, c.target)
			host := cvpn.ServiceHost(appName, envX, c.target)
			if u.InternalHost != host {
				t.Errorf("%s internal host = %q, want %q", c.target, u.InternalHost, host)
			}
			if u.InternalPort != c.port {
				t.Errorf("%s internal port = %d, want %d: the port the app was written for",
					c.target, u.InternalPort, c.port)
			}
			if want := fmt.Sprintf("%s%s:%d", c.scheme, host, c.port); u.InternalURL != want {
				t.Errorf("%s internal url = %q, want %q", c.target, u.InternalURL, want)
			}
			if want := fmt.Sprintf("%s:%d", detail.Env.VPNIP, c.port); u.InternalAddress != want {
				t.Errorf("%s internal address = %q, want %q", c.target, u.InternalAddress, want)
			}
			if u.Port == u.InternalPort && c.target != webService {
				t.Errorf("%s: the block port and the app's port are the same number (%d); "+
					"this test would not notice the two views being confused", c.target, u.Port)
			}
		}
	})

	t.Run("the app answers with the environment's name", func(t *testing.T) {
		body := s.get(t, webURL.URL+"/")
		if !strings.Contains(body, envX) {
			t.Errorf("GET / = %q, want it to name the environment (%s)", body, envX)
		}
	})

	t.Run("the dependencies answer by name on the environment's network", func(t *testing.T) {
		body := s.get(t, webURL.URL+"/deps")
		for _, want := range []string{"db ok", "cache ok"} {
			if !strings.Contains(body, want) {
				t.Errorf("GET /deps = %q, want %q: the service must reach the dependency at its alias", body, want)
			}
		}
	})

	t.Run("a datagram to the udp service comes back", func(t *testing.T) {
		echoURL := s.urlOf(t, envX, echoService)
		if echoURL.Protocol != string(cconfig.ProtocolUDP) || !strings.HasPrefix(echoURL.URL, "udp://") {
			t.Errorf("env url %s echo = %+v, want a udp:// address", envX, echoURL)
		}
		const payload = "caramelo"
		if got := s.echoUDP(t, echoURL.Port, payload); got != payload {
			t.Errorf("udp echo returned %q, want %q", got, payload)
		}
	})
}

func (s *suite) gossRun(t *testing.T) {
	s.needRepo(t)
	itest.RunGoss(t, s.m, itest.MustGossSpec(t, "run.yaml"))
}

func (s *suite) secondUpChangesNothing(t *testing.T) {
	s.needRepo(t)

	before := s.showEnv(t, envX)
	ids := map[string]string{}
	for _, svc := range before.Services {
		ids[svc.Name] = svc.ID
	}

	res := s.up(t, envX)
	for _, svc := range res.Services {
		if svc.Change != cenv.ChangeUnchanged {
			t.Errorf("second up: service %s reports %q, want %q (detail: %s)",
				svc.Name, svc.Change, cenv.ChangeUnchanged, svc.Detail)
		}
		if was := ids[svc.Name]; was != "" && svc.ID != was {
			t.Errorf("second up: service %s is container %s, was %s: it was recreated", svc.Name, svc.ID, was)
		}
	}

	t.Run("env show carries the services and both views", func(t *testing.T) {
		detail := s.showEnv(t, envX)
		if len(detail.Services) != 2 {
			t.Fatalf("env show %s: %d service(s), want 2: %+v", envX, len(detail.Services), detail.Services)
		}
		if want := cenv.NetworkName(appName, envX); detail.Network != want {
			t.Errorf("env show network = %q, want %q", detail.Network, want)
		}
		host := detail.Vars[string(cconfig.ViewHost)]
		network := detail.Vars[string(cconfig.ViewNetwork)]
		if host == nil || network == nil {
			t.Fatalf("env show %s: vars = %+v, want both a %s and a %s view",
				envX, detail.Vars, cconfig.ViewHost, cconfig.ViewNetwork)
		}
		if !strings.Contains(host["DATABASE_URL"], "127.0.0.1") {
			t.Errorf("host DATABASE_URL = %q, want 127.0.0.1 and the block port", host["DATABASE_URL"])
		}
		if !strings.Contains(network["DATABASE_URL"], "@db:5432") {
			t.Errorf("network DATABASE_URL = %q, want the dependency's alias and its own port", network["DATABASE_URL"])
		}
	})
}

func (s *suite) twoEnvsAreIsolated(t *testing.T) {
	s.needRepo(t)

	s.createEnv(t, envY, "--from", defaultBranch)
	t.Cleanup(func() { s.destroyEnv(t, envY, "--delete-branch") })
	s.up(t, envY)

	x, y := s.urlOf(t, envX, webService), s.urlOf(t, envY, webService)
	if x.Port == y.Port {
		t.Fatalf("both environments answer on port %d", x.Port)
	}
	for name, u := range map[string]cenv.URL{envX: x, envY: y} {
		if body := s.get(t, u.URL+"/"); !strings.Contains(body, name) {
			t.Errorf("%s answered %q, want it to name %s", u.URL, body, name)
		}
	}

	for _, name := range []string{envX, envY} {
		network := cenv.NetworkName(appName, name)
		if got := s.dockerNetworks(t, "name="+network); !contains(got, network) {
			t.Errorf("network %s missing; docker network ls has %v", network, got)
		}
		members := s.networkMembers(t, network)
		for _, want := range []string{
			cenv.ReplicaContainerName(appName, name, webService, 1),
			cenv.ContainerName(appName, name, "db"),
		} {
			if !contains(members, want) {
				t.Errorf("network %s does not hold %s: %v", network, want, members)
			}
		}
		other := envY
		if name == envY {
			other = envX
		}
		for _, member := range members {
			if strings.Contains(member, "-"+other+"-") {
				t.Errorf("network %s holds %s, a container of %s", network, member, other)
			}
		}
	}
}

func (s *suite) testAndRun(t *testing.T) {
	s.needRepo(t)

	t.Run("the test suite passes with the dependencies wired", func(t *testing.T) {
		res := s.mustInRepo(t, "test", envX)
		out := res.Stdout + res.Stderr
		if !strings.Contains(out, "OK") || !strings.Contains(out, "Ran ") {
			t.Errorf("caramelo test %s: no unittest summary in the output:\n%s", envX, out)
		}
	})

	t.Run("arguments pass through and the exit code comes back", func(t *testing.T) {
		res := s.inRepo(t, "test", envX, "--", "-k", "nonexistent")
		out := res.Stdout + res.Stderr
		if !strings.Contains(out, "Ran 0 tests") && !strings.Contains(out, "NO TESTS RAN") {
			t.Errorf("caramelo test %s -- -k nonexistent: the arguments never reached unittest:\n%s", envX, out)
		}
		if res.ExitCode == 0 {
			t.Errorf("caramelo test %s -- -k nonexistent: exit 0, want the test command's own code (5 on python 3.12)", envX)
		}
		t.Logf("test -- -k nonexistent: exit %d", res.ExitCode)
	})

	t.Run("run reaches the dependencies by name", func(t *testing.T) {
		res := s.inRepo(t, "run", envX, "--", "python", "-c",
			"import socket; socket.create_connection(('db', 5432), timeout=10)")
		if res.ExitCode != 0 {
			t.Errorf("run: exit %d\nstdout:\n%sstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
		}
	})

	t.Run("an exit code passes through", func(t *testing.T) {
		if res := s.inRepo(t, "run", envX, "--", "python", "-c", "import sys; sys.exit(7)"); res.ExitCode != 7 {
			t.Errorf("run: exit %d, want 7\nstderr:\n%s", res.ExitCode, res.Stderr)
		}
	})

	t.Run("stdin round-trips", func(t *testing.T) {
		const payload = "caramelo reads stdin\n"
		res := s.mustCommander(t, commanderOpts{Dir: s.repo, Stdin: strings.NewReader(payload)},
			"run", envX, "--", "cat")
		if !strings.Contains(res.Stdout, strings.TrimSpace(payload)) {
			t.Errorf("stdout = %q, want %q", res.Stdout, payload)
		}
	})

	t.Run("a silent command is not cut off", func(t *testing.T) {
		started := time.Now()
		res := s.commander(t, commanderOpts{Dir: s.repo, Timeout: itest.Scale(5 * time.Minute)},
			"run", envX, "--", "sleep", "90")
		if res.ExitCode != 0 {
			t.Fatalf("sleep 90: exit %d after %s\nstderr:\n%s", res.ExitCode, time.Since(started), res.Stderr)
		}
		if elapsed := time.Since(started); elapsed < 90*time.Second {
			t.Errorf("sleep 90 returned after %s; it cannot have run", elapsed)
		}
	})
}

func (s *suite) codeUpdate(t *testing.T) {
	s.needRepo(t)

	const updated = "updated hello from"
	env := s.commanderEnv()
	replaceInFile(t, s.repo, "greeting.py", `MESSAGE = "hello from"`, `MESSAGE = "`+updated+`"`)
	itest.GitCommitAll(t, s.repo, env, "sampleapp: a different greeting")

	s.up(t, envX)
	url := s.urlOf(t, envX, webService)
	if body := s.getWithin(t, url.URL+"/", itest.Scale(60*time.Second)); !strings.Contains(body, updated) {
		t.Errorf("GET / = %q, want the new greeting %q: the push did not reach the worktree", body, updated)
	}

	t.Run("a push is refused while the worktree is dirty", func(t *testing.T) {
		s.mustInRepo(t, "env", "exec", envX, "--", "sh", "-c", "echo '# the agent was here' >> greeting.py")
		replaceInFile(t, s.repo, "greeting.py", updated, "greetings from")
		itest.GitCommitAll(t, s.repo, env, "sampleapp: a greeting the environment must not get")

		res := s.inRepo(t, "up", envX)
		if res.ExitCode != 1 {
			t.Errorf("up with a dirty worktree: exit %d, want 1\nstdout:\n%sstderr:\n%s",
				res.ExitCode, res.Stdout, res.Stderr)
		}
		if worktree := cenv.WorktreePath(dataDir, appName, envX); !strings.Contains(res.Stderr, worktree) {
			t.Errorf("the refusal does not name the worktree %s:\n%s", worktree, res.Stderr)
		}
		if body := s.get(t, url.URL+"/"); strings.Contains(body, "greetings from") {
			t.Errorf("GET / = %q: the agent's worktree was overwritten anyway", body)
		}

		if res := s.inRepo(t, "up", envX, "--no-push"); res.ExitCode != 0 {
			t.Errorf("up --no-push: exit %d, want 0\nstderr:\n%s", res.ExitCode, res.Stderr)
		}
		s.mustInRepo(t, "env", "exec", envX, "--", "git", "checkout", "--", "greeting.py")
	})
}

func (s *suite) logs(t *testing.T) {
	s.needRepo(t)

	url := s.urlOf(t, envX, webService)
	s.get(t, url.URL+"/")

	t.Run("the requests are there", func(t *testing.T) {
		res := s.mustInRepo(t, "logs", envX, "--tail", "200")
		if !strings.Contains(res.Stdout, webService) {
			t.Errorf("logs %s: no %s lines:\n%s", envX, webService, res.Stdout)
		}
		if !strings.Contains(res.Stdout, "request") {
			t.Errorf("logs %s: the requests made above are not in the output:\n%s", envX, res.Stdout)
		}
	})

	t.Run("--json is one object per line", func(t *testing.T) {
		res := s.mustInRepo(t, "logs", envX, "--tail", "200", "--json")
		var seen bool
		for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var l cenv.LogLine
			if err := json.Unmarshal([]byte(line), &l); err != nil {
				t.Fatalf("logs --json: %v in %q", err, line)
			}
			if l.Service == webService {
				seen = true
			}
		}
		if !seen {
			t.Errorf("logs %s --json: no line from %s:\n%s", envX, webService, res.Stdout)
		}
	})

	t.Run("--deps adds the dependency containers", func(t *testing.T) {
		res := s.mustInRepo(t, "logs", envX, "--tail", "200", "--deps", "--json")
		var seen bool
		for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
			var l cenv.LogLine
			if strings.TrimSpace(line) == "" || json.Unmarshal([]byte(line), &l) != nil {
				continue
			}
			if l.Service == "db" {
				seen = true
			}
		}
		if !seen {
			t.Errorf("logs %s --deps --json: nothing from the db container:\n%s", envX, res.Stdout)
		}
	})

	t.Run("-f streams new lines", func(t *testing.T) {
		nonce := fmt.Sprintf("follow-probe-%d", time.Now().UnixNano())
		attachBudget := itest.Scale(45 * time.Second)
		streamBudget := itest.Scale(30 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), attachBudget+streamBudget+itest.Scale(time.Minute))
		defer cancel()

		cmd := exec.CommandContext(ctx, itest.BinaryPath(t), "logs", envX, "-f", "--tail", "1")
		cmd.Dir, cmd.Env = s.repo, s.commanderEnv()
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("logs -f: %v", err)
		}
		defer func() {
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			cancel()
			_ = cmd.Wait()
		}()

		attached, found := make(chan struct{}), make(chan struct{})
		go func() {
			sc := bufio.NewScanner(stdout)
			streaming := false
			for sc.Scan() {
				if !streaming {
					streaming = true
					close(attached)
				}
				if strings.Contains(sc.Text(), nonce) {
					close(found)
					return
				}
			}
			if !streaming {
				close(attached)
			}
		}()

		select {
		case <-attached:
		case <-time.After(attachBudget):
			t.Logf("logs -f printed nothing in %s; asking for the page anyway", attachBudget)
		}

		started := time.Now()
		deadline := time.Now().Add(streamBudget)
		for {
			s.get(t, url.URL+"/?"+nonce)
			select {
			case <-found:
				t.Logf("the new line arrived after %s", time.Since(started).Round(time.Millisecond))
				return
			case <-time.After(2 * time.Second):
			}
			if time.Now().After(deadline) {
				t.Errorf("logs -f did not stream the request %q within %s", nonce, streamBudget)
				return
			}
		}
	})
}

func (s *suite) dockerStack(t *testing.T) {
	s.needRepo(t)

	e := s.createEnv(t, envZ, "--from", dockerBranch)
	if e.Commit != s.dockerCommit {
		t.Fatalf("env create %s: commit %q, want the docker branch at %q", envZ, e.Commit, s.dockerCommit)
	}

	res := s.up(t, envZ, "--no-push")
	if res.Stack != "docker" {
		t.Errorf("up %s: stack = %q, want docker", envZ, res.Stack)
	}
	if !strings.HasPrefix(res.Image, cenv.NamePrefix+"/"+appName+":") {
		t.Errorf("up %s: image = %q, want %s/%s:<tree hash>", envZ, res.Image, cenv.NamePrefix, appName)
	}
	if len(res.Services) != 1 {
		t.Fatalf("up %s: %d service(s), want the one detection produced: %+v", envZ, len(res.Services), res.Services)
	}
	built := res.Image
	svc := res.Services[0]
	if svc.Image != built {
		t.Errorf("service %s runs %q, want the built image %q", svc.Name, svc.Image, built)
	}
	if body := s.get(t, s.urlOf(t, envZ, svc.Name).URL+"/"); !strings.Contains(body, envZ) {
		t.Errorf("GET / = %q, want it to name %s", body, envZ)
	}

	t.Run("an unchanged tree is not rebuilt", func(t *testing.T) {
		again := s.up(t, envZ, "--no-push")
		if again.Image != built {
			t.Errorf("image = %q, want the same %q: the tree did not change", again.Image, built)
		}
		if got := service(t, "up "+envZ, again.Services, svc.Name); got.Change != cenv.ChangeUnchanged || got.ID != svc.ID {
			t.Errorf("service %s reports %q and container %s (was %s): nothing changed, so nothing should have been done",
				got.Name, got.Change, got.ID, svc.ID)
		}
	})

	t.Run("a commit rebuilds", func(t *testing.T) {
		env := s.commanderEnv()
		itest.Git(t, s.repo, env, "checkout", dockerBranch)
		t.Cleanup(func() { itest.Git(t, s.repo, env, "checkout", defaultBranch) })
		replaceInFile(t, s.repo, "greeting.py", `MESSAGE = `, `MESSAGE = "rebuilt " + `)
		s.dockerCommit = itest.GitCommitAll(t, s.repo, env, "sampleapp: a change that must be built in")

		rebuilt := s.up(t, envZ)
		if rebuilt.Image == built {
			t.Errorf("image = %q, unchanged after a commit: the tree hash did not move", rebuilt.Image)
		}
		if body := s.getWithin(t, s.urlOf(t, envZ, svc.Name).URL+"/", itest.Scale(60*time.Second)); !strings.Contains(body, "rebuilt") {
			t.Errorf("GET / = %q, want the rebuilt greeting", body)
		}
	})
}

func (s *suite) serviceThatCannotStart(t *testing.T) {
	s.needRepo(t)

	s.createEnv(t, envCrash, "--from", crashBranch)
	t.Cleanup(func() { s.destroyEnv(t, envCrash, "--delete-branch") })

	started := time.Now()
	res := s.commander(t, commanderOpts{Dir: s.repo, Timeout: itest.Scale(5 * time.Minute)},
		"up", envCrash, "--no-push", "--timeout", "45s")
	if res.ExitCode != 1 {
		t.Fatalf("up %s: exit %d, want 1\nstdout:\n%sstderr:\n%s", envCrash, res.ExitCode, res.Stdout, res.Stderr)
	}
	if elapsed := time.Since(started); elapsed > itest.Scale(4*time.Minute) {
		t.Errorf("up %s took %s; --timeout 45s should have ended it much sooner", envCrash, elapsed)
	}
	if !strings.Contains(res.Stderr, "giving up") {
		t.Errorf("stderr does not carry the container's own last lines:\n%s", res.Stderr)
	}

	detail := s.showEnv(t, envCrash)
	if len(detail.Services) != 1 {
		t.Fatalf("env show %s: %d service(s), want 1: %+v", envCrash, len(detail.Services), detail.Services)
	}
	switch got := detail.Services[0].Status; got {
	case cenv.ServiceFailed, cenv.ServiceExited, cenv.ServiceStarting:
		t.Logf("the failed service is %q, and its container is still there", got)
	default:
		t.Errorf("service status = %q, want failed or exited", got)
	}
	container := cenv.ReplicaContainerName(appName, envCrash, webService, 1)
	if got := s.dockerNames(t, "label="+cenv.LabelEnv+"="+envCrash, true); !contains(got, container) {
		t.Errorf("container %s is gone; a failed service is left in place for inspection (have %v)", container, got)
	}
	if detail.Env.Status != cenv.StatusReady {
		t.Errorf("env %s is %q, want %q: a failed service does not fail the environment",
			envCrash, detail.Env.Status, cenv.StatusReady)
	}

	if res := s.inRepo(t, "down", envCrash); res.ExitCode != 0 {
		t.Errorf("down: exit %d, want 0\nstderr:\n%s", res.ExitCode, res.Stderr)
	}
	if got := s.dockerNames(t, "label="+cenv.LabelEnv+"="+envCrash, true); contains(got, container) {
		t.Errorf("down left %s behind: %v", container, got)
	}
	if res := s.inRepo(t, "down", envCrash); res.ExitCode != 0 {
		t.Errorf("second down: exit %d, want 0 (down is idempotent)\nstderr:\n%s", res.ExitCode, res.Stderr)
	}
}

func (s *suite) downKeepsTheData(t *testing.T) {
	s.needRepo(t)

	query := func(sql string) itest.Result {
		t.Helper()
		return s.inRepo(t, "env", "exec", envX, "--",
			"docker", "exec", cenv.ContainerName(appName, envX, "db"),
			"psql", "-U", "postgres", "-tAc", sql)
	}
	if res := query("create table if not exists kept (v text); insert into kept values ('survived')"); res.ExitCode != 0 {
		t.Fatalf("writing a row: exit %d\nstderr:\n%s", res.ExitCode, res.Stderr)
	}

	res := s.mustInRepo(t, "down", envX, "--json")
	down := decode[capi.DownResult](t, "down "+envX, res.Stdout)
	if len(down.Services) != 2 {
		t.Errorf("down %s removed %d service(s), want 2: %+v", envX, len(down.Services), down.Services)
	}
	for _, svc := range down.Services {
		if svc.Change != cenv.ChangeRemoved {
			t.Errorf("down: service %s reports %q, want %q", svc.Name, svc.Change, cenv.ChangeRemoved)
		}
	}

	running := s.dockerNames(t, "label="+cenv.LabelEnv+"="+envX, false)
	for _, name := range []string{webService, echoService} {
		if container := cenv.ReplicaContainerName(appName, envX, name, 1); contains(running, container) {
			t.Errorf("service container %s is still running after down: %v", container, running)
		}
	}
	for _, dep := range []string{"db", "cache"} {
		if container := cenv.ContainerName(appName, envX, dep); !contains(running, container) {
			t.Errorf("dependency %s was stopped by down; only `env destroy` may do that: %v", container, running)
		}
	}
	if network := cenv.NetworkName(appName, envX); !contains(s.dockerNetworks(t, "name="+network), network) {
		t.Errorf("down removed the network %s", network)
	}
	if vol := cenv.CacheVolumeName(appName, envX); !contains(s.dockerVolumes(t, "name="+vol), vol) {
		t.Errorf("down removed the cache volume %s", vol)
	}

	s.up(t, envX)
	if got := strings.TrimSpace(query("select v from kept").Stdout); got != "survived" {
		t.Errorf("the row written before down reads back as %q, want %q", got, "survived")
	}
	if body := s.getWithin(t, s.urlOf(t, envX, webService).URL+"/", itest.Scale(60*time.Second)); !strings.Contains(body, envX) {
		t.Errorf("GET / after up = %q, want it to name %s", body, envX)
	}
}

func (s *suite) configShow(t *testing.T) {
	s.needRepo(t)

	t.Run("a detected stack says what it inferred and why", func(t *testing.T) {
		res := s.mustInRepo(t, "config", "show", envZ, "--json")
		cfg := decode[capi.EffectiveConfig](t, "config show "+envZ, res.Stdout)
		if cfg.Stack != "docker" {
			t.Errorf("stack = %q, want docker", cfg.Stack)
		}
		if len(cfg.Fields) == 0 {
			t.Fatalf("config show %s: no fields", envZ)
		}
		var detected int
		for _, f := range cfg.Fields {
			switch f.Source {
			case capi.SourceFile, capi.SourceDetected, capi.SourceDefault:
			default:
				t.Errorf("field %s has source %q", f.Key, f.Source)
			}
			if f.Source == capi.SourceDetected {
				detected++
				if f.Evidence == "" {
					t.Errorf("field %s is detected but carries no evidence", f.Key)
				}
			}
		}
		if detected == 0 {
			t.Errorf("config show %s: nothing is detected, but the branch says nothing about how to run: %+v",
				envZ, cfg.Fields)
		}
	})

	t.Run("what the file says comes from the file", func(t *testing.T) {
		res := s.mustInRepo(t, "config", "show", envX, "--json")
		cfg := decode[capi.EffectiveConfig](t, "config show "+envX, res.Stdout)
		sources := map[string]capi.Source{}
		for _, f := range cfg.Fields {
			sources[f.Key] = f.Source
		}
		for _, key := range []string{"test", "services.web.run", "services.web.image"} {
			if got := sources[key]; got != capi.SourceFile {
				t.Errorf("%s came from %q, want %q: it is written in caramelo.yaml (fields: %+v)",
					key, got, capi.SourceFile, cfg.Fields)
			}
		}
	})
}

func (s *suite) parallelUp(t *testing.T) {
	s.needRepo(t)

	names := []string{"par-a", "par-b", "par-c", "par-d"}
	t.Cleanup(func() {
		for _, name := range names {
			s.destroyEnv(t, name, "--delete-branch")
		}
	})
	for _, name := range names {
		s.createEnv(t, name, "--from", defaultBranch, "--no-push", "--no-deps")
	}

	var wg sync.WaitGroup
	results := make([]itest.Result, len(names))
	errs := make([]error, len(names))
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			results[i], errs[i] = itest.RunCommander(context.Background(),
				s.opts(commanderOpts{Dir: s.repo, Timeout: itest.Scale(10 * time.Minute)}),
				"up", name, "--no-push", "--service", webService, "--json")
		}(i, name)
	}
	wg.Wait()

	for i, name := range names {
		res := results[i]
		if errs[i] != nil {
			t.Errorf("up %s: %v\nstderr:\n%s", name, errs[i], res.Stderr)
			continue
		}
		if res.ExitCode != 0 {
			t.Errorf("up %s: exit %d\nstderr:\n%s", name, res.ExitCode, res.Stderr)
			continue
		}
		out := decode[capi.UpResult](t, "up "+name, res.Stdout)
		web := service(t, "up "+name, out.Services, webService)
		if web.Status != cenv.ServiceRunning {
			t.Errorf("env %s: web is %q (detail: %s)", name, web.Status, web.Detail)
			continue
		}
		if body := s.get(t, s.urlOf(t, name, webService).URL+"/"); !strings.Contains(body, name) {
			t.Errorf("env %s answered %q, want it to name itself", name, body)
		}
	}
}

func (s *suite) replicas(t *testing.T) {
	s.needRepo(t)

	s.createEnv(t, envReplicas, "--from", replicasBranch)
	t.Cleanup(func() { s.destroyEnv(t, envReplicas, "--delete-branch") })

	out := s.up(t, envReplicas, "--no-push")
	web := service(t, "up "+envReplicas, out.Services, webService)
	if len(web.Replicas) != 2 {
		t.Fatalf("up %s: web has %d replica(s), want 2: %+v", envReplicas, len(web.Replicas), web.Replicas)
	}

	ports := map[int]int{}
	for _, r := range web.Replicas {
		if r.Index != 1 && r.Index != 2 {
			t.Errorf("replica index %d, want 1 or 2", r.Index)
		}
		if r.State != cenv.ReplicaActive {
			t.Errorf("replica %d is %q, want %q (detail: %s)", r.Index, r.State, cenv.ReplicaActive, r.Detail)
		}
		if r.Health != cenv.HealthOK {
			t.Errorf("replica %d health is %q, want %q", r.Index, r.Health, cenv.HealthOK)
		}
		if r.Port == 0 {
			t.Errorf("replica %d has no port: every replica publishes on its own", r.Index)
		}
		if ports[r.Port] != 0 {
			t.Errorf("replicas %d and %d share port %d", ports[r.Port], r.Index, r.Port)
		}
		ports[r.Port] = r.Index
	}

	t.Run("one container per replica", func(t *testing.T) {
		names := s.dockerNames(t, "label=caramelo.env="+envReplicas, true)
		for _, index := range []int{1, 2} {
			want := cenv.ReplicaContainerName(appName, envReplicas, webService, index)
			if !contains(names, want) {
				t.Errorf("no container %s; the environment has %v", want, names)
			}
		}
	})

	t.Run("each replica answers on its own port and says which it is", func(t *testing.T) {
		seen := map[string]bool{}
		for port, index := range ports {
			body := s.getWithin(t, fmt.Sprintf("http://127.0.0.1:%d/", port), itest.Scale(30*time.Second))
			if !strings.Contains(body, envReplicas) {
				t.Errorf("replica %d answered %q, want it to name the environment", index, body)
			}
			id := replicaID(body)
			if id == "" {
				t.Errorf("replica %d answered %q with no replica line", index, body)
			}
			if seen[id] {
				t.Errorf("two replicas reported the same identity %q: a rollout would be invisible", id)
			}
			seen[id] = true
		}
	})

	t.Run("logs are prefixed per replica", func(t *testing.T) {
		res := s.mustInRepo(t, "logs", envReplicas, "--tail", "200")
		for _, index := range []int{1, 2} {
			want := fmt.Sprintf("%s/%d", webService, index)
			if !strings.Contains(res.Stdout, want) {
				t.Errorf("logs %s: no %q prefix:\n%s", envReplicas, want, res.Stdout)
			}
		}
	})

	t.Run("env show reports both", func(t *testing.T) {
		detail := s.showEnv(t, envReplicas)
		shown := service(t, "env show "+envReplicas, detail.Services, webService)
		if len(shown.Replicas) != 2 {
			t.Errorf("env show %s: web has %d replica(s), want 2", envReplicas, len(shown.Replicas))
		}
		if len(detail.Routes) != 0 {
			t.Errorf("env show %s: %d route(s) on an app with no domain: %+v",
				envReplicas, len(detail.Routes), detail.Routes)
		}
	})
}

func (s *suite) restartKeepsTheAppServing(t *testing.T) {
	s.needRepo(t)

	before := s.showEnv(t, envX)
	ids := map[string]string{}
	for _, svc := range before.Services {
		ids[svc.Name] = svc.ID
	}
	if len(ids) != 2 {
		t.Fatalf("env show %s before the power cycle: %d service(s), want 2: %+v", envX, len(ids), before.Services)
	}
	url := s.urlOf(t, envX, webService)
	echoURL := s.urlOf(t, envX, echoService)

	itest.MustRestart(t, s.m)
	s.refresh(t)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(5*time.Minute))
	defer cancel()
	if err := itest.WaitForCaramelod(ctx, s.m); err != nil {
		t.Fatalf("caramelod did not come back after the power cycle: %v", err)
	}

	if body := s.getWithin(t, url.URL+"/", itest.Scale(3*time.Minute)); !strings.Contains(body, envX) {
		t.Errorf("GET / after the power cycle = %q, want it to name %s", body, envX)
	}
	if got := s.echoUDP(t, echoURL.Port, "caramelo"); got != "caramelo" {
		t.Errorf("udp echo after the power cycle returned %q, want %q", got, "caramelo")
	}

	after := s.showEnv(t, envX)
	for _, svc := range after.Services {
		if svc.Status != cenv.ServiceRunning {
			t.Errorf("service %s is %q after the power cycle, want %q (detail: %s)",
				svc.Name, svc.Status, cenv.ServiceRunning, svc.Detail)
		}
		if was := ids[svc.Name]; was != "" && svc.ID != was {
			t.Errorf("service %s is container %s after the power cycle, was %s: "+
				"restart: unless-stopped brings the container back, it does not replace it", svc.Name, svc.ID, was)
		}
	}
	if after.Env.PortBase != before.Env.PortBase {
		t.Errorf("port base = %d after the power cycle, want the allocation to be durable at %d",
			after.Env.PortBase, before.Env.PortBase)
	}
	if got := s.urlOf(t, envX, webService); got.Port != url.Port {
		t.Errorf("web answers on port %d after the power cycle, want the same %d", got.Port, url.Port)
	}

	t.Run("and up still reports nothing to do", func(t *testing.T) {
		res := s.up(t, envX, "--no-push")
		for _, svc := range res.Services {
			if svc.Change != cenv.ChangeUnchanged {
				t.Errorf("up after the power cycle: service %s reports %q, want %q (detail: %s)",
					svc.Name, svc.Change, cenv.ChangeUnchanged, svc.Detail)
			}
		}
	})
}

func replicaID(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "replica "); ok {
			return v
		}
	}
	return ""
}

func (s *suite) zzCacheImages(t *testing.T) {
	if s.m.Target() != itest.TargetDocker {
		t.Skipf("run suite: the image cache feeds the container tier; %s keeps the images it pulled itself", s.m.Alias)
	}
	itest.ExportImages(t, s.m, itest.RunImages...)
}

func (s *suite) zzResourcesAndLogRotation(t *testing.T) {
	s.needRepo(t)
	s.initM7Branch(t)

	s.createEnv(t, envM7, "--from", m7Branch)
	s.up(t, envM7, "--no-push")

	container := cenv.ReplicaContainerName(appName, envM7, webService, 1)
	dep := cenv.ContainerName(appName, envM7, "db")

	t.Run("resources become --memory and --cpus", func(t *testing.T) {
		if got := s.inspect(t, container, "{{.HostConfig.Memory}}"); got != "134217728" {
			t.Errorf("Memory = %q, want 134217728 (128 MiB)", got)
		}
		quota := s.inspect(t, container, "{{.HostConfig.NanoCpus}}")
		if quota == "0" || quota == "" {
			cfs := s.inspect(t, container, "{{.HostConfig.CpuQuota}}")
			if cfs == "0" || cfs == "" {
				t.Errorf("neither NanoCpus nor CpuQuota is set: resources.cpu must become --cpus")
			}
		}
	})

	t.Run("every container rotates its logs", func(t *testing.T) {
		for _, name := range []string{container, dep} {
			opts := s.inspect(t, name, "{{json .HostConfig.LogConfig}}")
			for _, want := range []string{"max-size", "max-file"} {
				if !strings.Contains(opts, want) {
					t.Errorf("%s has no %s in its log config: %s", name, want, opts)
				}
			}
		}
	})

	t.Run("the dependency took the password from the vault", func(t *testing.T) {
		shown := s.secretsExport(t, envM7, "--reveal")
		password := shown.Values["DB_PASSWORD"]
		if password == "" {
			t.Fatalf("no DB_PASSWORD for %s: %v", envM7, shown.Values)
		}
		t.Logf("%s's DB_PASSWORD resolves from %s", envM7, shown.Sources["DB_PASSWORD"])
		res := s.psql(t, envM7, password, "select 1")
		if res.ExitCode != 0 {
			t.Errorf("psql with the resolved password: exit %d: %s", res.ExitCode, res.Stderr)
		}
	})
}

func (s *suite) zzSecretsRollOutOnUp(t *testing.T) {
	s.needRepo(t)
	if !s.envExists(t, envM7) {
		t.Skip("run suite: " + envM7 + " was never created (an earlier step failed)")
	}

	const greeting = "hello from the vault"
	s.secretsSet(t, envM7, "GREETING="+greeting)

	out := s.up(t, envM7, "--no-push")
	if len(out.Services) == 0 {
		t.Fatal("up answered no services")
	}
	web := service(t, "up "+envM7, out.Services, webService)
	if web.Change == cenv.ChangeUnchanged {
		t.Errorf("up left the service alone after a secret changed; the definition moved")
	}

	container := cenv.ReplicaContainerName(appName, envM7, webService, 1)
	env := s.inspect(t, container, `{{range .Config.Env}}{{println .}}{{end}}`)
	if !strings.Contains(env, "GREETING="+greeting) {
		t.Errorf("the container's environment has no GREETING=%q:\n%s", greeting, env)
	}

	t.Run("and no plaintext is left on disk", func(t *testing.T) {
		secretsDir := runDir + "/secrets"
		left := s.onBox(t, "sudo -n ls -1A "+itest.ShellQuote(secretsDir)+" 2>/dev/null || true")
		if out := strings.Fields(left.Stdout); len(out) != 0 {
			t.Errorf("%s holds %v: an env-file lives for one docker run", secretsDir, out)
		}
		found := s.onBox(t, "sudo -n grep -rl "+itest.ShellQuote(greeting)+
			" "+itest.ShellQuote(stateDir)+" "+itest.ShellQuote(configDir)+" 2>/dev/null || true")
		if out := strings.TrimSpace(found.Stdout); out != "" {
			t.Errorf("the secret is in plaintext in Caramelo's own state: %s", out)
		}
		stray := dataDir + "/secrets"
		probe := s.onBox(t, "sudo -n ls -ld "+itest.ShellQuote(stray)+" 2>/dev/null || true")
		if out := strings.TrimSpace(probe.Stdout); out != "" {
			t.Errorf("%s exists: an env-file directory was made beside the data directory: %s", stray, out)
		}
	})
}

func (s *suite) zzDevAndReleaseSideBySide(t *testing.T) {
	s.needRepo(t)
	if !s.envExists(t, envM7) {
		t.Skip("run suite: " + envM7 + " was never created (an earlier step failed)")
	}

	rel := s.createEnv(t, envM7Release, "--release", "--from", m7Branch)
	if rel.Mode != cenv.ModeRelease {
		t.Fatalf("env create --release: mode %q, want %q", rel.Mode, cenv.ModeRelease)
	}

	res := s.commander(t, commanderOpts{Dir: s.repo, Timeout: itest.Scale(12 * time.Minute)},
		"deploy", envM7Release, "--json", "--no-push")
	if res.ExitCode != 0 {
		t.Fatalf("deploy %s: exit %d\nstdout:\n%sstderr:\n%s",
			envM7Release, res.ExitCode, res.Stdout, res.Stderr)
	}
	out := decode[capi.DeployResult](t, "deploy "+envM7Release, res.Stdout)
	if out.Deploy == nil || out.Deploy.Status != cenv.DeployPromoted {
		t.Fatalf("deploy %s ended %+v", envM7Release, out.Deploy)
	}
	t.Logf("deploy %s: release %s", envM7Release, out.Deploy.Release.Short())

	t.Run("the dev environment still serves its mounted worktree", func(t *testing.T) {
		url := s.urlOf(t, envM7, webService)
		body := s.get(t, url.URL)
		if !strings.Contains(body, envM7) {
			t.Errorf("GET %s = %q, want it to name %s", url.URL, body, envM7)
		}
	})

	t.Run("the release environment runs an image, not a mount", func(t *testing.T) {
		container := cenv.ReplicaContainerName(appName, envM7Release, webService, 1)
		names := s.dockerNames(t, "name="+container, true)
		if len(names) == 0 {
			container = cenv.ServiceContainerName(appName, envM7Release, webService)
			names = s.dockerNames(t, "name="+container, true)
		}
		if len(names) == 0 {
			t.Fatalf("no container for %s/%s", envM7Release, webService)
		}
		image := s.inspect(t, names[0], "{{.Config.Image}}")
		if !strings.HasPrefix(image, "caramelo/"+appName+"/") {
			t.Errorf("the release's container runs %q, want a caramelo/%s/<service>:<tree> image",
				image, appName)
		}
		mounts := s.inspect(t, names[0], `{{range .Mounts}}{{println .Destination}}{{end}}`)
		if strings.Contains(mounts, "/app") {
			t.Errorf("a release environment's container has the worktree mounted at /app: %s", mounts)
		}
	})

	t.Run("env list shows both, with their modes", func(t *testing.T) {
		list := decode[[]cenv.Env](t, "env list --all",
			s.mustInRepo(t, "env", "list", "--all", "--json").Stdout)
		modes := map[string]cenv.Mode{}
		for _, e := range list {
			modes[e.Name] = e.Mode
		}
		if modes[envM7] != cenv.ModeDev {
			t.Errorf("%s is %q, want %q", envM7, modes[envM7], cenv.ModeDev)
		}
		if modes[envM7Release] != cenv.ModeRelease {
			t.Errorf("%s is %q, want %q", envM7Release, modes[envM7Release], cenv.ModeRelease)
		}
	})

	t.Run("and the two verbs stay apart", func(t *testing.T) {
		if res := s.inRepo(t, "up", envM7Release, "--no-push"); res.ExitCode == 0 {
			t.Error("caramelo up succeeded on a release environment")
		} else if !strings.Contains(res.Stderr, "deploy") {
			t.Errorf("the refusal does not name deploy: %q", res.Stderr)
		}
		if res := s.inRepo(t, "deploy", envM7, "--no-push"); res.ExitCode == 0 {
			t.Error("caramelo deploy succeeded on a development environment")
		} else if !strings.Contains(res.Stderr, "--release") {
			t.Errorf("the refusal does not name --release: %q", res.Stderr)
		}
	})
}
