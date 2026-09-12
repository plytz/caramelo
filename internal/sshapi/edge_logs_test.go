package sshapi

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
)

type journalRunner struct {
	args   []string
	output string
	exit   int
}

func (r *journalRunner) Run(_ context.Context, c runner.Cmd) (runner.Result, error) {
	r.args = c.Args
	if c.Stdout != nil {
		_, _ = c.Stdout.Write([]byte(r.output))
	}
	return runner.Result{ExitCode: r.exit}, nil
}

func accessLine(t *testing.T, e edge.AccessLog) string {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return edge.AccessLogPrefix + " " + string(b) + "\n"
}

func logsDaemon(t *testing.T, r runner.Runner) *Daemon {
	t.Helper()
	cfg := serverconfig.Default()
	cfg.Edge, cfg.ACMEEmail = true, "ops@example.com"
	return &Daemon{Config: cfg, Runner: r}
}

func TestEdgeLogsRendersEveryRequest(t *testing.T) {
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	run := &journalRunner{output: strings.Join([]string{
		accessLine(t, edge.AccessLog{At: at, Host: "feat-x.shop.test", Method: "GET", Path: "/",
			Proto: "h2", Status: 200, Bytes: 12, Duration: 3 * time.Millisecond,
			Client: "203.0.113.7:51000", App: "shop", Env: "feat-x", Service: "web",
			Replica: 2, Target: "127.0.0.1:20005"}),
		"the edge's own diagnostic, which is not a request\n",
		accessLine(t, edge.AccessLog{At: at, Host: "feat-y.shop.test", Method: "GET",
			Status: 502, App: "shop", Env: "feat-y", Error: "no target"}),
	}, "")}
	var out, errOut bytes.Buffer

	if err := logsDaemon(t, run).Logs(context.Background(), env.LogsRequest{
		Edge: true, Stdout: &out, Stderr: &errOut,
	}); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("output = %q, want one line per request and nothing else", out.String())
	}
	for _, want := range []string{"feat-x.shop.test", "200", "web/2", "127.0.0.1:20005", "h2", "203.0.113.7:51000"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line %q does not carry %q", lines[0], want)
		}
	}
	if !strings.Contains(lines[1], "error=no target") {
		t.Errorf("a request with no status must say why: %q", lines[1])
	}

	if got := strings.Join(run.args, " "); !strings.Contains(got, "--unit "+EdgeLogUnit) ||
		!strings.Contains(got, "--lines all") {
		t.Errorf("journalctl args = %q", got)
	}
}

func TestEdgeLogsFiltersToOneEnvironment(t *testing.T) {
	run := &journalRunner{output: strings.Join([]string{
		accessLine(t, edge.AccessLog{Host: "feat-x.shop.test", App: "shop", Env: "feat-x", Status: 200}),
		accessLine(t, edge.AccessLog{Host: "feat-y.shop.test", App: "shop", Env: "feat-y", Status: 200}),
	}, "")}
	var out bytes.Buffer

	if err := logsDaemon(t, run).Logs(context.Background(), env.LogsRequest{
		Edge: true, App: "shop", Name: "feat-x", Stdout: &out,
	}); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if strings.Count(out.String(), "\n") != 1 || !strings.Contains(out.String(), "feat-x") {
		t.Errorf("output = %q, want only feat-x's requests", out.String())
	}
}

func TestEdgeLogsJSONIsTheEdgesOwnObject(t *testing.T) {
	run := &journalRunner{output: accessLine(t, edge.AccessLog{
		Host: "feat-x.shop.test", Env: "feat-x", Status: 200, Replica: 3, Target: "127.0.0.1:20006"})}
	var out bytes.Buffer

	if err := logsDaemon(t, run).Logs(context.Background(), env.LogsRequest{
		Edge: true, JSON: true, Stdout: &out,
	}); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	var got edge.AccessLog
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &got); err != nil {
		t.Fatalf("--json emitted %q: %v", out.String(), err)
	}
	if got.Replica != 3 || got.Target != "127.0.0.1:20006" {
		t.Errorf("parsed %+v, want the edge's own object", got)
	}
}

func TestEdgeLogsQuery(t *testing.T) {
	for name, tc := range map[string]struct {
		req  env.LogsRequest
		want []string
		not  []string
	}{
		"follow":      {env.LogsRequest{Follow: true}, []string{"--follow"}, []string{"--lines all"}},
		"a duration":  {env.LogsRequest{Since: "10m"}, []string{"--since -10m"}, nil},
		"a timestamp": {env.LogsRequest{Since: "2026-09-09 12:00:00"}, []string{"--since 2026-09-09 12:00:00"}, nil},
		"a tail":      {env.LogsRequest{Tail: 50}, []string{"--lines 50"}, []string{"--lines all"}},
	} {
		run := &journalRunner{}
		req := tc.req
		req.Edge, req.Stdout = true, &bytes.Buffer{}
		if err := logsDaemon(t, run).Logs(context.Background(), req); err != nil {
			t.Fatalf("%s: Logs: %v", name, err)
		}
		got := strings.Join(run.args, " ")
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s: journalctl args %q, want %q in them", name, got, want)
			}
		}
		for _, never := range tc.not {
			if strings.Contains(got, never) {
				t.Errorf("%s: journalctl args %q must not carry %q", name, got, never)
			}
		}
	}
}

func TestEdgeLogsOnAMachineWithNoEdge(t *testing.T) {
	run := &journalRunner{}
	d := &Daemon{Config: serverconfig.Default(), Runner: run}
	err := d.Logs(context.Background(), env.LogsRequest{Edge: true, Stdout: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "edge enable") {
		t.Fatalf("Logs = %v, want it to say how to get an edge", err)
	}
	if run.args != nil {
		t.Errorf("journalctl was run anyway: %q", run.args)
	}
}

func TestEdgeLogsReportsAFailedJournal(t *testing.T) {
	run := &journalRunner{exit: 1}
	err := logsDaemon(t, run).Logs(context.Background(), env.LogsRequest{Edge: true, Stdout: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "journalctl exited 1") {
		t.Fatalf("Logs = %v, want the journal's own failure", err)
	}
}

func TestEdgeLogsSaysWhenThereIsNothing(t *testing.T) {
	run := &journalRunner{}
	var out, errOut bytes.Buffer
	if err := logsDaemon(t, run).Logs(context.Background(), env.LogsRequest{
		Edge: true, Stdout: &out, Stderr: &errOut,
	}); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", out.String())
	}
	if !strings.Contains(errOut.String(), "no public requests") {
		t.Errorf("stderr = %q, want the note", errOut.String())
	}
}

func TestLineWriterStreams(t *testing.T) {
	var got []string
	w := &lineWriter{write: func(s string) { got = append(got, s) }}
	for _, chunk := range []string{"one\ntw", "o\nthree", ""} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(got, "|") != "one|two" {
		t.Errorf("lines = %q, want the two complete ones", got)
	}
	w.flush()
	if strings.Join(got, "|") != "one|two|three" {
		t.Errorf("after flush = %q, want the trailing line too", got)
	}
}
