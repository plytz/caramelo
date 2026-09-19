package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/cli/ui"
	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/task"
	"github.com/plytz/caramelo/internal/vpnclient"
)

func TestTaskListNamesEveryTaskWithItsDescription(t *testing.T) {
	isolate(t)
	code, stdout, stderr := run(t, "task", "list")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, name := range task.Tasks() {
		if !strings.Contains(stdout, name) {
			t.Errorf("stdout = %q, want it to name %s", stdout, name)
		}
	}
	if !strings.Contains(stdout, "Name this machine a commander") {
		t.Errorf("stdout = %q, want the description of commander-setup", stdout)
	}

	code, stdout, stderr = run(t, "task", "list", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var list []task.Info
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatalf("%v: %s", err, stdout)
	}
	if len(list) != len(task.Tasks()) || list[0].Desc == "" {
		t.Errorf("list = %+v", list)
	}
}

func TestTaskShowPrintsTheFileByteForByte(t *testing.T) {
	isolate(t)
	code, stdout, stderr := run(t, "task", "show", "commander-setup")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want, err := task.Source("commander-setup")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != string(want) {
		t.Errorf("task show is not the file:\n--- got ---\n%s\n--- want ---\n%s", stdout, want)
	}
}

func TestTaskShowOfAnUnknownTaskIsAUsageError(t *testing.T) {
	isolate(t)
	code, stdout, stderr := run(t, "task", "show", "nosuchtask")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d", code, ExitUsage)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	for _, want := range []string{"nosuchtask", "commander-setup"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to name %q", stderr, want)
		}
	}
}

func TestTaskRunWritesTheCommanderAndThenChangesNothing(t *testing.T) {
	config, cache := freshBox(t)
	code, stdout, stderr := run(t, "task", "run", "commander-setup", "--var", "name=laptop")
	if code != ExitOK {
		t.Fatalf("exit %d: %s%s", code, stdout, stderr)
	}
	for _, want := range []string{"commander-setup", "parallel", "changed=5"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to say %q", stdout, want)
		}
	}
	if strings.Contains(stdout, "\x1b[") {
		t.Error("the rendered run carries escape sequences on a writer that is no terminal")
	}
	wantMode(t, config, 0o700)
	wantMode(t, filepath.Join(config, "config.yaml"), 0o600)
	wantMode(t, cache, 0o700)

	code, stdout, stderr = run(t, "task", "run", "commander-setup", "--var", "name=laptop", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d: %s%s", code, stdout, stderr)
	}
	var rep task.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("%v: %s", err, stdout)
	}
	if rep.Changed != 0 || rep.Failed != 0 || rep.OK != 5 {
		t.Errorf("the second run reported %+v, want everything ok", rep)
	}
	if rep.Task != "commander-setup" || rep.RunID == "" {
		t.Errorf("report = %+v", rep)
	}
	entries, err := os.ReadDir(filepath.Join(cache, "runs"))
	if err != nil || len(entries) != 2 {
		t.Errorf("the runs directory holds %v (%v), want one report per run", entries, err)
	}
}

func TestTaskRunDryRunChangesNothing(t *testing.T) {
	config, _ := freshBox(t)
	code, stdout, stderr := run(t, "task", "run", "commander-setup", "--dry-run", "--var", "name=laptop")
	if code != ExitOK {
		t.Fatalf("exit %d: %s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "(dry run)") || !strings.Contains(stdout, "would-change=5") {
		t.Errorf("stdout = %q, want a dry run that would change everything", stdout)
	}
	if _, err := os.Stat(config); err == nil {
		t.Errorf("%s was created by a dry run", config)
	}
}

func TestTaskRunJSONIsTheSameBytesWhereverItGoes(t *testing.T) {
	freshBox(t)
	_, first, _ := run(t, "task", "run", "commander-setup", "--var", "name=laptop", "--json")
	_, second, _ := run(t, "task", "run", "commander-setup", "--var", "name=laptop", "--json")
	if strings.Contains(first, "\x1b[") || strings.Contains(second, "\x1b[") {
		t.Error("--json carries escape sequences")
	}
	var rep task.Report
	if err := json.Unmarshal([]byte(second), &rep); err != nil {
		t.Fatalf("%v: %s", err, second)
	}
	if rep.OK != 5 {
		t.Errorf("report = %+v", rep)
	}
}

func TestTaskRunWithProgressJSONEmitsOneEventPerLine(t *testing.T) {
	freshBox(t)
	code, _, stderr := run(t, "task", "run", "commander-setup", "--var", "name=laptop", "--progress", "json")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	steps := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		e, ok := ui.ParseEvent(line)
		if !ok {
			t.Fatalf("stderr line is not an event: %q", line)
		}
		if e.Action != progress.ActionTask || e.Task != "commander-setup" {
			t.Errorf("event = %+v, want a task event naming the task", e)
		}
		if e.Status != progress.StatusStarted {
			steps[e.Step] = e.Status
		}
	}
	for _, name := range []string{"config-dir", "vpn-dir", "cache-dir", "config", "identity"} {
		if steps[name] != progress.StatusChanged {
			t.Errorf("%s reported %q, want changed", name, steps[name])
		}
	}
}

