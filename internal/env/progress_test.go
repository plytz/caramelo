package env

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/progress"
)

func TestProgressfWritesM6sLines(t *testing.T) {
	var buf bytes.Buffer
	progressf(&buf, "changed", "route", "%s → %s", "feat-x.shop.test", "web")
	progressf(&buf, "ok", "health", "")
	progressf(&buf, "failed", "rollout", "%s: %s", "web-2 start", "no such image")
	want := "[changed] route: feat-x.shop.test → web\n" +
		"[ok] health\n" +
		"[failed] rollout: web-2 start: no such image\n"
	if got := buf.String(); got != want {
		t.Errorf("progress =\n%q\nwant\n%q", got, want)
	}
}

func TestProgressfTakesANilWriter(t *testing.T) {
	progressf(nil, "ok", "health", "nothing to write to")
}

func TestProgressfThroughAJSONWriter(t *testing.T) {
	var buf bytes.Buffer
	w := progress.New(&buf, progress.FormatJSON)
	progressf(w, "changed", "route", "%s → %s", "feat-x.shop.test", "web")

	line := strings.TrimSpace(buf.String())
	var e progress.Event
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		t.Fatalf("%q is not one JSON object: %v", line, err)
	}
	if e.Status != "changed" || e.Action != "route" || e.Detail != "feat-x.shop.test → web" {
		t.Errorf("event = %+v", e)
	}
	if e.At.IsZero() {
		t.Error("the event carries no instant")
	}
}

func TestRolloutEventsCarryTheirColumns(t *testing.T) {
	h, _ := edgeHarness(t, 2)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	h.cfg.Services[0].Run = "python app.py --v2"
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	rows := h.eventsOf(1, "rollout")
	if len(rows) == 0 {
		t.Fatal("the rollout wrote no events")
	}
	var flips int
	for _, r := range rows {
		if r.Service != "web" {
			t.Errorf("event %+v does not name its service", r)
		}

		if r.Step == "" && r.Replica != 0 {
			t.Errorf("event %+v names a replica but no step", r)
		}
		if r.Step == string(StepFlip) {
			flips++
			if r.Replica == 0 {
				t.Errorf("a flip event names no replica: %+v", r)
			}

			if !strings.Contains(r.Detail, "flip: ") {
				t.Errorf("flip detail = %q", r.Detail)
			}
		}
	}
	if flips == 0 {
		t.Error("no flip was recorded")
	}
}
