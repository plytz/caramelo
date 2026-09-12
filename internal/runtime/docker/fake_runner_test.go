package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/plytz/caramelo/internal/runner"
)

type fakeRunner struct {
	rules []rule
	def   runner.Result
	calls []runner.Cmd

	stream bool
	stdout string
	stderr string
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
	res, err := f.answer(cmdKey(c))
	if !f.stream {
		return res, err
	}

	out, errOut := f.stdout+res.Stdout, f.stderr+res.Stderr
	res.Stdout, res.Stderr = "", ""
	if c.Stdout != nil {
		fmt.Fprint(c.Stdout, out)
	}
	if c.Stderr != nil {
		fmt.Fprint(c.Stderr, errOut)
	}
	return res, err
}

func (f *fakeRunner) answer(k string) (runner.Result, error) {
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

func cmdKey(c runner.Cmd) string {
	return strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " "))
}

func ok(stdout string) runner.Result { return runner.Result{Stdout: stdout} }

func fail(code int, stderr string) runner.Result {
	return runner.Result{ExitCode: code, Stderr: stderr}
}
