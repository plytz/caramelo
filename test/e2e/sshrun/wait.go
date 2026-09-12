//go:build e2e

package sshrun

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const DefaultWaitInterval = 3 * time.Second

func WaitForSSH(ctx context.Context, dir string, h Host, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultWaitInterval
	}
	last := "no attempt finished"
	for {
		res, err := probe(ctx, dir, h)
		if err == nil && res.ExitCode == 0 {
			return nil
		}
		if errors.Is(err, ErrHostKeyChanged) {
			return fmt.Errorf("wait for ssh on %s: %w", h.label(), err)
		}
		aborted := res.ExitCode < 0 || ctx.Err() != nil
		if !aborted || last == "no attempt finished" {
			if err != nil {
				last = err.Error()
			} else {
				last = fmt.Sprintf("exit %d: %s", res.ExitCode, firstLine(res.Stderr+res.Stdout))
			}
		}
		if ctx.Err() != nil {
			return fmt.Errorf("wait for ssh on %s: %w (last attempt: %s)", h.label(), ctx.Err(), last)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("wait for ssh on %s: %w (last attempt: %s)", h.label(), ctx.Err(), last)
		case <-timer.C:
		}
	}
}

func probe(ctx context.Context, dir string, h Host) (Result, error) {
	args, err := baseOptions(dir, h, optionSet{control: false})
	if err != nil {
		return Result{}, err
	}
	args = append(args, "-p", fmt.Sprint(h.port()), h.Destination(), ShellCommand("true"))
	attempt, cancel := attemptContext(ctx)
	defer cancel()
	return runProcess(attempt, h, "ssh", args, false)
}

func attemptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	budget := 45 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline); left < budget {
			return context.WithCancel(ctx)
		}
	}
	return context.WithTimeout(ctx, budget)
}

func Reachable(ctx context.Context, dir string, h Host) (bool, error) {
	res, err := probe(ctx, dir, h)
	if errors.Is(err, ErrHostKeyChanged) {
		return false, err
	}
	if errors.Is(err, ErrUnreachable) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}
