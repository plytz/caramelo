package env

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/ports"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vault"
)

const prodHost = "shop.test"

const shortWatch = 5 * time.Millisecond

func deployHarness(t *testing.T, replicas int) (*harness, *fakeEdge, *walkBuilder) {
	t.Helper()
	h, e := edgeHarness(t, replicas)
	h.cfg.Envs = map[string]config.EnvOverride{
		"production": {Hosts: []string{prodHost}},
	}
	b := newBuilder(h.store, "aaaaaaaaaaaa")
	b.cfg = h.cfg

	b.driver = h.driver
	h.m.Builder = b
	h.m.WatchInterval = time.Millisecond
	return h, e, b
}

type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock {
	return &clock{at: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func (h *harness) mustDeploy(req DeployRequest) *Deploy {
	h.t.Helper()
	d, err := h.m.Deploy(context.Background(), req, &h.out)
	if err != nil {
		h.t.Fatalf("Deploy(%s): %v\n%s", req.Env, err, h.out.String())
	}
	return d
}

func (h *harness) mustPromote() *Deploy {
	h.t.Helper()
	d, err := h.m.Promote(context.Background(), PromoteRequest{App: "shop", Env: "production"}, &h.out)
	if err != nil {
		h.t.Fatalf("Promote: %v\n%s", err, h.out.String())
	}
	return d
}

func (h *harness) push(b *walkBuilder, tree string) {
	b.setTree(tree)
	h.setTree(tree)
}

func deploySteps(d *Deploy) []DeployStepName {
	out := make([]DeployStepName, 0, len(d.Steps))
	for _, s := range d.Steps {
		if s.Status == StepStarted {
			continue
		}
		out = append(out, s.Step)
	}
	return out
}

func hasDeployStepName(d *Deploy, want DeployStepName) bool {
	for _, s := range d.Steps {
		if s.Step == want && s.Status != StepStarted && s.Status != StepSkipped {
			return true
		}
	}
	return false
}

func hasDeployStep(d *Deploy, want DeployStepName, status StepStatus) bool {
	for _, s := range d.Steps {
		if s.Step == want && s.Status == status {
			return true
		}
	}
	return false
}

func stepsText(d *Deploy) string {
	b := &strings.Builder{}
	for _, s := range d.Steps {
		b.WriteString("  " + string(s.Step) + " " + string(s.Status) + ": " + s.Detail + "\n")
	}
	return b.String()
}

func (h *harness) waitStatus(id int64, want ...string) string {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		row, ok := h.store.deploy(id)
		if ok {
			last = row.Status
			for _, w := range want {
				if row.Status == w {
					return row.Status
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("deploy %d is %s, want one of %v\n%s", id, last, want, h.out.String())
	return last
}

func TestDeployWalksEveryGateAndPromotes(t *testing.T) {
	h, e, b := deployHarness(t, 2)
	h.cfg.Deploy = &config.Deploy{Before: "migrate", Check: "smoke", Watch: shortWatch}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})

	d := h.mustDeploy(DeployRequest{App: "shop", Env: "production"})

	if d.Status != DeployPromoted {
		t.Fatalf("status = %s, want promoted\n%s%s", d.Status, h.out.String(), stepsText(d))
	}
	if d.Kind != DeployKindDeploy || d.Release == nil || d.Release.Tree != "aaaaaaaaaaaa" {
		t.Errorf("deploy = %+v", d)
	}
	for _, want := range []DeployStepName{StepBuild, StepBefore, StepReplicas, StepCheck, StepSwitch, StepWatch, StepPromote} {
		if !hasDeployStepName(d, want) {
			t.Errorf("the walk has no %s step: %v", want, deploySteps(d))
		}
	}

	oneOffs := h.driver.oneOffs()
	if len(oneOffs) != 2 || !strings.Contains(oneOffs[0].command, "migrate") ||
		!strings.Contains(oneOffs[1].command, "smoke") {
		t.Fatalf("one-offs = %+v, want migrate then smoke", oneOffs)
	}
	if oneOffs[0].image != "caramelo/shop/web:aaaaaaaaaaaa" {
		t.Errorf("`before` ran in %q, want the release's image", oneOffs[0].image)
	}

	if len(h.driver.attached[0].Binds) != 0 {
		t.Errorf("`before` had the worktree mounted at %+v", h.driver.attached[0].Binds)
	}

	if host := oneOffs[1].env["CARAMELO_CHECK_HOST"]; host != ReplicaContainerName("shop", "production", "web", 1) {
		t.Errorf("CARAMELO_CHECK_HOST = %q, want a new replica's container", host)
	}
	if url := oneOffs[1].env["CARAMELO_CHECK_URL"]; !strings.HasPrefix(url, "http://caramelo-shop-production-web-1:") {
		t.Errorf("CARAMELO_CHECK_URL = %q", url)
	}

	for i := 1; i <= 2; i++ {
		spec, ok := h.driver.spec(ReplicaContainerName("shop", "production", "web", i))
		if !ok {
			t.Fatalf("replica %d has no container", i)
		}
		if spec.Image != "caramelo/shop/web:aaaaaaaaaaaa" {
			t.Errorf("replica %d runs %q", i, spec.Image)
		}
		if len(spec.Binds) != 0 {
			t.Errorf("replica %d has the worktree mounted at %+v", i, spec.Binds)
		}
	}

	rec, err := h.store.Env(context.Background(), "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ReleaseID != d.Release.ID {
		t.Errorf("env release = %d, want %d", rec.ReleaseID, d.Release.ID)
	}
	if rec.DeployID != 0 {
		t.Errorf("env deploy = %d, want it cleared when the deploy ended", rec.DeployID)
	}

	row, ok := h.store.deploy(d.ID)
	if !ok || row.Status != state.DeployPromoted || row.FinishedAt.IsZero() {
		t.Errorf("row = %+v", row)
	}
	if b.count() != 1 {
		t.Errorf("the walk built %d times, want once", b.count())
	}
	if got := b.keeps(); len(got) != 1 || got[0] != config.DefaultKeep {
		t.Errorf("pruned to %v, want the default keep", got)
	}

	r, ok := e.table().Route(prodHost)
	if !ok {
		t.Fatalf("the edge does not serve %s", prodHost)
	}
	if len(r.Targets) != 2 {
		t.Fatalf("targets = %+v, want the two new replicas", r.Targets)
	}
	for _, target := range r.Targets {
		if target.State != edge.TargetActive {
			t.Errorf("target %d is %s", target.Replica, target.State)
		}
	}
}

func TestDeployHoldsTheOldPoolThroughTheWatch(t *testing.T) {
	h, e, b := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: time.Hour, Promote: config.PromoteManual}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	first := h.mustDeploy(DeployRequest{App: "shop", Env: "production", NoWatch: true})
	h.waitStatus(first.ID, state.DeployWatching)
	h.mustPromote()

	h.push(b, "bbbbbbbbbbbb")
	second := h.mustDeploy(DeployRequest{App: "shop", Env: "production", NoWatch: true})
	h.waitStatus(second.ID, state.DeployWatching)

	states := h.waitPool(e, 1, 1)
	if !states.ok {
		t.Fatalf("the table holds %d and serves %d, want one of each\n%s", states.held, states.active, h.out.String())
	}

	if !h.driver.hasContainer(ReplicaContainerName("shop", "production", "web", 1)) {
		t.Error("the held replica's container is gone")
	}

	d := h.mustPromote()
	if d.Status != DeployPromoted {
		t.Fatalf("status = %s\n%s", d.Status, h.out.String())
	}
	if h.driver.hasContainer(ReplicaContainerName("shop", "production", "web", 1)) {
		t.Error("the held replica is still there after the promotion")
	}
	if len(e.states(prodHost)) != 1 {
		t.Errorf("the table still has %d targets, want only the new pool", len(e.states(prodHost)))
	}
}

func TestDeployCarriesThePerReplicaRollout(t *testing.T) {
	h, _, b := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: shortWatch}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	h.mustDeploy(DeployRequest{App: "shop", Env: "production"})
	h.push(b, "bbbbbbbbbbbb")

	d := h.mustDeploy(DeployRequest{App: "shop", Env: "production"})
	if len(d.Rollouts) != 1 || d.Rollouts[0].Service != "web" {
		t.Fatalf("rollouts = %+v, want one for web\n%s", d.Rollouts, h.out.String())
	}
	roll := d.Rollouts[0]
	if roll.Host != prodHost || roll.URL != "https://"+prodHost {
		t.Errorf("rollout = %+v, want the public name on it", roll)
	}
	var got []RolloutStepName
	for _, s := range roll.Steps {
		got = append(got, s.Step)
	}
	for _, want := range []RolloutStepName{StepStart, StepHealth, StepProbe, StepFlip, StepDrain} {
		if !hasStep(got, want) {
			t.Errorf("the rollout has no %s step: %v", want, got)
		}
	}

	if hasStep(got, StepStop) {
		t.Errorf("a deploy stopped the old replica during the rollout: %v", got)
	}
}

type poolCount struct {
	held, active int
	ok           bool
}

func (h *harness) waitPool(e *fakeEdge, held, active int) poolCount {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got poolCount
	for time.Now().Before(deadline) {
		got = poolCount{}
		for _, s := range e.states(prodHost) {
			switch s {
			case edge.TargetHeld:
				got.held++
			case edge.TargetActive:
				got.active++
			}
		}
		if got.held == held && got.active == active {
			got.ok = true
			return got
		}
		time.Sleep(time.Millisecond)
	}
	return got
}

func TestDeployRollsBackOnTheErrorRate(t *testing.T) {
	h, e, b := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: time.Hour, MaxErrors: "5%"}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	h.mustDeploy(DeployRequest{App: "shop", Env: "production", NoWatch: true})
	h.mustPromote()

	h.push(b, "bbbbbbbbbbbb")
	e.setCounts(&edge.Counts{Hosts: []edge.HostCounts{{
		Host: prodHost, Requests: 140, Status5xx: 40,
		Targets: []edge.TargetCounts{
			{Replica: 1, Requests: 40},
			{Replica: 2, Requests: 100, Status5xx: 40},
		},
	}}})

	d, err := h.m.Deploy(context.Background(), DeployRequest{App: "shop", Env: "production"}, &h.out)
	if err != nil {
		t.Fatalf("Deploy: %v\n%s", err, h.out.String())
	}
	if d.Status != DeployRolledBack {
		t.Fatalf("status = %s, want rolled_back\n%s", d.Status, stepsText(d))
	}
	if !strings.Contains(d.Reason, "40 errors in 100 requests") {
		t.Errorf("reason = %q, want the counts that caused it", d.Reason)
	}
	if !hasDeployStep(d, StepRollback, StepOK) {
		t.Errorf("no rollback step: %v", deploySteps(d))
	}

	got := e.states(prodHost)
	if got[1] != edge.TargetActive {
		t.Errorf("replica 1 is %s, want it serving again (%v)", got[1], got)
	}
	if len(got) != 1 {
		t.Errorf("the table has %v, want only the pool that is serving", got)
	}
	if h.driver.hasContainer(ReplicaContainerName("shop", "production", "web", 2)) {
		t.Error("the new replica is still there after the rollback")
	}

	rec, err := h.store.Env(context.Background(), "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ReleaseID == d.Release.ID {
		t.Error("the environment records the release that was rolled back")
	}

	if !h.hasEventWith("deploy", "\"requests\":100") {
		t.Errorf("the feed has no deploy event carrying the counts:\n%s", h.eventsText())
	}
}

