package progress

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

type Event struct {
	Seq int64 `json:"seq,omitempty"`

	App string `json:"app,omitempty"`
	Env string `json:"env,omitempty"`

	Service string `json:"service,omitempty"`
	Replica int    `json:"replica,omitempty"`

	Action string `json:"action"`

	Task string `json:"task,omitempty"`

	Step string `json:"step,omitempty"`

	Status string `json:"status"`

	Detail string `json:"detail,omitempty"`

	Identity string `json:"identity,omitempty"`

	Machine string `json:"machine,omitempty"`

	At time.Time `json:"at"`

	JSON json.RawMessage `json:"json,omitempty"`
}

const (
	StatusStarted = "started"

	StatusOK = "ok"

	StatusChanged = "changed"

	StatusSkipped = "skipped"

	StatusWouldChange = "would-change"

	StatusWarning = "warning"

	StatusFailed = "failed"
)

var Statuses = []string{StatusStarted, StatusOK, StatusChanged, StatusWouldChange, StatusSkipped, StatusWarning, StatusFailed}

type Format string

const (
	FormatText Format = "text"

	FormatJSON Format = "json"
)

var Formats = []Format{FormatText, FormatJSON}

func ParseFormat(s string) (Format, error) {
	switch f := Format(strings.ToLower(strings.TrimSpace(s))); f {
	case "", FormatText:
		return FormatText, nil
	case FormatJSON:
		return FormatJSON, nil
	default:
		return "", fmt.Errorf("unknown progress format %q: want %s or %s", s, FormatText, FormatJSON)
	}
}

func (f Format) String() string {
	if f == "" {
		return string(FormatText)
	}
	return string(f)
}

func Line(e Event) string {
	if e.Detail == "" {
		return "[" + e.Status + "] " + e.Action
	}
	return "[" + e.Status + "] " + e.Action + ": " + e.Detail
}

type Writer struct {
	mu     sync.Mutex
	w      io.Writer
	format Format

	enc *json.Encoder

	Now func() time.Time
}

func New(w io.Writer, f Format) *Writer {
	if w == nil {
		return nil
	}
	if f == "" {
		f = FormatText
	}
	pw := &Writer{w: w, format: f}
	if f == FormatJSON {
		pw.enc = json.NewEncoder(w)
	}
	return pw
}

func (p *Writer) Format() Format {
	if p == nil {
		return FormatText
	}
	return p.format
}

func (p *Writer) Emit(e Event) error {
	if p == nil {
		return nil
	}
	if e.At.IsZero() {
		e.At = p.now()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.format == FormatJSON {
		if err := p.enc.Encode(e); err != nil {
			return fmt.Errorf("write progress event: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprintln(p.w, Line(e)); err != nil {
		return fmt.Errorf("write progress line: %w", err)
	}
	return nil
}

func (p *Writer) Write(b []byte) (int, error) {
	if p == nil {
		return len(b), nil
	}
	if p.format != FormatJSON {
		p.mu.Lock()
		defer p.mu.Unlock()
		n, err := p.w.Write(b)
		if err != nil {
			return n, fmt.Errorf("write progress: %w", err)
		}
		return n, nil
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if err := p.Emit(Event{Action: ActionMessage, Status: StatusWarning, Detail: line}); err != nil {
			return 0, err
		}
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("write progress: %w", err)
	}
	return len(b), nil
}

const ActionMessage = "message"

const ActionTask = "task"

func (p *Writer) now() time.Time {
	if p != nil && p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

type Emitter interface {
	Emit(e Event) error
}

func Emit(w io.Writer, e Event) error {
	if w == nil {
		return nil
	}
	if pw, ok := w.(Emitter); ok {
		return pw.Emit(e)
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if _, err := fmt.Fprintln(w, Line(e)); err != nil {
		return fmt.Errorf("write progress line: %w", err)
	}
	return nil
}

type Notifier interface {
	Notify(e Event)

	Subscribe(buffer int) (<-chan Event, func())
}

type Nop struct{}

func (Nop) Notify(Event) {}

func (Nop) Subscribe(int) (<-chan Event, func()) {
	ch := make(chan Event)
	close(ch)
	return ch, func() {}
}

const DefaultBuffer = 256
