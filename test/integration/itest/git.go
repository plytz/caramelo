//go:build integration

package itest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const gitTimeout = 3 * time.Minute

const (
	PostgresImage = "postgres:16-alpine"
	RedisImage    = "redis:7-alpine"
	PythonImage   = "python:3.12-alpine"
)

var DepImages = []string{PostgresImage, RedisImage}

var RunImages = append(append([]string{}, DepImages...), PythonImage)

var EdgeImages = append(append([]string{}, RunImages...), PebbleImage)

var ProdImages = EdgeImages

func GitEnv(home string, extra ...string) []string {
	env := CommanderEnv(home,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Caramelo Lab",
		"GIT_AUTHOR_EMAIL=lab@caramelo.invalid",
		"GIT_COMMITTER_NAME=Caramelo Lab",
		"GIT_COMMITTER_EMAIL=lab@caramelo.invalid",
	)
	if opts := sshOptsFrom(env); opts != "" {
		env = append(env, "GIT_SSH_COMMAND=ssh "+opts+
			" -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR")
	}
	return append(env, extra...)
}

func sshOptsFrom(env []string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], "CARAMELO_SSH_OPTS="); ok {
			return v
		}
	}
	return ""
}

func GitRemote(host, app string) string {
	return GitRemoteAt(host+":"+strconv.Itoa(CarameloSSHPort), app)
}

func GitRemoteAt(addr, app string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Sprintf("ssh://%s@%s/%s", CarameloUser, addr, app)
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("ssh://%s@%s:%s/%s", CarameloUser, host, port, app)
}

func (m *Machine) GitRemote(t testing.TB, app string) string {
	t.Helper()
	return GitRemoteAt(m.CommanderMachine(t), app)
}

func (m *Machine) GitTunnelRemote(t testing.TB, app string) string {
	t.Helper()
	if _, err := m.HostPortProto(VPNPort, "udp"); err != nil {
		t.Fatalf("itest: %v", err)
	}
	return GitRemote(m.SSHAlias(), app)
}

func GitRun(ctx context.Context, dir string, env []string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()

	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return res, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, fmt.Errorf("git %s: %w", strings.Join(args, " "), ctxErr)
	}
	return res, nil
}

func Git(t testing.TB, dir string, env []string, args ...string) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), Scale(gitTimeout))
	defer cancel()
	res, err := GitRun(ctx, dir, env, args...)
	if err != nil {
		t.Fatalf("itest: git %s in %s: %v", strings.Join(args, " "), dir, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("itest: git %s in %s: exit %d\nstdout:\n%sstderr:\n%s",
			strings.Join(args, " "), dir, res.ExitCode, res.Stdout, res.Stderr)
	}
	t.Logf("[git] %s: exit 0", strings.Join(args, " "))
	return res
}

func GitCommitAll(t testing.TB, dir string, env []string, message string) string {
	t.Helper()
	Git(t, dir, env, "add", "-A")
	Git(t, dir, env, "commit", "-m", message)
	return strings.TrimSpace(Git(t, dir, env, "rev-parse", "HEAD").Stdout)
}

func GitRefs(t testing.TB, dir string, env []string, remote string) map[string]string {
	t.Helper()
	res := Git(t, dir, env, "ls-remote", remote)
	refs := map[string]string{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		commit, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		refs[strings.TrimSpace(ref)] = commit
	}
	return refs
}
