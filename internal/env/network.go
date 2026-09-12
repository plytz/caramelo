package env

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/state"
)

func (m *Manager) ensureNetwork(ctx context.Context, rec *state.EnvRecord, p *upPlan, progress io.Writer) error {
	name, err := m.ensureEnvNetwork(ctx, rec, progress)
	if err != nil {
		return err
	}
	return m.attachDeps(ctx, rec, p.cfg.Deps, name, progress)
}

func (m *Manager) ensureEnvNetwork(ctx context.Context, rec *state.EnvRecord, progress io.Writer) (string, error) {
	name := NetworkName(rec.App, rec.Name)
	labels := Labels(rec.App, rec.Name, "", m.Version)
	if err := m.Driver.CreateNetwork(ctx, name, labels); err != nil {
		return "", fmt.Errorf("create network %s: %w", name, err)
	}
	m.addResource(ctx, rec.ID, state.ResourceNetwork, name, "", 0)
	progressf(progress, "ok", "network", "%s", name)
	return name, nil
}

func (m *Manager) attachDeps(ctx context.Context, rec *state.EnvRecord, deps []config.Dep, name string, progress io.Writer) error {
	attached := 0
	for _, dep := range deps {
		container := ContainerName(rec.App, rec.Name, dep.Name)

		if _, err := m.Driver.Inspect(ctx, container); errors.Is(err, runtime.ErrNotFound) {
			progressf(progress, "warning", "deps", "%s has no container: services will not reach it at %s", dep.Name, dep.Name)
			continue
		} else if err != nil {
			return fmt.Errorf("inspect %s: %w", container, err)
		}
		if err := m.Driver.Connect(ctx, name, container, []string{dep.Name}); err != nil {
			return fmt.Errorf("attach %s to %s: %w", container, name, err)
		}
		attached++
	}
	if attached > 0 {
		progressf(progress, "ok", "deps", "%s reachable on %s", plural(attached, "dep"), name)
	}
	return nil
}

func (m *Manager) ensureCache(ctx context.Context, rec *state.EnvRecord, s svcPlan, progress io.Writer) error {
	if s.cache == "" {
		return nil
	}
	name, err := m.ensureCacheVolume(ctx, rec, s.name)
	if err != nil {
		return err
	}
	progressf(progress, "ok", "cache", "%s at %s", name, s.cache)
	return nil
}

func (m *Manager) ensureCacheVolume(ctx context.Context, rec *state.EnvRecord, service string) (string, error) {
	name := CacheVolumeName(rec.App, rec.Name)
	if err := m.Driver.CreateVolume(ctx, name, Labels(rec.App, rec.Name, "", m.Version)); err != nil {
		return "", fmt.Errorf("create cache volume %s: %w", name, err)
	}
	m.addServiceResource(ctx, rec.ID, state.ResourceCache, name, service, 0)
	return name, nil
}