func TestTaskRunRefusesAVarThatIsNoPair(t *testing.T) {
	isolate(t)
	code, _, stderr := run(t, "task", "run", "commander-setup", "--var", "name")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "--var") {
		t.Errorf("stderr = %q, want it to blame --var", stderr)
	}
}

func TestTaskRunOfAnUnknownTaskIsAUsageError(t *testing.T) {
	isolate(t)
	if code, _, _ := run(t, "task", "run", "nosuchtask"); code != ExitUsage {
		t.Errorf("exit %d, want %d", code, ExitUsage)
	}
}

func TestAFailingRunIsExitOne(t *testing.T) {
	config, _ := freshBox(t)
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, vpnclient.KeyDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := run(t, "task", "run", "commander-setup", "--var", "name=laptop")
	if code != ExitError {
		t.Fatalf("exit %d, want %d: %s%s", code, ExitError, stdout, stderr)
	}
	for _, want := range []string{"failed=1", "failed at vpn-dir; run again with --debug", "not run"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to say %q", stdout, want)
		}
	}
	if strings.Contains(stdout, "\x1b[") {
		t.Error("the rendered run carries escape sequences on a writer that is no terminal")
	}
}

func TestABuiltinThatCannotApplyIsReportedFailed(t *testing.T) {
	dir := isolate(t)
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := task.Parse([]byte(`version: 1
name: x
items:
  - name: one
    dir:
      path: `+blocked+`
      mode: '0700'
`), "x.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := (&task.Engine{Version: version}).Execute(t.Context(), f)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 1 {
		t.Fatalf("report = %+v, want a failure", rep)
	}
}

func TestADryRunSaysWouldChangeOnTheWire(t *testing.T) {
	freshBox(t)
	code, _, stderr := run(t, "task", "run", "commander-setup",
		"--dry-run", "--progress", "json", "--var", "name=laptop")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	steps := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		e, ok := ui.ParseEvent(line)
		if !ok {
			t.Fatalf("stderr line is not an event: %q", line)
		}
		if e.Action != progress.ActionTask || e.Task != "commander-setup" {
			t.Errorf("event = %+v, want a task event naming the task", e)
		}
		if e.Status == progress.StatusSkipped {
			t.Errorf("event = %+v, want the item's own word and never %q", e, progress.StatusSkipped)
		}
		if e.Status != progress.StatusStarted {
			steps[e.Step] = e.Status
		}
	}
	for _, name := range []string{"config-dir", "vpn-dir", "cache-dir", "config", "identity"} {
		if steps[name] != progress.StatusWouldChange {
			t.Errorf("%s reported %q, want would-change", name, steps[name])
		}
	}
}

func TestARunWhoseReportCannotBeWrittenStillSaysWhatItDid(t *testing.T) {
	dir := isolate(t)
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", filepath.Join(blocked, "cache"))
	code, stdout, stderr := run(t, "task", "run", "commander-setup", "--var", "name=laptop")
	if code != ExitError {
		t.Fatalf("exit %d, want %d: %s%s", code, ExitError, stdout, stderr)
	}
	if !strings.Contains(stdout, "commander-setup  ok=") || !strings.Contains(stdout, "changed=1") || !strings.Contains(stdout, "failed=1") {
		t.Errorf("stdout = %q, want the recap of the run that really happened", stdout)
	}
	if !strings.Contains(stderr, "report was not written") {
		t.Errorf("stderr = %q, want it to say the report was lost", stderr)
	}

	code, stdout, _ = run(t, "task", "run", "commander-setup", "--var", "name=laptop", "--json")
	if code != ExitError {
		t.Fatalf("exit %d, want %d", code, ExitError)
	}
	var rep task.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("%v: %s", err, stdout)
	}
	if rep.Task != "commander-setup" || len(rep.Results) == 0 {
		t.Errorf("report = %+v, want the whole run", rep)
	}
}

