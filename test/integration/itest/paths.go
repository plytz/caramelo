//go:build integration

package itest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

var (
	rootOnce sync.Once
	rootPath string
	rootErr  error
)

func RepoRootOr() (string, error) {
	rootOnce.Do(func() {
		dir, err := os.Getwd()
		if err != nil {
			rootErr = fmt.Errorf("getwd: %w", err)
			return
		}
		start := dir
		for {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				rootPath = dir
				return
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				rootErr = fmt.Errorf("no go.mod found walking up from %s", start)
				return
			}
			dir = parent
		}
	})
	return rootPath, rootErr
}

func RepoRoot(t testing.TB) string {
	t.Helper()
	root, err := RepoRootOr()
	if err != nil {
		t.Fatalf("itest: %v", err)
	}
	return root
}

func SharedCacheDir(name string) (string, error) {
	root, err := RepoRootOr()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "test", "integration", ".cache", name), nil
}

func CacheDir(arch, name string) (string, error) {
	if arch == "" {
		return "", fmt.Errorf("cache %s: no architecture", name)
	}
	root, err := RepoRootOr()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "test", "integration", ".cache", arch, name), nil
}

func CacheDirFor(ctx context.Context, m *Machine, name string) (string, error) {
	arch, err := m.Arch(ctx)
	if err != nil {
		return "", err
	}
	return CacheDir(arch, name)
}

func GossSpec(name string) (string, error) {
	root, err := RepoRootOr()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "test", "integration", "goss", name), nil
}

func MustGossSpec(t testing.TB, name string) string {
	t.Helper()
	p, err := GossSpec(name)
	if err != nil {
		t.Fatalf("itest: %v", err)
	}
	return p
}

func imageContext(kind string) (string, error) {
	root, err := RepoRootOr()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "test", "integration", "image", kind), nil
}

type fileLock struct{ f *os.File }

func lockCache(name string) (*fileLock, error) {
	dir, err := SharedCacheDir("locks")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, name+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return &fileLock{f: f}, nil
}

func (l *fileLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
