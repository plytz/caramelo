//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/plytz/caramelo/internal/place"
	"github.com/plytz/caramelo/internal/serverconfig"
)

const contextTimeout = 2 * time.Minute

func ContextOn(ctx context.Context, m *Machine, bin string) (place.Context, error) {
	res, err := m.Run(ctx, ShellQuote(bin)+" context --json")
	if err != nil {
		return place.Context{}, fmt.Errorf("caramelo context --json on %s: %w", m.Alias, err)
	}
	if res.ExitCode != 0 {
		return place.Context{}, fmt.Errorf("caramelo context --json on %s: exit %d: %s",
			m.Alias, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	var c place.Context
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &c); err != nil {
		return place.Context{}, fmt.Errorf("caramelo context --json on %s is not a place.Context: %w\nstdout:\n%s",
			m.Alias, err, res.Stdout)
	}
	return c, nil
}

func MustContextOn(t testing.TB, m *Machine, bin string) place.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), Scale(contextTimeout))
	defer cancel()
	c, err := ContextOn(ctx, m, bin)
	if err != nil {
		t.Fatalf("itest: %v", err)
	}
	t.Logf("[%s] %s", m.Alias, c.Header())
	return c
}

func ContextTextOn(t testing.TB, m *Machine, bin string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), Scale(contextTimeout))
	defer cancel()
	res, err := m.Run(ctx, ShellQuote(bin)+" context")
	if err != nil {
		t.Fatalf("itest: caramelo context on %s: %v", m.Alias, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("itest: caramelo context on %s: exit %d: %s", m.Alias, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout
}

func ServerConfigOn(ctx context.Context, m *Machine) (serverconfig.Config, error) {
	path := serverconfig.Path(serverconfig.DefaultConfigDir)
	res, err := m.Run(ctx, "sudo -n cat "+ShellQuote(path))
	if err != nil {
		return serverconfig.Config{}, fmt.Errorf("read %s on %s: %w", path, m.Alias, err)
	}
	if res.ExitCode != 0 {
		return serverconfig.Config{}, fmt.Errorf("read %s on %s: exit %d: %s",
			path, m.Alias, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	var cfg serverconfig.Config
	if err := yaml.Unmarshal([]byte(res.Stdout), &cfg); err != nil {
		return serverconfig.Config{}, fmt.Errorf("%s on %s is not a serverconfig.Config: %w\n%s",
			path, m.Alias, err, res.Stdout)
	}
	return cfg, nil
}

func MustServerConfigOn(t testing.TB, m *Machine) serverconfig.Config {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), Scale(contextTimeout))
	defer cancel()
	cfg, err := ServerConfigOn(ctx, m)
	if err != nil {
		t.Fatalf("itest: %v", err)
	}
	return cfg
}
