package task

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/runner"
)

var now = time.Now

type Leaf func(ctx context.Context, argv []string) (runner.Result, error)

type Engine struct {
	Runner runner.Runner

	Leaf Leaf

	Events io.Writer

	Observer func(res Result)

	Version string

	DryRun bool

	Machine string

	BinDir string

	Vars map[string]string

	Report ReportStore

	Shell string

	emitMu sync.Mutex
}

type state struct {
	mu      sync.Mutex
	stopped bool
}

func (s *state) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
}

func (s *state) stopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (e *Engine) Execute(ctx context.Context, f *File) (Report, error) {
	engineVars, err := MachineVars()
	if err != nil {
		return Report{}, err
	}
	r, err := render(f, engineVars, e.Vars)
	if err != nil {
		return Report{}, fmt.Errorf("%s: %w", f.Name, err)
	}
	start := now()
	rep := Report{
		RunID:   NewRunID(start),
		Task:    f.Name,
		Machine: e.Machine,
		Version: e.Version,
		DryRun:  e.DryRun,
		Results: []Result{},
	}
	st := &state{}
	rep.Results = e.runEntries(ctx, r.file.Items, st, r)
	rep.DurationMS = now().Sub(start).Milliseconds()
	rep.count()
	if !e.DryRun {
		path, err := e.Report.write(ctx, rep)
		if err != nil {
			rep.ReportErr = err
		}
		rep.Path = path
	}
	return rep, nil
}

func (e *Engine) Plan(f *File) (Plan, error) {
	engineVars, err := MachineVars()
	if err != nil {
		return Plan{}, err
	}
	r, err := render(f, engineVars, e.Vars)
	if err != nil {
		return Plan{}, fmt.Errorf("%s: %w", f.Name, err)
	}
	p := planOf(r.file)
	p.DryRun = e.DryRun
	p.Vars = r.order
	if e.Machine != "" {
		p.On = []string{e.Machine}
	}
	return p, nil
}

func (e *Engine) runEntries(ctx context.Context, items []Entry, st *state, r *rendered) []Result {
	out := make([]Result, len(items))
	for i, entry := range items {
		switch {
		case st.stopping():
			out[i] = notRun(entry)
		case entry.Block != nil:
			out[i] = e.runBlock(ctx, entry.Block, st, r)
		default:
			out[i] = e.runItem(ctx, entry.Item, st, r)
		}
	}
	return out
}

func (e *Engine) runBlock(ctx context.Context, b *Block, st *state, r *rendered) Result {
	start := now()
	res := Result{Name: blockName, Desc: b.Desc, Block: true, Results: make([]Result, len(b.Items))}
	var wg sync.WaitGroup
	for i, entry := range b.Items {
		wg.Add(1)
		go func(i int, entry Entry) {
			defer wg.Done()
			if entry.Block != nil {
				res.Results[i] = e.runBlock(ctx, entry.Block, st, r)
				return
			}
			res.Results[i] = e.runItem(ctx, entry.Item, st, r)
		}(i, entry)
	}
	wg.Wait()
	res.DurationMS = now().Sub(start).Milliseconds()
	res.Status = StatusOK
	for _, sub := range res.Results {
		if sub.Status == StatusFailed {
			res.Status = StatusFailed
			break
		}
	}
	return res
}

const blockName = "parallel"

func notRun(entry Entry) Result {
	if entry.Block == nil {
		return Result{Name: entry.Item.Name, Desc: entry.Item.Desc, Status: StatusNotRun}
	}
	res := Result{Name: blockName, Desc: entry.Block.Desc, Block: true,
		Status: StatusNotRun, Results: make([]Result, len(entry.Block.Items))}
	for i, sub := range entry.Block.Items {
		res.Results[i] = notRun(sub)
	}
	return res
}

func (e *Engine) runItem(ctx context.Context, it *Item, st *state, r *rendered) Result {
	res := Result{Name: it.Name, Desc: it.Desc}
	e.emit(r.file.Name, progress.Event{Step: it.Name, Status: progress.StatusStarted})
	start := now()
	e.decide(ctx, it, &res)
	res.DurationMS = now().Sub(start).Milliseconds()
	if res.Status == StatusFailed {
		st.stop()
	}
	e.emitResult(r.file.Name, res)
	return res
}

