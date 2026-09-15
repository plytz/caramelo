//go:build integration

package itest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

const PTYTool = "script"

const (
	PTYRows = 40
	PTYCols = 120
)

func PTYAvailable() bool {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return false
	}
	_, err := exec.LookPath(PTYTool)
	return err == nil
}

type PTYResult struct {
	Output   string
	ExitCode int
}

func (r PTYResult) Lines() []string {
	text := strings.ReplaceAll(r.Output, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	out := strings.Split(text, "\n")
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return out
}

func (r PTYResult) LastFrame() string {
	text := r.Output
	for _, marker := range []string{"\x1b[2J", "\x1b[H"} {
		if at := strings.LastIndex(text, marker); at >= 0 {
			text = text[at+len(marker):]
		}
	}
	return text
}

func PTYRun(ctx context.Context, dir string, env []string, argv ...string) (PTYResult, error) {
	if len(argv) == 0 {
		return PTYResult{}, errors.New("PTYRun: nothing to run")
	}
	if !PTYAvailable() {
		return PTYResult{}, fmt.Errorf(
			"PTYRun: this host (%s) cannot lend a terminal: %s is not on PATH, or this operating system has no port of it",
			runtime.GOOS, PTYTool)
	}
	line := ShellLine(argv)
	cmd := exec.CommandContext(ctx, PTYTool, hostPTYArgs(line)...)
	killProcessGroup(cmd)
	cmd.Dir = dir
	cmd.Env = ptyEnv(env)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()

	res := PTYResult{Output: trimPTYPrologue(out.String())}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	case errors.Is(err, exec.ErrWaitDelay):
	default:
		return res, fmt.Errorf("%s %q: %w", PTYTool, line, err)
	}
	if ctx.Err() != nil {
		return res, fmt.Errorf("%s %q: %w", PTYTool, line, ctx.Err())
	}
	return res, nil
}

func hostPTYArgs(line string) []string {
	if runtime.GOOS == "darwin" {
		return []string{"-q", "/dev/null", "/bin/sh", "-c", line}
	}
	return []string{"-qe", "-c", line, "/dev/null"}
}

func trimPTYPrologue(s string) string {
	return strings.TrimPrefix(strings.TrimPrefix(s, "^D"), "\b\b")
}

func ptyEnv(env []string) []string {
	if env == nil {
		return nil
	}
	out := append([]string(nil), env...)
	for _, kv := range ptyTerminalEnv() {
		key, _, _ := strings.Cut(kv, "=")
		if !hasEnvKey(out, key) {
			out = append(out, kv)
		}
	}
	return out
}

func ptyTerminalEnv() []string {
	return []string{
		"TERM=xterm-256color",
		fmt.Sprintf("COLUMNS=%d", PTYCols),
		fmt.Sprintf("LINES=%d", PTYRows),
	}
}

func hasEnvKey(env []string, key string) bool {
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k == key {
			return true
		}
	}
	return false
}

func (m *Machine) PTYRun(ctx context.Context, dir string, env []string, argv ...string) (PTYResult, error) {
	if len(argv) == 0 {
		return PTYResult{}, errors.New("PTYRun: nothing to run")
	}
	for _, kv := range env {
		if !strings.Contains(kv, "=") {
			return PTYResult{}, fmt.Errorf("PTYRun on %s: %q is not a KEY=VALUE entry", m.Alias, kv)
		}
	}
	if dir == "" {
		dir = m.Home()
	}
	line := fmt.Sprintf("stty rows %d cols %d 2>/dev/null; exec %s", PTYRows, PTYCols, ShellLine(argv))
	return m.drv.pty(ctx, dir, env, line)
}

func PTYRunOnCommander(ctx context.Context, m *Machine, dir string, env []string, argv ...string) (PTYResult, error) {
	if m == nil {
		return PTYResult{}, errors.New("PTYRunOnCommander: no machine to run on")
	}
	return m.PTYRun(ctx, dir, env, argv...)
}

func ShellLine(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, ShellQuote(arg))
	}
	return strings.Join(quoted, " ")
}
