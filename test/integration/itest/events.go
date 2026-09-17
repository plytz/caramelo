//go:build integration

package itest

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"

	cprogress "github.com/plytz/caramelo/internal/progress"
)

func ParseEvents(t *testing.T, what, stream string) []cprogress.Event {
	t.Helper()
	var out []cprogress.Event
	sc := bufio.NewScanner(strings.NewReader(stream))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var e cprogress.Event
		if err := json.Unmarshal([]byte(text), &e); err != nil {
			t.Fatalf("%s: line %d is not an event: %v\n%s", what, line, err, text)
		}
		out = append(out, e)
	}
	return out
}