func (e *Engine) decide(ctx context.Context, it *Item, res *Result) {
	if len(it.Platforms) > 0 && !slices.Contains(it.Platforms, runtime.GOOS) {
		res.Status = StatusSkipped
		res.Detail = fmt.Sprintf("runs on %s; this machine is %s", strings.Join(it.Platforms, " or "), runtime.GOOS)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, it.timeout())
	defer cancel()

	if it.When != "" {
		out, err := e.run(ctx, it, KindWhen, it.When, res)
		if err != nil {
			e.fail(res, err)
			return
		}
		if out.ExitCode != 0 {
			res.Status = StatusSkipped
			res.Detail = firstLine(out.Stdout)
			if res.Detail == "" {
				res.Detail = firstLine(out.Stderr)
			}
			return
		}
	}

	if b := builtinOf(it); b != nil {
		e.decideBuiltin(it, b, res)
		return
	}

	check, err := e.run(ctx, it, KindCheck, it.Check, res)
	if err != nil {
		e.fail(res, err)
		return
	}
	if it.Check != "" && check.ExitCode == 0 {
		res.Status = StatusOK
		res.Detail = firstLine(check.Stdout)
		return
	}
	if e.DryRun {
		res.Status = StatusWouldChange
		res.Detail = firstLine(check.Stdout)
		return
	}

	var last runner.Result
	for _, script := range it.scripts() {
		last, err = e.run(ctx, it, KindCmd, script, res)
		if err != nil {
			e.fail(res, err)
			return
		}
		if last.ExitCode != 0 {
			res.Status = StatusFailed
			res.Error = fmt.Sprintf("cmd exit %d", last.ExitCode)
			return
		}
	}
	if it.Check == "" {
		res.Status = StatusChanged
		res.Detail = firstLine(last.Stdout)
		return
	}
	again, err := e.run(ctx, it, KindCheck, it.Check, res)
	if err != nil {
		e.fail(res, err)
		return
	}
	if again.ExitCode != 0 {
		res.Status = StatusFailed
		res.Error = "cmd ran but the check still fails"
		return
	}
	res.Status = StatusChanged
	res.Detail = firstLine(last.Stdout)
	if res.Detail == "" {
		res.Detail = firstLine(again.Stdout)
	}
}

func (e *Engine) decideBuiltin(it *Item, b builtin, res *Result) {
	done, detail, err := b.check()
	if err != nil {
		e.fail(res, err)
		return
	}
	if done {
		res.Status, res.Detail = StatusOK, detail
		return
	}
	if e.DryRun {
		res.Status, res.Detail = StatusWouldChange, detail
		return
	}
	applied, err := b.apply()
	if err != nil {
		e.fail(res, err)
		return
	}
	done, again, err := b.check()
	if err != nil {
		e.fail(res, err)
		return
	}
	if !done {
		res.Status = StatusFailed
		res.Error = "the built-in ran but the check still fails: " + again
		return
	}
	res.Status, res.Detail = StatusChanged, applied
}

func (e *Engine) fail(res *Result, err error) {
	res.Status = StatusFailed
	res.Error = err.Error()
}

func (e *Engine) run(ctx context.Context, it *Item, kind, script string, res *Result) (runner.Result, error) {
	if script == "" {
		return runner.Result{}, nil
	}
	start := now()
	out, err := e.exec(ctx, script)
	rec := Command{Kind: kind, Script: script, Exit: out.ExitCode,
		DurationMS: now().Sub(start).Milliseconds(), Stdout: out.Stdout, Stderr: out.Stderr}
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			err = fmt.Errorf("%s: %s did not finish inside %s", it.Name, kind, it.timeout())
		}
		rec.Exit = -1
		res.Commands = append(res.Commands, rec)
		return out, err
	}
	res.Commands = append(res.Commands, rec)
	return out, nil
}

func (e *Engine) exec(ctx context.Context, script string) (runner.Result, error) {
	if argv, ok := InProcess(script); ok && e.Leaf != nil {
		res, err := e.Leaf(ctx, argv)
		if err != nil {
			return res, fmt.Errorf("caramelo %s: %w", strings.Join(argv[1:], " "), err)
		}
		return res, nil
	}
	if e.Runner == nil {
		return runner.Result{}, errors.New("this engine has no runner: nothing can execute a script")
	}
	res, err := e.Runner.Run(ctx, runner.Cmd{
		Name: e.shell(),
		Args: []string{"-c", "set -eu\n" + script},
		Env:  e.env(),
	})
	if err != nil {
		return res, fmt.Errorf("run %q: %w", firstLine(script), err)
	}
	return res, nil
}

func (e *Engine) shell() string {
	if e.Shell != "" {
		return e.Shell
	}
	return "/bin/sh"
}

func (e *Engine) env() []string {
	dir := e.BinDir
	if dir == "" {
		if exe, err := os.Executable(); err == nil {
			dir = filepath.Dir(exe)
		}
	}
	if dir == "" {
		return nil
	}
	path := dir
	if rest := os.Getenv("PATH"); rest != "" {
		path += string(os.PathListSeparator) + rest
	}
	return []string{"PATH=" + path}
}

func (e *Engine) emit(task string, ev progress.Event) {
	e.emitMu.Lock()
	defer e.emitMu.Unlock()
	e.write(task, ev)
}

func (e *Engine) write(task string, ev progress.Event) {
	if e.Events == nil {
		return
	}
	ev.Action = progress.ActionTask
	ev.Task = task
	ev.Machine = e.Machine
	if ev.At.IsZero() {
		ev.At = now().UTC()
	}
	_ = progress.Emit(e.Events, ev)
}

func (e *Engine) emitResult(task string, res Result) {
	ev := progress.Event{Step: res.Name, Status: res.Status, Detail: res.Detail}
	if res.Status == StatusFailed && res.Detail == "" {
		ev.Detail = res.Error
	}
	e.emitMu.Lock()
	defer e.emitMu.Unlock()
	e.write(task, ev)
	if e.Observer != nil {
		e.Observer(res)
	}
}
