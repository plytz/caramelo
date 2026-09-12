package env

import (
	"context"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/ports"
	"github.com/plytz/caramelo/internal/runtime"
)

func replicaConfig(n int) *config.App {
	cfg := serviceConfig()
	cfg.Services[0].ReplicaCount = n
	return cfg
}

func TestUpStartsEveryReplicaOnItsOwnPort(t *testing.T) {
	h := upHarness(t)
	h.cfg = replicaConfig(2)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	web := res.Services[0]
	if len(web.Replicas) != 2 {
		t.Fatalf("web has %d replicas, want 2\n%s", len(web.Replicas), h.out.String())
	}

	wantPorts := []int{ports.Base, ports.Base + 4}
	for i, rep := range web.Replicas {
		if rep.Index != i+1 {
			t.Errorf("replica %d has index %d", i, rep.Index)
		}
		if rep.Port != wantPorts[i] {
			t.Errorf("replica %d publishes on %d, want %d", rep.Index, rep.Port, wantPorts[i])
		}
		if rep.Container != ReplicaContainerName("shop", "feat-x", "web", rep.Index) {
			t.Errorf("replica %d container = %q", rep.Index, rep.Container)
		}
		if rep.Status != ServiceRunning || rep.Health != HealthOK {
			t.Errorf("replica %d = %s/%s, want a running, healthy replica", rep.Index, rep.Status, rep.Health)
		}
		if !h.driver.hasContainer(rep.Container) {
			t.Errorf("no container was started for %s", rep.Container)
		}
	}

	if web.Port != ports.Base || web.Container != ReplicaContainerName("shop", "feat-x", "web", 1) {
		t.Errorf("web = %d/%s, want the first replica's port and container", web.Port, web.Container)
	}

	one, _ := h.driver.spec(ReplicaContainerName("shop", "feat-x", "web", 1))
	two, _ := h.driver.spec(ReplicaContainerName("shop", "feat-x", "web", 2))
	if one.Env["PORT"] != two.Env["PORT"] {
		t.Errorf("replicas were given different $PORT (%q, %q): a replica is another copy of the same process",
			one.Env["PORT"], two.Env["PORT"])
	}
	if len(two.Aliases) != 1 || two.Aliases[0] != "web" {
		t.Errorf("replica 2 aliases = %v, want the service's own name so the env's network balances between them", two.Aliases)
	}
	if row, ok := h.store.service(1, "web"); !ok || row.Replicas != 2 {
		t.Errorf("the service row records %d replicas, want 2", row.Replicas)
	}
}

func TestUpIsIdempotentAcrossReplicas(t *testing.T) {
	h := upHarness(t)
	h.cfg = replicaConfig(2)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	before := len(h.driver.specs)

	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if res.Services[0].Change != ChangeUnchanged {
		t.Errorf("web = %q on the second up, want unchanged", res.Services[0].Change)
	}
	if len(h.driver.specs) != before {
		t.Errorf("%d containers were started again", len(h.driver.specs)-before)
	}

	h.cfg = replicaConfig(1)
	res = h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if len(res.Services[0].Replicas) != 1 {
		t.Fatalf("web has %d replicas, want 1", len(res.Services[0].Replicas))
	}
	if h.driver.hasContainer(ReplicaContainerName("shop", "feat-x", "web", 2)) {
		t.Error("the second replica's container is still there")
	}
}

func TestUpReplacesTheContainerM4Named(t *testing.T) {
	h := upHarness(t)
	h.cfg = replicaConfig(1)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	legacy := ServiceContainerName("shop", "feat-x", "web")
	if _, err := h.driver.Run(context.Background(), containerLike(legacy)); err != nil {
		t.Fatalf("run: %v", err)
	}

	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if h.driver.hasContainer(legacy) {
		t.Error("the unnumbered container is still there")
	}
	if !h.driver.hasContainer(ReplicaContainerName("shop", "feat-x", "web", 1)) {
		t.Error("the numbered replica was not started")
	}
}

func TestServicesReportEveryReplica(t *testing.T) {
	h := upHarness(t)
	h.cfg = replicaConfig(2)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if err := h.driver.Remove(context.Background(), ReplicaContainerName("shop", "feat-x", "web", 2), true); err != nil {
		t.Fatalf("remove: %v", err)
	}

	services, err := h.m.Services(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	web := services[0]
	if len(web.Replicas) != 2 {
		t.Fatalf("web has %d replicas, want both reported", len(web.Replicas))
	}
	if web.Replicas[0].Status != ServiceRunning || web.Replicas[1].Status != ServiceMissing {
		t.Errorf("replicas = %s/%s, want running and missing", web.Replicas[0].Status, web.Replicas[1].Status)
	}
	if web.Status != ServiceMissing {
		t.Errorf("web = %q, want the worst of its replicas", web.Status)
	}
}

func TestLogsPrefixesEachReplica(t *testing.T) {
	h := upHarness(t)
	h.cfg = replicaConfig(2)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	h.driver.logs[ReplicaContainerName("shop", "feat-x", "web", 1)] = "one\n"
	h.driver.logs[ReplicaContainerName("shop", "feat-x", "web", 2)] = "two\n"
	h.driver.logs[ReplicaContainerName("shop", "feat-x", "echo", 1)] = "echo: ping\n"

	var out strings.Builder
	if err := h.m.Logs(context.Background(), LogsRequest{
		App: "shop", Name: "feat-x", Stdout: &out, Stderr: &h.out,
	}); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	for _, want := range []string{"web/1 | one", "web/2 | two", "echo  | echo: ping"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("logs do not contain %q:\n%s", want, out.String())
		}
	}
}

