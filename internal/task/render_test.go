package task

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/progress"
)

var update = flag.Bool("update", false, "rewrite the golden panes in testdata")

const goldenDir = "testdata"

const (
	fixtureHome  = "/home/alex"
	fixtureDir   = fixtureHome + "/.config/caramelo"
	fixtureCache = fixtureHome + "/.cache/caramelo"
)

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(goldenDir, name+".txt")
	if *update {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run `go test ./internal/task -update` to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s moved.\n--- got ---\n%s\n--- want ---\n%s", path, got, string(want))
	}
}

func commanderPlan() Plan {
	return Plan{
		Task: "commander-setup",
		Desc: "Name this machine a commander and write the files it needs",
		Items: []PlanItem{
			{Name: blockName, Desc: "the config, vpn and cache directories, private to the user", Block: true, Items: []PlanItem{
				{Name: "config-dir"},
				{Name: "vpn-dir"},
				{Name: "cache-dir"},
			}},
			{Name: "config"},
			{Name: "identity"},
		},
	}
}

func commanderReport(dryRun bool, results ...Result) Report {
	rep := Report{
		RunID:   "20260918T203311.412Z",
		Task:    "commander-setup",
		Version: "0.0.1",
		DryRun:  dryRun,
		Results: []Result{
			{Name: blockName, Desc: "the config, vpn and cache directories, private to the user", Block: true,
				Status: blockStatus(results[:3]), Results: []Result{results[0], results[1], results[2]}},
			results[3],
			results[4],
		},
	}
	rep.count()
	return rep
}

func blockStatus(results []Result) string {
	for _, res := range results {
		if res.Status == StatusFailed {
			return StatusFailed
		}
	}
	return StatusOK
}

func firstRunResults() []Result {
	return []Result{
		{Name: "config-dir", Status: StatusChanged, Detail: "created, 0700"},
		{Name: "vpn-dir", Status: StatusChanged, Detail: "created, 0700"},
		{Name: "cache-dir", Status: StatusChanged, Detail: "created, 0700"},
		{Name: "config", Status: StatusChanged, Detail: fixtureDir + "/config.yaml written"},
		{Name: "identity", Status: StatusChanged, Detail: fixtureDir + "/identity.key created"},
	}
}

func secondRunResults() []Result {
	return []Result{
		{Name: "config-dir", Status: StatusOK},
		{Name: "vpn-dir", Status: StatusOK},
		{Name: "cache-dir", Status: StatusOK},
		{Name: "config", Status: StatusOK, Detail: "name laptop, role commander"},
		{Name: "identity", Status: StatusOK, Detail: fixtureDir + "/identity.key"},
	}
}

const permissionDenied = "install: cannot create directory '" + fixtureCache + "': Permission denied"

const permissionDeniedParent = "install: (parent " + fixtureHome + "/.cache is owned by root, mode 0755)"

func failureResults() []Result {
	return []Result{
		{Name: "config-dir", Status: StatusOK, Commands: []Command{
			{Kind: KindCheck, Script: `test -d "` + fixtureDir + `" && test "$(stat -c %a ... )" = 700`, DurationMS: 2},
		}},
		{Name: "vpn-dir", Status: StatusOK, Commands: []Command{
			{Kind: KindCheck, Script: `test -d "` + fixtureDir + `/vpn"`, DurationMS: 1},
		}},
		{Name: "cache-dir", Status: StatusFailed, Error: "cmd exit 1", Commands: []Command{
			{Kind: KindCheck, Script: `test -d "` + fixtureCache + `"`, Exit: 1, DurationMS: 1},
			{Kind: KindCmd, Script: `install -d -m 0700 "` + fixtureCache + `"`, Exit: 1, DurationMS: 6,
				Stderr: permissionDenied + "\n" + permissionDeniedParent + "\n"},
		}},
		{Name: "config", Status: StatusNotRun},
		{Name: "identity", Status: StatusNotRun},
	}
}

