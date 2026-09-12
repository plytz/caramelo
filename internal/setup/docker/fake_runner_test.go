package dockersetup

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup"
)

type fakeRunner struct {
	rules []rule
	def   runner.Result
	calls []runner.Cmd

	hook func(runner.Cmd) (runner.Result, bool)
}

type rule struct {
	match string
	res   runner.Result
	err   error
}

func (f *fakeRunner) on(match string, res runner.Result) *fakeRunner {
	f.rules = append(f.rules, rule{match: match, res: res})
	return f
}

func (f *fakeRunner) onErr(match string, err error) *fakeRunner {
	f.rules = append(f.rules, rule{match: match, err: err})
	return f
}

func (f *fakeRunner) Run(_ context.Context, c runner.Cmd) (runner.Result, error) {
	f.calls = append(f.calls, c)
	if f.hook != nil {
		if res, handled := f.hook(c); handled {
			return res, nil
		}
	}
	k := cmdKey(c)

	for _, r := range f.rules {
		if k == r.match {
			return r.res, r.err
		}
	}
	for _, r := range f.rules {
		if strings.HasPrefix(k, r.match) {
			return r.res, r.err
		}
	}
	return f.def, nil
}

func (f *fakeRunner) log() []string {
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, cmdKey(c))
	}
	return out
}

func (f *fakeRunner) indexOf(prefix string, start int) int {
	for i, line := range f.log() {
		if i >= start && strings.HasPrefix(line, prefix) {
			return i
		}
	}
	return -1
}

func cmdKey(c runner.Cmd) string {
	return strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " "))
}

func ok(stdout string) runner.Result { return runner.Result{Stdout: stdout} }
func fail(code int, stderr string) runner.Result {
	return runner.Result{ExitCode: code, Stderr: stderr}
}

func testEnv(f *fakeRunner) *setup.Env {
	cfg := serverconfig.Default()
	return &setup.Env{
		Config: cfg,
		Opts:   setup.Options{InstallPackages: true, Yes: true},
		Run:    f,
		Log:    io.Discard,
	}
}

func requireOrder(t *testing.T, f *fakeRunner, prefixes ...string) {
	t.Helper()
	prev := -1
	for _, p := range prefixes {
		i := f.indexOf(p, prev+1)
		if i < 0 {
			t.Fatalf("command %q never ran after index %d; log:\n%s",
				p, prev, strings.Join(f.log(), "\n"))
		}
		prev = i
	}
}
