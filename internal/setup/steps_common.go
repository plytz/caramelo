package setup

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/runner"
)

var (
	geteuid = os.Geteuid

	sleep = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
)

func runCmd(ctx context.Context, env *Env, c runner.Cmd) (runner.Result, error) {
	if env.Run == nil {
		return runner.Result{}, fmt.Errorf("no command runner")
	}
	res, err := env.Run.Run(ctx, c)
	if err != nil {
		return res, fmt.Errorf("%s: %w", cmdLine(c), err)
	}
	return res, nil
}

func mustRun(ctx context.Context, env *Env, c runner.Cmd) (string, error) {
	res, err := runCmd(ctx, env, c)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%s: exit %d: %s", cmdLine(c), res.ExitCode, firstLine(res.Stderr, res.Stdout))
	}
	return strings.TrimRight(res.Stdout, "\n"), nil
}

func succeeds(ctx context.Context, env *Env, c runner.Cmd) (bool, error) {
	res, err := runCmd(ctx, env, c)
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

func cmdLine(c runner.Cmd) string {
	line := c.Name
	if len(c.Args) > 0 {
		line += " " + strings.Join(c.Args, " ")
	}
	if c.User != "" {
		line = "(as " + c.User + ") " + line
	}
	return line
}

func firstLine(candidates ...string) string {
	for _, s := range candidates {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			return s[:i]
		}
		return s
	}
	return ""
}

func logf(env *Env, format string, args ...any) {
	if env.Log == nil {
		return
	}
	fmt.Fprintf(env.Log, format+"\n", args...)
}

type pathInfo struct {
	Exists bool
	Owner  string
	Group  string
	Mode   string
	Type   string
}

func statPath(ctx context.Context, env *Env, path string) (pathInfo, error) {
	res, err := runCmd(ctx, env, runner.Cmd{Name: "stat", Args: []string{"-c", "%U:%G:%a:%F", "--", path}})
	if err != nil {
		return pathInfo{}, err
	}
	if res.ExitCode != 0 {
		return pathInfo{}, nil
	}
	parts := strings.SplitN(strings.TrimSpace(res.Stdout), ":", 4)
	if len(parts) != 4 {
		return pathInfo{}, fmt.Errorf("stat %s: unexpected output %q", path, res.Stdout)
	}
	return pathInfo{Exists: true, Owner: parts[0], Group: parts[1], Mode: normalMode(parts[2]), Type: parts[3]}, nil
}

func normalMode(m string) string {
	m = strings.TrimSpace(m)
	for len(m) < 4 {
		m = "0" + m
	}
	return m
}

func readFile(ctx context.Context, env *Env, path string) (content string, exists bool, err error) {
	res, err := runCmd(ctx, env, runner.Cmd{Name: "cat", Args: []string{"--", path}})
	if err != nil {
		return "", false, err
	}
	if res.ExitCode != 0 {
		return "", false, nil
	}
	return res.Stdout, true, nil
}

type dirSpec struct {
	Path  string
	Mode  string
	Owner string
	Group string

	AsUser string
}

func (d dirSpec) check(ctx context.Context, env *Env) (bool, string, error) {
	st, err := statPath(ctx, env, d.Path)
	if err != nil {
		return false, "", err
	}
	switch {
	case !st.Exists:
		return false, d.Path + " missing", nil
	case st.Type != "directory":
		return false, d.Path + " is not a directory", nil
	case d.Owner != "" && (st.Owner != d.Owner || st.Group != d.Group):
		return false, fmt.Sprintf("%s owned by %s:%s, want %s:%s", d.Path, st.Owner, st.Group, d.Owner, d.Group), nil
	case d.Mode != "" && st.Mode != normalMode(d.Mode):
		return false, fmt.Sprintf("%s mode %s, want %s", d.Path, st.Mode, normalMode(d.Mode)), nil
	}
	return true, "", nil
}

func (d dirSpec) apply(ctx context.Context, env *Env) error {
	if _, err := mustRun(ctx, env, runner.Cmd{Name: "mkdir", Args: []string{"-p", "--", d.Path}, User: d.AsUser}); err != nil {
		return err
	}
	if d.Owner != "" {
		if _, err := mustRun(ctx, env, runner.Cmd{Name: "chown", Args: []string{d.Owner + ":" + d.Group, "--", d.Path}}); err != nil {
			return err
		}
	}
	if d.Mode != "" {
		if _, err := mustRun(ctx, env, runner.Cmd{Name: "chmod", Args: []string{d.Mode, "--", d.Path}}); err != nil {
			return err
		}
	}
	return nil
}

type fileSpec struct {
	Path    string
	Content string
	Mode    string
	Owner   string
	Group   string

	AsUser string
}

func (f fileSpec) check(ctx context.Context, env *Env) (bool, string, error) {
	st, err := statPath(ctx, env, f.Path)
	if err != nil {
		return false, "", err
	}
	if !st.Exists {
		return false, f.Path + " missing", nil
	}
	if f.Owner != "" && (st.Owner != f.Owner || st.Group != f.Group) {
		return false, fmt.Sprintf("%s owned by %s:%s, want %s:%s", f.Path, st.Owner, st.Group, f.Owner, f.Group), nil
	}
	if f.Mode != "" && st.Mode != normalMode(f.Mode) {
		return false, fmt.Sprintf("%s mode %s, want %s", f.Path, st.Mode, normalMode(f.Mode)), nil
	}
	got, exists, err := readFile(ctx, env, f.Path)
	if err != nil {
		return false, "", err
	}
	if !exists || got != f.Content {
		return false, f.Path + " differs", nil
	}
	return true, "", nil
}

func (f fileSpec) apply(ctx context.Context, env *Env) error {
	if f.Mode != "" {
		if _, err := mustRun(ctx, env, runner.Cmd{
			Name: "install", Args: []string{"-m", f.Mode, "/dev/null", f.Path}, User: f.AsUser,
		}); err != nil {
			return err
		}
	}
	res, err := runCmd(ctx, env, runner.Cmd{
		Name: "tee", Args: []string{"--", f.Path}, User: f.AsUser,
		Stdin: strings.NewReader(f.Content),
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("write %s: exit %d: %s", f.Path, res.ExitCode, firstLine(res.Stderr))
	}
	if f.Owner != "" {
		if _, err := mustRun(ctx, env, runner.Cmd{Name: "chown", Args: []string{f.Owner + ":" + f.Group, "--", f.Path}}); err != nil {
			return err
		}
	}
	if f.Mode != "" {
		if _, err := mustRun(ctx, env, runner.Cmd{Name: "chmod", Args: []string{f.Mode, "--", f.Path}}); err != nil {
			return err
		}
	}
	return nil
}

func waitFor(ctx context.Context, timeout time.Duration, what string, cond func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		done, err := cond()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s", timeout, what)
		}
		if err := sleep(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
}

func waitPath(ctx context.Context, env *Env, path string, timeout time.Duration) error {
	return waitFor(ctx, timeout, path, func() (bool, error) {
		return succeeds(ctx, env, runner.Cmd{Name: "test", Args: []string{"-e", path}})
	})
}

func uidOf(ctx context.Context, env *Env, name string) (int, error) {
	out, err := mustRun(ctx, env, runner.Cmd{Name: "id", Args: []string{"-u", name}})
	if err != nil {
		return 0, err
	}
	uid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("uid of %s: %q: %w", name, out, err)
	}
	return uid, nil
}
