package env

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/state"
)

func healthHarness(t *testing.T, replicas int) (*harness, *fakeEdge, *HealthLoop) {
	t.Helper()
	h, e := edgeHarness(t, replicas)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	l := NewHealthLoop(h.m)
	l.Interval, l.Failures = time.Millisecond, 3
	l.Log = &h.out
	return h, e, l
}

func TestHealthLoopLeavesAHealthyReplicaAlone(t *testing.T) {
	h, e, l := healthHarness(t, 2)
	before := e.pushes()
	for i := 0; i < 5; i++ {
		if err := l.Once(context.Background()); err != nil {
			t.Fatalf("Once: %v\n%s", err, h.out.String())
		}
	}
	if e.pushes() != before {
		t.Errorf("the loop pushed %d tables over a healthy environment", e.pushes()-before)
	}
	if len(h.driver.restarted) != 0 {
		t.Errorf("the loop restarted %+v", h.driver.restarted)
	}
	for _, s := range e.states(webHost) {
		if s != edge.TargetActive {
			t.Errorf("a healthy replica is %s", s)
		}
	}
}

func TestHealthLoopTakesAnUnhealthyReplicaOutOfThePoolAndRestartsIt(t *testing.T) {
	h, e, l := healthHarness(t, 2)
	ctx := context.Background()
	bad := ReplicaContainerName("shop", "feat-x", "web", 2)
	h.m.HTTPStatus = func(_ context.Context, url string) (int, error) {
		if strings.Contains(url, portOf(h, 2)) {
			return 0, errors.New("connection refused")
		}
		return 200, nil
	}

	for i := 1; i <= 2; i++ {
		if err := l.Once(ctx); err != nil {
			t.Fatalf("Once: %v", err)
		}
		if got := e.states(webHost)[2]; got != edge.TargetActive {
			t.Fatalf("after %d failures replica 2 is %s, want it still in the pool", i, got)
		}
		if len(h.driver.restarted) != 0 {
			t.Fatalf("after %d failures it was already restarted", i)
		}
	}
	if err := l.Once(ctx); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := e.states(webHost)[2]; got != edge.TargetUnhealthy {
		t.Errorf("replica 2 is %s, want unhealthy\n%s", got, h.out.String())
	}
	if got := e.states(webHost)[1]; got != edge.TargetActive {
		t.Errorf("replica 1 is %s: the healthy one must keep serving", got)
	}
	if len(h.driver.restarted) != 1 || h.driver.restarted[0].name != bad {
		t.Errorf("restarted = %+v, want %s once", h.driver.restarted, bad)
	}

	if !h.hasEvent(HealthActionHealth, "failed", "3 probes") {
		t.Errorf("no health event naming the failures:\n%s", h.eventsText())
	}
	if !h.hasEvent(HealthActionRestart, "changed", bad) {
		t.Errorf("no restart event:\n%s", h.eventsText())
	}

	h.m.HTTPStatus = func(context.Context, string) (int, error) { return 200, nil }
	if err := l.Once(ctx); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := e.states(webHost)[2]; got != edge.TargetActive {
		t.Errorf("replica 2 is %s after answering again", got)
	}
	if !h.hasEvent(HealthActionHealth, "ok", "answered again") {
		t.Errorf("no event for the replica coming back:\n%s", h.eventsText())
	}
}

func TestHealthLoopReportsACrashLoop(t *testing.T) {
	h, _, l := healthHarness(t, 1)
	ctx := context.Background()
	name := ReplicaContainerName("shop", "feat-x", "web", 1)

	if err := l.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if h.hasEvent(HealthActionCrashloop, "warning", name) {
		t.Errorf("the first pass reported a crash loop:\n%s", h.eventsText())
	}
	h.driver.bumpRestarts(name, 4)
	if err := l.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.hasEvent(HealthActionCrashloop, "warning", "restarted 4 times") {
		t.Errorf("no crashloop event:\n%s", h.eventsText())
	}

	h.clearEvents()
	if err := l.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if h.hasEvent(HealthActionCrashloop, "warning", name) {
		t.Errorf("a settled container is still reported:\n%s", h.eventsText())
	}
}

func TestHealthLoopRestartsADependencyThatStopsAnswering(t *testing.T) {
	h, _, l := healthHarness(t, 1)
	ctx := context.Background()
	db := ContainerName("shop", "feat-x", "db")
	h.driver.ExecResult = func(name string, argv []string) (runner.Result, error) {
		if name == db {
			return runner.Result{ExitCode: 1, Stderr: "could not connect"}, nil
		}
		return runner.Result{}, nil
	}

	for i := 0; i < 3; i++ {
		if err := l.Once(ctx); err != nil {
			t.Fatalf("Once: %v", err)
		}
	}
	found := 0
	for _, r := range h.driver.restarted {
		if r.name == db {
			found++
		}
	}
	if found != 1 {
		t.Errorf("the dependency was restarted %d times, want once\n%s", found, h.eventsText())
	}
	if !h.hasEvent(HealthActionHealth, "failed", "could not connect") {
		t.Errorf("no event naming what the check said:\n%s", h.eventsText())
	}
}

