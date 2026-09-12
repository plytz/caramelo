package env

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/ports"
	"github.com/plytz/caramelo/internal/state"
)

func edgeHarness(t *testing.T, replicas int) (*harness, *fakeEdge) {
	t.Helper()
	h := upHarness(t)
	h.cfg = replicaConfig(replicas)
	h.cfg.Domain = "shop.test"
	e := newEdge()
	h.m.Edge = e
	h.m.StopTimeout = time.Millisecond

	h.setTree("aaaaaaaaaaaa")
	return h, e
}

func (h *harness) setTree(hash string) {
	h.m.Runner = fixedTree(hash)
}

const webHost = "feat-x.shop.test"

func TestUpExposesAndFlipsInTheFirstReplicas(t *testing.T) {
	h, e := edgeHarness(t, 2)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	if res.URL != "https://"+webHost {
		t.Errorf("up URL = %q, want the derived public URL\n%s", res.URL, h.out.String())
	}
	if len(res.Rollouts) != 1 || res.Rollouts[0].Service != "web" {
		t.Fatalf("rollouts = %+v, want one for web", res.Rollouts)
	}
	roll := res.Rollouts[0]
	if roll.Failed != nil {
		t.Fatalf("rollout failed at %+v\n%s", roll.Failed, h.out.String())
	}

	want := []RolloutStepName{StepStart, StepHealth, StepProbe, StepFlip, StepDrain}
	for i, index := range []int{1, 2} {
		got := stepsOf(roll, index)
		if len(got) != len(want) {
			t.Fatalf("replica %d walked %v, want %v", index, got, want)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Errorf("replica %d step %d = %q, want %q", index, j, got[j], want[j])
			}
		}
		_ = i
	}

	states := e.states(webHost)
	if states[1] != edge.TargetActive || states[2] != edge.TargetActive {
		t.Errorf("the edge holds %v, want both replicas active", states)
	}
	if got := h.store.hosts(); len(got) != 1 || got[0] != webHost {
		t.Errorf("routes = %v, want just %s", got, webHost)
	}
	if svc := res.Services[0]; svc.Host != webHost || svc.PublicURL != "https://"+webHost {
		t.Errorf("web = %+v, want its public name", svc)
	}
}

func TestRolloutReplacesReplicasWithoutEmptyingThePool(t *testing.T) {
	h, e := edgeHarness(t, 2)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	before := e.pushes()

	h.cfg.Services[0].Run = "python app.py --v2"
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	roll := res.Rollouts[0]
	if roll.Failed != nil {
		t.Fatalf("rollout failed at %+v\n%s", roll.Failed, h.out.String())
	}

	if got := replicaIndexes(res.Services[0].Replicas); len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Errorf("the new replicas are %v, want 3 and 4", got)
	}
	for _, index := range []int{1, 2} {
		steps := stepsOf(roll, index)
		if !hasStep(steps, StepDrain) || !hasStep(steps, StepStop) {
			t.Errorf("replica %d walked %v, want a drain and a stop", index, steps)
		}
		container := ReplicaContainerName("shop", "feat-x", "web", index)
		if h.driver.hasContainer(container) {
			t.Errorf("%s is still there after it was drained", container)
		}
		if !wasStopped(h.driver, container) {
			t.Errorf("%s was killed rather than stopped: an app must get its SIGTERM", container)
		}
	}

	if e.pushes() <= before {
		t.Fatal("the rollout pushed no table")
	}
	for i, table := range e.tables {
		r, ok := table.Route(webHost)
		if !ok {
			continue
		}
		if len(r.Active()) == 0 {
			t.Fatalf("table %d has no active target for %s: the pool was empty", i, webHost)
		}
	}
	if states := e.states(webHost); len(states) != 2 || states[3] != edge.TargetActive || states[4] != edge.TargetActive {
		t.Errorf("the edge ends with %v, want only the new replicas active", states)
	}

	events := h.eventsOf(1, "rollout")
	for _, want := range []string{"web/3 start", "web/3 flip", "web/1 drain", "web/1 stop"} {
		if !containsDetail(events, want) {
			t.Errorf("the audit trail has no %q:\n%v", want, events)
		}
	}
}

