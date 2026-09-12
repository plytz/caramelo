package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/runtime"
)

const errTail = 8 << 10

func (c *CLI) Build(ctx context.Context, spec runtime.BuildSpec) (string, error) {
	if spec.Context == "" || spec.Tag == "" {
		return "", fmt.Errorf("docker build: a context directory and a tag are required")
	}
	args := []string{"build", "--tag", spec.Tag}
	if spec.Dockerfile != "" {

		args = append(args, "--file", filepath.Join(spec.Context, spec.Dockerfile))
	}
	args = append(args, labelArgs("--label", spec.Labels)...)
	if spec.NoCache {
		args = append(args, "--no-cache")
	}
	args = append(args, spec.Context)

	var tail lastBytes
	cmd := runner.Cmd{Name: "docker", Args: args, User: c.User}
	if spec.Progress != nil {

		cmd.Stdout = io.MultiWriter(spec.Progress, &tail)
		cmd.Stderr = io.MultiWriter(spec.Progress, &tail)
	}
	res, err := c.exec(ctx, cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		if spec.Progress != nil {
			res.Stderr = tail.String()
		}
		return "", cmdErr(args, res)
	}

	id, err := c.imageID(ctx, spec.Tag)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (c *CLI) ImageExists(ctx context.Context, ref string) (bool, error) {
	if ref == "" {
		return false, fmt.Errorf("docker image inspect: no image given")
	}
	res, err := c.run(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

func (c *CLI) imageID(ctx context.Context, ref string) (string, error) {
	args := []string{"image", "inspect", "--format", "{{.Id}}", ref}
	res, err := c.runOK(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

func (c *CLI) CreateNetwork(ctx context.Context, name string, labels map[string]string) error {
	if name == "" {
		return fmt.Errorf("docker network create: no name given")
	}
	args := []string{"network", "create"}
	args = append(args, labelArgs("--label", labels)...)
	args = append(args, name)
	res, err := c.run(ctx, args...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 && !isNetworkExists(res) {
		return cmdErr(args, res)
	}
	return nil
}

func (c *CLI) RemoveNetwork(ctx context.Context, name string) error {
	args := []string{"network", "rm", name}
	res, err := c.run(ctx, args...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 && !isNoSuchNetwork(res) {
		return cmdErr(args, res)
	}
	return nil
}

func (c *CLI) Connect(ctx context.Context, network, container string, aliases []string) error {
	if network == "" || container == "" {
		return fmt.Errorf("docker network connect: a network and a container are required")
	}
	args := []string{"network", "connect"}
	for _, a := range aliases {
		args = append(args, "--alias", a)
	}
	args = append(args, network, container)
	res, err := c.run(ctx, args...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 && !isAlreadyConnected(res) {
		return cmdErr(args, res)
	}
	return nil
}

func (c *CLI) RunAttached(ctx context.Context, spec runtime.ContainerSpec, streams runtime.Streams) (int, error) {
	if spec.Image == "" {
		return 0, fmt.Errorf("docker run: container needs an image")
	}
	if err := checkImage(spec.Image); err != nil {
		return 0, err
	}

	spec.AutoRemove = true
	args := []string{"run"}
	if streams.Stdin != nil {
		args = append(args, "--interactive")
	}
	args = append(args, runArgs(spec)...)

	var tail lastBytes
	cmd := runner.Cmd{Name: "docker", Args: args, User: c.User, Stdin: streams.Stdin}
	cmd.Stdout = writerOrDiscard(streams.Stdout)
	cmd.Stderr = io.MultiWriter(writerOrDiscard(streams.Stderr), &tail)
	res, err := c.exec(ctx, cmd)
	if ctx.Err() != nil {

		c.removeAfterCancel(spec.Name)
	}
	if err != nil {
		return 0, err
	}
	if res.ExitCode == dockerFailedToRun && streams.Stderr == nil {

		res.Stderr = tail.String()
		return 0, cmdErr(args, res)
	}
	if res.ExitCode == reservedExit {
		return 1, nil
	}
	return res.ExitCode, nil
}

func (c *CLI) removeAfterCancel(name string) {
	if name == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), removeGrace)
	defer cancel()

	_ = c.Remove(ctx, name, true)
}

const removeGrace = 30 * time.Second

const dockerFailedToRun = 125

const reservedExit = 255

func (c *CLI) Logs(ctx context.Context, name string, opts runtime.LogOptions, stdout, stderr io.Writer) error {
	if name == "" {
		return fmt.Errorf("docker logs: no container given")
	}
	args := []string{"logs"}
	if opts.Follow {
		args = append(args, "--follow")
	}
	if opts.Since != "" {
		args = append(args, "--since", opts.Since)
	}
	if opts.Tail > 0 {
		args = append(args, "--tail", strconv.Itoa(opts.Tail))
	}
	if opts.Timestamps {
		args = append(args, "--timestamps")
	}
	args = append(args, name)

	var tail lastBytes
	cmd := runner.Cmd{Name: "docker", Args: args, User: c.User}
	cmd.Stdout = writerOrDiscard(stdout)
	cmd.Stderr = io.MultiWriter(writerOrDiscard(stderr), &tail)
	res, err := c.exec(ctx, cmd)
	switch {
	case ctx.Err() != nil:

		return nil
	case err != nil:
		return err
	case res.ExitCode == 0:
		return nil
	}
	res.Stderr = tail.String()
	if isNoSuchContainer(res) {
		return fmt.Errorf("container %s: %w", name, runtime.ErrNotFound)
	}
	return cmdErr(args, res)
}

func (c *CLI) exec(ctx context.Context, cmd runner.Cmd) (runner.Result, error) {
	if c.Runner == nil {
		return runner.Result{}, fmt.Errorf("docker %s: no command runner configured", strings.Join(cmd.Args, " "))
	}
	res, err := c.Runner.Run(ctx, cmd)
	if err != nil {
		if errors.Is(err, ctx.Err()) && ctx.Err() != nil {
			return res, err
		}
		return res, fmt.Errorf("docker %s: %w", strings.Join(cmd.Args, " "), err)
	}
	return res, nil
}

func writerOrDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

type lastBytes struct {
	buf bytes.Buffer
}

func (l *lastBytes) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > errTail {
		p = p[len(p)-errTail:]
	}
	l.buf.Write(p)
	if l.buf.Len() > errTail {
		b := l.buf.Bytes()
		keep := append([]byte(nil), b[l.buf.Len()-errTail:]...)
		l.buf.Reset()
		l.buf.Write(keep)
	}
	return n, nil
}

func (l *lastBytes) String() string { return l.buf.String() }

func isNetworkExists(res runner.Result) bool {
	s := strings.ToLower(res.Stderr + "\n" + res.Stdout)
	return strings.Contains(s, "already exists")
}

func isNoSuchNetwork(res runner.Result) bool {
	s := strings.ToLower(res.Stderr + "\n" + res.Stdout)
	return strings.Contains(s, "no such network") || strings.Contains(s, "not found")
}

func isAlreadyConnected(res runner.Result) bool {
	s := strings.ToLower(res.Stderr + "\n" + res.Stdout)
	return strings.Contains(s, "already exists in network") || strings.Contains(s, "is already attached to network")
}
