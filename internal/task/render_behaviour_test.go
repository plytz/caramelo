package task

import (
	"bytes"
	"strings"
	"testing"
)

func statusColumns(out string) map[string]int {
	cols := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		for _, status := range Statuses {
			if i := strings.Index(line, " "+status); i > 0 {
				cols[strings.Fields(line)[0]] = i + 1
				break
			}
		}
	}
	return cols
}

func TestTheStatusColumnIsTheSameForEveryNameAndEveryDepth(t *testing.T) {
	long := strings.Repeat("a", 24)
	plan := Plan{Task: "t", Items: []PlanItem{
		{Name: "ab"},
		{Name: blockName, Desc: "together", Block: true, Items: []PlanItem{{Name: long}}},
	}}
	var b bytes.Buffer
	r := &Renderer{W: &b}
	if err := r.Start(plan); err != nil {
		t.Fatal(err)
	}
	for _, res := range []Result{
		{Name: "ab", Status: StatusOK, Detail: "short"},
		{Name: long, Status: StatusOK, Detail: "long"},
	} {
		if err := r.Result(res); err != nil {
			t.Fatal(err)
		}
	}
	cols := statusColumns(b.String())
	if cols["ab"] == 0 || cols[long] == 0 {
		t.Fatalf("no status column found:\n%s", b.String())
	}
	if cols["ab"] != cols[long] {
		t.Errorf("the status column is %d for a 2-character name and %d for a 24-character one:\n%s",
			cols["ab"], cols[long], b.String())
	}
}

func TestADetailIsItsFirstLineAndNothingIsWrappedOrTruncated(t *testing.T) {
	long := strings.Repeat("x", 300) + " ünïcode"
	var b bytes.Buffer
	r := &Renderer{W: &b}
	if err := r.Start(Plan{Task: "t", Items: []PlanItem{{Name: "one"}, {Name: "two"}}}); err != nil {
		t.Fatal(err)
	}
	_ = r.Result(Result{Name: "one", Status: StatusOK, Detail: "first\nsecond"})
	_ = r.Result(Result{Name: "two", Status: StatusOK, Detail: long})
	out := b.String()
	if strings.Contains(out, "second") {
		t.Errorf("a detail's second line was printed:\n%s", out)
	}
	if !strings.Contains(out, long) {
		t.Errorf("a long detail was wrapped or truncated:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(line, " ") {
			t.Errorf("a line ends in whitespace: %q", line)
		}
	}
}

func TestColourIsOffWhenItIsNotAskedFor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		colour bool
		want   bool
	}{
		{"off", false, false},
		{"on", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			r := &Renderer{W: &b, Colour: tc.colour}
			_ = r.Start(commanderPlan())
			for _, res := range firstRunResults() {
				_ = r.Result(res)
			}
			rep := commanderReport(false, firstRunResults()...)
			_ = r.Finish(rep)
			if got := strings.Contains(b.String(), "\x1b["); got != tc.want {
				t.Errorf("escape sequences present = %v, want %v:\n%q", got, tc.want, b.String())
			}
		})
	}
}

func TestOnlyTheStatusWordsTheCountsAndTheBlockHeaderAreColoured(t *testing.T) {
	var b bytes.Buffer
	r := &Renderer{W: &b, Colour: true}
	_ = r.Start(commanderPlan())
	for _, res := range firstRunResults() {
		_ = r.Result(res)
	}
	_ = r.Finish(commanderReport(false, firstRunResults()...))
	for _, line := range strings.Split(b.String(), "\n") {
		if !strings.Contains(line, "\x1b[") {
			continue
		}
		coloured := between(line)
		for _, s := range coloured {
			if s == blockName || strings.HasPrefix(s, "ok=") || strings.HasPrefix(s, "changed=") ||
				strings.HasPrefix(s, "skipped=") || strings.HasPrefix(s, "failed=") {
				continue
			}
			if !contains(Statuses, s) {
				t.Errorf("%q is coloured, and only status words, the recap counts and %q are:\n%s",
					s, blockName, line)
			}
		}
	}
}

func between(line string) []string {
	var out []string
	rest := line
	for {
		i := strings.Index(rest, "m")
		start := strings.Index(rest, "\x1b[")
		if start < 0 {
			return out
		}
		rest = rest[start+2:]
		i = strings.Index(rest, "m")
		if i < 0 {
			return out
		}
		rest = rest[i+1:]
		end := strings.Index(rest, "\x1b[0m")
		if end < 0 {
			return out
		}
		out = append(out, rest[:end])
		rest = rest[end+4:]
	}
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func TestADurationReadsAsSeconds(t *testing.T) {
	for _, tc := range []struct {
		ms   int64
		want string
	}{
		{91, "0.09s"},
		{44, "0.04s"},
		{4200, "4.2s"},
		{300, "0.3s"},
		{1000, "1s"},
		{0, "0s"},
	} {
		if got := seconds(tc.ms); got != tc.want {
			t.Errorf("seconds(%d) = %q, want %q", tc.ms, got, tc.want)
		}
	}
	if got := shortDuration(6); got != "6ms" {
		t.Errorf("shortDuration(6) = %q, want 6ms", got)
	}
	if got := shortDuration(4200); got != "4.2s" {
		t.Errorf("shortDuration(4200) = %q, want 4.2s", got)
	}
}
