package sshapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/git"
)

func (d *Daemon) withIdentity(ctx context.Context) context.Context {
	sess, ok := SessionFrom(ctx)
	if !ok {
		return ctx
	}
	return env.WithIdentity(ctx, sess.Author())
}

func (d *Daemon) envs() (*env.Manager, error) {
	if d.EnvManager == nil {
		return nil, errors.New("environments are not available on this daemon")
	}
	return d.EnvManager, nil
}

func (d *Daemon) CreateEnv(ctx context.Context, req env.CreateRequest, progress io.Writer) (*env.Env, error) {
	m, err := d.envs()
	if err != nil {
		return nil, err
	}

	if err := d.placeAndForward(ctx, m, &req, progress); err != nil {
		return nil, err
	}
	return m.Create(d.withIdentity(ctx), req, progress)
}

func (d *Daemon) Envs(ctx context.Context, app string) ([]env.Env, error) {
	m, err := d.envs()
	if err != nil {
		return nil, err
	}
	return m.List(d.withIdentity(ctx), app)
}

func (d *Daemon) Env(ctx context.Context, app, name string) (*api.EnvDetail, error) {

	if det, ok, err := d.envFromDirectory(ctx, app, name); err != nil {
		return nil, err
	} else if ok {
		return det, nil
	}
	if err := d.forwardEnv(ctx, app, name); err != nil {

		var forwarded *api.ForwardedError
		if errors.As(err, &forwarded) {
			return nil, err
		}

		if det, ok, derr := d.envInDirectory(ctx, app, name); derr == nil && ok {
			d.logf("env show %s/%s: %s could not be reached (%v); answering from the directory",
				app, name, det.Machine, err)
			return det, nil
		}
		return nil, err
	}
	m, err := d.envs()
	if err != nil {
		return nil, err
	}
	e, deps, events, err := m.Show(d.withIdentity(ctx), app, name)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, fmt.Errorf("no such env %q in app %q", name, app)
	}
	detail := &api.EnvDetail{Env: *e, Deps: deps, Events: events}

	services, err := m.Services(ctx, app, name)
	if err != nil {
		return nil, fmt.Errorf("read the services of env %q: %w", name, err)
	}
	detail.Services = services
	if len(services) > 0 {
		detail.Network = env.NetworkName(app, name)
	}

	detail.Routes, detail.Certificates = d.envEdge(ctx, app, name)
	overlayInflight(detail.Services, detail.Routes)

	d.envProduction(ctx, detail)
	detail.Vars = map[string]map[string]string{}
	for _, view := range []config.View{config.ViewHost, config.ViewNetwork} {
		vars, err := m.VarsIn(ctx, app, name, view, false)
		if err != nil {
			return nil, fmt.Errorf("read the %s view of env %q: %w", view, name, err)
		}
		detail.Vars[string(view)] = vars
	}
	return detail, nil
}

func (d *Daemon) DestroyEnv(ctx context.Context, req env.DestroyRequest, progress io.Writer) error {
	if err := d.forwardEnv(ctx, req.App, req.Name); err != nil {
		return err
	}
	m, err := d.envs()
	if err != nil {
		return err
	}
	return m.Destroy(d.withIdentity(ctx), req, progress)
}

func (d *Daemon) ExecEnv(ctx context.Context, req env.ExecRequest) (int, error) {
	if err := d.forwardEnv(ctx, req.App, req.Name); err != nil {
		return 0, err
	}
	m, err := d.envs()
	if err != nil {
		return 0, err
	}
	return m.Exec(d.withIdentity(ctx), req)
}

func (d *Daemon) ExportEnv(ctx context.Context, app, name string, format env.ExportFormat, view config.View,
	reveal bool) (string, error) {
	if err := d.forwardEnv(ctx, app, name); err != nil {
		return "", err
	}
	m, err := d.envs()
	if err != nil {
		return "", err
	}
	return m.ExportView(d.withIdentity(ctx), app, name, format, view, reveal)
}

func (d *Daemon) Apps(ctx context.Context) ([]api.AppInfo, error) {
	if d.Store == nil {
		return nil, errors.New("no state store")
	}
	apps, err := d.Store.Apps(ctx)
	if err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	out := make([]api.AppInfo, 0, len(apps))
	for _, a := range apps {
		info := api.AppInfo{Name: a.Name, DefaultBranch: a.DefaultBranch, Stack: a.Stack}
		envs, err := d.Store.Envs(ctx, a.Name)
		if err != nil {
			return nil, fmt.Errorf("count envs of app %q: %w", a.Name, err)
		}
		info.EnvCount = len(envs)
		repo := a.RepoPath
		if repo == "" {
			repo = env.RepoPath(d.Config.DataDir, a.Name)
		}
		info.RepoBytes = d.repoBytes(ctx, repo)
		out = append(out, info)
	}
	return out, nil
}

func (d *Daemon) repoBytes(ctx context.Context, repo string) int64 {
	if d.EnvManager != nil {
		if sizer, ok := d.EnvManager.Git.(git.Sizer); ok {
			if n, err := sizer.RepoSize(ctx, repo); err == nil {
				return n
			}
		}
	}
	return dirBytes(repo)
}

func dirBytes(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if info, err := entry.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
