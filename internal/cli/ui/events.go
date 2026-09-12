package ui

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"

	"github.com/plytz/caramelo/internal/progress"
)

func ParseEvent(line string) (progress.Event, bool) {
	s := strings.TrimSpace(line)
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return progress.Event{}, false
	}
	var e progress.Event

	if err := json.Unmarshal([]byte(s), &e); err != nil {
		return progress.Event{}, false
	}
	if e.Action == "" || e.Status == "" {
		return progress.Event{}, false
	}
	return e, true
}

func PlainLine(e progress.Event) string {
	if e.Action == progress.ActionMessage {
		return e.Detail
	}
	return progress.Line(e)
}

const maxLine = 1 << 20

type Sink struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	onEvent func(progress.Event)
	onLine  func(string)
	closed  bool
}

func NewSink(onEvent func(progress.Event), onLine func(string)) *Sink {
	return &Sink{onEvent: onEvent, onLine: onLine}
}

func (s *Sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return len(p), nil
	}
	s.buf.Write(p)
	for {
		line, err := s.buf.ReadString('\n')
		if err != nil {

			s.buf.Reset()
			s.buf.WriteString(line)
			break
		}
		s.deliver(strings.TrimRight(line, "\r\n"))
	}

	if s.buf.Len() > maxLine {
		s.deliver(s.buf.String())
		s.buf.Reset()
	}
	return len(p), nil
}

func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if rest := strings.TrimRight(s.buf.String(), "\r\n"); rest != "" {
		s.deliver(rest)
	}
	s.buf.Reset()
	return nil
}

func (s *Sink) deliver(line string) {
	if e, ok := ParseEvent(line); ok {
		if s.onEvent != nil {
			s.onEvent(e)
		}
		return
	}
	if s.onLine != nil {
		s.onLine(line)
	}
}

func SinkFor(p Progress) *Sink {
	if p == nil {
		return NewSink(nil, nil)
	}
	return NewSink(p.Event, p.Line)
}
