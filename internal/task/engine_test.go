package task

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/setup/testutil"
)

func fakeEngine(f *testutil.FakeRunner) *Engine {
	return &Engine{Runner: f, Version: "0.0.1"}
}

func sh(script string) string {
	return "/bin/sh -c set -eu\n" + script
}

func runFile(t *testing.T, e *Engine, body string) Report {
	t.Helper()
	f, err := Parse([]byte(body), "x.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := e.Execute(context.Background(), f)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return rep
}

func resultOf(t *testing.T, rep Report, name string) Result {
	t.Helper()
	var found Result
	var walk func(rs []Result)
	walk = func(rs []Result) {
		for _, res := range rs {
			if res.Block {
				walk(res.Results)
				continue
			}
			if res.Name == name {
				found = res
			}
		}
	}
	walk(rep.Results)
	if found.Name == "" {
		t.Fatalf("no result called %q in %+v", name, rep.Results)
	}
	return found
}

const oneItem = `version: 1
name: x
items:
  - name: one
    check: check-one
    cmd: do-one
`

func TestACheckThatPassesLeavesTheItemAlone(t *testing.T) {
	f := testutil.New()
	f.Stdout(sh("check-one"), "already there\n")
	rep := runFile(t, fakeEngine(f), oneItem)
	res := resultOf(t, rep, "one")
	if res.Status != StatusOK || res.Detail != "already there" {
		t.Errorf("result = %+v, want ok with the check's first line", res)
	}
	if f.Ran(sh("do-one")) {
		t.Error("the command ran although the check passed")
	}
	if rep.OK != 1 || rep.Changed != 0 {
		t.Errorf("report = %+v, want one ok", rep)
	}
}

func TestACheckThatFailsRunsTheCommandAndChecksAgain(t *testing.T) {
	f := testutil.New()
	calls := 0
	f.Handler = func(c runner.Cmd) (runner.Result, error, bool) {
		line := testutil.Key(c)
		if strings.HasSuffix(line, "check-one") {
			calls++
			if calls == 1 {
				return runner.Result{ExitCode: 1}, nil, true
			}
			return runner.Result{}, nil, true
		}
		return runner.Result{Stdout: "wrote it\n"}, nil, true
	}
	rep := runFile(t, fakeEngine(f), oneItem)
	res := resultOf(t, rep, "one")
	if res.Status != StatusChanged || res.Detail != "wrote it" {
		t.Errorf("result = %+v, want changed with the command's first line", res)
	}
	if calls != 2 {
		t.Errorf("the check ran %d times, want it to decide and then confirm", calls)
	}
}

func TestACommandThatLeavesTheCheckFailingIsAFailure(t *testing.T) {
	f := testutil.New()
	f.Exit(sh("check-one"), 1)
	f.Respond(sh("do-one"), runner.Result{Stdout: "claimed to work\n"})
	rep := runFile(t, fakeEngine(f), oneItem)
	res := resultOf(t, rep, "one")
	if res.Status != StatusFailed {
		t.Fatalf("result = %+v, want failed", res)
	}
	if res.Error != "cmd ran but the check still fails" {
		t.Errorf("error = %q", res.Error)
	}
	if len(res.Commands) != 3 {
		t.Errorf("commands = %+v, want the check, the command and the check again", res.Commands)
	}
	if res.Commands[1].Stdout != "claimed to work\n" {
		t.Errorf("the command's output was not kept: %+v", res.Commands[1])
	}
}

func TestACommandThatExitsNonZeroIsAFailureWithItsOutput(t *testing.T) {
	f := testutil.New()
	f.Exit(sh("check-one"), 1)
	f.Respond(sh("do-one"), runner.Result{ExitCode: 2, Stderr: "no room left\n"})
	rep := runFile(t, fakeEngine(f), oneItem)
	res := resultOf(t, rep, "one")
	if res.Status != StatusFailed || res.Error != "cmd exit 2" {
		t.Errorf("result = %+v, want failed with the exit code", res)
	}
	if got := res.Commands[len(res.Commands)-1].Stderr; got != "no room left\n" {
		t.Errorf("stderr = %q, want the command's", got)
	}
}

func TestADryRunStopsAtTheCheck(t *testing.T) {
	f := testutil.New()
	f.Exit(sh("check-one"), 1)
	e := fakeEngine(f)
	e.DryRun = true
	rep := runFile(t, e, oneItem)
	if res := resultOf(t, rep, "one"); res.Status != StatusWouldChange {
		t.Errorf("result = %+v, want would-change", res)
	}
	if len(f.Calls()) != 1 {
		t.Errorf("a dry run ran %d commands, want only the check: %s", len(f.Calls()), f.Transcript())
	}
	if rep.WouldChange != 1 || rep.Changed != 0 {
		t.Errorf("report = %+v, want one would-change", rep)
	}
}

func TestWhenDecidesWhetherAnItemRunsAtAll(t *testing.T) {
	f := testutil.New()
	f.Respond(sh("test -d /nowhere"), runner.Result{ExitCode: 1, Stdout: "no kernel module\n"})
	rep := runFile(t, fakeEngine(f), `version: 1
name: x
items:
  - name: one
    when: test -d /nowhere
    check: check-one
    cmd: do-one
`)
	res := resultOf(t, rep, "one")
	if res.Status != StatusSkipped || res.Detail != "no kernel module" {
		t.Errorf("result = %+v, want skipped with the reason", res)
	}
	if f.Ran(sh("check-one")) || f.Ran(sh("do-one")) {
		t.Errorf("a skipped item ran something: %s", f.Transcript())
	}
}

func TestPlatformsSkipAnItemThisMachineIsNot(t *testing.T) {
	other := "linux"
	if runtime.GOOS == "linux" {
		other = "darwin"
	}
	f := testutil.New()
	rep := runFile(t, fakeEngine(f), `version: 1
name: x
items:
  - name: one
    platforms: [`+other+`]
    check: check-one
    cmd: do-one
`)
	res := resultOf(t, rep, "one")
	if res.Status != StatusSkipped || !strings.Contains(res.Detail, runtime.GOOS) {
		t.Errorf("result = %+v, want skipped naming this machine", res)
	}
	if len(f.Calls()) != 0 {
		t.Errorf("a skipped item ran something: %s", f.Transcript())
	}
}

const twoBlocks = `version: 1
name: x
items:
  - parallel:
      desc: together
      items:
        - name: a
          cmd: do-a
        - name: b
          cmd: do-b
        - name: c
          cmd: do-c
  - name: after
    cmd: do-after
`

func TestAParallelBlockStartsEveryItemBeforeAnyFinishes(t *testing.T) {
	f := testutil.New()
	var mu sync.Mutex
	arrived := 0
	gate := make(chan struct{})
	f.Handler = func(c runner.Cmd) (runner.Result, error, bool) {
		if !strings.Contains(testutil.Key(c), "do-after") {
			mu.Lock()
			arrived++
			if arrived == 3 {
				close(gate)
			}
			mu.Unlock()
			select {
			case <-gate:
			case <-time.After(10 * time.Second):
				return runner.Result{}, errors.New("the block did not start its items together"), true
			}
		}
		return runner.Result{}, nil, true
	}
	rep := runFile(t, fakeEngine(f), twoBlocks)
	if rep.Changed != 4 {
		t.Errorf("report = %+v, want four changed", rep)
	}
	lines := f.Lines()
	if len(lines) != 4 || !strings.Contains(lines[3], "do-after") {
		t.Errorf("the item after the block did not wait for it: %v", lines)
	}
}

func TestSerialItemsRunOneAtATimeInFileOrder(t *testing.T) {
	f := testutil.New()
	rep := runFile(t, fakeEngine(f), `version: 1
name: x
items:
  - name: one
    cmd: do-one
  - name: two
    cmd: do-two
  - name: three
    cmd: do-three
`)
	want := []string{"do-one", "do-two", "do-three"}
	for i, line := range f.Lines() {
		if !strings.HasSuffix(line, want[i]) {
			t.Errorf("call %d = %q, want %q", i, line, want[i])
		}
	}
	for i, res := range rep.Results {
		if !strings.HasSuffix(want[i], res.Name) && res.Name != []string{"one", "two", "three"}[i] {
			t.Errorf("result %d = %q", i, res.Name)
		}
	}
}

func TestAFailureLetsItsBlockFinishAndStopsTheRun(t *testing.T) {
	f := testutil.New()
	f.Exit(sh("do-b"), 1)
	rep := runFile(t, fakeEngine(f), twoBlocks)
	if res := resultOf(t, rep, "b"); res.Status != StatusFailed {
		t.Errorf("b = %+v, want failed", res)
	}
	for _, name := range []string{"a", "c"} {
		if res := resultOf(t, rep, name); res.Status != StatusChanged {
			t.Errorf("%s = %+v, want it to have finished beside the failure", name, res)
		}
	}
	if res := resultOf(t, rep, "after"); res.Status != StatusNotRun {
		t.Errorf("after = %+v, want not run", res)
	}
	if f.Ran(sh("do-after")) {
		t.Error("the run went on after a failure")
	}
	if rep.Failed != 1 || rep.NotRun != 1 || rep.Changed != 2 {
		t.Errorf("report = %+v", rep)
	}
}

func TestAFailureOfTheFirstItemOfABlockStillLetsItsSiblingsFinish(t *testing.T) {
	f := testutil.New()
	f.Exit(sh("do-a"), 1)
	rep := runFile(t, fakeEngine(f), twoBlocks)
	if res := resultOf(t, rep, "a"); res.Status != StatusFailed {
		t.Errorf("a = %+v, want failed", res)
	}
	for _, name := range []string{"b", "c"} {
		if res := resultOf(t, rep, name); res.Status != StatusChanged {
			t.Errorf("%s = %+v, want it to have finished beside the failure", name, res)
		}
	}
	if res := resultOf(t, rep, "after"); res.Status != StatusNotRun {
		t.Errorf("after = %+v, want not run", res)
	}
	if rep.Failed != 1 || rep.NotRun != 1 || rep.Changed != 2 {
		t.Errorf("report = %+v", rep)
	}
}

func TestAFailingSerialItemRunsNothingAfterIt(t *testing.T) {
	f := testutil.New()
	f.Exit(sh("do-one"), 1)
	rep := runFile(t, fakeEngine(f), `version: 1
name: x
items:
  - name: one
    cmd: do-one
  - name: two
    cmd: do-two
  - parallel:
      items:
        - name: three
          cmd: do-three
`)
	for _, name := range []string{"two", "three"} {
		if res := resultOf(t, rep, name); res.Status != StatusNotRun {
			t.Errorf("%s = %+v, want not run", name, res)
		}
	}
	if len(f.Calls()) != 1 {
		t.Errorf("the run went on after a failure: %s", f.Transcript())
	}
}

func TestABlockNestsInABlock(t *testing.T) {
	f := testutil.New()
	rep := runFile(t, fakeEngine(f), `version: 1
name: x
items:
  - parallel:
      desc: outer
      items:
        - name: a
          cmd: do-a
        - parallel:
            desc: inner
            items:
              - name: b
                cmd: do-b
              - name: c
                cmd: do-c
`)
	if rep.Changed != 3 {
		t.Errorf("report = %+v, want three changed", rep)
	}
	outer := rep.Results[0]
	if !outer.Block || len(outer.Results) != 2 || !outer.Results[1].Block {
		t.Fatalf("results = %+v, want a block inside a block", rep.Results)
	}
	if len(outer.Results[1].Results) != 2 {
		t.Errorf("the inner block holds %+v", outer.Results[1].Results)
	}
}

func TestTheReportIsInFileOrderWhileTheEventsAreInCompletionOrder(t *testing.T) {
	f := testutil.New()
	release := make(chan struct{})
	f.Handler = func(c runner.Cmd) (runner.Result, error, bool) {
		if strings.Contains(testutil.Key(c), "do-a") {
			<-release
		}
		return runner.Result{}, nil, true
	}
	var mu sync.Mutex
	var order []string
	e := fakeEngine(f)
	e.Observer = func(res Result) {
		mu.Lock()
		order = append(order, res.Name)
		if len(order) == 2 {
			close(release)
		}
		mu.Unlock()
	}
	rep := runFile(t, e, `version: 1
name: x
items:
  - parallel:
      items:
        - name: a
          cmd: do-a
        - name: b
          cmd: do-b
        - name: c
          cmd: do-c
`)
	names := []string{}
	for _, res := range rep.Results[0].Results {
		names = append(names, res.Name)
	}
	if strings.Join(names, ",") != "a,b,c" {
		t.Errorf("the report is not in file order: %v", names)
	}
	mu.Lock()
	defer mu.Unlock()
	if order[len(order)-1] != "a" {
		t.Errorf("the events are not in completion order: %v", order)
	}
}

func TestATimeoutKillsAHungCommand(t *testing.T) {
	e := &Engine{Runner: runner.Exec{}, Version: "0.0.1"}
	start := time.Now()
	rep := runFile(t, e, `version: 1
name: x
items:
  - name: one
    timeout: 200ms
    cmd: sleep 30 & wait
`)
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("the run took %v, want the timeout to stop it", d)
	}
	res := resultOf(t, rep, "one")
	if res.Status != StatusFailed || !strings.Contains(res.Error, "200ms") {
		t.Errorf("result = %+v, want failed naming the timeout", res)
	}
}

