package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/cli/ui"
	"github.com/plytz/caramelo/internal/progress"
)

type fakeDaemon struct {
	args []string

	out    string
	err    string
	events []progress.Event

	feed bool
	code int
	fail error
}

func (d *fakeDaemon) install(t *testing.T) {
	t.Helper()
	old := forward
	forward = func(_ context.Context, a *app) (int, error) {
		d.args = append([]string(nil), a.args...)
		for _, e := range d.events {

			w, format := a.stderr, formatOf(a.args)
			if d.feed {
				w, format = a.stdout, progress.FormatJSON
			}
			if err := progress.Emit(progress.New(w, format), e); err != nil {
				t.Errorf("emit: %v", err)
			}
		}
		if d.err != "" {
			fmt.Fprint(a.stderr, d.err)
		}
		if d.out != "" {
			fmt.Fprint(a.stdout, d.out)
		}
		return d.code, d.fail
	}
	t.Cleanup(func() { forward = old })
}

func formatOf(args []string) progress.Format {
	for i, a := range args {
		if a == "--progress" && i+1 < len(args) {
			f, err := progress.ParseFormat(args[i+1])
			if err == nil {
				return f
			}
		}
	}
	return progress.FormatText
}

func has(args []string, want ...string) bool {
	return strings.Contains(strings.Join(args, "\x00"), strings.Join(want, "\x00"))
}

func TestForwardAsksForJSONAndRendersHere(t *testing.T) {
	d := &fakeDaemon{out: `{"version":"v0.1.0","hostname":"worker1","transport":"socket"}` + "\n"}
	d.install(t)

	code, stdout, stderr := run(t, "status")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if !has(d.args, "--json") || !has(d.args, "--progress", "json") {
		t.Errorf("forwarded %v, want --json and --progress json", d.args)
	}
	if !strings.HasPrefix(stdout, "caramelod v0.1.0 on worker1 (via socket)") {
		t.Errorf("stdout = %q, want the rendered status", stdout)
	}
	if strings.Contains(stdout, "{") {
		t.Errorf("stdout = %q, want no JSON", stdout)
	}
}

func TestForwardWithJSONPassesThrough(t *testing.T) {
	body := `{"version":"v0.1.0"}` + "\n"
	d := &fakeDaemon{out: body}
	d.install(t)

	code, stdout, _ := run(t, "status", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if stdout != body {
		t.Errorf("stdout = %q, want the daemon's bytes %q", stdout, body)
	}
	if strings.Count(strings.Join(d.args, " "), "--json") != 1 {
		t.Errorf("forwarded %v, want exactly one --json", d.args)
	}
	if has(d.args, "--progress") {
		t.Errorf("forwarded %v, want no --progress", d.args)
	}
}

func TestForwardWithAnExplicitProgressPassesThrough(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			d := &fakeDaemon{out: "caramelod v0.1.0 on worker1\n"}
			d.install(t)
			if code, stdout, _ := run(t, "status", "--progress", format); code != ExitOK {
				t.Fatalf("exit = %d", code)
			} else if stdout != d.out {
				t.Errorf("stdout = %q, want the daemon's bytes", stdout)
			}
			if has(d.args, "--json") {
				t.Errorf("forwarded %v, want no --json", d.args)
			}
			if strings.Count(strings.Join(d.args, " "), "--progress") != 1 {
				t.Errorf("forwarded %v, want exactly one --progress", d.args)
			}
		})
	}
}

func TestForwardLeavesStreamingCommandsAlone(t *testing.T) {
	d := &fakeDaemon{out: "web | listening\nweb | request\n"}
	d.install(t)

	code, stdout, _ := run(t, "logs", "feat-x", "--app", "shop", "--follow")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if stdout != d.out {
		t.Errorf("stdout = %q, want the lines as they arrived", stdout)
	}
	if has(d.args, "--json") || has(d.args, "--progress") {
		t.Errorf("forwarded %v, want neither --json nor --progress", d.args)
	}
}

