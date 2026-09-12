package env

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/state"
)

func TestUpIsRefusedOnAReleaseEnv(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Release: true})

	_, err := h.m.Up(context.Background(), UpRequest{App: "shop", Name: "production"}, nil)
	if err == nil {
		t.Fatal("up on a release environment was allowed")
	}
	for _, want := range []string{"production", "releases", "caramelo deploy production"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not mention %q", err, want)
		}
	}

	if IsNotImplemented(err) {
		t.Error("the refusal reads as a missing implementation")
	}
}

func TestDeployIsRefusedOnADevEnv(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	_, err := h.m.Deploy(context.Background(), DeployRequest{App: "shop", Env: "feat-x"}, nil)
	if err == nil {
		t.Fatal("deploy on a dev environment was allowed")
	}
	for _, want := range []string{"feat-x", "development environment", "--release"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not mention %q", err, want)
		}
	}
	if IsNotImplemented(err) {
		t.Error("the refusal reads as a missing implementation")
	}
}

func TestCreateRecordsTheMode(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		req       CreateRequest
		mode      Mode
		protected bool
	}{
		{CreateRequest{App: "shop", Name: "feat-x"}, ModeDev, false},
		{CreateRequest{App: "shop", Name: "pr-41", Protected: true}, ModeDev, true},
		{CreateRequest{App: "shop", Name: "staging", Release: true}, ModeRelease, false},

		{CreateRequest{App: "shop", Name: "production", Production: true}, ModeRelease, true},
	} {
		e := h.mustCreate(tc.req)
		if e.Mode != tc.mode {
			t.Errorf("%s: mode = %q, want %q", tc.req.Name, e.Mode, tc.mode)
		}
		if e.Protected != tc.protected {
			t.Errorf("%s: protected = %v, want %v", tc.req.Name, e.Protected, tc.protected)
		}

		rec, err := h.store.Env(context.Background(), "shop", tc.req.Name)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Mode != string(tc.mode) || rec.Protected != tc.protected {
			t.Errorf("%s: row = mode %q protected %v", tc.req.Name, rec.Mode, rec.Protected)
		}
	}
}

func TestModeIsAlwaysReported(t *testing.T) {
	e, err := recordToEnv(&state.EnvRecord{App: "shop", Name: "feat-x", Mode: ""})
	if err != nil {
		t.Fatal(err)
	}
	if e.Mode != ModeDev {
		t.Errorf("an environment with no mode reports %q, want %q", e.Mode, ModeDev)
	}

	if _, err := recordToEnv(&state.EnvRecord{App: "shop", Name: "x", Mode: "canary"}); err == nil {
		t.Error("an unknown mode was accepted")
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeDev, "dev": ModeDev, "release": ModeRelease} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseMode("production"); err == nil {
		t.Error("ParseMode(\"production\") was accepted: production is a name, not a mode")
	}
	if !ModeRelease.IsRelease() || ModeDev.IsRelease() {
		t.Error("IsRelease is wrong")
	}
	if Mode("").String() != "dev" {
		t.Error("the empty mode does not print as dev")
	}
}

func TestDeployStatusesMatchTheStore(t *testing.T) {
	if len(DeployStatuses) != len(state.DeployStatuses) {
		t.Fatalf("%d statuses here, %d in the store", len(DeployStatuses), len(state.DeployStatuses))
	}
	for i, s := range DeployStatuses {
		if string(s) != state.DeployStatuses[i] {
			t.Errorf("status %d: %q here, %q in the store", i, s, state.DeployStatuses[i])
		}
	}
	for _, s := range []DeployStatus{DeployBuilding, DeployMigrating, DeployStarting, DeployChecking, DeployWatching} {
		if s.Done() {
			t.Errorf("%s reports done", s)
		}
	}
	for _, s := range []DeployStatus{DeployPromoted, DeployRolledBack, DeployFailed} {
		if !s.Done() {
			t.Errorf("%s does not report done", s)
		}
	}

	for _, s := range []DeployStatus{DeployBuilding, DeployMigrating, DeployStarting, DeployChecking, DeployFailed} {
		if s.Flipped() {
			t.Errorf("%s reports flipped", s)
		}
	}
	for _, s := range []DeployStatus{DeployWatching, DeployPromoted, DeployRolledBack} {
		if !s.Flipped() {
			t.Errorf("%s does not report flipped", s)
		}
	}
}

