package progress

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLineIsM6sBytes(t *testing.T) {
	for _, tc := range []struct {
		e    Event
		want string
	}{
		{Event{Status: "changed", Action: "route", Detail: "feat-x.shop.test → web"},
			"[changed] route: feat-x.shop.test → web"},
		{Event{Status: "ok", Action: "health"}, "[ok] health"},
		{Event{Status: "failed", Action: "rollout", Detail: "web-2 start: no such image"},
			"[failed] rollout: web-2 start: no such image"},

		{Event{Status: "changed", Action: "rollout", Service: "web", Replica: 2, Step: "flip",
			Detail: "web-2 flip: active", Identity: "alex@laptop", App: "shop", Env: "production"},
			"[changed] rollout: web-2 flip: active"},
	} {
		if got := Line(tc.e); got != tc.want {
			t.Errorf("Line(%+v) = %q, want %q", tc.e, got, tc.want)
		}
	}
}

func TestWriterTextIsTheLinePlusNewline(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf, FormatText)
	if err := w.Emit(Event{Status: "changed", Action: "route", Detail: "a → b"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Emit(Event{Status: "ok", Action: "edge", Detail: "2 routes"}); err != nil {
		t.Fatal(err)
	}
	want := "[changed] route: a → b\n[ok] edge: 2 routes\n"
	if got := buf.String(); got != want {
		t.Errorf("text stream = %q, want %q", got, want)
	}
}

func TestWriterDefaultsToText(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf, "")
	if w.Format() != FormatText {
		t.Errorf("format = %q, want %q", w.Format(), FormatText)
	}
	if err := w.Emit(Event{Status: "ok", Action: "ports"}); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "[ok] ports\n" {
		t.Errorf("= %q", got)
	}
}

func TestWriterJSONIsOneObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	w := New(&buf, FormatJSON)
	w.Now = func() time.Time { return at }
	if err := w.Emit(Event{App: "shop", Env: "production", Service: "web", Replica: 2,
		Action: "rollout", Step: "flip", Status: "changed", Detail: "web-2 flip: active",
		Identity: "alex@laptop", JSON: json.RawMessage(`{"inflight":3}`)}); err != nil {
		t.Fatal(err)
	}

	if err := w.Emit(Event{Action: "edge", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2: %q", len(lines), buf.String())
	}
	var first Event
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 1 is not one JSON object: %v", err)
	}
	if first.Service != "web" || first.Replica != 2 || first.Step != "flip" ||
		string(first.JSON) != `{"inflight":3}` {
		t.Errorf("decoded = %+v", first)
	}
	var second Event
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if !second.At.Equal(at) {
		t.Errorf("an event with no instant was not stamped: %v", second.At)
	}

	if strings.Contains(lines[1], "service") || strings.Contains(lines[1], "replica") {
		t.Errorf("line 2 carries empty fields: %s", lines[1])
	}
}

func TestWriterIsAnIOWriter(t *testing.T) {
	var text bytes.Buffer
	if _, err := New(&text, FormatText).Write([]byte("caramelo: warning: x\n")); err != nil {
		t.Fatal(err)
	}
	if got := text.String(); got != "caramelo: warning: x\n" {
		t.Errorf("text pass-through = %q", got)
	}

	var jsn bytes.Buffer
	if _, err := New(&jsn, FormatJSON).Write([]byte("caramelo: warning: x\n\nand y\n")); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(jsn.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2 (the blank one is dropped): %q", len(lines), jsn.String())
	}
	var e Event
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatal(err)
	}
	if e.Action != ActionMessage || e.Detail != "caramelo: warning: x" {
		t.Errorf("wrapped message = %+v", e)
	}
}

