package env

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/state"
)

const (
	streamStdout = "stdout"
	streamStderr = "stderr"
)

func (m *Manager) Logs(ctx context.Context, req LogsRequest) error {
	rec, err := m.env(ctx, req.App, req.Name)
	if err != nil {
		return err
	}
	sources, err := m.logSources(ctx, rec, req)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return fmt.Errorf("env %q has nothing to show logs for: run 'caramelo up %s' first", req.Name, req.Name)
	}

	out := req.Stdout
	if out == nil {
		out = io.Discard
	}
	sink := &logSink{out: out, json: req.JSON, width: nameWidth(sources)}
	opts := runtime.LogOptions{Follow: req.Follow, Since: req.Since, Tail: req.Tail, Timestamps: req.JSON}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var problems []error
	for _, s := range sources {
		stdout, stderr := sink.writer(s.name, streamStdout), sink.writer(s.name, streamStderr)
		wg.Add(1)
		go func(s logSource) {
			defer wg.Done()
			defer func() { stdout.flush(); stderr.flush() }()
			err := m.Driver.Logs(ctx, s.container, opts, stdout, stderr)
			switch {
			case err == nil, errors.Is(err, context.Canceled):
			case errors.Is(err, runtime.ErrNotFound):
				progressf(req.Stderr, "warning", "logs", "%s has no container yet", s.name)
			default:
				mu.Lock()
				problems = append(problems, fmt.Errorf("logs of %s: %w", s.name, err))
				mu.Unlock()
			}
		}(s)
	}
	wg.Wait()
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	return nil
}

type logSource struct {
	name      string
	container string
}

func (m *Manager) logSources(ctx context.Context, rec *state.EnvRecord, req LogsRequest) ([]logSource, error) {
	rows, err := m.serviceRows(ctx, rec)
	if err != nil {
		return nil, err
	}
	named := len(req.Services) > 0
	if named {
		if rows, err = pickRows(rows, req.Services); err != nil {
			return nil, err
		}
	}
	out := make([]logSource, 0, len(rows))
	for _, r := range rows {

		live, err := m.liveReplicas(ctx, rec, r.Name)
		if err != nil {
			return nil, err
		}
		if len(live) == 0 {
			out = append(out, logSource{name: r.Name, container: ReplicaContainerName(rec.App, rec.Name, r.Name, 1)})
			continue
		}
		for _, rep := range live {
			name := r.Name
			if len(live) > 1 {
				name = replicaName(r.Name, rep.index)
			}
			out = append(out, logSource{name: name, container: rep.container})
		}
	}
	if !req.Deps || named {
		return out, nil
	}
	cfg, err := decodeConfig(rec)
	if err != nil {
		return nil, err
	}
	for _, d := range cfg.Deps {
		out = append(out, logSource{name: d.Name, container: ContainerName(rec.App, rec.Name, d.Name)})
	}
	return out, nil
}

func nameWidth(sources []logSource) int {
	w := 0
	for _, s := range sources {
		if len(s.name) > w {
			w = len(s.name)
		}
	}
	return w
}

type logSink struct {
	mu    sync.Mutex
	out   io.Writer
	json  bool
	width int
}

func (s *logSink) writer(service, stream string) *logWriter {
	return &logWriter{sink: s, service: service, stream: stream}
}

func (s *logSink) line(service, stream, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.json {
		fmt.Fprintf(s.out, "%-*s | %s\n", s.width, service, text)
		return
	}
	l := LogLine{Service: service, Stream: stream, Line: text}

	if ts, rest, ok := strings.Cut(text, " "); ok && looksLikeTimestamp(ts) {
		l.TS, l.Line = ts, rest
	}
	b, err := json.Marshal(l)
	if err != nil {
		return
	}

	_, _ = s.out.Write(append(b, '\n'))
}

type logWriter struct {
	sink    *logSink
	service string
	stream  string
	partial strings.Builder
}

func (w *logWriter) Write(p []byte) (int, error) {
	text := p
	for {
		i := indexByte(text, '\n')
		if i < 0 {
			w.partial.Write(text)
			return len(p), nil
		}
		line := w.partial.String() + string(text[:i])
		w.partial.Reset()
		w.sink.line(w.service, w.stream, strings.TrimSuffix(line, "\r"))
		text = text[i+1:]
	}
}

func (w *logWriter) flush() {
	if w.partial.Len() == 0 {
		return
	}
	w.sink.line(w.service, w.stream, w.partial.String())
	w.partial.Reset()
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func looksLikeTimestamp(s string) bool {
	const shortest = len("2006-01-02T15:04:05Z")
	if len(s) < shortest {
		return false
	}
	if s[4] != '-' || s[7] != '-' || s[10] != 'T' {
		return false
	}
	return strings.HasSuffix(s, "Z") || strings.ContainsAny(s[19:], "+-")
}