func TestDeployIgnoresAnErrorRateBelowTheFloor(t *testing.T) {
	h, e, _ := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: shortWatch, MaxErrors: "5%"}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	e.setCounts(&edge.Counts{Hosts: []edge.HostCounts{{
		Host: prodHost, Requests: 3, Status5xx: 3,
		Targets: []edge.TargetCounts{{Replica: 1, Requests: 3, Status5xx: 3}},
	}}})

	d := h.mustDeploy(DeployRequest{App: "shop", Env: "production"})
	if d.Status != DeployPromoted {
		t.Fatalf("status = %s, want promoted on health alone\n%s", d.Status, stepsText(d))
	}
	if d.Watch == nil || d.Watch.Requests != 3 || d.Watch.MinRequests != config.MinErrorRequests {
		t.Errorf("watch = %+v", d.Watch)
	}
	if d.Watch.Breached() {
		t.Error("a rate below the floor reads as breached")
	}
}

func TestDeployRollsBackWhenANewReplicaDies(t *testing.T) {
	h, e, b := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: time.Hour}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	h.mustDeploy(DeployRequest{App: "shop", Env: "production", NoWatch: true})
	h.mustPromote()

	h.push(b, "bbbbbbbbbbbb")

	flipped := ReplicaContainerName("shop", "production", "web", 2)
	h.driver.OnInspect = func(name string, c *runtime.ContainerState) {
		if name == flipped && e.states(prodHost)[2] == edge.TargetActive {
			c.Status = runtime.StatusExited
		}
	}

	d, err := h.m.Deploy(context.Background(), DeployRequest{App: "shop", Env: "production"}, &h.out)
	if err != nil {
		t.Fatalf("Deploy: %v\n%s", err, h.out.String())
	}
	if d.Status != DeployRolledBack {
		t.Fatalf("status = %s, want rolled_back\n%s", d.Status, stepsText(d))
	}
	if !strings.Contains(d.Reason, "replica 2") || !strings.Contains(d.Reason, "exited") {
		t.Errorf("reason = %q, want the replica and what it was doing", d.Reason)
	}
}