func TestEmitOnAPlainWriterWritesTheLine(t *testing.T) {
	var buf bytes.Buffer
	if err := Emit(&buf, Event{Status: "ok", Action: "cache", Detail: "volume"}); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "[ok] cache: volume\n" {
		t.Errorf("= %q", got)
	}

	var jsn bytes.Buffer
	if err := Emit(New(&jsn, FormatJSON), Event{Status: "ok", Action: "cache"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(jsn.String(), `{"action":"cache"`) {
		t.Errorf("= %q", jsn.String())
	}
}

func TestNilWriterDiscards(t *testing.T) {
	var w *Writer
	if err := w.Emit(Event{Action: "x"}); err != nil {
		t.Fatal(err)
	}
	if n, err := w.Write([]byte("x")); n != 1 || err != nil {
		t.Errorf("Write on a nil writer = %d, %v", n, err)
	}
	if w.Format() != FormatText {
		t.Errorf("nil writer format = %q", w.Format())
	}
	if New(nil, FormatJSON) != nil {
		t.Error("New(nil, …) is not nil")
	}
	if err := Emit(nil, Event{Action: "x"}); err != nil {
		t.Fatal(err)
	}
}

func TestWriterReportsAFailedWrite(t *testing.T) {
	boom := errors.New("boom")
	for _, f := range Formats {
		w := New(failingWriter{boom}, f)
		if err := w.Emit(Event{Action: "x", Status: "ok"}); !errors.Is(err, boom) {
			t.Errorf("%s: Emit = %v, want boom", f, err)
		}
	}
	if _, err := New(failingWriter{boom}, FormatText).Write([]byte("x")); !errors.Is(err, boom) {
		t.Errorf("Write = %v, want boom", err)
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func TestWriterIsConcurrencySafe(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf, FormatJSON)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			_ = w.Emit(Event{Action: "rollout", Status: "ok", Replica: i})
		}(i)
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 50 {
		t.Fatalf("%d lines, want 50", len(lines))
	}
	for i, l := range lines {
		var e Event
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("line %d is not one object: %v (%q)", i, err, l)
		}
	}
}

func TestParseFormat(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Format
	}{
		{"", FormatText}, {"text", FormatText}, {"TEXT", FormatText},
		{" json ", FormatJSON}, {"json", FormatJSON},
	} {
		got, err := ParseFormat(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseFormat(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"yaml", "pretty", "1"} {
		if _, err := ParseFormat(bad); err == nil {
			t.Errorf("ParseFormat(%q) was accepted", bad)
		}
	}
	if Format("").String() != "text" {
		t.Error("the empty format does not print as text")
	}
}

func TestNopNotifier(t *testing.T) {
	var n Notifier = Nop{}
	n.Notify(Event{Action: "x"})
	ch, stop := n.Subscribe(DefaultBuffer)
	stop()
	stop()
	if _, open := <-ch; open {
		t.Error("the nop notifier's channel delivered an event")
	}

	for range ch {
		t.Error("the nop notifier's channel is not closed")
	}
}

func TestMachineFieldIsAbsentOnAMachineOfOne(t *testing.T) {
	e := Event{Action: "up", Status: StatusChanged, Detail: "web-1 started"}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "machine") {
		t.Errorf("event = %s, want no machine field at all", b)
	}
	if got := Line(e); got != "[changed] up: web-1 started" {
		t.Errorf("Line = %q", got)
	}
	e.Machine = "nx2"
	b, err = json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"machine":"nx2"`) {
		t.Errorf("event = %s, want the machine that recorded it", b)
	}

	if got := Line(e); got != "[changed] up: web-1 started" {
		t.Errorf("Line with a machine = %q, want the M4 line unchanged", got)
	}
}

func TestATaskEventCarriesItsTaskAndRoundTrips(t *testing.T) {
	var b bytes.Buffer
	w := New(&b, FormatJSON)
	at := time.Date(2026, 9, 18, 20, 33, 11, 412_000_000, time.UTC)
	if err := w.Emit(Event{Action: ActionTask, Task: "commander-setup", Step: "config-dir",
		Status: StatusChanged, Detail: "created, 0700", At: at}); err != nil {
		t.Fatal(err)
	}
	want := `{"action":"task","task":"commander-setup","step":"config-dir","status":"changed",` +
		`"detail":"created, 0700","at":"2026-09-18T20:33:11.412Z"}` + "\n"
	if b.String() != want {
		t.Errorf("event =\n%s\nwant\n%s", b.String(), want)
	}
	var back Event
	if err := json.Unmarshal(b.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if back.Task != "commander-setup" {
		t.Errorf("task = %q", back.Task)
	}
}

func TestAnEventWithNoTaskCarriesNoTaskField(t *testing.T) {
	b, err := json.Marshal(Event{Action: ActionMessage, Status: StatusWarning})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(`"task"`)) {
		t.Errorf("event = %s, want no task field", b)
	}
}

func TestTheStatusVocabularyHoldsWouldChange(t *testing.T) {
	want := []string{StatusStarted, StatusOK, StatusChanged, StatusWouldChange, StatusSkipped, StatusWarning, StatusFailed}
	if len(Statuses) != len(want) {
		t.Fatalf("statuses = %v, want %v", Statuses, want)
	}
	for i, s := range want {
		if Statuses[i] != s {
			t.Errorf("statuses[%d] = %q, want %q", i, Statuses[i], s)
		}
	}
}

func TestTheProgressLineIsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		e    Event
		want string
	}{
		{Event{Action: "task", Status: "changed"}, "[changed] task"},
		{Event{Action: "task", Status: "changed", Detail: "created, 0700"}, "[changed] task: created, 0700"},
		{Event{Action: "task", Task: "commander-setup", Step: "config-dir", Status: "ok"}, "[ok] task"},
	} {
		if got := Line(tc.e); got != tc.want {
			t.Errorf("Line(%+v) = %q, want %q", tc.e, got, tc.want)
		}
	}
}
