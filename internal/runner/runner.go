package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strings"
)

type Cmd struct {
	Name string
	Args []string

	User  string
	Env   []string
	Dir   string
	Stdin io.Reader

	Stdout io.Writer
	Stderr io.Writer
}

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type Runner interface {
	Run(ctx context.Context, c Cmd) (Result, error)
}

type Exec struct {
	Log io.Writer
}

func (e Exec) Run(ctx context.Context, c Cmd) (Result, error) {
	name, args, env, err := prepare(c)
	if err != nil {
		return Result{}, err
	}
	if e.Log != nil {
		fmt.Fprintf(e.Log, "+ %s %s\n", name, strings.Join(args, " "))
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Dir = c.Dir
	cmd.Stdin = c.Stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	}
	if c.Stderr != nil {
		cmd.Stderr = c.Stderr
	}
	runErr := cmd.Run()
	res := Result{Stdout: out.String(), Stderr: errb.String()}
	var ee *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		return res, fmt.Errorf("run %s: %w", name, runErr)
	}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	return res, nil
}

func SessionEnv(username string) ([]string, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return nil, fmt.Errorf("lookup user %q: %w", username, err)
	}
	return userEnv(u), nil
}

func userEnv(u *user.User) []string {
	return []string{
		"HOME=" + u.HomeDir,
		"USER=" + u.Username,
		"LOGNAME=" + u.Username,
		"XDG_RUNTIME_DIR=/run/user/" + u.Uid,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + u.Uid + "/bus",
		"DOCKER_HOST=unix:///run/user/" + u.Uid + "/docker.sock",
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
	}
}

func prepare(c Cmd) (name string, args, env []string, err error) {
	env = append(env, c.Env...)
	if c.User == "" {
		return c.Name, c.Args, env, nil
	}
	u, err := user.Lookup(c.User)
	if err != nil {
		return "", nil, nil, fmt.Errorf("lookup user %q: %w", c.User, err)
	}
	sessionEnv := userEnv(u)
	if os.Geteuid() != 0 {
		cur, _ := user.Current()
		if cur != nil && cur.Uid == u.Uid {

			return c.Name, c.Args, append(sessionEnv, env...), nil
		}
		return "", nil, nil, fmt.Errorf("cannot run as %q: not root", c.User)
	}

	args = append([]string{"-u", u.Username, "--", "env"}, append(sessionEnv, c.Env...)...)
	args = append(args, c.Name)
	args = append(args, c.Args...)
	return "runuser", args, nil, nil
}
