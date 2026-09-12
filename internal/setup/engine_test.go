package setup

import (
	"bytes"
	"context"
	"errors"
	"os/user"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup/testutil"
)

type fakeStep struct {
	name      string
	done      bool
	detail    string
	checkErr  error
	applyErr  error
	applied   int
	checks    int
	afterText string
}

func (f *fakeStep) Name() string { return f.name }

func (f *fakeStep) Check(ctx context.Context, env *Env) (bool, string, error) {
	f.checks++
	if f.applied > 0 && f.applyErr == nil {
		return true, f.afterText, nil
	}
	return f.done, f.detail, f.checkErr
}

func (f *fakeStep) Apply(ctx context.Context, env *Env) error {
	f.applied++
	return f.applyErr
}

func testEnv(t *testing.T, run *testutil.FakeRunner) (*Env, *bytes.Buffer) {
	t.Helper()
	log := &bytes.Buffer{}
	cfg := serverconfig.Default()
	return &Env{
		Config:     cfg,
		ConfigDir:  serverconfig.DefaultConfigDir,
		Opts:       Options{InstallPackages: true},
		Run:        run,
		Log:        log,
		Version:    "test",
		BinaryPath: "/tmp/caramelo",
	}, log
}

func noUser(t *testing.T) {
	t.Helper()
	prev := lookupUser
	lookupUser = func(string) (*user.User, error) { return nil, errors.New("no such user") }
	t.Cleanup(func() { lookupUser = prev })
}

func TestExecuteStatuses(t *testing.T) {
	noUser(t)
	steps := []Step{
		&fakeStep{name: "already", done: true, detail: "nothing to do"},
		&fakeStep{name: "work", detail: "missing", afterText: "created"},
		&fakeStep{name: "off", checkErr: Skip{Reason: "not asked for"}},
	}
	env, log := testEnv(t, testutil.New())
	var seen []Result
	rep := Execute(context.Background(), steps, env, func(r Result) { seen = append(seen, r) })

	if rep.Changed != 1 || rep.Failed != 0 {
		t.Fatalf("changed=%d failed=%d, want 1/0", rep.Changed, rep.Failed)
	}
	want := []Status{StatusOK, StatusChanged, StatusSkipped}
	for i, w := range want {
		if rep.Results[i].Status != w {
			t.Errorf("step %d status %q, want %q", i, rep.Results[i].Status, w)
		}
	}
	if len(seen) != 3 {
		t.Errorf("report callback called %d times, want 3", len(seen))
	}
	if got := rep.Results[1].Detail; got != "created" {
		t.Errorf("detail after apply = %q, want the detail of the second Check", got)
	}
	if got := rep.Results[2].Detail; got != "not asked for" {
		t.Errorf("skip detail = %q, want the skip reason", got)
	}
	for _, want := range []string{"[ok] already: nothing to do", "[changed] work: created", "[skipped] off: not asked for"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log %q does not contain %q", log.String(), want)
		}
	}
}

func TestExecuteStopsAtFirstFailure(t *testing.T) {
	noUser(t)
	boom := &fakeStep{name: "boom", applyErr: errors.New("kaboom")}
	later := &fakeStep{name: "later"}
	env, log := testEnv(t, testutil.New())
	rep := Execute(context.Background(), []Step{boom, later}, env, nil)

	if rep.Failed != 1 {
		t.Fatalf("failed=%d, want 1", rep.Failed)
	}
	if len(rep.Results) != 1 {
		t.Fatalf("got %d results, want only the failed step", len(rep.Results))
	}
	if rep.Results[0].Error != "kaboom" {
		t.Errorf("error = %q, want kaboom", rep.Results[0].Error)
	}
	if later.checks != 0 || later.applied != 0 {
		t.Errorf("step after a failure ran (checks=%d applies=%d)", later.checks, later.applied)
	}
	if !strings.Contains(log.String(), "[failed] boom: kaboom") {
		t.Errorf("log = %q, want the failure line", log.String())
	}
}

