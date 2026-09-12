//go:build e2e

package preflight

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	BinEnv     = "CARAMELO_ITEST_BIN"
	HostBinEnv = "CARAMELO_ITEST_HOST_BIN"

	RemoteBinary   = "/var/tmp/caramelo-preflight"
	ClockTolerance = 5 * time.Second

	connectTimeout = 60 * time.Second
	commandTimeout = 60 * time.Second
	shipTimeout    = 5 * time.Minute
	buildTimeout   = 10 * time.Minute
	reachTimeout   = 30 * time.Second
	tcpWaitSeconds = 5
	pingCount      = 2
	pingDeadline   = 10
)

type versionReport struct {
	Version string `json:"version"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

func parseNTPSynchronized(out string) (bool, error) {
	value := strings.TrimSpace(out)
	if i := strings.Index(value, "="); i >= 0 {
		value = strings.TrimSpace(value[i+1:])
	}
	switch strings.ToLower(value) {
	case "yes", "true", "1":
		return true, nil
	case "no", "false", "0":
		return false, nil
	case "":
		return false, fmt.Errorf("timedatectl reported no NTPSynchronized value")
	default:
		return false, fmt.Errorf("timedatectl reported NTPSynchronized as %q, which is neither yes nor no", value)
	}
}

func remoteEpoch(out string) (time.Time, error) {
	value := strings.TrimSpace(out)
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("the machine reported the time as %q instead of seconds since the epoch: %w", value, err)
	}
	return time.Unix(seconds, 0).UTC(), nil
}

func clockSkew(remote, before, after time.Time) time.Duration {
	if remote.Before(before) {
		return before.Sub(remote)
	}
	if remote.After(after) {
		return remote.Sub(after)
	}
	return 0
}

func archFromUname(uname string) (string, error) {
	switch strings.TrimSpace(uname) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	case "armv7l", "armv6l":
		return "arm", nil
	case "":
		return "", fmt.Errorf("the machine reported no machine hardware name")
	default:
		return "", fmt.Errorf("the machine reported the hardware name %q, which is not one the tier maps to a Go architecture", strings.TrimSpace(uname))
	}
}

func hasCommand(name string) string {
	return "command -v " + name + " >/dev/null 2>&1"
}

func reachCommand(addr string, port int) string {
	target := addr + " " + strconv.Itoa(port)
	nc := "nc -z -w " + strconv.Itoa(tcpWaitSeconds) + " " + target
	bash := "bash -c 'exec 3<>/dev/tcp/" + addr + "/" + strconv.Itoa(port) + "'"
	return "if " + hasCommand("nc") + "; then " + nc + "; else timeout " + strconv.Itoa(tcpWaitSeconds) + " " + bash + "; fi"
}

func pingCommand(addr string) string {
	return "ping -n -c " + strconv.Itoa(pingCount) + " -w " + strconv.Itoa(pingDeadline) + " " + addr
}

func targetAddress(host string) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		return host, nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return "", fmt.Errorf("the inventory host %s does not resolve on the machine running the tests: %w", host, err)
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("the inventory host %s resolves to no address on the machine running the tests", host)
	}
	return addrs[0], nil
}

type binary struct {
	path   string
	source string
	err    error
}

var (
	binMu     sync.Mutex
	binCache  = map[string]binary{}
	buildOnce sync.Once
	buildDir  string
	buildErr  error
)

func machineBinary(arch string) (string, string, error) {
	binMu.Lock()
	defer binMu.Unlock()
	if b, ok := binCache[arch]; ok {
		return b.path, b.source, b.err
	}
	path, source, err := findMachineBinary(arch)
	binCache[arch] = binary{path: path, source: source, err: err}
	return path, source, err
}

func findMachineBinary(arch string) (string, string, error) {
	if arch == "" {
		return "", "", fmt.Errorf("no architecture was given, so no binary can be picked")
	}
	for _, env := range []string{BinEnv, BinEnv + "_" + strings.ToUpper(arch)} {
		path := os.Getenv(env)
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			return "", "", fmt.Errorf("%s names %s, which cannot be used: %w", env, path, err)
		}
		return path, env, nil
	}
	name := "caramelo-linux-" + arch
	for _, dir := range siblingDirs() {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			return path, "the " + name + " beside " + dir, nil
		}
	}
	return buildMachineBinary(arch)
}

func siblingDirs() []string {
	var dirs []string
	if p := os.Getenv(HostBinEnv); p != "" {
		dirs = append(dirs, filepath.Dir(p))
	}
	if root, err := repoRoot(); err == nil {
		dirs = append(dirs, filepath.Join(root, "bin"))
	}
	return dirs
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("finding the repository root: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s, so the repository root is unknown", dir)
		}
		dir = parent
	}
}

func buildMachineBinary(arch string) (string, string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", "", err
	}
	buildOnce.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "caramelo-preflight-bin-")
		if buildErr != nil {
			buildErr = fmt.Errorf("making a directory to build the machine binary in: %w", buildErr)
		}
	})
	if buildErr != nil {
		return "", "", buildErr
	}
	out := filepath.Join(buildDir, "caramelo-linux-"+arch)
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", out, "./cmd/caramelo")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("building caramelo for linux/%s: %w: %s", arch, err, strings.TrimSpace(stderr.String()))
	}
	return out, "go build for linux/" + arch, nil
}