func TestDeployFailsAtTheCheckWithoutFlipping(t *testing.T) {
	h, e, b := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Check: "smoke", Watch: shortWatch}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	h.mustDeploy(DeployRequest{App: "shop", Env: "production"})
	before := e.states(prodHost)

	h.push(b, "bbbbbbbbbbbb")
	h.driver.Attached = func(spec runtime.ContainerSpec, _ runtime.Streams) (int, error) {
		if strings.Contains(strings.Join(spec.Command, " "), "smoke") {
			return 1, nil
		}
		return 0, nil
	}

	d, err := h.m.Deploy(context.Background(), DeployRequest{App: "shop", Env: "production"}, &h.out)
	if err == nil {
		t.Fatal("a failed check was not an error")
	}
	if !strings.Contains(err.Error(), "check") || !strings.Contains(err.Error(), "exited 1") {
		t.Errorf("error = %v, want the gate and the exit code", err)
	}
	if d.Status != DeployFailed {
		t.Fatalf("status = %s, want failed\n%s", d.Status, stepsText(d))
	}
	if hasDeployStepName(d, StepSwitch) {
		t.Error("a deploy that failed at the check still switched")
	}

	if got := e.states(prodHost); len(got) != len(before) || got[1] != edge.TargetActive {
		t.Errorf("the table moved: %v, was %v", got, before)
	}
	if h.driver.hasContainer(ReplicaContainerName("shop", "production", "web", 2)) {
		t.Error("the replica the failed deploy started is still there")
	}
	rec, _ := h.store.Env(context.Background(), "shop", "production")
	if rec.DeployID != 0 {
		t.Errorf("env deploy = %d, want it cleared", rec.DeployID)
	}
}

func TestDeployFailsAtTheMigration(t *testing.T) {
	h, _, _ := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Before: "migrate", Watch: shortWatch}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	h.driver.Attached = func(runtime.ContainerSpec, runtime.Streams) (int, error) { return 3, nil }

	d, err := h.m.Deploy(context.Background(), DeployRequest{App: "shop", Env: "production"}, &h.out)
	if err == nil || !strings.Contains(err.Error(), "before") {
		t.Fatalf("error = %v, want the `before` gate", err)
	}
	if d.Status != DeployFailed || hasDeployStepName(d, StepReplicas) {
		t.Errorf("status = %s, steps = %v", d.Status, deploySteps(d))
	}
	if h.driver.hasContainer(ReplicaContainerName("shop", "production", "web", 1)) {
		t.Error("a migration that failed still started a replica")
	}
}

func TestDeployFailsAtTheBuild(t *testing.T) {
	h, _, b := deployHarness(t, 1)
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	b.Err = errFake("no such ref")

	d, err := h.m.Deploy(context.Background(), DeployRequest{App: "shop", Env: "production", Ref: "v9"}, &h.out)
	if err == nil || !strings.Contains(err.Error(), "no such ref") {
		t.Fatalf("error = %v", err)
	}
	if d.Status != DeployFailed {
		t.Errorf("status = %s", d.Status)
	}
	row, ok := h.store.deploy(d.ID)
	if !ok || row.Status != state.DeployFailed || row.Error == "" {
		t.Errorf("row = %+v, want a failed row carrying the error", row)
	}
}

func TestDeployWatchIsJudgedOnTheManagersClock(t *testing.T) {
	h, _, _ := deployHarness(t, 1)
	c := newClock()
	h.m.Now = c.now
	h.cfg.Deploy = &config.Deploy{Watch: time.Minute}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})

	done := make(chan *Deploy, 1)
	go func() {
		d, err := h.m.Deploy(context.Background(), DeployRequest{App: "shop", Env: "production"}, &h.out)
		if err != nil {
			t.Errorf("Deploy: %v\n%s", err, h.out.String())
		}
		done <- d
	}()

	select {
	case d := <-done:
		t.Fatalf("the deploy ended at %s with the clock held still\n%s", d.Status, stepsText(d))
	case <-time.After(50 * time.Millisecond):
	}
	c.add(time.Minute)
	select {
	case d := <-done:
		if d.Status != DeployPromoted {
			t.Fatalf("status = %s\n%s", d.Status, stepsText(d))
		}
		if d.Watch == nil || d.Watch.Window != time.Minute || d.Watch.Elapsed < time.Minute {
			t.Errorf("watch = %+v", d.Watch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the deploy did not end after the window passed")
	}
}

func TestDeployWithNoWatchLeavesTheWindowToTheDaemon(t *testing.T) {
	h, _, _ := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: shortWatch}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})

	d := h.mustDeploy(DeployRequest{App: "shop", Env: "production", NoWatch: true})
	if d.Status != DeployWatching {
		t.Fatalf("status = %s, want watching\n%s", d.Status, stepsText(d))
	}
	h.waitStatus(d.ID, state.DeployPromoted)
}

func TestDeployIsRefusedWhileOneIsStillWatching(t *testing.T) {
	h, _, _ := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: time.Hour, Promote: config.PromoteManual}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	d := h.mustDeploy(DeployRequest{App: "shop", Env: "production", NoWatch: true})
	h.waitStatus(d.ID, state.DeployWatching)

	_, err := h.m.Deploy(context.Background(), DeployRequest{App: "shop", Env: "production"}, &h.out)
	if err == nil {
		t.Fatal("a second deploy was allowed")
	}
	for _, want := range []string{"caramelo promote production", "caramelo rollback production"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
	h.mustPromote()
	if row, ok := h.store.deploy(d.ID); !ok || row.Status != state.DeployPromoted {
		t.Errorf("row = %+v", row)
	}
}

