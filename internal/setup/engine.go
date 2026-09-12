package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/runner"
)

var lookupUser = user.Lookup

var now = time.Now

func Execute(ctx context.Context, steps []Step, env *Env, report func(Result)) Report {
	rep := Report{RunID: NewRunID(now()), Version: env.Version, DryRun: env.DryRun, Results: []Result{}}
	for _, step := range steps {
		res := runStep(ctx, step, env)
		rep.Results = append(rep.Results, res)
		switch res.Status {
		case StatusChanged:
			rep.Changed++
		case StatusFailed:
			rep.Failed++
		}
		if report != nil {
			report(res)
		}
		logResult(env, res)
		if res.Status == StatusFailed {
			break
		}
	}
	if err := writeReport(ctx, env, rep); err != nil {
		logf(env, "warning: could not record this run: %v", err)
	}
	return rep
}

func runStep(ctx context.Context, step Step, env *Env) (res Result) {
	start := now()
	res = Result{Step: step.Name()}
	defer func() { res.Duration = now().Sub(start) }()

	done, detail, err := step.Check(ctx, env)
	res.Detail = detail
	var skip Skip
	switch {
	case errors.As(err, &skip):
		res.Status = StatusSkipped
		if res.Detail == "" {
			res.Detail = skip.Reason
		}
		return res
	case err != nil:
		res.Status, res.Error = StatusFailed, err.Error()
		return res
	case done:
		res.Status = StatusOK
		return res
	case env.DryRun:
		res.Status = StatusWouldChange
		return res
	}

	if err := step.Apply(ctx, env); err != nil {
		res.Status, res.Error = StatusFailed, err.Error()
		return res
	}
	res.Status = StatusChanged

	if done, detail, err := step.Check(ctx, env); err == nil && done && detail != "" {
		res.Detail = detail
	}
	return res
}

func NewRunID(t time.Time) string {
	return t.UTC().Format("20060102T150405.000Z")
}

func logResult(env *Env, res Result) {
	msg := res.Detail
	if res.Status == StatusFailed {
		msg = res.Error
	}
	if msg == "" {
		logf(env, "[%s] %s", res.Status, res.Step)
		return
	}
	logf(env, "[%s] %s: %s", res.Status, res.Step, msg)
}

func writeReport(ctx context.Context, env *Env, rep Report) error {
	if env.DryRun {
		return nil
	}
	cfg := env.Config
	if cfg.StateDir == "" || cfg.User == "" {
		return nil
	}
	if _, err := lookupUser(cfg.User); err != nil {

		return nil
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	dir := filepath.Join(cfg.StateDir, "setup")
	if err := (dirSpec{Path: dir, Mode: "0750", AsUser: cfg.User}).apply(ctx, env); err != nil {
		return err
	}
	path := filepath.Join(dir, rep.RunID+".json")
	res, err := runCmd(ctx, env, runner.Cmd{
		Name: "tee", Args: []string{"--", path}, User: cfg.User,
		Stdin: strings.NewReader(string(b) + "\n"),
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("write %s: exit %d: %s", path, res.ExitCode, firstLine(res.Stderr))
	}
	if _, err := mustRun(ctx, env, runner.Cmd{Name: "chmod", Args: []string{"0640", "--", path}, User: cfg.User}); err != nil {
		return err
	}
	return nil
}
