package env

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/ports"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/stack"
)

func serviceConfig() *config.App {
	cfg := sampleConfig()
	cfg.Services = []config.Service{
		{
			Name:    "web",
			Image:   "python:3.12-alpine",
			Install: "pip install -r requirements.txt",
			Run:     "python app.py",
			Health:  &config.Health{Path: "/healthz"},
			Env:     map[string]string{"SERVICE_DSN": "postgres://${deps.db.host}:${deps.db.port}/x"},
		},
		{Name: "echo", Image: "python:3.12-alpine", Run: "python echo.py", Protocol: config.ProtocolUDP, Port: 9999},
	}
	cfg.Test = "python -m unittest"
	return cfg
}

func upHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.cfg = serviceConfig()
	h.m.UpTimeout = 50 * time.Millisecond
	h.m.DialTCP = func(context.Context, string, time.Duration) error { return nil }
	h.m.HTTPStatus = func(context.Context, string) (int, error) { return 200, nil }
	return h
}

func (h *harness) up(req UpRequest) (*UpResult, error) {
	h.t.Helper()
	return h.m.Up(context.Background(), req, &h.out)
}

func (h *harness) mustUp(req UpRequest) *UpResult {
	h.t.Helper()
	res, err := h.up(req)
	if err != nil {
		h.t.Fatalf("Up(%s): %v\n%s", req.Name, err, h.out.String())
	}
	return res
}

func TestUpStartsEveryServiceOnTheEnvNetwork(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	if len(res.Services) != 2 {
		t.Fatalf("up started %d services, want 2\n%s", len(res.Services), h.out.String())
	}
	web := res.Services[0]
	if web.Name != "web" || web.Status != ServiceRunning || web.Change != ChangeCreated {
		t.Errorf("web = %+v, want a created, running service", web)
	}

	if web.Port != ports.Base || web.ContainerPort != ports.Base {
		t.Errorf("web ports = %d/%d, want the block port %d in both views", web.Port, web.ContainerPort, ports.Base)
	}
	if web.URL != fmt.Sprintf("http://127.0.0.1:%d", ports.Base) {
		t.Errorf("web URL = %q", web.URL)
	}
	echo := res.Services[1]
	if echo.Port != ports.Base+3 || echo.ContainerPort != 9999 {
		t.Errorf("echo ports = %d/%d, want %d published to its own 9999", echo.Port, echo.ContainerPort, ports.Base+3)
	}
	if echo.Health != HealthNone {
		t.Errorf("echo health = %q, want none: a UDP service cannot be probed by connecting", echo.Health)
	}

	spec, ok := h.driver.spec(ReplicaContainerName("shop", "feat-x", "web", 1))
	if !ok {
		t.Fatal("no container was started for web")
	}
	if spec.Network != NetworkName("shop", "feat-x") || !reflect.DeepEqual(spec.Aliases, []string{"web"}) {
		t.Errorf("web is on network %q as %v, want the env network under its own name", spec.Network, spec.Aliases)
	}
	if spec.User != rootUser {
		t.Errorf("web runs as %q, want root so the mounted worktree is writable", spec.User)
	}
	if spec.WorkDir != MountPath || len(spec.Binds) != 1 || spec.Binds[0].Path != MountPath {
		t.Errorf("web mounts %v at %q, want the worktree at %s", spec.Binds, spec.WorkDir, MountPath)
	}
	if spec.Binds[0].Host != WorktreePath(h.data, "shop", "feat-x") {
		t.Errorf("web mounts %q, want the env's worktree", spec.Binds[0].Host)
	}
	if spec.Restart != runtime.RestartUnlessStopped {
		t.Errorf("web restart policy = %q, want %q", spec.Restart, runtime.RestartUnlessStopped)
	}
	want := []runtime.PortMap{{HostIP: DepHost, HostPort: ports.Base, ContainerPort: ports.Base, Protocol: "tcp"}}
	if !reflect.DeepEqual(spec.Publish, want) {
		t.Errorf("web publishes %v, want %v", spec.Publish, want)
	}
	if !strings.Contains(spec.Command[2], "pip install") || !strings.Contains(spec.Command[2], "python app.py") {
		t.Errorf("web command = %q, want the install step before the run command", spec.Command)
	}

	udp, _ := h.driver.spec(ReplicaContainerName("shop", "feat-x", "echo", 1))
	if len(udp.Publish) != 1 || udp.Publish[0].Protocol != string(config.ProtocolUDP) {
		t.Errorf("echo publishes %v, want a UDP mapping", udp.Publish)
	}
}

