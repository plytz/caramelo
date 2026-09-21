package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/userdir"
)

func Main(m *testing.M) {
	home, err := os.MkdirTemp("", "caramelo-test-home-")
	if err != nil {
		panic("isolating the tests: " + err.Error())
	}
	run := newShortDir()
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "CARAMELO_") {
			os.Unsetenv(k)
		}
	}
	os.Setenv("CARAMELO_EXPERIMENTAL", "1")
	config := filepath.Join(home, ".config")
	cache := filepath.Join(home, ".cache")
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", config)
	os.Setenv("XDG_CACHE_HOME", cache)
	os.Setenv("XDG_RUNTIME_DIR", run)
	if got, err := userdir.Config(); err != nil || got != config {
		panic("test isolation failed: config dir resolves to " + got)
	}
	if got, err := userdir.Cache(); err != nil || got != cache {
		panic("test isolation failed: cache dir resolves to " + got)
	}
	code := m.Run()
	os.RemoveAll(home)
	os.RemoveAll(run)
	os.Exit(code)
}

func ShortDir(t testing.TB) string {
	t.Helper()
	dir := newShortDir()
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newShortDir() string {
	base := ""
	if runtime.GOOS != "windows" && len(os.TempDir()) > 24 {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "c")
	if err != nil {
		panic("short temp dir: " + err.Error())
	}
	return dir
}
