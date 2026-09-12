package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/plytz/caramelo/internal/progress"
)

func TestTableIsTheSameTabwriter(t *testing.T) {
	tb := NewTable("NAME", "STATUS").Row("web", "running").Row("worker-with-a-long-name", "exited")

	var want bytes.Buffer
	tw := tabwriter.NewWriter(&want, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS")
	fmt.Fprintln(tw, "web\trunning")
	fmt.Fprintln(tw, "worker-with-a-long-name\texited")
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}

	var got bytes.Buffer
	if err := tb.Write(&got); err != nil {
		t.Fatal(err)
	}
	if got.String() != want.String() {
		t.Errorf("table\n got %q\nwant %q", got.String(), want.String())
	}
}

func TestTableNotesArePrintedAsWritten(t *testing.T) {
	tb := NewTable("NAME").Row("web").Note("\n%s: %s", "web", "health check failed")
	var got bytes.Buffer
	if err := tb.Write(&got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got.String(), "\nweb: health check failed\n") {
		t.Errorf("table = %q", got.String())
	}
}

func TestFieldsIsTheLabelValueBlock(t *testing.T) {
	f := NewFields("shop/feat-x  ready").
		Add("branch", "%s at %s", "feat-x", "0123456").
		Add("ports", "%d-%d", 20000, 20015)
	var got bytes.Buffer
	if err := f.Write(&got); err != nil {
		t.Fatal(err)
	}
	want := "shop/feat-x  ready\n  branch  feat-x at 0123456\n  ports   20000-20015\n"
	if got.String() != want {
		t.Errorf("fields\n got %q\nwant %q", got.String(), want)
	}
}

func TestFieldsWithNoTitlePrintsOnlyRows(t *testing.T) {
	var got bytes.Buffer
	if err := NewFields("").Add("mode", "userspace").Write(&got); err != nil {
		t.Fatal(err)
	}
	if got.String() != "  mode  userspace\n" {
		t.Errorf("fields = %q", got.String())
	}
}

func TestViewSectionSkipsAnEmptyTable(t *testing.T) {
	v := NewView().Text("head").Section(NewTable("A")).Section(NewTable("B").Row("b"))
	want := "head\n\nB\nb\n"
	if got := v.String(); got != want {
		t.Errorf("view\n got %q\nwant %q", got, want)
	}
	if got := NewView().Text("head").Section(nil).String(); got != "head\n" {
		t.Errorf("a nil table printed %q", got)
	}
}

func TestViewRawIsWrittenExactly(t *testing.T) {
	if got := NewView().Raw("a\nb").String(); got != "a\nb" {
		t.Errorf("raw = %q", got)
	}
}

func TestParseEventClassifiesALine(t *testing.T) {
	for _, tc := range []struct {
		line  string
		event bool
	}{
		{`{"action":"up","status":"ok"}`, true},
		{`  {"action":"up","status":"changed","detail":"x"}  `, true},
		{`{"action":"up","status":"ok","unknown_field":3}`, true},
		{`{"action":"up"}`, false},
		{`{"status":"ok"}`, false},
		{`{"not":"an event"}`, false},
		{`warning: detached HEAD`, false},
		{`[{"action":"up","status":"ok"}]`, false},
		{``, false},
	} {
		_, ok := ParseEvent(tc.line)
		if ok != tc.event {
			t.Errorf("ParseEvent(%q) = %v, want %v", tc.line, ok, tc.event)
		}
	}
}

func TestPlainProgressWritesTheReferenceLines(t *testing.T) {
	e := progress.Event{Action: "rollout", Service: "web", Replica: 2,
		Step: "flip", Status: progress.StatusChanged, Detail: "active"}
	var b bytes.Buffer
	s := SinkFor(NewPlain(&b))
	if _, err := fmt.Fprintln(s, mustJSON(t, e)); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(s, "warning: detached HEAD")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	want := progress.Line(e) + "\nwarning: detached HEAD\n"
	if b.String() != want {
		t.Errorf("plain progress\n got %q\nwant %q", b.String(), want)
	}
}

func TestPlainLineUnwrapsAMessage(t *testing.T) {
	e := progress.Event{Action: progress.ActionMessage, Status: progress.StatusOK,
		Detail: "pushing HEAD to feat-x"}
	if got := PlainLine(e); got != "pushing HEAD to feat-x" {
		t.Errorf("PlainLine = %q", got)
	}
}

func TestSinkJoinsAPartialLine(t *testing.T) {
	var events []progress.Event
	var lines []string
	s := NewSink(func(e progress.Event) { events = append(events, e) },
		func(l string) { lines = append(lines, l) })
	line := `{"action":"up","status":"ok","detail":"done"}`
	if _, err := s.Write([]byte(line[:10])); err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("an event was delivered before its newline: %v", events)
	}
	if _, err := s.Write([]byte(line[10:] + "\ntail without a newline")); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Detail != "done" {
		t.Fatalf("events = %v, want one", events)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "tail without a newline" {
		t.Errorf("lines = %v, want the tail Close flushed", lines)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("later\n")); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Errorf("lines = %v, want nothing after Close", lines)
	}
}

func mustJSON(t *testing.T, e progress.Event) string {
	t.Helper()
	var b bytes.Buffer
	if err := progress.New(&b, progress.FormatJSON).Emit(e); err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(b.String(), "\n")
}

func TestTheJSONRoundTripDoesNotMoveTheLine(t *testing.T) {
	for _, e := range []progress.Event{
		{Action: "up", Status: progress.StatusOK},
		{Action: "up", Step: "build", Status: progress.StatusChanged, Detail: "image built"},
		{Action: "rollout", Service: "web", Replica: 2, Step: "flip",
			Status: progress.StatusOK, Detail: "web-2 flip: active"},
		{App: "shop", Env: "production", Action: "deploy", Step: "watch",
			Status: progress.StatusWarning, Detail: "2 of 184 requests failed",
			Identity: "ci", At: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
			JSON: json.RawMessage(`{"requests":184,"errors":2}`)},
	} {
		var wire bytes.Buffer
		if err := progress.New(&wire, progress.FormatJSON).Emit(e); err != nil {
			t.Fatal(err)
		}
		got, ok := ParseEvent(strings.TrimRight(wire.String(), "\n"))
		if !ok {
			t.Fatalf("%q did not come back as an event", wire.String())
		}
		var text bytes.Buffer
		if err := progress.New(&text, progress.FormatText).Emit(e); err != nil {
			t.Fatal(err)
		}
		want := strings.TrimRight(text.String(), "\n")
		if PlainLine(got) != want {
			t.Errorf("round trip\n got %q\nwant %q", PlainLine(got), want)
		}
	}
}

func TestARawLineSurvivesTheRoundTrip(t *testing.T) {
	const line = "warning: you are on a detached HEAD"

	var text bytes.Buffer
	fmt.Fprintln(progress.New(&text, progress.FormatText), line)
	if text.String() != line+"\n" {
		t.Fatalf("text mode wrote %q", text.String())
	}

	var wire bytes.Buffer
	fmt.Fprintln(progress.New(&wire, progress.FormatJSON), line)
	var out bytes.Buffer
	s := SinkFor(NewPlain(&out))
	if _, err := s.Write(wire.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if out.String() != text.String() {
		t.Errorf("round trip\n got %q\nwant %q", out.String(), text.String())
	}
}