func TestACancelledRunStillWritesItsReport(t *testing.T) {
	dir := t.TempDir()
	e := &Engine{Runner: runner.Exec{}, Version: "0.0.1", Report: ReportStore{Dir: dir}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	f, err := Parse([]byte(`version: 1
name: x
items:
  - name: one
    cmd: sleep 30
`), "x.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := e.Execute(ctx, f)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Failed != 1 {
		t.Errorf("report = %+v, want the cancelled item failed", rep)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("the report of a cancelled run: %v %v", entries, err)
	}
}

func TestAReportThatCannotBeWrittenKeepsTheRunItRecords(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f := testutil.New()
	e := fakeEngine(f)
	e.Report = ReportStore{Dir: filepath.Join(blocked, "runs")}
	rep := runFile(t, e, `version: 1
name: x
items:
  - name: one
    cmd: do-one
`)
	if rep.Changed != 1 || rep.Results[0].Status != StatusChanged {
		t.Errorf("report = %+v, want the run it really did", rep)
	}
	if rep.ReportErr == nil || rep.Path != "" {
		t.Errorf("report = %+v, want the write error carried on the report", rep)
	}
}

func TestADryRunSaysWouldChangeOnTheWire(t *testing.T) {
	f := testutil.New()
	f.Exit(sh("check-one"), 1)
	var events bytes.Buffer
	e := fakeEngine(f)
	e.DryRun = true
	e.Events = progress.New(&events, progress.FormatJSON)
	runFile(t, e, oneItem)
	terminal := 0
	for _, line := range strings.Split(strings.TrimSpace(events.String()), "\n") {
		var ev progress.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("%v: %q", err, line)
		}
		if ev.Action != progress.ActionTask || ev.Task != "x" {
			t.Errorf("event = %+v, want a task event naming the task", ev)
		}
		if ev.Status == progress.StatusStarted {
			continue
		}
		terminal++
		if ev.Status != StatusWouldChange {
			t.Errorf("event = %+v, want %q", ev, StatusWouldChange)
		}
	}
	if terminal != 1 {
		t.Errorf("the wire carried %d terminal events, want one", terminal)
	}
}

