package env

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/plytz/caramelo/internal/runner"
)

const killDelay = 5 * time.Second

func (m *Manager) Exec(ctx context.Context, req ExecRequest) (int, error) {
	if len(req.Argv) == 0 {
		return 0, errors.New("env exec: a command is required")
	}
	rec, err := m.env(ctx, req.App, req.Name)
	if err != nil {
		return 0, err
	}
	e, err := recordToEnv(rec)
	if err != nil {
		return 0, err
	}

	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Dir = rec.Worktree
	cmd.Env = m.childEnv(e.Vars)
	cmd.Stdout, cmd.Stderr = req.Stdout, req.Stderr
	stdin, closeStdin, err := stdinPipe(req.Stdin)
	if err != nil {
		return 0, err
	}
	defer closeStdin()
	if stdin != nil {
		cmd.Stdin = stdin
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}

		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return os.ErrProcessDone
	}
	cmd.WaitDelay = killDelay

	runErr := cmd.Run()
	code, ok := exitCode(cmd.ProcessState, runErr)
	if !ok {
		m.event(context.WithoutCancel(ctx), rec.ID, "exec", "failed", runErr.Error())
		return 0, fmt.Errorf("run %s in env %q: %w", req.Argv[0], req.Name, runErr)
	}
	status := "ok"
	if code != 0 {
		status = "failed"
	}
	m.event(context.WithoutCancel(ctx), rec.ID, "exec", status, fmt.Sprintf("%s (exit %d)", strings.Join(req.Argv, " "), code))
	return code, nil
}

func stdinPipe(r io.Reader) (*os.File, func(), error) {
	if r == nil {
		return nil, func() {}, nil
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("open a pipe for standard input: %w", err)
	}
	go func() {

		_, _ = io.Copy(pw, r)
		_ = pw.Close()
	}()
	return pr, func() { _, _ = pr.Close(), pw.Close() }, nil
}

func exitCode(st *os.ProcessState, err error) (code int, ok bool) {
	if st == nil {
		if err == nil {
			return 0, true
		}
		return 0, false
	}
	switch c := st.ExitCode(); {
	case c < 0:

		return 1, true
	case c == 255:
		return 1, true
	default:
		return c, true
	}
}

func (m *Manager) childEnv(vars map[string]string) []string {
	out := os.Environ()
	if m.Dirs.User != "" {
		if session, err := runner.SessionEnv(m.Dirs.User); err == nil {
			out = append(out, session...)
		}
	}
	return append(out, envPairs(vars)...)
}

func envPairs(vars map[string]string) []string {
	out := make([]string, 0, len(vars))
	for k, v := range vars {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
