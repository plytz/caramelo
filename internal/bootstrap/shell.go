package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/plytz/caramelo/internal/remote"
)

type Cmd struct {
	Line   string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type Shell interface {
	Run(ctx context.Context, c Cmd) (int, error)
}

const sshFailure = 255

type OpenSSH struct {
	Target remote.Target

	Bin   string
	Extra []string
}

func (s OpenSSH) Run(ctx context.Context, c Cmd) (int, error) {
	bin := s.Bin
	if bin == "" {
		bin = "ssh"
	}
	args := []string{
		"-o", "BatchMode=yes",

		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ServerAliveInterval=15",
		"-o", "ConnectTimeout=15",
	}
	args = append(args, s.Extra...)
	args = append(args, "-p", strconv.Itoa(s.Target.Port), s.Target.Destination(), "--", c.Line)

	var tail tailBuffer
	stderr := io.Writer(&tail)
	if c.Stderr != nil {
		stderr = io.MultiWriter(c.Stderr, &tail)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = c.Stdin
	cmd.Stdout = c.Stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() != sshFailure {
		return ee.ExitCode(), nil
	}
	if ctx.Err() != nil {
		return sshFailure, fmt.Errorf("ssh to %s: %w", s.Target, ctx.Err())
	}
	msg := strings.TrimSpace(tail.String())
	if msg == "" {
		msg = err.Error()
	}
	return sshFailure, fmt.Errorf("ssh to %s failed: %s", s.Target, msg)
}

type tailBuffer struct {
	buf bytes.Buffer
}

const tailSize = 4096

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if t.buf.Len() > tailSize {
		rest := t.buf.Bytes()[t.buf.Len()-tailSize:]
		t.buf = *bytes.NewBuffer(append([]byte(nil), rest...))
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return t.buf.String() }