func TestForwardRendersTheEventStreamAsTheOldLines(t *testing.T) {
	events := []progress.Event{
		{Action: "up", Step: "build", Status: progress.StatusChanged, Detail: "image built"},
		{Action: "rollout", Service: "web", Replica: 2, Step: "flip", Status: progress.StatusOK, Detail: "active"},
	}
	d := &fakeDaemon{
		out:    `{"env":{"app":"shop","name":"feat-x","status":"ready"},"services":[]}` + "\n",
		events: events,
		err:    "warning: detached HEAD\n",
	}
	d.install(t)

	code, _, stderr := run(t, "up", "feat-x", "--app", "shop", "--no-push")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	for _, e := range events {
		if want := progress.Line(e); !strings.Contains(stderr, want) {
			t.Errorf("stderr %q is missing %q", stderr, want)
		}
	}
	if !strings.Contains(stderr, "warning: detached HEAD") {
		t.Errorf("stderr %q dropped the line that was not an event", stderr)
	}
	if strings.Contains(stderr, `"action"`) {
		t.Errorf("stderr %q still has the JSON in it", stderr)
	}
}

func TestForwardPrintsNoResultWhenTheCommandFailed(t *testing.T) {
	d := &fakeDaemon{
		out:  `{"env":{"app":"shop","name":"feat-x","status":"failed"},"services":[]}` + "\n",
		err:  "caramelo: web never became healthy\n",
		code: ExitError,
	}
	d.install(t)

	code, stdout, stderr := run(t, "up", "feat-x", "--app", "shop", "--no-push")
	if code != ExitError {
		t.Fatalf("exit = %d, want %d", code, ExitError)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	if !strings.Contains(stderr, "web never became healthy") {
		t.Errorf("stderr = %q, want the daemon's error", stderr)
	}
}

func TestForwardPassesThroughWhatItCannotDecode(t *testing.T) {
	d := &fakeDaemon{out: "caramelod v0.1.0 on worker1 (via socket)\n  uptime  3m\n"}
	d.install(t)

	code, stdout, _ := run(t, "status")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if stdout != d.out {
		t.Errorf("stdout = %q, want the bytes as they arrived %q", stdout, d.out)
	}
}

func TestForwardReportsATransportFailure(t *testing.T) {
	d := &fakeDaemon{fail: errors.New("no caramelod socket"), code: ExitError}
	d.install(t)

	code, stdout, stderr := run(t, "status")
	if code != ExitError {
		t.Fatalf("exit = %d, want %d", code, ExitError)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	if !strings.Contains(stderr, "no caramelod socket") {
		t.Errorf("stderr = %q, want the transport's error", stderr)
	}
}

func TestTheLiveViewsAreRegistered(t *testing.T) {
	for _, tc := range []struct {
		path string
		kind viewKind
	}{
		{"events", viewFeed},
		{"up", viewNone}, {"env create", viewNone}, {"deploy", viewNone},
		{"env list", viewNone}, {"status", viewNone}, {"logs", viewNone},
	} {
		if got := viewFor(tc.path); got != tc.kind {
			t.Errorf("viewFor(%q) = %v, want %v", tc.path, got, tc.kind)
		}
	}
}

func TestProgressIsAlwaysPlain(t *testing.T) {
	a := &app{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	if p := a.newProgress("up"); p == nil {
		t.Fatal("no progress renderer")
	} else if _, plain := p.(*ui.PlainWriter); !plain {
		t.Errorf("progress renderer is %T, want the plain one", p)
	}
	if p := a.newProgress("events"); p == nil {
		t.Fatal("no progress renderer")
	} else if _, plain := p.(*ui.PlainFeed); !plain {
		t.Errorf("feed renderer is %T, want the plain one", p)
	}
}

func TestForwardRendersTheFeedFromTheDaemonsStdout(t *testing.T) {
	events := []progress.Event{
		{App: "shop", Env: "feat-x", Action: "up", Status: progress.StatusOK, Detail: "web started",

			At: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)},
	}
	d := &fakeDaemon{events: events, feed: true, err: "a line the daemon put on stderr\n"}
	d.install(t)

	code, stdout, stderr := run(t, "events", "--follow")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if !has(d.args, "--progress", "json") {
		t.Errorf("forwarded %v, want --progress json", d.args)
	}

	if !has(d.args, "--json") {
		t.Errorf("forwarded %v, want --json", d.args)
	}

	if want := ui.FeedLine(events[0]); !strings.Contains(stdout, want) {
		t.Errorf("stdout %q is missing the feed line %q", stdout, want)
	}

	if strings.Contains(stdout, `"action"`) {
		t.Errorf("stdout %q carries the wire format", stdout)
	}
	if !strings.Contains(stderr, "a line the daemon put on stderr") {
		t.Errorf("stderr %q lost what the daemon wrote there", stderr)
	}
}

func TestForwardLeavesTheStreamingCommandsUntouched(t *testing.T) {
	for _, args := range [][]string{
		{"env", "exec", "feat-x", "--app", "shop", "--", "sh", "-c", "echo hi"},
		{"run", "feat-x", "--app", "shop", "--", "ls"},
		{"test", "feat-x", "--app", "shop"},
		{"env", "export", "feat-x", "--app", "shop"},
		{"logs", "feat-x", "--app", "shop"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			d := &fakeDaemon{out: "whatever the command printed\n"}
			d.install(t)
			code, stdout, stderr := run(t, args...)
			if code != ExitOK {
				t.Fatalf("exit = %d (stderr %q)", code, stderr)
			}
			if stdout != d.out {
				t.Errorf("stdout = %q, want it passed through", stdout)
			}
			if has(d.args, "--json") || has(d.args, "--progress") {
				t.Errorf("forwarded %v, want neither --json nor --progress", d.args)
			}
		})
	}
}

func TestForwardKeepsTheExitCode(t *testing.T) {
	for _, code := range []int{0, 1, 2, 42} {
		d := &fakeDaemon{out: `{"services":[]}` + "\n", code: code}
		d.install(t)
		if got, _, _ := run(t, "down", "feat-x", "--app", "shop"); got != code {
			t.Errorf("exit = %d, want %d", got, code)
		}
	}
}

func TestForwardAsksForJSONForTheProductionCommands(t *testing.T) {
	for _, name := range []string{"build", "deploy", "rollback", "promote", "releases"} {
		t.Run(name, func(t *testing.T) {
			if renderFor(name) == nil {
				t.Fatalf("%s has no renderer, so a person gets the document", name)
			}
			d := &fakeDaemon{out: "{}\n"}
			d.install(t)
			if code, _, stderr := run(t, name, "production", "--app", "shop"); code != ExitOK {
				t.Fatalf("exit = %d (stderr %q)", code, stderr)
			}
			if !has(d.args, "--json") {
				t.Errorf("forwarded %v, want --json: the renderer on this side decodes it", d.args)
			}
			if !has(d.args, "--progress", "json") {
				t.Errorf("forwarded %v, want --progress json for the view", d.args)
			}
		})
	}
}

func TestAnOlderDaemonIsNamedRatherThanGuessedAt(t *testing.T) {
	d := &fakeDaemon{err: "Error: unknown flag: --progress\n", code: ExitUsage}
	d.install(t)

	code, stdout, stderr := run(t, "status")
	if code != ExitError {
		t.Errorf("exit = %d, want %d", code, ExitError)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	for _, want := range []string{"older", "--progress", "server setup --target"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr %q does not mention %q", stderr, want)
		}
	}

	if !strings.Contains(stderr, "unknown flag: --progress") {
		t.Errorf("stderr %q lost what the daemon said", stderr)
	}
}