func TestRollbackInsideTheWindowIsAFlipBack(t *testing.T) {
	h, e, b := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: time.Hour, Promote: config.PromoteManual}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	first := h.mustDeploy(DeployRequest{App: "shop", Env: "production", NoWatch: true})
	h.waitStatus(first.ID, state.DeployWatching)
	h.mustPromote()

	h.push(b, "bbbbbbbbbbbb")
	second := h.mustDeploy(DeployRequest{App: "shop", Env: "production", NoWatch: true})
	h.waitStatus(second.ID, state.DeployWatching)
	builds := b.count()

	d, err := h.m.Rollback(context.Background(), RollbackRequest{App: "shop", Env: "production"}, &h.out)
	if err != nil {
		t.Fatalf("Rollback: %v\n%s", err, h.out.String())
	}
	if d.ID != second.ID {
		t.Errorf("the rollback ended deploy %d, want the one that was watching (%d)", d.ID, second.ID)
	}
	if d.Status != DeployRolledBack {
		t.Fatalf("status = %s, want rolled_back", d.Status)
	}
	if b.count() != builds {
		t.Errorf("the rollback built something: %d builds, was %d", b.count(), builds)
	}
	if got := e.states(prodHost); got[1] != edge.TargetActive {
		t.Errorf("replica 1 is %s, want it serving again (%v)", got[1], got)
	}
}

func TestRollbackOutsideTheWindowDeploysThePreviousRelease(t *testing.T) {
	h, _, b := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: shortWatch}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	first := h.mustDeploy(DeployRequest{App: "shop", Env: "production"})
	h.push(b, "bbbbbbbbbbbb")
	h.mustDeploy(DeployRequest{App: "shop", Env: "production"})
	builds := b.count()

	d, err := h.m.Rollback(context.Background(), RollbackRequest{App: "shop", Env: "production"}, &h.out)
	if err != nil {
		t.Fatalf("Rollback: %v\n%s", err, h.out.String())
	}
	if d.Kind != DeployKindRollback || d.Status != DeployPromoted {
		t.Fatalf("deploy = kind %s status %s\n%s", d.Kind, d.Status, stepsText(d))
	}
	if d.Release == nil || d.Release.ID != first.Release.ID {
		t.Errorf("it went back to %+v, want the first release", d.Release)
	}
	if b.count() != builds {
		t.Errorf("a rollback built something: %d builds, was %d", b.count(), builds)
	}
	if !hasDeployStep(d, StepBuild, StepSkipped) {
		t.Errorf("the build step is %v, want it skipped", deploySteps(d))
	}

	to, err := h.m.Rollback(context.Background(),
		RollbackRequest{App: "shop", Env: "production", To: "bbbbbbbbbbbb"}, &h.out)
	if err != nil {
		t.Fatalf("Rollback --to: %v\n%s", err, h.out.String())
	}
	if to.Release == nil || to.Release.Tree != "bbbbbbbbbbbb" {
		t.Errorf("--to went to %+v", to.Release)
	}

	_, err = h.m.Rollback(context.Background(),
		RollbackRequest{App: "shop", Env: "production", To: "ffffffffffff"}, &h.out)
	if err == nil || !strings.Contains(err.Error(), "caramelo releases") {
		t.Errorf("an unknown --to = %v", err)
	}
}

func TestRollbackWithNoPreviousReleaseSaysSo(t *testing.T) {
	h, _, _ := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: shortWatch}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	h.mustDeploy(DeployRequest{App: "shop", Env: "production"})

	_, err := h.m.Rollback(context.Background(), RollbackRequest{App: "shop", Env: "production"}, &h.out)
	if err == nil || !strings.Contains(err.Error(), "no release to go back to") {
		t.Fatalf("error = %v", err)
	}
}

func TestReleasesIsTheDeployHistory(t *testing.T) {
	h, _, b := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: shortWatch}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	first := h.mustDeploy(DeployRequest{App: "shop", Env: "production"})
	h.push(b, "bbbbbbbbbbbb")
	second := h.mustDeploy(DeployRequest{App: "shop", Env: "production"})

	got, err := h.m.Releases(context.Background(), "shop", "production", 0)
	if err != nil {
		t.Fatalf("Releases: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("history has %d rows, want 2", len(got))
	}
	if got[0].ID != second.ID || got[1].ID != first.ID {
		t.Errorf("history is %d, %d; want newest first", got[0].ID, got[1].ID)
	}
	if got[0].Release == nil || got[0].Release.Tree != "bbbbbbbbbbbb" {
		t.Errorf("the newest row carries %+v", got[0].Release)
	}
	if got[0].FromRelease == nil || got[0].FromRelease.Tree != "aaaaaaaaaaaa" {
		t.Errorf("the newest row came from %+v", got[0].FromRelease)
	}
	if got[1].FromRelease != nil {
		t.Errorf("the first deploy came from %+v, want nothing", got[1].FromRelease)
	}
	if got[0].Status != DeployPromoted || got[0].Kind != DeployKindDeploy {
		t.Errorf("row = %+v", got[0])
	}
}

func (h *harness) restart() *Manager {
	m := New(h.store, h.driver, h.repo, ports.New(h.store, allowAll{}), h.m.Runner,
		Dirs{Data: h.data, User: "caramelo", Run: h.run})
	m.Version = h.m.Version
	m.ReadyInterval, m.Timeout = h.m.ReadyInterval, h.m.Timeout
	m.UpTimeout, m.StopTimeout, m.WatchInterval = h.m.UpTimeout, h.m.StopTimeout, h.m.WatchInterval
	m.LoadConfig, m.Detect = h.m.LoadConfig, h.m.Detect
	m.DialTCP, m.HTTPStatus = h.m.DialTCP, h.m.HTTPStatus
	m.Edge, m.Builder, m.Secrets, m.Now = h.m.Edge, h.m.Builder, h.m.Secrets, h.m.Now
	return h.daemon(m)
}