func TestUpAfterARolloutChangesNothing(t *testing.T) {
	h, e := edgeHarness(t, 2)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	h.cfg.Services[0].Run = "python app.py --v2"
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	started := len(h.driver.specs)

	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if len(res.Rollouts[0].Steps) != 0 {
		t.Errorf("the third up walked %d steps, want none\n%s", len(res.Rollouts[0].Steps), h.out.String())
	}
	if res.Services[0].Change != ChangeUnchanged {
		t.Errorf("web = %q, want unchanged", res.Services[0].Change)
	}
	if len(h.driver.specs) != started {
		t.Errorf("%d containers were started again", len(h.driver.specs)-started)
	}
	if states := e.states(webHost); states[3] != edge.TargetActive || states[4] != edge.TargetActive {
		t.Errorf("the edge holds %v, want the pool refreshed and unchanged", states)
	}
}

func TestRolloutReplacesTheContainerM4Named(t *testing.T) {
	h, e := edgeHarness(t, 1)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	legacy := ServiceContainerName("shop", "feat-x", "web")
	if err := h.driver.Remove(context.Background(), ReplicaContainerName("shop", "feat-x", "web", 1), true); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := h.driver.Run(context.Background(), containerLike(legacy)); err != nil {
		t.Fatalf("run: %v", err)
	}

	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if h.driver.hasContainer(legacy) {
		t.Error("the unnumbered container is still there")
	}
	if !wasStopped(h.driver, legacy) {
		t.Error("the unnumbered container was killed rather than stopped")
	}
	if states := e.states(webHost); len(states) != 1 || states[1] != edge.TargetActive {
		t.Errorf("the edge holds %v, want the numbered replica active", states)
	}
}

func TestRolloutStopsAtTheFirstUnhealthyReplica(t *testing.T) {
	h, e := edgeHarness(t, 2)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	healthy := e.states(webHost)

	h.cfg.Services[0].Run = "python app.py --broken"
	h.m.HTTPStatus = func(context.Context, string) (int, error) { return 0, errConnRefused }

	res, err := h.up(UpRequest{App: "shop", Name: "feat-x"})
	if err == nil {
		t.Fatalf("up = nil, want the rollout to fail\n%s", h.out.String())
	}
	if !strings.Contains(err.Error(), "replica 3") || !strings.Contains(err.Error(), string(StepHealth)) {
		t.Errorf("err = %v, want it to name the replica and the step", err)
	}
	roll := res.Rollouts[0]
	if roll.Failed == nil || roll.Failed.Step != StepHealth || roll.Failed.Replica != 3 {
		t.Fatalf("failed step = %+v, want health of replica 3", roll.Failed)
	}

	for _, s := range roll.Steps {
		if s.Step == StepFlip || s.Step == StepDrain || s.Step == StepStop {
			t.Errorf("the failed rollout still walked %q of replica %d", s.Step, s.Replica)
		}
	}
	if states := e.states(webHost); !sameStates(states, healthy) {
		t.Errorf("the edge holds %v, want the old pool untouched (%v)", states, healthy)
	}
	for _, index := range []int{1, 2} {
		if !h.driver.hasContainer(ReplicaContainerName("shop", "feat-x", "web", index)) {
			t.Errorf("old replica %d was removed by a failed rollout", index)
		}
	}

	if !h.driver.hasContainer(ReplicaContainerName("shop", "feat-x", "web", 3)) {
		t.Error("the failed replica's container was removed")
	}
}

func TestRolloutStopsADrainThatNeverFinishes(t *testing.T) {
	h, e := edgeHarness(t, 1)
	h.cfg.Services[0].Drain = 20 * time.Millisecond
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	e.Drain = blockedDrain
	h.cfg.Services[0].Run = "python app.py --v2"
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	drain := stepOf(res.Rollouts[0], 1, StepDrain)
	if drain == nil {
		t.Fatalf("replica 1 never drained\n%s", h.out.String())
	}
	if !strings.Contains(drain.Detail, "deadline") {
		t.Errorf("drain detail = %q, want it to say the deadline passed", drain.Detail)
	}
	if stop := stepOf(res.Rollouts[0], 1, StepStop); stop == nil || stop.Status != StepOK {
		t.Errorf("replica 1 was not stopped after the deadline: %+v", stop)
	}
	if !wasStopped(h.driver, ReplicaContainerName("shop", "feat-x", "web", 1)) {
		t.Error("the replica whose drain timed out was not stopped")
	}
}

func TestRolloutWaitsForTheLastRequest(t *testing.T) {
	h, _ := edgeHarness(t, 1)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	h.cfg.Services[0].Run = "python app.py --v2"
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	drain := stepOf(res.Rollouts[0], 1, StepDrain)
	if drain == nil || !strings.Contains(drain.Detail, "last request") {
		t.Errorf("drain = %+v, want it to say the last request finished\n%s", drain, h.out.String())
	}
}