func dryRunResults() []Result {
	return []Result{
		{Name: "config-dir", Status: StatusWouldChange},
		{Name: "vpn-dir", Status: StatusWouldChange},
		{Name: "cache-dir", Status: StatusOK},
		{Name: "config", Status: StatusWouldChange, Detail: fixtureDir + "/config.yaml missing"},
		{Name: "identity", Status: StatusWouldChange, Detail: fixtureDir + "/identity.key missing"},
	}
}

func fixtureVars() []Var {
	return []Var{
		{Name: "name", Value: "laptop"},
		{Name: "dir", Value: fixtureDir},
		{Name: VarCacheDir, Value: fixtureHome + "/.cache"},
		{Name: VarOS, Value: "linux"},
	}
}

type pane struct {
	name    string
	plan    Plan
	order   []int
	results []Result
	report  Report
	debug   bool
	fleet   bool
	reports []Report
}

func commanderPane(name string, results []Result, order []int, dryRun bool, ms int64) pane {
	rep := commanderReport(dryRun, results...)
	rep.DurationMS = ms
	plan := commanderPlan()
	plan.DryRun = dryRun
	return pane{name: name, plan: plan, order: order, results: results, report: rep}
}

func panes() []pane {
	first := commanderPane("first-run", firstRunResults(), []int{0, 2, 1, 3, 4}, false, 91)
	second := commanderPane("second-run", secondRunResults(), []int{0, 1, 2, 3, 4}, false, 44)
	failure := commanderPane("failure", failureResults(), []int{0, 1, 2}, false, 50)
	dry := commanderPane("dry-run", dryRunResults(), []int{0, 1, 2, 3, 4}, true, 30)

	debug := commanderPane("debug", failureResults(), []int{0, 1, 2}, false, 50)
	debug.debug = true
	debug.plan.Vars = fixtureVars()
	debug.report.Path = fixtureCache + "/runs/20260918T203311.412Z.json"

	return []pane{first, second, failure, debug, dry, fleetPane()}
}

func fleetPane() pane {
	rows := []struct {
		machine string
		res     Result
	}{
		{"m1", Result{Name: "module", Status: StatusOK, Detail: "wireguard 6.12.30"}},
		{"m2", Result{Name: "module", Status: StatusOK, Detail: "wireguard 6.12.30"}},
		{"m3", Result{Name: "module", Status: StatusSkipped, Detail: "no kernel module; staying in userspace"}},
		{"m1", Result{Name: "interface", Status: StatusChanged, Detail: "caramelo0 up, 10.86.0.1/16"}},
		{"m2", Result{Name: "interface", Status: StatusChanged, Detail: "caramelo0 up, 10.87.0.1/16"}},
		{"m1", Result{Name: "unit", Status: StatusChanged, Detail: "caramelo-tunnel.service enabled"}},
		{"m2", Result{Name: "unit", Status: StatusChanged, Detail: "caramelo-tunnel.service enabled"}},
		{"m1", Result{Name: "caramelod", Status: StatusChanged, Detail: "restarted, vpn_mode kernel"}},
		{"m2", Result{Name: "caramelod", Status: StatusChanged, Detail: "restarted, vpn_mode kernel"}},
	}
	p := pane{
		name:  "fleet",
		fleet: true,
		plan: Plan{
			Task: "tunnel-kernel",
			Desc: "Move the tunnel from caramelod to the kernel",
			On:   []string{"m1", "m2", "m3"},
		},
	}
	for _, row := range rows {
		p.results = append(p.results, row.res)
		p.order = append(p.order, len(p.order))
		p.plan.Items = append(p.plan.Items, PlanItem{Name: row.res.Name})
		p.reports = append(p.reports, Report{Machine: row.machine})
	}
	machines := []struct {
		name                 string
		ok, changed, skipped int
		ms                   int64
	}{
		{"m1", 1, 3, 0, 4200},
		{"m2", 1, 3, 0, 4600},
		{"m3", 0, 0, 1, 300},
	}
	p.reports = nil
	for _, m := range machines {
		p.reports = append(p.reports, Report{
			RunID: "20260918T203311.412Z", Task: "tunnel-kernel", Machine: m.name, Version: "0.0.1",
			OK: m.ok, Changed: m.changed, Skipped: m.skipped, DurationMS: m.ms,
		})
	}
	p.plan.Items = nil
	return p
}

