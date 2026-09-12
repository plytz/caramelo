package ui

import (
	"fmt"
	"io"
	"strings"
	"sync"

	cprogress "github.com/plytz/caramelo/internal/progress"
)

func FeedLine(e cprogress.Event) string {
	at := "        "
	if !e.At.IsZero() {
		at = e.At.Local().Format("15:04:05")
	}
	where := "machine"
	if e.App != "" || e.Env != "" {
		where = e.App + "/" + e.Env
	}
	what := e.Action
	if e.Step != "" {
		what += " " + e.Step
	}
	if e.Service != "" {
		what += " " + e.Service
		if e.Replica > 0 {
			what += fmt.Sprintf("-%d", e.Replica)
		}
	}

	line := at
	if e.Machine != "" {
		line += fmt.Sprintf("  %-10s", truncate(e.Machine, 10))
	}
	line += fmt.Sprintf("  %-20s  %-8s  %-24s  %s",
		truncate(where, 20), e.Status, truncate(what, 24), e.Detail)
	if e.Identity != "" {
		line += "  (" + e.Identity + ")"
	}
	return strings.TrimRight(line, " ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

type PlainFeed struct {
	mu sync.Mutex
	w  io.Writer
}

func NewPlainFeed(w io.Writer) *PlainFeed { return &PlainFeed{w: w} }

func (p *PlainFeed) Event(e cprogress.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintln(p.w, FeedLine(e))
}

func (p *PlainFeed) Line(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintln(p.w, s)
}

func (p *PlainFeed) Close(error, string) error { return nil }
