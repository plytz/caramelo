package ui

import (
	"fmt"
	"io"
	"sync"

	cprogress "github.com/plytz/caramelo/internal/progress"
)

type Progress interface {
	Event(e cprogress.Event)

	Line(s string)

	Close(err error, detail string) error
}

type PlainWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func NewPlain(w io.Writer) *PlainWriter { return &PlainWriter{w: w} }

func (p *PlainWriter) Event(e cprogress.Event) { p.line(PlainLine(e)) }

func (p *PlainWriter) Line(s string) { p.line(s) }

func (p *PlainWriter) line(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintln(p.w, s)
}

func (p *PlainWriter) Close(error, string) error { return nil }
