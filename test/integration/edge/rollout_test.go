//go:build integration

package edge

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	cenv "github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/test/integration/itest"
)

const drainWindow = 5 * time.Second

type wsClose struct {
	at  time.Time
	err error
}

type slowResult struct {
	status  int
	version string
	err     error
	took    time.Duration
}

func TestRolloutIsInvisibleToTheClient(t *testing.T) {
	m := begin(t)
	needRepo(t)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(10*time.Minute))
	defer cancel()

	load := itest.StartLoad(itest.Load{
		Client: internet.Client(0),
		URL:    urlX + "/",
		Marker: versionOf,
	})
	defer func() {
		if rep := load.Stop(); rep.Total > 0 {
			t.Logf("load generator: %s", rep)
		}
	}()
	if err := load.WaitForRequests(50, itest.Scale(time.Minute)); err != nil {
		t.Fatalf("the load never got going: %v", err)
	}

	ws, err := internet.DialWebSocket(ctx, "wss://"+hostX+"/ws")
	if err != nil {
		t.Fatalf("open a websocket before the rollout: %v", err)
	}
	defer ws.Close()
	if got, err := ws.Echo("before the rollout"); err != nil || got != "before the rollout" {
		t.Fatalf("the websocket does not echo before the rollout: %q, %v", got, err)
	}
	wsClosed := make(chan wsClose, 1)
	go func() {
		_, err := ws.WaitForClose(itest.Scale(8 * time.Minute))
		wsClosed <- wsClose{at: time.Now(), err: err}
	}()

	slow := make(chan slowResult, 1)
	go func() {
		started := time.Now()
		res, err := internet.Get(ctx, urlX+"/slow")
		if err != nil {
			slow <- slowResult{err: err, took: time.Since(started)}
			return
		}
		slow <- slowResult{status: res.Status, version: versionOf(res.Body), took: time.Since(started)}
	}()

	setVersion(t, repo, "v2")
	started := time.Now()
	out := up(t, envX)
	t.Logf("the rollout took %s", time.Since(started).Round(time.Millisecond))

	roll := rolloutOf(t, "up "+envX, out.Rollouts, webService)
	t.Logf("steps: %v", steps(roll))
	if roll.Failed != nil {
		t.Fatalf("the rollout failed at %+v", roll.Failed)
	}

	t.Run("every step of the replacement is reported", func(t *testing.T) {
		for _, want := range []cenv.RolloutStepName{
			cenv.StepStart, cenv.StepHealth, cenv.StepProbe,
			cenv.StepFlip, cenv.StepDrain, cenv.StepStop,
		} {
			if !hasStep(roll, want) {
				t.Errorf("no %q step in the rollout: %v", want, steps(roll))
			}
		}
		seen := map[int]bool{}
		for _, s := range roll.Steps {
			seen[s.Replica] = true
		}
		if len(seen) < 2 {
			t.Errorf("the rollout only mentions %v; two replicas were replaced", seen)
		}
	})

	t.Run("no request failed and the answer changed once", func(t *testing.T) {
		time.Sleep(itest.Scale(2 * time.Second))
		rep := load.Stop()
		t.Logf("load generator: %s", rep)
		if rep.Failed != 0 {
			t.Errorf("%d of %d requests failed during the rollout: %q", rep.Failed, rep.Total, rep.Errors)
		}
		if rep.Total < 50 {
			t.Errorf("only %d requests were made; the measurement is too thin to mean anything", rep.Total)
		}
		if got := rep.Sequence(); len(got) != 2 || got[0] != "v1" || got[1] != "v2" {
			t.Errorf("the answers went %v, want [v1 v2]: the build must switch at one point in time and never switch back", got)
		}
	})

	t.Run("a request in flight finishes on the replica that took it", func(t *testing.T) {
		select {
		case res := <-slow:
			if res.err != nil {
				t.Fatalf("the slow request failed after %s: %v", res.took.Round(time.Millisecond), res.err)
			}
			if res.status != http.StatusOK {
				t.Errorf("the slow request answered HTTP %d", res.status)
			}
			if res.version != "v1" {
				t.Errorf("the slow request answered version %q, want v1: it started on an old replica and must "+
					"finish there rather than be cut off", res.version)
			}
			t.Logf("the slow request took %s and was answered by %s", res.took.Round(time.Millisecond), res.version)
		case <-time.After(itest.Scale(time.Minute)):
			t.Error("the slow request never came back")
		}
	})

	t.Run("the websocket is closed at the drain deadline, not before", func(t *testing.T) {
		var flippedAt time.Time
		for _, s := range roll.Steps {
			if s.Step == cenv.StepFlip && s.Status == cenv.StepOK {
				flippedAt = s.At
				break
			}
		}
		select {
		case closed := <-wsClosed:
			if closed.err != nil {
				t.Logf("waiting for the websocket to close: %v", closed.err)
			}
			if flippedAt.IsZero() {
				t.Logf("no flip timestamp in the rollout; the websocket closed at %s",
					closed.at.Format(time.RFC3339))
				return
			}
			offset := m.ClockOffsetOrZero(t, ctx)
			lived := closed.at.Sub(flippedAt.Add(-offset))
			t.Logf("the websocket lived %s after the flip (drain is %s; the machine's clock is %s from this one)",
				lived.Round(time.Millisecond), drainWindow, offset.Round(time.Millisecond))
			if lived < drainWindow-itest.Scale(2*time.Second) {
				t.Errorf("the websocket was closed %s after the flip, before its %s drain deadline",
					lived.Round(time.Millisecond), drainWindow)
			}
		case <-time.After(itest.Scale(30 * time.Second)):
			t.Error("the websocket was still open 30s after the rollout finished; the drain deadline must close it")
		}
	})

	t.Run("every transition is an event", func(t *testing.T) {
		detail := showEnv(t, envX)
		for _, want := range []cenv.RolloutStepName{cenv.StepFlip, cenv.StepDrain, cenv.StepStop} {
			if !hasEvent(detail.Events, string(want)) {
				t.Errorf("no %q among the environment's events: a rollout has to be readable afterwards", want)
			}
		}
	})
}