func TestUpRecreatesAServiceThatIsNotExposed(t *testing.T) {
	h, e := edgeHarness(t, 1)

	h.cfg.Services[0].Expose = config.ExposeNone
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	if len(res.Rollouts) != 0 {
		t.Errorf("rollouts = %+v, want none for an env with nothing exposed", res.Rollouts)
	}
	if len(h.store.hosts()) != 0 {
		t.Errorf("routes = %v, want none", h.store.hosts())
	}
	if e.pushes() != 0 {
		t.Errorf("%d tables were pushed for an env with no route", e.pushes())
	}
	h.cfg.Services[0].Run = "python app.py --v2"
	res = h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if res.Services[0].Change != ChangeRecreated {
		t.Errorf("web = %q, want it recreated in place", res.Services[0].Change)
	}
	if got := replicaIndexes(res.Services[0].Replicas); len(got) != 1 || got[0] != 1 {
		t.Errorf("the recreated replicas are %v, want replica 1 again", got)
	}
}

func TestUpWithoutAnEdgeStartsTheAppAnyway(t *testing.T) {
	h := upHarness(t)
	h.cfg = replicaConfig(1)
	h.cfg.Domain = "shop.test"
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if res.URL != "" || len(res.Routes) != 0 {
		t.Errorf("up = %q / %v on a machine with no edge, want nothing public", res.URL, res.Routes)
	}
	if !strings.Contains(h.out.String(), "no edge") {
		t.Errorf("nothing said the machine has no edge:\n%s", h.out.String())
	}
	if res.Services[0].Status != ServiceRunning {
		t.Errorf("web = %q, want it running regardless", res.Services[0].Status)
	}
}

func TestRolloutReportsAPushThatFails(t *testing.T) {
	h, e := edgeHarness(t, 1)
	e.PushErr = errEdgeDown
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	_, err := h.up(UpRequest{App: "shop", Name: "feat-x"})
	if err == nil || !strings.Contains(err.Error(), "push the route table") {
		t.Fatalf("up = %v, want the failed push named", err)
	}
	if got := h.store.hosts(); len(got) != 1 {
		t.Errorf("routes = %v, want the row kept so a retry can push it", got)
	}
}

func TestRolloutRetiresTheReplicasNobodyAskedFor(t *testing.T) {
	h, e := edgeHarness(t, 2)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	h.cfg = replicaConfig(1)
	h.cfg.Domain = "shop.test"
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	if got := replicaIndexes(res.Services[0].Replicas); len(got) != 1 {
		t.Fatalf("web has replicas %v, want one\n%s", got, h.out.String())
	}

	drained := false
	for _, table := range e.tables {
		r, ok := table.Route(webHost)
		if !ok {
			continue
		}
		for _, tg := range r.Targets {
			if tg.State == edge.TargetDraining {
				drained = true
			}
		}
	}
	if !drained {
		t.Error("no table ever marked the retired replica draining")
	}
	if states := e.states(webHost); len(states) != 1 {
		t.Errorf("the edge ends with %v, want one target", states)
	}
	if len(e.table().Routes[0].Active()) != 1 {
		t.Error("the remaining replica is not active")
	}
}

func TestUpNoWaitStillRollsAnExposedService(t *testing.T) {
	h, e := edgeHarness(t, 1)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x", NoWait: true})
	if !strings.Contains(h.out.String(), "waited for anyway") {
		t.Errorf("nothing said --no-wait does not apply:\n%s", h.out.String())
	}
	if len(res.Rollouts[0].Steps) == 0 {
		t.Error("the service was not rolled")
	}
	if states := e.states(webHost); states[1] != edge.TargetActive {
		t.Errorf("the edge holds %v, want the replica active", states)
	}
}