func TestResumeContinuesAWatch(t *testing.T) {
	h, e, b := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: time.Hour, Promote: config.PromoteManual}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	first := h.mustDeploy(DeployRequest{App: "shop", Env: "production", NoWatch: true})
	h.waitStatus(first.ID, state.DeployWatching)
	h.mustPromote()

	h.push(b, "bbbbbbbbbbbb")
	old := h.restart()
	ctx, kill := context.WithCancel(context.Background())
	walked := make(chan struct{})
	go func() {
		defer close(walked)

		_, _ = old.Deploy(ctx, DeployRequest{App: "shop", Env: "production"}, &h.out)
	}()
	if got := h.waitPool(e, 1, 1); !got.ok {
		t.Fatalf("the table holds %d and serves %d\n%s", got.held, got.active, h.out.String())
	}

	kill()
	<-walked
	h.stop(old)
	h.waitStatus(deployIDOf(t, h, "production"), state.DeployWatching)
	rec, err := h.store.Env(context.Background(), "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	if rec.DeployID == 0 {
		t.Fatal("the killed daemon cleared the deploy: there would be nothing to resume")
	}
	row, ok := h.store.deploy(rec.DeployID)
	if !ok || row.Status != state.DeployWatching {
		t.Fatalf("row = %+v, want it left watching", row)
	}

	next := h.restart()
	if err := next.ResumeDeploys(context.Background()); err != nil {
		t.Fatalf("ResumeDeploys: %v\n%s", err, h.out.String())
	}
	out, err := next.Promote(context.Background(), PromoteRequest{App: "shop", Env: "production"}, &h.out)
	if err != nil {
		t.Fatalf("promote the resumed deploy: %v\n%s", err, h.out.String())
	}
	if out.Status != DeployPromoted {
		t.Fatalf("status = %s", out.Status)
	}
	if h.driver.hasContainer(ReplicaContainerName("shop", "production", "web", 1)) {
		t.Error("the held replica survived the resumed promotion")
	}
	if len(e.states(prodHost)) != 1 {
		t.Errorf("the table has %d targets, want only the new pool", len(e.states(prodHost)))
	}
}