func TestColourIsOffUnlessStdoutIsATerminalNobodyPipedAway(t *testing.T) {
	terminal := func(want bool) {
		t.Helper()
		old := writerIsATerminal
		writerIsATerminal = func(*os.File) bool { return want }
		t.Cleanup(func() { writerIsATerminal = old })
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { read.Close(); write.Close() })

	t.Run("a terminal", func(t *testing.T) {
		terminal(true)
		t.Setenv("NO_COLOR", "")
		if a := (&app{stdout: write}); !a.colour() {
			t.Error("colour is off on a terminal")
		}
	})
	t.Run("NO_COLOR", func(t *testing.T) {
		terminal(true)
		t.Setenv("NO_COLOR", "1")
		if a := (&app{stdout: write}); a.colour() {
			t.Error("colour survived NO_COLOR")
		}
	})
	t.Run("--json", func(t *testing.T) {
		terminal(true)
		t.Setenv("NO_COLOR", "")
		if a := (&app{stdout: write, json: true}); a.colour() {
			t.Error("colour survived --json")
		}
	})
	t.Run("a pipe", func(t *testing.T) {
		terminal(false)
		t.Setenv("NO_COLOR", "")
		if a := (&app{stdout: write}); a.colour() {
			t.Error("colour reached something that is no terminal")
		}
	})
	t.Run("a writer that is no file", func(t *testing.T) {
		terminal(true)
		t.Setenv("NO_COLOR", "")
		if a := (&app{stdout: io.Discard}); a.colour() {
			t.Error("colour reached a writer that is no file")
		}
	})
}

func TestEveryCarameloLeafATaskNamesIsARealCommand(t *testing.T) {
	root := NewRootCmd(os.Stdout, os.Stderr)
	for _, name := range task.Tasks() {
		f, err := task.Load(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, script := range taskScripts(f) {
			argv, ok := task.InProcess(script)
			if !ok {
				continue
			}
			cmd, rest, err := root.Find(argv[1:])
			if err != nil {
				t.Errorf("%s names %q: %v", name, script, err)
				continue
			}
			if cmd.HasSubCommands() {
				t.Errorf("%s names %q, which is a group and not a command", name, script)
			}
			checkFlags(t, name, script, cmd, rest)
		}
	}
}

func taskScripts(f *task.File) []string {
	var out []string
	var walk func(items []task.Entry)
	walk = func(items []task.Entry) {
		for _, e := range items {
			if e.Block != nil {
				walk(e.Block.Items)
				continue
			}
			out = append(out, e.Item.When, e.Item.Check, e.Item.Cmd)
			out = append(out, e.Item.Cmds...)
		}
	}
	walk(f.Items)
	return out
}

func checkFlags(t *testing.T, task, script string, cmd *cobra.Command, args []string) {
	t.Helper()
	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		name := strings.TrimPrefix(strings.SplitN(arg, "=", 2)[0], "--")
		if cmd.Flags().Lookup(name) == nil && cmd.Root().PersistentFlags().Lookup(name) == nil {
			t.Errorf("%s names %q, and %s has no --%s", task, script, cmd.CommandPath(), name)
		}
	}
}

func TestATaskEventSurvivesTheRelayWithItsPayload(t *testing.T) {
	res := task.Result{Name: "config-dir", Status: "would-change", Detail: "created, 0700"}
	payload, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(progress.Event{
		Action: progress.ActionTask, Task: "commander-setup", Step: res.Name,
		Status: res.Status, Detail: res.Detail, JSON: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	e, ok := ui.ParseEvent(string(line))
	if !ok {
		t.Fatalf("the relay dropped the event: %s", line)
	}
	if e.Task != "commander-setup" || e.Status != "would-change" {
		t.Errorf("event = %+v", e)
	}
	back, ok := task.ResultOf(e)
	if !ok || back.Name != res.Name || back.Status != res.Status {
		t.Errorf("payload = %+v (%v), want it intact", back, ok)
	}
}