func TestUpGivesServicesTheNetworkViewOfTheVariables(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	spec, _ := h.driver.spec(ReplicaContainerName("shop", "feat-x", "web", 1))
	if got := spec.Env["DATABASE_URL"]; got != "postgres://postgres:caramelo@db:5432/postgres" {
		t.Errorf("DATABASE_URL = %q, want the dependency's network alias and container port", got)
	}
	if got := spec.Env["SERVICE_DSN"]; got != "postgres://db:5432/x" {
		t.Errorf("the service's own variables were not expanded in the network view: %q", got)
	}
	if got := spec.Env["CARAMELO_SERVICE"]; got != "web" {
		t.Errorf("CARAMELO_SERVICE = %q, want the service's name", got)
	}
	if got := spec.Env["PORT"]; got != fmt.Sprint(ports.Base) {
		t.Errorf("PORT = %q, want the block port", got)
	}

	e, _, _, err := h.m.Show(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if got := e.Vars["DATABASE_URL"]; !strings.Contains(got, "127.0.0.1:"+fmt.Sprint(ports.Base+1)) {
		t.Errorf("the env's own DATABASE_URL = %q, want the host view", got)
	}
}

func TestUpAttachesTheDependenciesToTheNetwork(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	for _, dep := range []string{"db", "cache"} {
		aliases, ok := h.driver.networkOf(NetworkName("shop", "feat-x"), ContainerName("shop", "feat-x", dep))
		if !ok {
			t.Errorf("dependency %q was not attached to the env network", dep)
			continue
		}
		if !reflect.DeepEqual(aliases, []string{dep}) {
			t.Errorf("dependency %q is on the network as %v, want its own name", dep, aliases)
		}
	}
}

func TestUpTwiceChangesNothing(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	first := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	before := len(h.driver.specs)

	second := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if len(h.driver.specs) != before {
		t.Errorf("the second up started %d more containers, want none", len(h.driver.specs)-before)
	}
	for i, s := range second.Services {
		if s.Change != ChangeUnchanged {
			t.Errorf("service %q = %q on the second up, want unchanged (%s)", s.Name, s.Change, s.Detail)
		}
		if s.ID != first.Services[i].ID {
			t.Errorf("service %q was replaced: id %q → %q", s.Name, first.Services[i].ID, s.ID)
		}
	}
}

func TestUpRecreatesAServiceWhoseDefinitionChanged(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	h.cfg.Services[0].Run = "python app.py --reload"
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if res.Services[0].Change != ChangeRecreated {
		t.Fatalf("web = %q after its command changed, want recreated", res.Services[0].Change)
	}
	if !strings.Contains(res.Services[0].Detail, "command") {
		t.Errorf("detail = %q, want it to name what changed", res.Services[0].Detail)
	}
	if res.Services[1].Change != ChangeUnchanged {
		t.Errorf("echo = %q, want the untouched service left alone", res.Services[1].Change)
	}
}

func TestUpOnlyTouchesTheServicesItWasAskedFor(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x", Services: []string{"echo"}})
	if len(res.Services) != 1 || res.Services[0].Name != "echo" {
		t.Fatalf("up --service echo acted on %d services", len(res.Services))
	}
	if _, ok := h.driver.spec(ReplicaContainerName("shop", "feat-x", "web", 1)); ok {
		t.Error("web was started although only echo was asked for")
	}
	if _, err := h.up(UpRequest{App: "shop", Name: "feat-x", Services: []string{"nope"}}); err == nil {
		t.Fatal("up --service nope succeeded")
	} else if !strings.Contains(err.Error(), `"echo"`) {
		t.Errorf("err %q does not say what services there are", err)
	}
}

func TestUpFailsWhenAServiceNeverBecomesHealthy(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.m.HTTPStatus = func(context.Context, string) (int, error) { return 0, errors.New("connection refused") }
	h.driver.logs[ReplicaContainerName("shop", "feat-x", "web", 1)] = "Traceback: no such module\n"

	res, err := h.up(UpRequest{App: "shop", Name: "feat-x"})
	if err == nil {
		t.Fatal("up of a service that never answers succeeded")
	}
	if !strings.Contains(err.Error(), "web") || !strings.Contains(err.Error(), "healthy") {
		t.Errorf("err %q does not name the service and what failed", err)
	}
	if !strings.Contains(h.out.String(), "Traceback") {
		t.Errorf("the service's last lines were not shown:\n%s", h.out.String())
	}

	if _, ok := h.driver.spec(ReplicaContainerName("shop", "feat-x", "web", 1)); !ok {
		t.Error("the failed service's container was removed")
	}
	if h.status("feat-x") != "ready" {
		t.Errorf("env status = %q after a failed up, want it left ready", h.status("feat-x"))
	}
	if res == nil || res.Services[0].Status != ServiceFailed || res.Services[0].Health != HealthFailed {
		t.Errorf("the result does not report the failed service: %+v", res)
	}
	row, ok := h.store.service(1, "web")
	if !ok || row.Status != "failed" {
		t.Errorf("the stored row says %q, want failed", row.Status)
	}
}

func TestUpRunsAHealthCommandInsideTheContainer(t *testing.T) {
	h := upHarness(t)
	h.cfg.Services = []config.Service{{
		Name: "web", Image: "python:3.12-alpine", Run: "python app.py",
		Health: &config.Health{Command: []string{"python", "-c", "print(1)"}},
	}}
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	var got []string
	h.driver.ExecResult = func(name string, argv []string) (runner.Result, error) {
		if name == ReplicaContainerName("shop", "feat-x", "web", 1) {
			got = argv
		}
		return runner.Result{}, nil
	}
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if !reflect.DeepEqual(got, []string{"python", "-c", "print(1)"}) {
		t.Errorf("the health command was %v, want the one the config wrote", got)
	}
}

func TestUpWithoutAPortHasNothingToWaitFor(t *testing.T) {
	h := upHarness(t)
	h.cfg.Services = []config.Service{{Name: "worker", Image: "python:3.12-alpine", Run: "python worker.py", Port: config.PortNone}}
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.m.DialTCP = func(context.Context, string, time.Duration) error {
		t.Error("a service without a port was probed")
		return nil
	}
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if res.Services[0].Port != 0 || res.Services[0].URL != "" {
		t.Errorf("worker = %+v, want no port and no address", res.Services[0])
	}
	spec, _ := h.driver.spec(ContainerName("shop", "feat-x", "worker"))
	if len(spec.Publish) != 0 {
		t.Errorf("worker publishes %v, want nothing", spec.Publish)
	}
}

func TestUpRecordsAServiceWithNoCheckAsRunning(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	row, ok := h.store.service(1, "echo")
	if !ok || row.Status != "running" {
		t.Fatalf("the UDP service is stored as %q, want running: there is nothing to wait for", row.Status)
	}
	live, err := h.m.Services(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if live[1].Status != ServiceRunning {
		t.Errorf("env show reports the UDP service as %q", live[1].Status)
	}
}

func TestUpStartsTheServicesOfAnEnvWhoseDependenciesAreNotThere(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", NoDeps: true})
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if len(res.Services) != 2 {
		t.Fatalf("up with no dependency containers started %d services", len(res.Services))
	}
	if !strings.Contains(h.out.String(), "db has no container") {
		t.Errorf("nothing was said about the missing dependency:\n%s", h.out.String())
	}
}

func TestUpNoWaitSkipsTheHealthCheck(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.m.HTTPStatus = func(context.Context, string) (int, error) {
		t.Error("--no-wait still waited")
		return 0, errors.New("no")
	}
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x", NoWait: true})
	if res.Services[0].Status != ServiceStarting {
		t.Errorf("web = %q with --no-wait, want starting", res.Services[0].Status)
	}
}

func TestUpRefusesAnAppThatSaysNothingAboutHowToRun(t *testing.T) {
	h := upHarness(t)
	h.cfg = sampleConfig()
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	_, err := h.up(UpRequest{App: "shop", Name: "feat-x"})
	if err == nil {
		t.Fatal("up of an app with nothing to run succeeded")
	}
	for _, want := range []string{"run:", "Dockerfile", config.FileName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q does not mention %q", err, want)
		}
	}
}

func TestUpFillsTheGapsFromStackDetection(t *testing.T) {
	h := upHarness(t)
	h.cfg = sampleConfig()
	h.cfg.Services = []config.Service{{Name: "web", Run: "python app.py"}}
	h.m.Detect = func(string) (*stack.Guess, error) {
		return &stack.Guess{
			Stack:   stack.Python,
			Image:   stack.Field{Value: "python:3.12-alpine", Evidence: "requirements.txt"},
			Install: stack.Field{Value: "pip install -r requirements.txt"},
			Run:     stack.Field{Value: "python manage.py runserver"},
			Test:    stack.Field{Value: "pytest"},
			Cache:   "/root/.cache/pip",
		}, nil
	}
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	if res.Stack != stack.Python {
		t.Errorf("stack = %q, want python", res.Stack)
	}
	spec, _ := h.driver.spec(ReplicaContainerName("shop", "feat-x", "web", 1))
	if spec.Image != "python:3.12-alpine" {
		t.Errorf("image = %q, want the detected one", spec.Image)
	}
	if !strings.Contains(spec.Command[2], "python app.py") {
		t.Errorf("command = %q, want the file's run command to win over the detected one", spec.Command)
	}
	if !strings.Contains(spec.Command[2], "pip install") {
		t.Errorf("command = %q, want the detected install step", spec.Command)
	}
	if len(spec.Volumes) != 1 || spec.Volumes[0].Path != "/root/.cache/pip" {
		t.Errorf("volumes = %v, want the cache volume at the toolchain's cache path", spec.Volumes)
	}
	if spec.Volumes[0].Volume != CacheVolumeName("shop", "feat-x") {
		t.Errorf("cache volume = %q", spec.Volumes[0].Volume)
	}
	if !contains(h.driver.volumeNames(), CacheVolumeName("shop", "feat-x")) {
		t.Errorf("the cache volume was not created: %v", h.driver.volumeNames())
	}
}

func TestUpWithoutAnyServiceUsesTheDetectedOne(t *testing.T) {
	h := upHarness(t)
	h.cfg = sampleConfig()
	h.m.Detect = func(string) (*stack.Guess, error) {
		return &stack.Guess{Stack: stack.Go, Image: stack.Field{Value: "golang:1.23"}, Run: stack.Field{Value: "go run ."}}, nil
	}
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if len(res.Services) != 1 || res.Services[0].Name != config.DefaultServiceName {
		t.Fatalf("up produced %+v, want one service called %q", res.Services, config.DefaultServiceName)
	}
}

func TestUpBuildsTheAppsOwnImageAndDoesNotRebuildAnUnchangedTree(t *testing.T) {
	h := upHarness(t)
	h.cfg = sampleConfig()
	h.cfg.Services = []config.Service{{Name: "web", Build: &config.Build{Context: "."}}}
	h.m.Runner = fixedTree("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	want := ImageRef("shop", "aaaaaaaaaaaa")
	if res.Image != want {
		t.Fatalf("image = %q, want %q", res.Image, want)
	}
	if len(h.driver.builds) != 1 || h.driver.builds[0].Tag != want {
		t.Fatalf("builds = %+v, want one build tagged with the tree hash", h.driver.builds)
	}
	if h.driver.builds[0].Context != WorktreePath(h.data, "shop", "feat-x") {
		t.Errorf("build context = %q, want the worktree", h.driver.builds[0].Context)
	}
	spec, _ := h.driver.spec(ReplicaContainerName("shop", "feat-x", "web", 1))
	if len(spec.Binds) != 0 {
		t.Errorf("a built image mounts %v, want nothing: the image carries the code", spec.Binds)
	}
	if len(spec.Command) != 0 {
		t.Errorf("command = %v, want the image's own CMD", spec.Command)
	}

	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if len(h.driver.builds) != 1 {
		t.Errorf("the second up built again: %+v", h.driver.builds)
	}

	h.m.Runner = fixedTree("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	res = h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if len(h.driver.builds) != 2 || res.Image != ImageRef("shop", "bbbbbbbbbbbb") {
		t.Errorf("a changed tree produced %d builds and image %q", len(h.driver.builds), res.Image)
	}
}

func TestUpBuildForcesARebuildOfTheSameTree(t *testing.T) {
	h := upHarness(t)
	h.cfg = sampleConfig()
	h.cfg.Services = []config.Service{{Name: "web", Build: &config.Build{Context: "."}}}
	h.m.Runner = fixedTree("cccccccccccccccccccccccccccccccccccccccc")
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x", Build: true})
	if len(h.driver.builds) != 2 {
		t.Fatalf("--build produced %d builds, want a second one", len(h.driver.builds))
	}
	if !h.driver.builds[1].NoCache {
		t.Error("--build did not ask docker to skip its cache")
	}
}

func TestUpRefusesABuildContextOutsideTheWorktree(t *testing.T) {
	h := upHarness(t)
	h.cfg = sampleConfig()
	h.cfg.Services = []config.Service{{Name: "web", Build: &config.Build{Context: "../../../etc"}}}
	h.m.Runner = fixedTree("dddddddddddddddddddddddddddddddddddddddd")
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	if _, err := h.up(UpRequest{App: "shop", Name: "feat-x"}); err == nil || !strings.Contains(err.Error(), "outside the worktree") {
		t.Fatalf("up with a context outside the worktree = %v", err)
	}
}

func TestUpRefusesABuildContextThroughASymlink(t *testing.T) {
	h := upHarness(t)
	h.cfg = sampleConfig()
	h.cfg.Services = []config.Service{{Name: "web", Build: &config.Build{Context: "escape"}}}
	h.m.Runner = fixedTree("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	outside := t.TempDir()
	link := filepath.Join(WorktreePath(h.data, "shop", "feat-x"), "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := h.up(UpRequest{App: "shop", Name: "feat-x"}); err == nil || !strings.Contains(err.Error(), "outside the worktree") {
		t.Fatalf("up with a context that is a symlink out of the worktree = %v", err)
	}
	if len(h.driver.builds) != 0 {
		t.Errorf("docker was asked to build %v", h.driver.builds)
	}
}

func TestUpRefusesADockerfileThroughASymlink(t *testing.T) {
	h := upHarness(t)
	h.cfg = sampleConfig()
	h.cfg.Services = []config.Service{{Name: "web", Build: &config.Build{Context: ".", Dockerfile: "escape/Dockerfile"}}}
	h.m.Runner = fixedTree("ffffffffffffffffffffffffffffffffffffffff")
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatalf("write the Dockerfile: %v", err)
	}
	link := filepath.Join(WorktreePath(h.data, "shop", "feat-x"), "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := h.up(UpRequest{App: "shop", Name: "feat-x"}); err == nil || !strings.Contains(err.Error(), "outside the build context") {
		t.Fatalf("up with a dockerfile that is a symlink out of the context = %v", err)
	}
}

func TestTreeHashSerialisesPerEnv(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	rec, err := h.store.Env(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("Env: %v", err)
	}

	var mu sync.Mutex
	inFlight, overlaps := 0, 0
	h.m.Runner = runnerFunc(func(ctx context.Context, c runner.Cmd) (runner.Result, error) {
		mu.Lock()
		inFlight++
		if inFlight > 1 {
			overlaps++
		}
		mu.Unlock()
		time.Sleep(time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		if len(c.Args) > 0 && c.Args[0] == "write-tree" {
			return runner.Result{Stdout: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"}, nil
		}
		return runner.Result{}, nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.m.treeHash(context.Background(), rec); err != nil {
				t.Errorf("treeHash: %v", err)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if overlaps != 0 {
		t.Errorf("%d git commands ran while another held the env's build index", overlaps)
	}
}

func fixedTree(hash string) runner.Runner {
	return runnerFunc(func(ctx context.Context, c runner.Cmd) (runner.Result, error) {
		if len(c.Args) > 0 && c.Args[0] == "write-tree" {
			return runner.Result{Stdout: hash + "\n"}, nil
		}
		return runner.Result{}, nil
	})
}

type runnerFunc func(context.Context, runner.Cmd) (runner.Result, error)

func (f runnerFunc) Run(ctx context.Context, c runner.Cmd) (runner.Result, error) { return f(ctx, c) }

func TestLayoutKeepsPortZeroForTheAppAndTheDepsWhereM3PutThem(t *testing.T) {
	deps := []config.Dep{{Name: "db", Port: 5432}, {Name: "cache", Port: 6379}}
	services := []svcPlan{{name: "web"}, {name: "worker", portless: true}, {name: "admin", port: 8080}}
	l, err := layout(20000, 16, deps, services)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	if l.deps["db"] != 20001 || l.deps["cache"] != 20002 {
		t.Errorf("dep ports = %v, want M3's layout", l.deps)
	}
	if l.services["web"] != 20000 || l.containerPorts["web"] != 20000 {
		t.Errorf("web = %d/%d, want the block port in both views", l.services["web"], l.containerPorts["web"])
	}
	if _, ok := l.services["worker"]; ok {
		t.Errorf("a portless service was given port %d", l.services["worker"])
	}
	if l.services["admin"] != 20003 || l.containerPorts["admin"] != 8080 {
		t.Errorf("admin = %d/%d, want the next free block port and its own container port",
			l.services["admin"], l.containerPorts["admin"])
	}
}

func TestLayoutRefusesMoreThanTheBlockHolds(t *testing.T) {
	deps := []config.Dep{{Name: "db", Port: 1}, {Name: "cache", Port: 2}}
	services := make([]svcPlan, 0, 16)
	for i := 0; i < 16; i++ {
		services = append(services, svcPlan{name: fmt.Sprintf("s%d", i)})
	}
	if _, err := layout(20000, 16, deps, services); err == nil {
		t.Fatal("a block of 16 held 2 deps and 16 services")
	}
}

func TestCheckServiceRefusesAContainerThatIsNotRunning(t *testing.T) {
	h := newHarness(t)
	h.m.DialTCP = func(context.Context, string, time.Duration) error { return nil }
	h.driver.containers[ReplicaContainerName("shop", "feat-x", "web", 1)] = runtime.ContainerState{
		Name: ReplicaContainerName("shop", "feat-x", "web", 1), ID: "id-web", Status: runtime.StatusExited,
	}

	svc := &Service{Name: "web", Container: ReplicaContainerName("shop", "feat-x", "web", 1), Port: 20000}
	err := h.m.checkService(context.Background(), svc, nil)
	if err == nil || !strings.Contains(err.Error(), "container is") {
		t.Errorf("err = %v, want it to say the container is not running", err)
	}

	svc.Container = "caramelo-shop-feat-x-gone"
	if err := h.m.checkService(context.Background(), svc, nil); err == nil ||
		!strings.Contains(err.Error(), "is gone") {
		t.Errorf("err = %v, want it to name the missing container", err)
	}
}

func TestDialProbesForALiveServer(t *testing.T) {
	h := newHarness(t)
	h.m.ReadyInterval = time.Second

	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer held.Close()
	go func() {
		for {
			conn, err := held.Accept()
			if err != nil {
				return
			}

			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	if err := h.m.dial(context.Background(), held.Addr().String()); err != nil {
		t.Errorf("a server holding the connection open is healthy, got %v", err)
	}

	dropped, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer dropped.Close()
	go func() {
		for {
			conn, err := dropped.Accept()
			if err != nil {
				return
			}

			_ = conn.Close()
		}
	}()
	err = h.m.dial(context.Background(), dropped.Addr().String())
	if err == nil || !strings.Contains(err.Error(), "nothing is listening") {
		t.Errorf("err = %v, want the probe to reject a connection dropped at once", err)
	}
}

func TestUpRefusesAServiceThatRestartsUnderTheProbe(t *testing.T) {
	h := upHarness(t)
	h.cfg = serviceConfig()
	h.cfg.Services = h.cfg.Services[:1]
	h.cfg.Services[0].Health = nil
	h.m.DialTCP = func(context.Context, string, time.Duration) error { return nil }
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	container := ReplicaContainerName("shop", "feat-x", "web", 1)
	var seen int
	h.driver.OnInspect = func(name string, c *runtime.ContainerState) {
		if name != container {
			return
		}
		c.Status = runtime.StatusRunning
		seen++
		c.Restarts = seen / 2
	}

	_, err := h.up(UpRequest{App: "shop", Name: "feat-x"})
	if err == nil {
		t.Fatalf("up called a crash-looping service healthy\n%s", h.out.String())
	}
	if !strings.Contains(err.Error(), "restarted while it was being checked") {
		t.Errorf("err = %v, want it to name the restarts", err)
	}
}

func TestUpAcceptsAServiceThatIsNotRestarting(t *testing.T) {
	h := upHarness(t)
	h.cfg = serviceConfig()
	h.cfg.Services = h.cfg.Services[:1]
	h.cfg.Services[0].Health = nil
	h.m.DialTCP = func(context.Context, string, time.Duration) error { return nil }
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	container := ReplicaContainerName("shop", "feat-x", "web", 1)
	h.driver.OnInspect = func(name string, c *runtime.ContainerState) {
		if name == container {
			c.Status, c.Restarts = runtime.StatusRunning, 7
		}
	}

	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if len(res.Services) != 1 || res.Services[0].Status != ServiceRunning {
		t.Errorf("up = %+v, want the service running\n%s", res.Services, h.out.String())
	}
}