func TestExecuteCheckErrorFails(t *testing.T) {
	noUser(t)
	env, _ := testEnv(t, testutil.New())
	rep := Execute(context.Background(), []Step{&fakeStep{name: "x", checkErr: errors.New("cannot look")}}, env, nil)
	if rep.Failed != 1 || rep.Results[0].Status != StatusFailed {
		t.Fatalf("got %+v, want a failure", rep.Results)
	}
}

func TestExecuteDryRunChangesNothing(t *testing.T) {
	noUser(t)
	step := &fakeStep{name: "work", detail: "missing"}
	env, _ := testEnv(t, testutil.New())
	env.DryRun = true
	rep := Execute(context.Background(), []Step{step}, env, nil)

	if rep.Results[0].Status != StatusWouldChange {
		t.Errorf("status = %q, want %q", rep.Results[0].Status, StatusWouldChange)
	}
	if step.applied != 0 {
		t.Errorf("Apply ran %d times in a dry run", step.applied)
	}
	if rep.Changed != 0 || !rep.DryRun {
		t.Errorf("changed=%d dryRun=%v, want 0/true", rep.Changed, rep.DryRun)
	}
}

func TestExecuteWritesReport(t *testing.T) {
	prev := lookupUser
	lookupUser = func(name string) (*user.User, error) { return &user.User{Username: name}, nil }
	t.Cleanup(func() { lookupUser = prev })

	run := testutil.New()
	env, _ := testEnv(t, run)
	rep := Execute(context.Background(), []Step{&fakeStep{name: "ok", done: true}}, env, nil)

	path := "/var/lib/caramelo/setup/" + rep.RunID + ".json"
	call, found := run.Find("tee -- " + path)
	if !found {
		t.Fatalf("report was not written; commands were:\n%s", run.Transcript())
	}
	if call.Cmd.User != "caramelo" {
		t.Errorf("report written as %q, want the caramelo user", call.Cmd.User)
	}
	if !strings.Contains(call.Stdin, `"run_id"`) || !strings.Contains(call.Stdin, `"step": "ok"`) {
		t.Errorf("report content = %q", call.Stdin)
	}
	if !run.Ran("mkdir -p -- /var/lib/caramelo/setup") {
		t.Errorf("report directory was not created:\n%s", run.Transcript())
	}
	if !run.Ran("chmod 0640 -- " + path) {
		t.Errorf("report was not chmodded:\n%s", run.Transcript())
	}
}

func TestExecuteDryRunWritesNoReport(t *testing.T) {
	prev := lookupUser
	lookupUser = func(name string) (*user.User, error) { return &user.User{Username: name}, nil }
	t.Cleanup(func() { lookupUser = prev })

	run := testutil.New()
	env, _ := testEnv(t, run)
	env.DryRun = true
	Execute(context.Background(), []Step{&fakeStep{name: "ok", done: true}}, env, nil)
	if run.Ran("tee") {
		t.Errorf("a dry run wrote a report:\n%s", run.Transcript())
	}
}

func TestRunIDIsSortableAndFileSafe(t *testing.T) {
	a := NewRunID(time.Date(2026, 9, 8, 16, 32, 0, 0, time.UTC))
	b := NewRunID(time.Date(2026, 9, 8, 16, 32, 1, 0, time.UTC))
	if a >= b {
		t.Errorf("run ids not sortable: %q >= %q", a, b)
	}
	if strings.ContainsAny(a, "/ :") {
		t.Errorf("run id %q is not usable as a file name", a)
	}
}

func TestStepDurationIsRecorded(t *testing.T) {
	noUser(t)
	prevNow := now
	t.Cleanup(func() { now = prevNow })
	start := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	calls := 0
	now = func() time.Time {
		calls++
		return start.Add(time.Duration(calls) * time.Second)
	}
	env, _ := testEnv(t, testutil.New())
	rep := Execute(context.Background(), []Step{&fakeStep{name: "x", done: true}}, env, nil)
	if rep.Results[0].Duration <= 0 {
		t.Errorf("duration = %v, want a positive time", rep.Results[0].Duration)
	}
}