func TestResumeRollsBackADeployThatNeverSwitched(t *testing.T) {
	h, _, _ := deployHarness(t, 1)
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	ctx := context.Background()
	rec, err := h.store.Env(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.m.Builder.Build(ctx, release.BuildRequest{App: "shop", Env: "production"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	row, err := h.store.CreateDeploy(ctx, state.Deploy{
		EnvID: rec.ID, ReleaseID: res.Release.ID, Kind: state.DeployKindDeploy,
		Status: state.DeployStarting, StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetEnvDeploy(ctx, rec.ID, row.ID); err != nil {
		t.Fatal(err)
	}

	stray := ReplicaContainerName("shop", "production", "web", 7)
	if _, err := h.driver.Run(ctx, runtime.ContainerSpec{
		Name: stray, Image: "caramelo/shop/web:aaaaaaaaaaaa",
		Labels: ReplicaLabels("shop", "production", "web", "test", "aaaaaaaaaaaa"),
	}); err != nil {
		t.Fatal(err)
	}

	if err := h.m.ResumeDeploys(ctx); err != nil {
		t.Fatalf("ResumeDeploys: %v", err)
	}
	got, ok := h.store.deploy(row.ID)
	if !ok || got.Status != state.DeployRolledBack {
		t.Fatalf("row = %+v, want rolled_back", got)
	}
	if !strings.Contains(got.Reason, "starting") {
		t.Errorf("reason = %q, want it to name how far it got", got.Reason)
	}
	if h.driver.hasContainer(stray) {
		t.Error("the replica the interrupted deploy started is still there")
	}
	rec, _ = h.store.Env(ctx, "shop", "production")
	if rec.DeployID != 0 {
		t.Errorf("env deploy = %d, want it cleared", rec.DeployID)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

func TestDeployCarriesTheEnvironmentsSecrets(t *testing.T) {
	h, _, _ := deployHarness(t, 1)
	h.cfg.Env = map[string]string{
		"DATABASE_URL": "postgres://postgres:${deps.db.password}@${deps.db.host}:${deps.db.port}/postgres",
		"GREETING_REF": "${secrets.GREETING}",
	}

	h.cfg.Deploy = &config.Deploy{Before: "python migrate.py", Watch: time.Millisecond}

	ctx := context.Background()
	for ref, value := range map[vault.Ref]string{
		vault.EnvRef("shop", "production", "DB_PASSWORD"): "chosen-by-the-operator",
		vault.AppRef("shop", "GREETING"):                  "hello",
	} {
		if _, err := h.m.Secrets.Set(ctx, ref, value); err != nil {
			t.Fatalf("set %s: %v", ref.Name, err)
		}
	}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})

	files := h.captureEnvFiles()
	d := h.mustDeploy(DeployRequest{App: "shop", Env: "production"})
	if d.Status != DeployPromoted {
		t.Fatalf("deploy ended %q: %s\n%s", d.Status, d.Error, h.out.String())
	}

	if len(files) == 0 {
		t.Fatalf("no container of the deploy was given an env-file:\n%s", h.out.String())
	}
	for name, body := range files {
		if !strings.Contains(body, "DATABASE_URL=postgres://postgres:chosen-by-the-operator@") {
			t.Errorf("%s's env-file has no resolved dependency password:\n%s", name, body)
		}
		if !strings.Contains(body, "GREETING_REF=hello") {
			t.Errorf("%s's env-file has no resolved secret reference:\n%s", name, body)
		}
	}

	offs := h.driver.oneOffs()
	if len(offs) == 0 {
		t.Fatal("deploy.before ran no one-off")
	}
	for _, off := range offs {
		if v := off.env["DATABASE_URL"]; strings.Contains(v, "postgres:@") {
			t.Errorf("the gate %q was given %q: the password is empty", off.command, v)
		}
	}
}

func TestResumeDeploysTakesOnAllOfThem(t *testing.T) {
	h, _, _ := deployHarness(t, 1)
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	ctx := context.Background()
	rec, err := h.store.Env(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}

	for _, envID := range []int64{rec.ID + 999, rec.ID} {
		if _, err := h.store.CreateDeploy(ctx, state.Deploy{
			EnvID: envID, Kind: state.DeployKindDeploy,
			Status: string(DeployStarting), StartedAt: h.m.now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.m.ResumeDeploys(ctx); err != nil {
		t.Fatalf("ResumeDeploys: %v", err)
	}
	left, err := h.store.UnfinishedDeploys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("%d deploy(s) are still unfinished after a resume: %+v", len(left), left)
	}
}

func TestManualPromoteReturnsOnceTheWindowHasPassed(t *testing.T) {
	h, _, _ := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: time.Millisecond, Promote: config.PromoteManual}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})

	done := make(chan *Deploy, 1)
	go func() { done <- h.mustDeploy(DeployRequest{App: "shop", Env: "production"}) }()

	var d *Deploy
	select {
	case d = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deploy never returned: a manual promote must not hold the caller's terminal")
	}
	if d.Status != DeployWatching {
		t.Fatalf("deploy ended %q, want %q: the hold is the daemon's now", d.Status, DeployWatching)
	}
	if _, ok := lastStep(d, StepPromote); ok {
		t.Error("a manual-promote deploy promoted itself")
	}

	if _, err := h.m.Promote(context.Background(), PromoteRequest{App: "shop", Env: "production"}, &h.out); err != nil {
		t.Fatalf("Promote: %v\n%s", err, h.out.String())
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		hist, err := h.m.Releases(context.Background(), "shop", "production", 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(hist) > 0 && hist[0].Status == DeployPromoted {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the held deploy was never promoted: %+v", hist)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func lastStep(d *Deploy, name DeployStepName) (DeployStep, bool) {
	out, found := DeployStep{}, false
	for _, s := range d.Steps {
		if s.Step == name {
			out, found = s, true
		}
	}
	return out, found
}

func TestRollbackToAPrunedReleaseIsRefused(t *testing.T) {
	h, _, b := deployHarness(t, 1)

	h.cfg.Deploy = &config.Deploy{Watch: time.Millisecond}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	first := h.mustDeploy(DeployRequest{App: "shop", Env: "production"})
	if first.Release == nil {
		t.Fatal("the first deploy made no release")
	}
	b.tree = "bbbbbbbbbbbb"
	if _, err := h.m.Deploy(context.Background(),
		DeployRequest{App: "shop", Env: "production"}, &h.out); err != nil {
		t.Fatalf("the second deploy: %v\n%s", err, h.out.String())
	}

	for _, ref := range first.Release.ImageList() {
		if err := h.driver.RemoveImage(context.Background(), ref); err != nil {
			t.Fatal(err)
		}
	}

	h.out.Reset()
	_, err := h.m.Rollback(context.Background(),
		RollbackRequest{App: "shop", Env: "production", To: first.Release.Short()}, &h.out)
	if err == nil {
		t.Fatal("a rollback to a pruned release was allowed")
	}
	for _, want := range []string{first.Release.Short(), "pruned", "deploy.keep"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not mention %q", err, want)
		}
	}

	if offs := h.driver.oneOffs(); len(offs) != 0 {
		t.Errorf("a refused rollback ran %d one-off(s): %+v", len(offs), offs)
	}
}

func TestDeployMarksAnUnexposedServiceRunning(t *testing.T) {
	h, _, _ := deployHarness(t, 1)

	h.cfg.Domain = ""
	h.cfg.Envs = nil
	h.cfg.Deploy = &config.Deploy{Watch: time.Millisecond}
	for i := range h.cfg.Services {
		h.cfg.Services[i].Expose = config.ExposeNone
	}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})

	d := h.mustDeploy(DeployRequest{App: "shop", Env: "production"})
	if d.Status != DeployPromoted {
		t.Fatalf("deploy ended %q: %s\n%s", d.Status, d.Error, h.out.String())
	}
	services, err := h.m.Services(context.Background(), "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(services) == 0 {
		t.Fatal("no services after a deploy")
	}
	for _, s := range services {
		if s.Status != ServiceRunning {
			t.Errorf("service %s is %q after a promoted deploy, want %q", s.Name, s.Status, ServiceRunning)
		}
		for _, rep := range s.Replicas {
			if rep.Status != ServiceRunning {
				t.Errorf("%s/%d is %q, want %q", s.Name, rep.Index, rep.Status, ServiceRunning)
			}
		}
	}
}

func deployIDOf(t *testing.T, h *harness, name string) int64 {
	t.Helper()
	rec, err := h.store.Env(context.Background(), "shop", name)
	if err != nil {
		t.Fatal(err)
	}
	return rec.DeployID
}

func TestAWatchWhoseCommanderLeftIsFinishedByTheDaemon(t *testing.T) {
	h, _, _ := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: time.Hour, Promote: config.PromoteManual}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})

	ctx, hangUp := context.WithCancel(context.Background())
	walked := make(chan *Deploy, 1)
	go func() {
		d, _ := h.m.Deploy(ctx, DeployRequest{App: "shop", Env: "production"}, &h.out)
		walked <- d
	}()
	id := int64(0)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && id == 0 {
		if got := deployIDOf(t, h, "production"); got != 0 {
			if row, ok := h.store.deploy(got); ok && row.Status == state.DeployWatching {
				id = got
			}
		}
		time.Sleep(time.Millisecond)
	}
	if id == 0 {
		t.Fatalf("no deploy reached the watch\n%s", h.out.String())
	}
	hangUp()
	select {
	case d := <-walked:
		if d == nil || d.Status != DeployWatching {
			t.Fatalf("the interrupted deploy answered %+v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the deploy did not return when its commander left")
	}

	d, err := h.m.Promote(context.Background(), PromoteRequest{App: "shop", Env: "production"}, &h.out)
	if err != nil {
		t.Fatalf("promote the deploy whose commander left: %v\n%s", err, h.out.String())
	}
	if d.ID != id || d.Status != DeployPromoted {
		t.Fatalf("deploy %d is %s, want %d promoted", d.ID, d.Status, id)
	}
	if got := deployIDOf(t, h, "production"); got != 0 {
		t.Errorf("env still points at deploy %d", got)
	}
}

func TestPromoteAdoptsAWatchThisDaemonIsNotWatching(t *testing.T) {
	h, e, _ := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: time.Hour, Promote: config.PromoteManual}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	first := h.mustDeploy(DeployRequest{App: "shop", Env: "production", NoWatch: true})
	h.waitStatus(first.ID, state.DeployWatching)

	h.stop(h.m)
	h.waitStatus(first.ID, state.DeployWatching)

	next := h.restart()
	d, err := next.Promote(context.Background(), PromoteRequest{App: "shop", Env: "production"}, &h.out)
	if err != nil {
		t.Fatalf("Promote: %v\n%s", err, h.out.String())
	}
	if d.ID != first.ID || d.Status != DeployPromoted {
		t.Fatalf("deploy %d is %s, want %d promoted", d.ID, d.Status, first.ID)
	}
	if got := h.waitPool(e, 0, 1); !got.ok {
		t.Errorf("the table holds %d and serves %d, want only the new pool", got.held, got.active)
	}
}

func TestADeployThatFailsReplacingAWorkerLeavesTheServingPoolAlone(t *testing.T) {
	h, e, b := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: shortWatch}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	first := h.mustDeploy(DeployRequest{App: "shop", Env: "production"})
	if first.Status != DeployPromoted {
		t.Fatalf("the first deploy ended %s\n%s", first.Status, stepsText(first))
	}
	serving := activeReplica(t, e)

	h.push(b, "bbbbbbbbbbbb")
	h.driver.RunErr = func(spec runtime.ContainerSpec) error {
		if strings.Contains(spec.Image, "/echo:") && strings.Contains(spec.Image, "bbbbbbbbbbbb") {
			return errors.New("the worker's image is broken")
		}
		return nil
	}

	d, err := h.m.Deploy(context.Background(), DeployRequest{App: "shop", Env: "production"}, &h.out)
	if err == nil {
		t.Fatalf("the deploy was reported as working\n%s", stepsText(d))
	}
	if d.Status != DeployFailed {
		t.Fatalf("deploy ended %s, want failed\n%s", d.Status, stepsText(d))
	}

	states := e.states(prodHost)
	if len(states) != 1 || states[serving] != edge.TargetActive {
		t.Fatalf("the table is %v, want only replica %d active", states, serving)
	}
	if !h.driver.hasContainer(ReplicaContainerName("shop", "production", "web", serving)) {
		t.Errorf("the replica the table names is gone")
	}

	echo := ReplicaContainerName("shop", "production", "echo", 1)
	if !h.driver.hasContainer(echo) {
		t.Fatalf("the worker is gone after a failed deploy\n%s", h.out.String())
	}
	if img := h.driver.imageOf(echo); !strings.Contains(img, "aaaaaaaaaaaa") {
		t.Errorf("the worker runs %q, want the release that is serving", img)
	}
}

