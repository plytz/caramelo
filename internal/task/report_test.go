package task

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/setup/testutil"
)

func TestARunIDReadsAsAnInstant(t *testing.T) {
	at := time.Date(2026, 9, 18, 20, 33, 11, 412_000_000, time.UTC)
	if got := NewRunID(at); got != "20260918T203311.412Z" {
		t.Errorf("run id = %q", got)
	}
}

func TestTheCountsComeFromTheLeavesOnly(t *testing.T) {
	rep := Report{Results: []Result{
		{Name: blockName, Block: true, Status: StatusFailed, Results: []Result{
			{Name: "a", Status: StatusOK},
			{Name: "b", Status: StatusFailed},
		}},
		{Name: "c", Status: StatusNotRun},
		{Name: "d", Status: StatusSkipped},
	}}
	rep.count()
	if rep.OK != 1 || rep.Failed != 1 || rep.NotRun != 1 || rep.Skipped != 1 || rep.Changed != 0 {
		t.Errorf("counts = %+v", rep)
	}
	failed, ok := rep.FirstFailure()
	if !ok || failed.Name != "b" {
		t.Errorf("first failure = %+v (%v)", failed, ok)
	}
}

func TestTheReportOfACommanderIsWrittenPrivateToTheUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs")
	f := testutil.New()
	e := fakeEngine(f)
	e.Report = ReportStore{Dir: dir}
	rep := runFile(t, e, oneItem)
	if rep.Path == "" {
		t.Fatal("the run wrote no report")
	}
	if got := filepath.Base(rep.Path); got != rep.RunID+".json" {
		t.Errorf("report = %q, want it named after the run", got)
	}
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("%s: %v %v", dir, fi, err)
	}
	fi, err = os.Stat(rep.Path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("%s: %v %v", rep.Path, fi, err)
	}
	b, err := os.ReadFile(rep.Path)
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, b)
	}
	if back.Task != "x" || len(back.Results) != 1 {
		t.Errorf("report = %+v", back)
	}
}

func TestADryRunWritesNoReport(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs")
	f := testutil.New()
	e := fakeEngine(f)
	e.DryRun = true
	e.Report = ReportStore{Dir: dir}
	rep := runFile(t, e, oneItem)
	if rep.Path != "" {
		t.Errorf("a dry run wrote %s", rep.Path)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("a dry run made %s", dir)
	}
}

func TestTheReportOfAServerIsWrittenAsTheConfiguredUser(t *testing.T) {
	f := testutil.New()
	store := ReportStore{Dir: "/var/lib/caramelo/task", User: "caramelo", Run: f}
	rep := Report{RunID: "20260918T203311.412Z", Task: "x", Results: []Result{}}
	path, err := store.write(context.Background(), rep)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/var/lib/caramelo/task/20260918T203311.412Z.json" {
		t.Errorf("path = %q", path)
	}
	transcript := f.Transcript()
	for _, want := range []string{
		"(caramelo) install -d -m 0750 -- /var/lib/caramelo/task",
		"(caramelo) tee -- " + path,
		"(caramelo) chmod 0640 -- " + path,
	} {
		if !strings.Contains(transcript, want) {
			t.Errorf("the report was not written with %q:\n%s", want, transcript)
		}
	}
	call, _ := f.Find("tee")
	if !strings.Contains(call.Stdin, `"task": "x"`) {
		t.Errorf("tee was handed %q", call.Stdin)
	}
}

func TestAServerReportThatCannotBeWrittenIsAnError(t *testing.T) {
	f := testutil.New()
	f.Respond("tee -- /var/lib/caramelo/task/id.json", runner.Result{ExitCode: 1, Stderr: "read-only file system\n"})
	store := ReportStore{Dir: "/var/lib/caramelo/task", User: "caramelo", Run: f}
	_, err := store.write(context.Background(), Report{RunID: "id", Task: "x"})
	if err == nil || !strings.Contains(err.Error(), "read-only file system") {
		t.Errorf("err = %v, want it to carry what the machine said", err)
	}
}