func fleetEvents() []progress.Event {
	at := time.Date(2026, 9, 18, 20, 33, 11, 412_000_000, time.UTC)
	var out []progress.Event
	for i, row := range []struct {
		machine string
		res     Result
	}{
		{"m1", Result{Name: "module", Status: StatusOK, Detail: "wireguard 6.12.30"}},
		{"m2", Result{Name: "module", Status: StatusOK, Detail: "wireguard 6.12.30"}},
		{"m3", Result{Name: "module", Status: StatusSkipped, Detail: "no kernel module; staying in userspace"}},
		{"m1", Result{Name: "interface", Status: StatusChanged, Detail: "caramelo0 up, 10.86.0.1/16"}},
		{"m2", Result{Name: "interface", Status: StatusChanged, Detail: "caramelo0 up, 10.87.0.1/16"}},
		{"m1", Result{Name: "unit", Status: StatusChanged, Detail: "caramelo-tunnel.service enabled"}},
		{"m2", Result{Name: "unit", Status: StatusChanged, Detail: "caramelo-tunnel.service enabled"}},
		{"m1", Result{Name: "caramelod", Status: StatusChanged, Detail: "restarted, vpn_mode kernel"}},
		{"m2", Result{Name: "caramelod", Status: StatusChanged, Detail: "restarted, vpn_mode kernel"}},
	} {
		out = append(out, progress.Event{
			Action: progress.ActionTask, Task: "tunnel-kernel", Machine: row.machine,
			Step: row.res.Name, Status: row.res.Status, Detail: row.res.Detail,
			At: at.Add(time.Duration(i) * time.Millisecond),
		})
	}
	return out
}

func renderPane(t *testing.T, p pane, colour bool) string {
	t.Helper()
	var b bytes.Buffer
	r := &Renderer{W: &b, Colour: colour, Debug: p.debug, Fleet: p.fleet}
	if err := r.Start(p.plan); err != nil {
		t.Fatal(err)
	}
	if p.fleet {
		for _, e := range fleetEvents() {
			if err := r.Event(e); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.Finish(p.reports...); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	for _, i := range p.order {
		if err := r.Result(p.results[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Finish(p.report); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestGoldenPanes(t *testing.T) {
	for _, p := range panes() {
		t.Run(p.name, func(t *testing.T) {
			checkGolden(t, p.name, renderPane(t, p, false))
		})
		t.Run(p.name+"-ansi", func(t *testing.T) {
			checkGolden(t, p.name+".ansi", renderPane(t, p, true))
		})
	}
}

func jsonPane(dryRun bool) string {
	results := firstRunResults()
	if dryRun {
		results = dryRunResults()
	}
	rep := commanderReport(dryRun, results...)
	rep.DurationMS = 91
	at := time.Date(2026, 9, 18, 20, 33, 11, 412_000_000, time.UTC)
	var b bytes.Buffer
	w := progress.New(&b, progress.FormatJSON)
	order := []int{0, 2, 1, 3, 4}
	starts := []int64{0, 1, 2, 23, 26}
	ends := []int64{7, 9, 11, 25, 77}
	for n, i := range order {
		res := results[i]
		_ = w.Emit(progress.Event{Action: progress.ActionTask, Task: rep.Task, Step: res.Name,
			Status: progress.StatusStarted, At: at.Add(time.Duration(starts[n]) * time.Millisecond)})
		_ = w.Emit(progress.Event{Action: progress.ActionTask, Task: rep.Task, Step: res.Name,
			Status: res.Status, Detail: res.Detail, At: at.Add(time.Duration(ends[n]) * time.Millisecond)})
	}
	line, err := json.Marshal(rep)
	if err != nil {
		panic(err)
	}
	b.Write(line)
	b.WriteString("\n")
	return b.String()
}

func TestGoldenJSONPane(t *testing.T) {
	checkGolden(t, "json", jsonPane(false))
	checkGolden(t, "json.ansi", jsonPane(false))
	if !strings.Contains(jsonPane(true), `"status":"would-change"`) {
		t.Error("a dry-run event does not carry the would-change status word")
	}
}