func TestTheRunnerHandsAnObserverOneResultAtATime(t *testing.T) {
	f := testutil.New()
	e := fakeEngine(f)
	seen := map[string]string{}
	e.Observer = func(res Result) { seen[res.Name] = res.Status }
	rep := runFile(t, e, `version: 1
name: x
items:
  - parallel:
      items:
        - name: a
          cmd: do-a
        - name: b
          cmd: do-b
        - name: c
          cmd: do-c
        - name: d
          cmd: do-d
`)
	if rep.Changed != 4 {
		t.Fatalf("report = %+v", rep)
	}
	if len(seen) != 4 {
		t.Errorf("the observer saw %v, want one result per item", seen)
	}
}

func TestTheRendererFollowsAParallelRunWithoutRacing(t *testing.T) {
	f := testutil.New()
	e := fakeEngine(f)
	var b strings.Builder
	r := &Renderer{W: &b}
	if err := r.Start(Plan{Task: "x", Items: []PlanItem{
		{Name: blockName, Desc: "together", Block: true, Items: []PlanItem{
			{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"},
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	e.Observer = func(res Result) { _ = r.Result(res) }
	rep := runFile(t, e, `version: 1
name: x
items:
  - parallel:
      desc: together
      items:
        - name: a
          cmd: do-a
        - name: b
          cmd: do-b
        - name: c
          cmd: do-c
        - name: d
          cmd: do-d
`)
	if err := r.Finish(rep); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c", "d"} {
		if !strings.Contains(b.String(), "  "+name+" ") {
			t.Errorf("%s is missing from the rendering:\n%s", name, b.String())
		}
	}
	if n := strings.Count(b.String(), "together"); n != 1 {
		t.Errorf("the block header was printed %d times:\n%s", n, b.String())
	}
}