func TestURLsFollowTheReplicasAfterARollout(t *testing.T) {
	h, _ := edgeHarness(t, 1)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	h.cfg.Services[0].Run = "python app.py --v2"
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	want := res.Services[0].Replicas[0].Port
	urls, err := h.m.URLs(context.Background(), "shop", "feat-x", "web")
	if err != nil {
		t.Fatalf("URLs: %v", err)
	}
	if urls[0].Port != want {
		t.Errorf("env url says %d, want the live replica's port %d", urls[0].Port, want)
	}

	services, err := h.m.Services(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if services[0].Port != want {
		t.Errorf("env show says %d, want %d", services[0].Port, want)
	}

	if row, ok := h.store.service(1, "web"); !ok || row.Port != ports.Base {
		t.Errorf("the service row holds port %d, want the layout's %d", row.Port, ports.Base)
	}
}

func TestServicesReportTheEdgesViewOfEachReplica(t *testing.T) {
	h, _ := edgeHarness(t, 2)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	route, err := h.store.RouteByHost(context.Background(), webHost)
	if err != nil {
		t.Fatalf("RouteByHost: %v", err)
	}
	if err := h.store.PutTarget(context.Background(), state.EdgeTarget{
		RouteID: route.ID, Replica: 2, Port: ports.Base + 4, State: state.TargetDraining, Inflight: 3,
	}); err != nil {
		t.Fatalf("PutTarget: %v", err)
	}

	services, err := h.m.Services(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	web := services[0]
	if web.Host != webHost || web.PublicURL != "https://"+webHost {
		t.Errorf("web = %+v, want its public name", web)
	}
	if web.Replicas[0].State != ReplicaActive {
		t.Errorf("replica 1 = %q, want active", web.Replicas[0].State)
	}
	if web.Replicas[1].State != ReplicaDraining || web.Replicas[1].Inflight != 3 {
		t.Errorf("replica 2 = %q with %d in flight, want draining with 3",
			web.Replicas[1].State, web.Replicas[1].Inflight)
	}
}

var (
	errConnRefused = &probeError{"connection refused"}
	errEdgeDown    = &probeError{"the edge is not listening"}
)

type probeError struct{ s string }

func (e *probeError) Error() string { return e.s }

func stepsOf(r Rollout, replica int) []RolloutStepName {
	out := []RolloutStepName{}
	for _, s := range r.Steps {
		if s.Replica == replica {
			out = append(out, s.Step)
		}
	}
	return out
}

func stepOf(r Rollout, replica int, name RolloutStepName) *RolloutStep {
	for i := range r.Steps {
		if r.Steps[i].Replica == replica && r.Steps[i].Step == name {
			return &r.Steps[i]
		}
	}
	return nil
}

func hasStep(steps []RolloutStepName, want RolloutStepName) bool {
	for _, s := range steps {
		if s == want {
			return true
		}
	}
	return false
}

func replicaIndexes(replicas []Replica) []int {
	out := make([]int, 0, len(replicas))
	for _, r := range replicas {
		out = append(out, r.Index)
	}
	return out
}

func sameStates(a, b map[int]edge.TargetState) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func wasStopped(d *fakeDriver, container string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.stopped {
		if s.name == container && s.timeout > 0 {
			return true
		}
	}
	return false
}

func (h *harness) eventsOf(envID int64, action string) []state.EnvEvent {
	h.t.Helper()
	rows, err := h.store.Events(context.Background(), envID, 0)
	if err != nil {
		h.t.Fatalf("events: %v", err)
	}
	out := []state.EnvEvent{}
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Action == action {
			out = append(out, rows[i])
		}
	}
	return out
}

func containsDetail(events []state.EnvEvent, want string) bool {
	for _, e := range events {
		if strings.Contains(e.Detail, want) {
			return true
		}
	}
	return false
}

func TestANewCommitRollsAnExposedService(t *testing.T) {
	h, e := edgeHarness(t, 2)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	first := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if roll := first.Rollouts[0]; roll.Failed != nil {
		t.Fatalf("the first rollout failed at %+v", roll.Failed)
	}

	again := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if got := stepsOf(again.Rollouts[0], 1); len(got) != 0 {
		t.Errorf("an up with the same tree walked %v; it must be a no-op", got)
	}

	h.setTree("bbbbbbbbbbbb")
	rolled := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	roll := rolled.Rollouts[0]
	if roll.Failed != nil {
		t.Fatalf("the rollout after a commit failed at %+v\n%s", roll.Failed, h.out.String())
	}
	for _, want := range []RolloutStepName{StepStart, StepHealth, StepProbe, StepFlip, StepDrain, StepStop} {
		if !hasStepNamed(roll, want) {
			t.Errorf("no %q step after a commit: %v\n%s", want, allSteps(roll), h.out.String())
		}
	}

	states := e.states(webHost)
	if len(states) != 2 {
		t.Fatalf("the edge holds %d target(s), want 2: %v", len(states), states)
	}
	for index, st := range states {
		if st != edge.TargetActive {
			t.Errorf("replica %d is %q after the rollout, want active", index, st)
		}
	}

	for _, rep := range h.m.mustReplicas(t, "shop", "feat-x", "web") {
		if rep.tree != "bbbbbbbbbbbb" {
			t.Errorf("replica %d carries tree %q, want the one up just deployed", rep.index, rep.tree)
		}
	}
}

func hasStepNamed(r Rollout, name RolloutStepName) bool {
	for _, s := range r.Steps {
		if s.Step == name {
			return true
		}
	}
	return false
}

func allSteps(r Rollout) []RolloutStepName {
	out := make([]RolloutStepName, 0, len(r.Steps))
	for _, s := range r.Steps {
		out = append(out, s.Step)
	}
	return out
}

func (m *Manager) mustReplicas(t *testing.T, app, name, service string) []replica {
	t.Helper()
	rec, err := m.env(context.Background(), app, name)
	if err != nil {
		t.Fatalf("read env %q: %v", name, err)
	}
	live, err := m.liveReplicas(context.Background(), rec, service)
	if err != nil {
		t.Fatalf("live replicas of %q: %v", service, err)
	}
	return live
}

func TestEveryDrainIsWatchedFromTheFlip(t *testing.T) {
	h, e := edgeHarness(t, 2)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	h.setTree("bbbbbbbbbbbb")
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	roll := res.Rollouts[0]
	if roll.Failed != nil {
		t.Fatalf("rollout failed at %+v\n%s", roll.Failed, h.out.String())
	}
	var flip time.Time
	for _, s := range roll.Steps {
		if s.Step == StepFlip && s.Status == StepOK {
			flip = s.At
			break
		}
	}
	if flip.IsZero() {
		t.Fatalf("no flip step: %v", allSteps(roll))
	}
	windows := e.subscriptions()
	if len(windows) != 2 {
		t.Fatalf("%d subscriptions, want one per draining replica", len(windows))
	}
	for i, since := range windows {
		if !since.Equal(flip) {
			t.Errorf("drain %d was watched from %s, not from the flip at %s: the event it waits for "+
				"was published at the flip and is never sent twice, so a window that starts anywhere "+
				"later finds nothing and waits out the whole drain", i+1, since, flip)
		}
	}

	for _, s := range roll.Steps {
		if s.Step == StepDrain && strings.Contains(s.Detail, "deadline") {
			t.Errorf("replica %d reported %q for a drain the edge had already reported", s.Replica, s.Detail)
		}
	}
}

func TestTheFlipIsOnePushAndNeverServesTwoBuildsAtOnce(t *testing.T) {
	h, e := edgeHarness(t, 2)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	h.setTree("bbbbbbbbbbbb")
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if roll := res.Rollouts[0]; roll.Failed != nil {
		t.Fatalf("rollout failed at %+v\n%s", roll.Failed, h.out.String())
	}

	oldPool := map[int]bool{1: true, 2: true}
	newPool := map[int]bool{3: true, 4: true}
	for i, table := range e.tables {
		r, ok := table.Route(webHost)
		if !ok {
			continue
		}
		var olds, news int
		for _, target := range r.Active() {
			switch {
			case oldPool[target.Replica]:
				olds++
			case newPool[target.Replica]:
				news++
			}
		}
		if olds > 0 && news > 0 {
			t.Errorf("table %d serves %d old and %d new replica(s) at once: the flip must be one push",
				i, olds, news)
		}
		if olds+news == 0 {
			t.Errorf("table %d has no active target: the pool was empty", i)
		}
	}

	roll := res.Rollouts[0]
	var flips []RolloutStep
	for _, s := range roll.Steps {
		if s.Step == StepFlip && s.Status == StepOK {
			flips = append(flips, s)
		}
	}
	if len(flips) != 2 {
		t.Fatalf("%d flip steps, want one per new replica: %v", len(flips), allSteps(roll))
	}
	if !flips[0].At.Equal(flips[1].At) {
		t.Errorf("the two replicas were flipped at %s and %s; one push is one instant",
			flips[0].At, flips[1].At)
	}
}

func TestASkippedRolloutStepPrintsItsStatus(t *testing.T) {
	h, _ := edgeHarness(t, 1)
	rec, err := h.store.Env(context.Background(), "shop", "feat-x")
	if err != nil {
		h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
		if rec, err = h.store.Env(context.Background(), "shop", "feat-x"); err != nil {
			t.Fatal(err)
		}
	}
	roll := &Rollout{Service: "web", Host: webHost}
	h.out.Reset()
	h.m.step(context.Background(), rec, roll, &h.out, RolloutStep{
		Replica: 1, Step: StepDrain, Status: StepSkipped, State: ReplicaDraining,
		Detail: "nothing was replaced",
	})
	want := "[skipped] rollout: web/1 drain: nothing was replaced\n"
	if got := h.out.String(); got != want {
		t.Errorf("the line is %q, want %q", got, want)
	}
}