func hasEvent(events []cenv.Event, want string) bool {
	for _, e := range events {
		if strings.Contains(e.Action, want) || strings.Contains(e.Detail, want) {
			return true
		}
	}
	return false
}

func TestAFailedRolloutChangesNothing(t *testing.T) {
	begin(t)
	needRepo(t)

	load := itest.StartLoad(itest.Load{
		Client: internet.Client(0),
		URL:    urlX + "/",
		Marker: versionOf,
	})
	if err := load.WaitForRequests(20, itest.Scale(time.Minute)); err != nil {
		load.Stop()
		t.Fatalf("the load never got going: %v", err)
	}

	copyAs(t, repo, "health.unhealthy.py", "health.py")
	itest.GitCommitAll(t, repo, commanderEnv(), "sampleapp: a health check that never passes")

	res := commander(t, commanderOpts{Dir: repo, Timeout: itest.Scale(6 * time.Minute)},
		"up", envX, "--json", "--timeout", "60s")
	if res.ExitCode != 1 {
		load.Stop()
		t.Fatalf("up with a failing health check: exit %d, want 1\nstdout:\n%sstderr:\n%s",
			res.ExitCode, res.Stdout, res.Stderr)
	}
	t.Logf("[up %s] progress:\n%s", envX, res.Stderr)

	rep := load.Stop()
	t.Logf("load generator: %s", rep)

	t.Run("the client saw nothing", func(t *testing.T) {
		if rep.Failed != 0 {
			t.Errorf("%d of %d requests failed while a rollout was failing: %q", rep.Failed, rep.Total, rep.Errors)
		}
		if got := rep.Sequence(); len(got) != 1 || got[0] != "v2" {
			t.Errorf("the answers went %v, want [v2] throughout: nothing was ever flipped in", got)
		}
	})

	t.Run("up says which replica and which step", func(t *testing.T) {
		if strings.TrimSpace(res.Stdout) == "" {
			t.Fatalf("up --json printed nothing on failure; a failed rollout has to be machine-readable\nstderr:\n%s", res.Stderr)
		}
		out := decode[struct {
			Rollouts []cenv.Rollout `json:"rollouts"`
		}](t, "up "+envX, res.Stdout)
		roll := rolloutOf(t, "up "+envX, out.Rollouts, webService)
		t.Logf("steps: %v", steps(roll))
		if roll.Failed == nil {
			t.Fatalf("the rollout reports no failed step although up exited 1")
		}
		if roll.Failed.Step != cenv.StepHealth {
			t.Errorf("the rollout failed at %q, want %q", roll.Failed.Step, cenv.StepHealth)
		}
		if roll.Failed.Replica == 0 {
			t.Errorf("the failed step names no replica: %+v", roll.Failed)
		}
		if hasStep(roll, cenv.StepDrain) {
			t.Errorf("a rollout that never got a healthy replica drained something: %v", steps(roll))
		}
	})

	t.Run("the old replicas are still serving", func(t *testing.T) {
		detail := showEnv(t, envX)
		web := serviceOf(t, "env show "+envX, detail.Services, webService)
		active := 0
		for _, r := range web.Replicas {
			if r.State == cenv.ReplicaActive {
				active++
			}
		}
		if active != 2 {
			t.Errorf("%d active replica(s) after a failed rollout, want the two that were serving: %+v",
				active, web.Replicas)
		}
		route := routeOf(t, "env show "+envX, detail.Routes, hostX)
		if got := len(route.Active()); got != 2 {
			t.Errorf("the route has %d active target(s), want 2: %+v", got, route.Targets)
		}
	})

	copyIn(t, repo, "health.py")
	itest.GitCommitAll(t, repo, commanderEnv(), "sampleapp: a health check that passes again")
	out := up(t, envX)
	if roll := rolloutOf(t, "up "+envX, out.Rollouts, webService); roll.Failed != nil {
		t.Fatalf("the recovery rollout failed at %+v", roll.Failed)
	}
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
	defer cancel()
	if _, err := internet.GetWithin(ctx, urlX+"/", itest.Scale(time.Minute)); err != nil {
		t.Fatalf("the environment does not serve again after the recovery: %v", err)
	}
}