func TestDeployWatchBreached(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    DeployWatch
		want bool
	}{
		{"clean", DeployWatch{Requests: 100, Errors: 0, Rate: 0, MaxRate: 0.05, MinRequests: 20}, false},
		{"over", DeployWatch{Requests: 100, Errors: 10, Rate: 0.10, MaxRate: 0.05, MinRequests: 20}, true},
		{"exactly at the limit is not over it",
			DeployWatch{Requests: 100, Errors: 5, Rate: 0.05, MaxRate: 0.05, MinRequests: 20}, false},
		{"below the floor", DeployWatch{Requests: 3, Errors: 3, Rate: 1, MaxRate: 0.05, MinRequests: 20}, false},

		{"0% is breached by one error",
			DeployWatch{Requests: 100, Errors: 1, Rate: 0.01, MaxRate: 0, MinRequests: 20}, true},
		{"0% with no errors is not breached",
			DeployWatch{Requests: 100, Errors: 0, Rate: 0, MaxRate: 0, MinRequests: 20}, false},

		{"none", DeployWatch{Requests: 100, Errors: 100, Rate: 1,
			MaxRate: config.NoMaxErrors, MinRequests: 20}, false},
	} {
		if got := tc.w.Breached(); got != tc.want {
			t.Errorf("%s: Breached = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDeployNeedsAnEdge(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	ctx := context.Background()

	for name, err := range map[string]error{
		"deploy":   errDeployOf(h.m.Deploy(ctx, DeployRequest{App: "shop", Env: "production"}, nil)),
		"promote":  errDeployOf(h.m.Promote(ctx, PromoteRequest{App: "shop", Env: "production"}, nil)),
		"rollback": errDeployOf(h.m.Rollback(ctx, RollbackRequest{App: "shop", Env: "production"}, nil)),
	} {
		if err == nil || !strings.Contains(err.Error(), "edge enable") {
			t.Errorf("%s = %v, want it to name how to get an edge", name, err)
		}
	}

	_, err := h.m.Deploy(ctx, DeployRequest{App: "shop", Env: "Not A Slug"}, nil)
	if err == nil || strings.Contains(err.Error(), "edge") {
		t.Errorf("an invalid env name = %v, want a validation error", err)
	}
}

func errDeployOf(_ *Deploy, err error) error { return err }

func TestHealthLoopDefaults(t *testing.T) {
	l := NewHealthLoop(newHarness(t).m)
	if l.interval() != DefaultHealthInterval || l.failures() != DefaultHealthFailures {
		t.Errorf("defaults = %s, %d", l.interval(), l.failures())
	}
	l.Interval, l.Failures = time.Second, 5
	if l.interval() != time.Second || l.failures() != 5 {
		t.Error("the overrides do not take")
	}

	empty := &HealthLoop{}
	if err := empty.Once(context.Background()); err == nil {
		t.Error("a loop with no manager probed something")
	}
	if err := empty.Run(context.Background()); err == nil {
		t.Error("a loop with no manager ran")
	}
}

func TestManagerTakesABuilder(t *testing.T) {
	h := newHarness(t)
	h.m.Builder = release.NotImplemented{}
	if _, err := h.m.Builder.Build(context.Background(), release.BuildRequest{App: "shop", Env: "production"}, nil); err == nil {
		t.Error("the stub builder built something")
	}
}

func TestImagePrefixesAgree(t *testing.T) {
	if release.NamePrefix != NamePrefix {
		t.Errorf("release.NamePrefix = %q, env.NamePrefix = %q", release.NamePrefix, NamePrefix)
	}
}

func TestDeployEventShape(t *testing.T) {
	d := &Deploy{App: "shop", Env: "production", Identity: "alex@laptop"}
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	e := deployEvent(d, DeployStep{Step: StepCheck, Status: StepFailed, Detail: "exit 1", At: at})
	if e.Action != "deploy" || e.Step != "check" || e.Status != "failed" {
		t.Errorf("event = %+v", e)
	}
	if e.App != "shop" || e.Env != "production" || e.Identity != "alex@laptop" || !e.At.Equal(at) {
		t.Errorf("event = %+v", e)
	}

	if got := deployEvent(d, DeployStep{Step: StepReplicas, Service: "web"}); got.Service != "web" {
		t.Errorf("event = %+v", got)
	}
	if e.Service != "" {
		t.Errorf("a check event names service %q", e.Service)
	}

	if got := deployEvent(nil, DeployStep{Step: StepBuild}); got.Action != "deploy" {
		t.Errorf("event = %+v", got)
	}
}

func TestDestroyOfAProtectedEnvNeedsForce(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	ctx := context.Background()

	err := h.m.Destroy(ctx, DestroyRequest{App: "shop", Name: "production"}, nil)
	if err == nil {
		t.Fatal("a protected environment was destroyed without --force")
	}
	for _, want := range []string{"production", "protected", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not mention %q", err, want)
		}
	}

	if _, err := h.store.Env(ctx, "shop", "production"); err != nil {
		t.Fatalf("the refusal destroyed something anyway: %v", err)
	}

	if err := h.m.Destroy(ctx, DestroyRequest{App: "shop", Name: "production", Force: true}, nil); err != nil {
		t.Fatalf("--force did not destroy it: %v", err)
	}

	if err := h.m.Destroy(ctx, DestroyRequest{App: "shop", Name: "production"}, nil); err != nil {
		t.Errorf("destroying it twice: %v", err)
	}
}

func TestDestroyOfAnUnprotectedEnvNeedsNothing(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	if err := h.m.Destroy(context.Background(), DestroyRequest{App: "shop", Name: "feat-x"}, nil); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
}

func TestDownIsRefusedOnAProtectedEnv(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Production: true})
	ctx := context.Background()

	_, err := h.m.Down(ctx, DownRequest{App: "shop", Name: "production"}, nil)
	if err == nil {
		t.Fatal("down on a protected environment was allowed")
	}
	for _, want := range []string{"production", "protected", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not mention %q", err, want)
		}
	}
	if _, err := h.m.Down(ctx, DownRequest{App: "shop", Name: "production", Force: true}, nil); err != nil {
		t.Errorf("down --force = %v", err)
	}

	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	if _, err := h.m.Down(ctx, DownRequest{App: "shop", Name: "feat-x"}, nil); err != nil {
		t.Errorf("down on a dev environment = %v", err)
	}
}
