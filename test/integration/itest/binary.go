//go:build integration

package itest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/bootstrap"
)

const buildTimeout = 10 * time.Minute

var ErrNoBinary = errors.New("no caramelo binary for this platform")

type builtBinary struct {
	path string
	err  error
}

var (
	binMu    sync.Mutex
	binCache = map[string]builtBinary{}
)

func HostBinary() (string, error) {
	if p := os.Getenv(HostBinEnv); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("%w: %s=%s: %w", ErrNoBinary, HostBinEnv, p, err)
		}
		return p, nil
	}
	return ensureBinary(runtime.GOOS, runtime.GOARCH, "caramelo")
}

func BinaryPathOr() (string, error) { return HostBinary() }

func BinaryPath(t testing.TB) string {
	t.Helper()
	p, err := HostBinary()
	if err != nil {
		t.Fatalf("itest: %v", err)
	}
	return p
}

func MachineBinary() (string, error) {
	arch, err := DockerArch()
	if err != nil {
		return "", err
	}
	return BinaryForArch(arch)
}

func BinaryForArch(arch string) (string, error) {
	if p := os.Getenv(BinEnv); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("%w: %s=%s: %w", ErrNoBinary, BinEnv, p, err)
		}
		return p, nil
	}
	return ensureBinary("linux", arch, "caramelo-linux-"+arch)
}

func BinaryFor(ctx context.Context, m *Machine) (string, error) {
	arch, err := m.Arch(ctx)
	if err != nil {
		return "", err
	}
	return BinaryForArch(arch)
}

func ensureBinary(goos, goarch, name string) (string, error) {
	key := goos + "/" + goarch
	binMu.Lock()
	if b, ok := binCache[key]; ok {
		binMu.Unlock()
		return b.path, b.err
	}
	binMu.Unlock()

	path, err := buildBinary(goos, goarch, name)

	binMu.Lock()
	binCache[key] = builtBinary{path: path, err: err}
	binMu.Unlock()
	return path, err
}

func buildBinary(goos, goarch, name string) (string, error) {
	root, err := RepoRootOr()
	if err != nil {
		return "", err
	}
	out := filepath.Join(root, "bin", name)
	lock, err := lockCache("build-" + name)
	if err != nil {
		return "", err
	}
	defer lock.release()

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", filepath.Dir(out), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", out, "./cmd/caramelo")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: build caramelo for %s/%s: %w: %s", ErrNoBinary, goos, goarch, err, stderr.String())
	}
	fmt.Fprintf(os.Stderr, "itest: built %s for %s/%s in %s\n", out, goos, goarch, time.Since(start).Round(time.Millisecond))
	return out, nil
}

func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func InstallClientBinaries(ctx context.Context, laptop, target *Machine) (string, error) {
	laptopArch, err := laptop.Arch(ctx)
	if err != nil {
		return "", err
	}
	targetArch, err := target.Arch(ctx)
	if err != nil {
		return "", err
	}
	bin, err := BinaryForArch(laptopArch)
	if err != nil {
		return "", err
	}
	if err := installExecutable(ctx, laptop, bin, RemoteBin); err != nil {
		return "", err
	}
	if laptopArch == targetArch {
		return RemoteBin, nil
	}
	cross, err := BinaryForArch(targetArch)
	if err != nil {
		return "", err
	}
	sibling := path.Join(path.Dir(RemoteBin), bootstrap.SiblingName("linux", targetArch))
	if err := installExecutable(ctx, laptop, cross, sibling); err != nil {
		return "", err
	}
	return RemoteBin, nil
}

func installExecutable(ctx context.Context, m *Machine, local, remote string) error {
	if err := m.Copy(ctx, local, remote); err != nil {
		return fmt.Errorf("ship %s to %s as %s: %w", local, m.Alias, remote, err)
	}
	res, err := m.Run(ctx, "chmod +x "+ShellQuote(remote))
	if err != nil {
		return fmt.Errorf("chmod %s on %s: %w", remote, m.Alias, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("chmod %s on %s: exit %d: %s", remote, m.Alias, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}