func TestHealthLoopLeavesAStoppedServiceAlone(t *testing.T) {
	h, _, l := healthHarness(t, 1)
	ctx := context.Background()
	if _, err := h.m.Down(ctx, DownRequest{App: "shop", Name: "feat-x"}, &h.out); err != nil {
		t.Fatalf("Down: %v", err)
	}
	h.driver.restarted = nil
	for i := 0; i < 4; i++ {
		if err := l.Once(ctx); err != nil {
			t.Fatalf("Once: %v", err)
		}
	}
	for _, r := range h.driver.restarted {
		if strings.Contains(r.name, "-web-") {
			t.Errorf("the loop restarted %s after `down`", r.name)
		}
	}
}

func TestHealthLoopDoesNotTouchAHeldReplica(t *testing.T) {
	h, _, l := healthHarness(t, 1)
	ctx := context.Background()
	row, err := h.store.RouteByHost(ctx, webHost)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.PutTarget(ctx, state.EdgeTarget{
		RouteID: row.ID, Replica: 1, Port: portOfInt(h, 1),
		State: string(edge.TargetHeld), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	h.m.HTTPStatus = func(context.Context, string) (int, error) { return 0, errors.New("connection refused") }
	for i := 0; i < 5; i++ {
		if err := l.Once(ctx); err != nil {
			t.Fatalf("Once: %v", err)
		}
	}
	if len(h.driver.restarted) != 0 {
		t.Errorf("the loop restarted a held replica: %+v", h.driver.restarted)
	}
	targets, err := h.store.Targets(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		if target.Replica == 1 && edge.TargetState(target.State) != edge.TargetHeld {
			t.Errorf("the held replica is %s, want it left held", target.State)
		}
	}
}

func TestHealthLoopRunStopsOnCancel(t *testing.T) {
	_, _, l := healthHarness(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil on a clean cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestHealthLoopSurvivesABrokenRuntime(t *testing.T) {
	h, _, l := healthHarness(t, 1)
	h.m.Driver = &unreachableRuntime{fakeDriver: h.driver, err: errors.New("cannot connect to the Docker daemon")}
	if err := l.Once(context.Background()); err != nil {
		t.Errorf("Once = %v, want the pass to go on", err)
	}
	if !strings.Contains(h.out.String(), "cannot connect") {
		t.Errorf("the loop said nothing about the broken runtime:\n%s", h.out.String())
	}
}

func portOf(h *harness, replica int) string {
	return ":" + itoa(portOfInt(h, replica))
}

func portOfInt(h *harness, replica int) int {
	rec, err := h.store.Env(context.Background(), "shop", "feat-x")
	if err != nil {
		h.t.Fatal(err)
	}
	cfg, err := decodeConfig(rec)
	if err != nil {
		h.t.Fatal(err)
	}
	l, err := h.m.layoutOf(rec, cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	port, err := l.replicaPort("web", replica)
	if err != nil {
		h.t.Fatal(err)
	}
	return port
}

func TestHealthLoopDoesNotTouchAnEnvironmentACommandIsHolding(t *testing.T) {
	h, e, l := healthHarness(t, 1)
	ctx := context.Background()
	h.m.HTTPStatus = func(context.Context, string) (int, error) {
		return 0, errors.New("connection refused: it is still installing")
	}
	pushes := e.pushes()

	release := h.m.lockEnv("shop", "feat-x")
	for range 5 {
		if err := l.Once(ctx); err != nil {
			t.Fatalf("Once: %v\n%s", err, h.out.String())
		}
	}
	if len(h.driver.restarted) != 0 {
		t.Fatalf("the loop restarted %+v while a command held the environment", h.driver.restarted)
	}
	if e.pushes() != pushes {
		t.Errorf("the loop pushed %d tables while a command held the environment", e.pushes()-pushes)
	}

	release()
	for range 3 {
		if err := l.Once(ctx); err != nil {
			t.Fatalf("Once: %v\n%s", err, h.out.String())
		}
	}
	if len(h.driver.restarted) == 0 {
		t.Errorf("the loop never supervised the environment again\n%s", h.out.String())
	}
}

func TestUpHoldsTheEnvironmentWhileItStartsReplicas(t *testing.T) {
	h, _, _ := healthHarness(t, 1)
	held := false
	h.m.HTTPStatus = func(context.Context, string) (int, error) {
		held = held || h.m.envBusy("shop", "feat-x")
		return 200, nil
	}

	h.setTree("bbbbbbbbbbbb")
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if !held {
		t.Error("`up` was checking a replica with the environment free for the health loop to restart")
	}
	if h.m.envBusy("shop", "feat-x") {
		t.Error("`up` did not let the environment go")
	}
}