func activeReplica(t *testing.T, e *fakeEdge) int {
	t.Helper()
	for index, state := range e.states(prodHost) {
		if state == edge.TargetActive {
			return index
		}
	}
	t.Fatalf("nothing is active in %v", e.states(prodHost))
	return 0
}

func TestTheDeployRowSaysWatchingBeforeTheFlipIsPushed(t *testing.T) {
	h, e, _ := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: shortWatch}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})

	var mu sync.Mutex
	statusAt := ""
	e.OnPush = func(table edge.Table) {
		active := false
		for _, route := range table.Routes {
			for _, target := range route.Targets {
				if target.State == edge.TargetActive {
					active = true
				}
			}
		}
		if !active {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if statusAt != "" {
			return
		}
		if row, ok := h.store.deploy(deployIDOf(t, h, "production")); ok {
			statusAt = row.Status
		}
	}

	d := h.mustDeploy(DeployRequest{App: "shop", Env: "production"})
	if d.Status != DeployPromoted {
		t.Fatalf("deploy ended %s\n%s", d.Status, stepsText(d))
	}
	mu.Lock()
	defer mu.Unlock()
	if statusAt != state.DeployWatching {
		t.Errorf("the row said %q when the new pool went into the table, want %q",
			statusAt, state.DeployWatching)
	}
}

func TestDeployRollsBackOnTheRoutesOwn502s(t *testing.T) {
	h, e, b := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: time.Hour, MaxErrors: "5%"}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	h.mustDeploy(DeployRequest{App: "shop", Env: "production", NoWatch: true})
	h.mustPromote()

	h.push(b, "bbbbbbbbbbbb")
	e.setCounts(&edge.Counts{Hosts: []edge.HostCounts{{
		Host: prodHost, Requests: 504, Status5xx: 500, ConnectFailures: 4,
		Targets: []edge.TargetCounts{
			{Replica: 1},
			{Replica: 2, Requests: 4, ConnectFailures: 4},
		},
		Unrouted: edge.UnroutedCounts{Requests: 500, Status5xx: 500},
	}}})

	d, err := h.m.Deploy(context.Background(), DeployRequest{App: "shop", Env: "production"}, &h.out)
	if err != nil {
		t.Fatalf("Deploy: %v\n%s", err, h.out.String())
	}
	if d.Status != DeployRolledBack {
		t.Fatalf("status = %s, want rolled_back\n%s", d.Status, stepsText(d))
	}
	if d.Watch == nil || d.Watch.Requests != 504 || d.Watch.Errors != 504 {
		t.Fatalf("watch = %+v, want every request counted and every one an error", d.Watch)
	}
	if got := e.states(prodHost); got[1] != edge.TargetActive || len(got) != 1 {
		t.Errorf("the table is %v, want the previous pool serving alone", got)
	}
}

func TestResumeKeepsAPoolThatWasStillDraining(t *testing.T) {
	h, e, b := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: time.Hour, Promote: config.PromoteManual}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	first := h.mustDeploy(DeployRequest{App: "shop", Env: "production", NoWatch: true})
	h.waitStatus(first.ID, state.DeployWatching)
	h.mustPromote()

	h.push(b, "bbbbbbbbbbbb")
	second := h.mustDeploy(DeployRequest{App: "shop", Env: "production", NoWatch: true})
	h.waitStatus(second.ID, state.DeployWatching)

	h.stop(h.m)
	if n := h.store.setTargetStates(string(edge.TargetHeld), string(edge.TargetDraining)); n == 0 {
		t.Fatal("nothing was held to put back into a drain")
	}

	next := h.restart()
	if err := next.ResumeDeploys(context.Background()); err != nil {
		t.Fatalf("ResumeDeploys: %v\n%s", err, h.out.String())
	}
	d, err := next.Rollback(context.Background(), RollbackRequest{App: "shop", Env: "production"}, &h.out)
	if err != nil {
		t.Fatalf("Rollback: %v\n%s", err, h.out.String())
	}
	if d.ID != second.ID || d.Status != DeployRolledBack {
		t.Fatalf("deploy %d is %s, want %d rolled back", d.ID, d.Status, second.ID)
	}

	got := e.states(prodHost)
	if got[1] != edge.TargetActive || len(got) != 1 {
		t.Fatalf("the table is %v, want replica 1 serving alone", got)
	}
	if !h.driver.hasContainer(ReplicaContainerName("shop", "production", "web", 1)) {
		t.Error("the replica the table names is gone")
	}
	if !strings.Contains(d.Reason, "rolled back") {
		t.Errorf("reason = %q", d.Reason)
	}
}

func TestDeployWithZeroMaxErrorsRollsBackOnOneError(t *testing.T) {
	h, e, b := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: time.Hour, MaxErrors: "0%"}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	h.mustDeploy(DeployRequest{App: "shop", Env: "production", NoWatch: true})
	h.mustPromote()

	h.push(b, "bbbbbbbbbbbb")
	e.setCounts(&edge.Counts{Hosts: []edge.HostCounts{{
		Host: prodHost, Requests: 100, Status5xx: 1,
		Targets: []edge.TargetCounts{
			{Replica: 1},
			{Replica: 2, Requests: 100, Status5xx: 1},
		},
	}}})

	d, err := h.m.Deploy(context.Background(), DeployRequest{App: "shop", Env: "production"}, &h.out)
	if err != nil {
		t.Fatalf("Deploy: %v\n%s", err, h.out.String())
	}
	if d.Status != DeployRolledBack {
		t.Fatalf("status = %s, want rolled_back on one error at 0%%\n%s", d.Status, stepsText(d))
	}

	h.cfg.Deploy = &config.Deploy{Watch: shortWatch, MaxErrors: config.MaxErrorsNone}
	h.push(b, "cccccccccccc")
	e.setCounts(&edge.Counts{Hosts: []edge.HostCounts{{
		Host: prodHost, Requests: 100, Status5xx: 100,
		Targets: []edge.TargetCounts{{Replica: 2, Requests: 100, Status5xx: 100}},
	}}})
	d = h.mustDeploy(DeployRequest{App: "shop", Env: "production"})
	if d.Status != DeployPromoted {
		t.Fatalf("status = %s, want promoted under `max_errors: none`\n%s", d.Status, stepsText(d))
	}
}

