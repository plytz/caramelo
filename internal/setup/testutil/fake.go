package testutil

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/plytz/caramelo/internal/runner"
)

type Call struct {
	Cmd   runner.Cmd
	Line  string
	Stdin string
}

type FakeRunner struct {
	mu       sync.Mutex
	exact    map[string]response
	prefixes map[string]response
	calls    []Call

	DefaultResult runner.Result

	Strict bool

	Handler func(c runner.Cmd) (res runner.Result, err error, ok bool)
}

type response struct {
	res runner.Result
	err error
}

func New() *FakeRunner {
	return &FakeRunner{exact: map[string]response{}, prefixes: map[string]response{}}
}

func Key(c runner.Cmd) string {
	if len(c.Args) == 0 {
		return c.Name
	}
	return c.Name + " " + strings.Join(c.Args, " ")
}

func (f *FakeRunner) Respond(line string, res runner.Result) *FakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exact[line] = response{res: res}
	return f
}

func (f *FakeRunner) RespondPrefix(prefix string, res runner.Result) *FakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prefixes[prefix] = response{res: res}
	return f
}

func (f *FakeRunner) Stdout(line, out string) *FakeRunner {
	return f.Respond(line, runner.Result{Stdout: out})
}

func (f *FakeRunner) StdoutPrefix(prefix, out string) *FakeRunner {
	return f.RespondPrefix(prefix, runner.Result{Stdout: out})
}

func (f *FakeRunner) Exit(line string, code int) *FakeRunner {
	return f.Respond(line, runner.Result{ExitCode: code})
}

func (f *FakeRunner) ExitPrefix(prefix string, code int) *FakeRunner {
	return f.RespondPrefix(prefix, runner.Result{ExitCode: code})
}

func (f *FakeRunner) Fail(line string, err error) *FakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exact[line] = response{err: err}
	return f
}

func (f *FakeRunner) Run(ctx context.Context, c runner.Cmd) (runner.Result, error) {
	if err := ctx.Err(); err != nil {
		return runner.Result{}, err
	}
	line := Key(c)
	var stdin string
	if c.Stdin != nil {
		b, err := io.ReadAll(c.Stdin)
		if err != nil {
			return runner.Result{}, fmt.Errorf("fake runner: read stdin of %q: %w", line, err)
		}
		stdin = string(b)
	}

	f.mu.Lock()
	f.calls = append(f.calls, Call{Cmd: c, Line: line, Stdin: stdin})
	handler := f.Handler
	r, ok := f.exact[line]
	if !ok {
		best := ""
		for p := range f.prefixes {
			if strings.HasPrefix(line, p) && len(p) >= len(best) {
				best, r, ok = p, f.prefixes[p], true
			}
		}
	}
	strict, def := f.Strict, f.DefaultResult
	f.mu.Unlock()

	if handler != nil {
		if res, err, handled := handler(c); handled {
			return res, err
		}
	}
	if ok {
		return r.res, r.err
	}
	if strict {
		return runner.Result{}, fmt.Errorf("fake runner: unscripted command %q", line)
	}
	return def, nil
}

func (f *FakeRunner) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

func (f *FakeRunner) Lines() []string {
	out := []string{}
	for _, c := range f.Calls() {
		out = append(out, c.Line)
	}
	return out
}

func (f *FakeRunner) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func (f *FakeRunner) Ran(prefix string) bool { return f.Count(prefix) > 0 }

func (f *FakeRunner) Count(prefix string) int {
	n := 0
	for _, c := range f.Calls() {
		if strings.HasPrefix(c.Line, prefix) {
			n++
		}
	}
	return n
}

func (f *FakeRunner) Find(prefix string) (Call, bool) {
	for _, c := range f.Calls() {
		if strings.HasPrefix(c.Line, prefix) {
			return c, true
		}
	}
	return Call{}, false
}

func (f *FakeRunner) Users() []string {
	seen := map[string]bool{}
	for _, c := range f.Calls() {
		seen[c.Cmd.User] = true
	}
	out := []string{}
	for u := range seen {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

func (f *FakeRunner) Transcript() string {
	var b strings.Builder
	for _, c := range f.Calls() {
		if c.Cmd.User != "" {
			b.WriteString("(" + c.Cmd.User + ") ")
		}
		b.WriteString(c.Line)
		if c.Stdin != "" {
			fmt.Fprintf(&b, " <<%q", c.Stdin)
		}
		b.WriteString("\n")
	}
	return b.String()
}
