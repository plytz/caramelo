//go:build integration

package itest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const dockerBin = "docker"

type dockerResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

func runDocker(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) (dockerResult, error) {
	cmd := exec.CommandContext(ctx, dockerBin, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdin = stdin
	if stdout != nil {
		cmd.Stdout = stdout
	} else {
		cmd.Stdout = &outBuf
	}
	cmd.Stderr = &errBuf
	err := cmd.Run()
	res := dockerResult{Stdout: outBuf.String(), Stderr: errBuf.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return res, fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, fmt.Errorf("docker %s: %w", strings.Join(args, " "), ctxErr)
	}
	return res, nil
}

func docker(ctx context.Context, args ...string) (string, error) {
	res, err := runDocker(ctx, nil, nil, args...)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return res.Stdout, fmt.Errorf("docker %s: exit %d: %s",
			strings.Join(args, " "), res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
}

func dockerQuiet(ctx context.Context, args ...string) error {
	_, err := docker(ctx, args...)
	return err
}

func isDockerFailure(stderr string) bool {
	s := strings.TrimSpace(stderr)
	return strings.HasPrefix(s, "Error response from daemon") ||
		strings.HasPrefix(s, "Cannot connect to the Docker daemon") ||
		strings.HasPrefix(s, "Error: No such container") ||
		strings.Contains(s, "is not running")
}

var (
	dockerArchOnce sync.Once
	dockerArchVal  string
	dockerArchErr  error
)

func DockerArch() (string, error) {
	dockerArchOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := docker(ctx, "version", "--format", "{{.Server.Arch}}")
		if err != nil {
			dockerArchErr = fmt.Errorf("read the docker server architecture: %w", err)
			return
		}
		arch := GoArch(out)
		if arch == "" {
			dockerArchErr = fmt.Errorf("the docker server reported no architecture")
			return
		}
		dockerArchVal = arch
	})
	return dockerArchVal, dockerArchErr
}

func RequireDocker(ctx context.Context) error {
	if _, err := exec.LookPath(dockerBin); err != nil {
		return fmt.Errorf("the integration suites need the docker CLI: %w", err)
	}
	if _, err := docker(ctx, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return fmt.Errorf("the integration suites need a running docker daemon: %w", err)
	}
	return nil
}

func createNetwork(ctx context.Context, name string) error {
	return dockerQuiet(ctx, "network", "create", "--label", label(), name)
}

func removeNetwork(ctx context.Context, name string) error {
	return dockerQuiet(ctx, "network", "rm", name)
}

func createVolume(ctx context.Context, name string) error {
	return dockerQuiet(ctx, "volume", "create", "--label", label(), name)
}

func removeVolume(ctx context.Context, name string) error {
	return dockerQuiet(ctx, "volume", "rm", "-f", name)
}

func removeContainer(ctx context.Context, name string) error {
	return dockerQuiet(ctx, "rm", "-f", "-v", name)
}

func imageExists(ctx context.Context, tag string) bool {
	_, err := docker(ctx, "image", "inspect", "--format", "{{.Id}}", tag)
	return err == nil
}

func containerIP(ctx context.Context, name, network string) (string, error) {
	format := fmt.Sprintf(`{{ with index .NetworkSettings.Networks %q }}{{ .IPAddress }}{{ end }}`, network)
	out, err := docker(ctx, "inspect", "--format", format, name)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", fmt.Errorf("container %s has no address on network %s", name, network)
	}
	return out, nil
}

func publishedPorts(ctx context.Context, name string) (map[string]int, error) {
	out, err := docker(ctx, "port", name)
	if err != nil {
		return nil, err
	}
	ports := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		spec, hostAddr, ok := strings.Cut(line, " -> ")
		if !ok {
			continue
		}
		spec = strings.TrimSpace(spec)
		hostAddr = strings.TrimSpace(hostAddr)
		i := strings.LastIndex(hostAddr, ":")
		if i < 0 {
			continue
		}
		port, err := parsePort(hostAddr[i+1:])
		if err != nil {
			return nil, fmt.Errorf("read the published ports of %s: %q: %w", name, line, err)
		}
		if _, seen := ports[spec]; seen && strings.HasPrefix(hostAddr, "[") {
			continue
		}
		ports[spec] = port
	}
	if len(ports) == 0 {
		return nil, fmt.Errorf("container %s publishes no port", name)
	}
	return ports, nil
}

func parsePort(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil {
		return 0, err
	}
	if n <= 0 || n > 65535 {
		return 0, fmt.Errorf("%q is not a port", s)
	}
	return n, nil
}