func TestASecondDeployWalksOnTheRowItFindsUnderTheLock(t *testing.T) {
	h, _, b := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: shortWatch}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	first := h.mustDeploy(DeployRequest{App: "shop", Env: "production"})

	h.push(b, "bbbbbbbbbbbb")
	var wg sync.WaitGroup
	got := make([]*Deploy, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got[i], errs[i] = h.m.Deploy(context.Background(),
				DeployRequest{App: "shop", Env: "production"}, &h.out)
		}()
	}
	close(start)
	wg.Wait()

	last := (*Deploy)(nil)
	for i, d := range got {
		if errs[i] != nil {

			continue
		}
		if d.Status != DeployPromoted {
			t.Fatalf("deploy %d ended %s\n%s", d.ID, d.Status, stepsText(d))
		}
		if last == nil || d.ID > last.ID {
			last = d
		}
	}
	if last == nil {
		t.Fatalf("neither deploy ran: %v, %v", errs[0], errs[1])
	}
	if last.ID == first.ID {
		return
	}

	if last.FromRelease == nil {
		t.Fatalf("deploy %d recorded no predecessor", last.ID)
	}
	rows, err := h.m.Releases(context.Background(), "shop", "production", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == last.ID || row.Status != DeployPromoted || row.Release == nil {
			continue
		}

		if row.Release.ID != last.FromRelease.ID {
			t.Errorf("deploy %d says it replaced release %d, but %d was the one running",
				last.ID, last.FromRelease.ID, row.Release.ID)
		}
		break
	}
}

func TestARollbackRewritesTheExposedServicesRows(t *testing.T) {
	h, e, b := deployHarness(t, 1)
	h.cfg.Deploy = &config.Deploy{Watch: shortWatch}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	first := h.mustDeploy(DeployRequest{App: "shop", Env: "production"})
	if first.Release == nil {
		t.Fatal("the first deploy carried no release")
	}

	h.push(b, "bbbbbbbbbbbb")
	h.cfg.Deploy = &config.Deploy{Watch: time.Hour, MaxErrors: "5%"}
	e.setCounts(&edge.Counts{Hosts: []edge.HostCounts{{
		Host: prodHost, Requests: 100, Status5xx: 40,
		Targets: []edge.TargetCounts{{Replica: 1}, {Replica: 2, Requests: 100, Status5xx: 40}},
	}}})
	d, err := h.m.Deploy(context.Background(), DeployRequest{App: "shop", Env: "production"}, &h.out)
	if err != nil {
		t.Fatalf("Deploy: %v\n%s", err, h.out.String())
	}
	if d.Status != DeployRolledBack {
		t.Fatalf("status = %s, want rolled_back\n%s", d.Status, stepsText(d))
	}

	services, err := h.m.Services(context.Background(), "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	web := (*Service)(nil)
	for i := range services {
		if services[i].Name == "web" {
			web = &services[i]
		}
	}
	if web == nil {
		t.Fatal("no web service")
	}
	if !strings.Contains(web.Image, release.ShortTree(first.Release.Tree)) {
		t.Errorf("web says it runs %q, want the release that is serving (%s)",
			web.Image, first.Release.Short())
	}
	if d.Release != nil && strings.Contains(web.Image, release.ShortTree(d.Release.Tree)) {
		t.Errorf("web still names the release that was rolled back: %q", web.Image)
	}
	if web.Status != ServiceRunning {
		t.Errorf("web is %q after a rollback, want running", web.Status)
	}
}

func TestTheWatchCountsFromWhereTheEdgeSaysItCountsFrom(t *testing.T) {
	h, e, _ := deployHarness(t, 1)
	c := newClock()
	h.m.Now = c.now
	h.cfg.Deploy = &config.Deploy{Watch: time.Minute}
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})

	restarted := c.now().Add(30 * time.Second)
	e.setCounts(&edge.Counts{Since: restarted, Hosts: []edge.HostCounts{{Host: prodHost}}})

	done := make(chan *Deploy, 1)
	go func() {
		d, err := h.m.Deploy(context.Background(), DeployRequest{App: "shop", Env: "production"}, &h.out)
		if err != nil {
			t.Errorf("Deploy: %v\n%s", err, h.out.String())
		}
		done <- d
	}()
	waitForWatching(t, h)

	c.add(30 * time.Second)
	waitForEvent(t, h, "deploy", "only been counting since")

	c.add(30 * time.Second)
	select {
	case d := <-done:
		t.Fatalf("the deploy ended at %s on a window the edge counted half of\n%s", d.Status, stepsText(d))
	case <-time.After(50 * time.Millisecond):
	}

	c.add(30 * time.Second)
	select {
	case d := <-done:
		if d.Status != DeployPromoted {
			t.Fatalf("status = %s\n%s", d.Status, stepsText(d))
		}
		if d.Watch == nil || !d.Watch.CountedFrom.Equal(restarted) {
			t.Fatalf("watch = %+v, want it counting from the edge's own start", d.Watch)
		}
		if d.Watch.Counted < time.Minute {
			t.Errorf("counted = %s, want a whole window", d.Watch.Counted)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the deploy did not end after the counted window passed\n%s", h.out.String())
	}
}

func waitForEvent(t *testing.T, h *harness, action, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.store.mu.Lock()
		for _, e := range h.store.events {
			if e.Action == action && strings.Contains(e.Detail, want) {
				h.store.mu.Unlock()
				return
			}
		}
		h.store.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no %s event saying %q\n%s", action, want, h.out.String())
}

func waitForWatching(t *testing.T, h *harness) int64 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if id := deployIDOf(t, h, "production"); id != 0 {
			if row, ok := h.store.deploy(id); ok && row.Status == state.DeployWatching {
				return id
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no deploy reached the watch\n%s", h.out.String())
	return 0
}