func TestDownRemovesEveryReplica(t *testing.T) {
	h := upHarness(t)
	h.cfg = replicaConfig(2)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	stray := ReplicaContainerName("shop", "feat-x", "web", 4)
	if _, err := h.driver.Run(context.Background(), containerLike(stray)); err != nil {
		t.Fatalf("run: %v", err)
	}

	if _, err := h.m.Down(context.Background(), DownRequest{App: "shop", Name: "feat-x"}, &h.out); err != nil {
		t.Fatalf("Down: %v", err)
	}
	for _, name := range []string{
		ReplicaContainerName("shop", "feat-x", "web", 1),
		ReplicaContainerName("shop", "feat-x", "web", 2),
		stray,
	} {
		if h.driver.hasContainer(name) {
			t.Errorf("%s survived down", name)
		}
	}
}

func TestReplicaPortsAreDeterministicAndInterleaved(t *testing.T) {
	deps := []config.Dep{{Name: "db", Port: 5432}, {Name: "cache", Port: 6379}}
	services := []svcPlan{{name: "web"}, {name: "worker", portless: true}, {name: "admin", port: 8080}}
	l, err := layout(ports.Base, ports.BlockSize, deps, services)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	for _, tc := range []struct {
		service string
		replica int
		want    int
	}{
		{"web", 1, ports.Base},
		{"admin", 1, ports.Base + 3},
		{"web", 2, ports.Base + 4},
		{"admin", 2, ports.Base + 5},
		{"web", 3, ports.Base + 6},
		{"admin", 3, ports.Base + 7},
	} {
		got, err := l.replicaPort(tc.service, tc.replica)
		if err != nil {
			t.Fatalf("replicaPort(%s, %d): %v", tc.service, tc.replica, err)
		}
		if got != tc.want {
			t.Errorf("replicaPort(%s, %d) = %d, want %d", tc.service, tc.replica, got, tc.want)
		}
	}
	if _, err := l.replicaPort("worker", 2); err == nil {
		t.Error("a portless service was given a replica port")
	}

	again, _ := layout(ports.Base, ports.BlockSize, deps, services)
	if a, b := mustPort(t, l, "web", 3), mustPort(t, again, "web", 3); a != b {
		t.Errorf("replicaPort is not deterministic: %d then %d", a, b)
	}
}

func TestReplicaPortsRefuseWhatTheBlockCannotHold(t *testing.T) {
	deps := make([]config.Dep, 0, 26)
	for i := 0; i < 26; i++ {
		deps = append(deps, config.Dep{Name: "d" + itoa(i), Port: 1000 + i})
	}
	l, err := layout(ports.Base, ports.BlockSize, deps, []svcPlan{{name: "web"}})
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	err = l.fits("web", 8)
	if err == nil {
		t.Fatal("a 32-port block held 26 deps and 8 replicas")
	}
	if !strings.Contains(err.Error(), "block") {
		t.Errorf("err = %v, want it to name the block", err)
	}
}

func TestReplicaPortsInALegacyBlock(t *testing.T) {
	l, err := layout(ports.Base, ports.LegacyBlockSize, []config.Dep{{Name: "db", Port: 5432}}, []svcPlan{{name: "web"}})
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	if got := mustPort(t, l, "web", 2); got != ports.Base+2 {
		t.Errorf("replica 2 = %d, want %d", got, ports.Base+2)
	}
	if err := l.fits("web", 16); err == nil {
		t.Error("a 16-port block held sixteen replicas")
	}
}

func TestFreeIndexesAlternate(t *testing.T) {
	live := []replica{{index: 1}, {index: 2}}
	if got := freeIndexes(live, 2); got[0] != 3 || got[1] != 4 {
		t.Errorf("freeIndexes = %v, want 3 and 4", got)
	}
	if got := freeIndexes([]replica{{index: 3}, {index: 4}}, 2); got[0] != 1 || got[1] != 2 {
		t.Errorf("freeIndexes = %v, want the numbers back", got)
	}
	if got := freeIndexes(nil, 3); len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Errorf("freeIndexes on an empty service = %v, want 1..3", got)
	}
}

func TestReplicaIndexOf(t *testing.T) {
	for _, tc := range []struct {
		container string
		want      int
		ok        bool
	}{
		{ReplicaContainerName("shop", "feat-x", "web", 2), 2, true},
		{ServiceContainerName("shop", "feat-x", "web"), legacyReplica, true},
		{ContainerName("shop", "feat-x", "db"), 0, false},
		{"caramelo-shop-feat-x-web-", 0, false},
		{"caramelo-shop-feat-x-web-0", 0, false},
		{"caramelo-shop-feat-x-webbing-1", 0, false},
	} {
		got, ok := replicaIndexOf("shop", "feat-x", "web", tc.container)
		if got != tc.want || ok != tc.ok {
			t.Errorf("replicaIndexOf(%q) = %d, %v; want %d, %v", tc.container, got, ok, tc.want, tc.ok)
		}
	}
}

func containerLike(name string) runtime.ContainerSpec {
	return runtime.ContainerSpec{Name: name, Image: "python:3.12-alpine",
		Labels: ServiceLabels("shop", "feat-x", "web", "0.0.1-test")}
}

func mustPort(t *testing.T, l portLayout, service string, replica int) int {
	t.Helper()
	port, err := l.replicaPort(service, replica)
	if err != nil {
		t.Fatalf("replicaPort(%s, %d): %v", service, replica, err)
	}
	return port
}
