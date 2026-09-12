package sshapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/setup"
)

const EdgeLogUnit = setup.EdgeServiceUnit

func (d *Daemon) edgeLogs(ctx context.Context, req env.LogsRequest) error {
	if !d.Config.Edge {
		return fmt.Errorf("this machine has no edge, so it serves no public requests: " +
			"run 'caramelo edge enable' on it")
	}
	out := req.Stdout
	if out == nil {
		out = io.Discard
	}
	var n int
	write := d.edgeLogLine(req, out, &n)

	args := []string{"--no-pager", "--output", "cat", "--unit", EdgeLogUnit}
	if req.Follow {
		args = append(args, "--follow")
	}
	if since := journalSince(req.Since); since != "" {
		args = append(args, "--since", since)
	}
	switch {
	case req.Tail > 0:
		args = append(args, "--lines", fmt.Sprintf("%d", req.Tail))
	case !req.Follow:

		args = append(args, "--lines", "all")
	}

	lines := &lineWriter{write: write}
	res, err := d.Runner.Run(ctx, runner.Cmd{Name: "journalctl", Args: args, Stdout: lines})
	lines.flush()
	if err != nil {

		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("read the edge's access log: %w", err)
	}
	if res.ExitCode != 0 && ctx.Err() == nil {
		return fmt.Errorf("read the edge's access log: journalctl exited %d: %s",
			res.ExitCode, strings.TrimSpace(firstLine(res.Stderr)))
	}
	if n == 0 && !req.Follow {
		fmt.Fprintln(errWriter(req), "caramelo: no public requests in the edge's log yet")
	}
	return nil
}

func (d *Daemon) edgeLogLine(req env.LogsRequest, out io.Writer, n *int) func(string) {
	return func(line string) {
		entry, ok := parseAccessLog(line)
		if !ok {
			return
		}

		if req.Name != "" && !entry.matches(req.App, req.Name) {
			return
		}
		*n++
		if req.JSON {

			fmt.Fprintln(out, line[len(edge.AccessLogPrefix)+1:])
			return
		}
		fmt.Fprintln(out, formatAccessLog(entry))
	}
}

type accessEntry struct{ edge.AccessLog }

func (e accessEntry) matches(app, name string) bool {
	if e.Env != name {
		return false
	}
	return app == "" || e.App == "" || e.App == app
}

func parseAccessLog(line string) (accessEntry, bool) {
	rest, ok := strings.CutPrefix(line, edge.AccessLogPrefix+" ")
	if !ok {
		return accessEntry{}, false
	}
	var entry accessEntry
	if err := json.Unmarshal([]byte(rest), &entry.AccessLog); err != nil {
		return accessEntry{}, false
	}
	return entry, true
}

func formatAccessLog(e accessEntry) string {
	var b strings.Builder
	if !e.At.IsZero() {
		b.WriteString(e.At.Local().Format(time.RFC3339))
		b.WriteByte(' ')
	}
	fmt.Fprintf(&b, "%s %s %s", strOr(e.Method, "-"), strOr(e.Host, "-"), strOr(e.Path, "/"))
	if e.Status != 0 {
		fmt.Fprintf(&b, " %d", e.Status)
	}
	if e.Duration > 0 {
		fmt.Fprintf(&b, " %s", e.Duration.Round(time.Millisecond))
	}
	if e.Bytes > 0 {
		fmt.Fprintf(&b, " %db", e.Bytes)
	}
	target := e.Target
	if e.Service != "" {
		target = fmt.Sprintf("%s/%d", e.Service, e.Replica)
		if e.Target != "" {
			target += " (" + e.Target + ")"
		}
	}
	if target != "" {
		fmt.Fprintf(&b, " -> %s", target)
	}
	if e.Proto != "" {
		fmt.Fprintf(&b, " %s", e.Proto)
	}
	if e.WebSocket {
		b.WriteString(" websocket")
	}
	if e.Client != "" {
		fmt.Fprintf(&b, " %s", e.Client)
	}
	if e.Error != "" {
		fmt.Fprintf(&b, " error=%s", e.Error)
	}
	return b.String()
}

func journalSince(since string) string {
	since = strings.TrimSpace(since)
	if since == "" {
		return ""
	}
	if _, err := time.ParseDuration(since); err == nil {
		return "-" + since
	}
	return since
}

func errWriter(req env.LogsRequest) io.Writer {
	if req.Stderr == nil {
		return io.Discard
	}
	return req.Stderr
}

func strOr(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

type lineWriter struct {
	write func(string)
	buf   bytes.Buffer
}

func (w *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.buf.Write(p)
			return n, nil
		}
		w.buf.Write(p[:i])
		w.emit()
		p = p[i+1:]
	}
}

func (w *lineWriter) flush() {
	if w.buf.Len() > 0 {
		w.emit()
	}
}

func (w *lineWriter) emit() {
	line := strings.TrimRight(w.buf.String(), "\r")
	w.buf.Reset()
	if line != "" {
		w.write(line)
	}
}
