package sshapi

import (
	"context"
	"fmt"
	"io"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/stack"
)

func (d *Daemon) Up(ctx context.Context, req env.UpRequest, progress io.Writer) (*api.UpResult, error) {
	if err := d.forwardEnv(ctx, req.App, req.Name); err != nil {
		return nil, err
	}
	m, err := d.envs()
	if err != nil {
		return nil, err
	}
	res, upErr := m.Up(d.withIdentity(ctx), req, progress)
	if res == nil {
		return nil, upErr
	}

	if res.Stack != "" && d.Store != nil {
		if err := d.Store.SetAppStack(ctx, req.App, res.Stack); err != nil {
			fmt.Fprintf(progress, "caramelo: warning: record the stack of app %q: %v\n", req.App, err)
		}
	}
	out := &api.UpResult{
		Env:      res.Env,
		Services: res.Services,
		Network:  res.Network,
		Image:    res.Image,
		Stack:    res.Stack,
	}

	out.Rollouts = res.Rollouts
	out.Routes, _ = d.envEdge(ctx, req.App, req.Name)
	overlayInflight(out.Services, out.Routes)
	if len(out.Routes) > 0 {
		out.URL = publicURL(out.Routes[0].Host)
	}
	return out, upErr
}

func (d *Daemon) Down(ctx context.Context, req env.DownRequest, progress io.Writer) (*api.DownResult, error) {
	if err := d.forwardEnv(ctx, req.App, req.Name); err != nil {
		return nil, err
	}
	m, err := d.envs()
	if err != nil {
		return nil, err
	}
	services, err := m.Down(d.withIdentity(ctx), req, progress)
	if err != nil {
		return nil, err
	}
	return &api.DownResult{Services: services}, nil
}

func (d *Daemon) Test(ctx context.Context, req env.RunRequest) (int, error) {
	if err := d.forwardEnv(ctx, req.App, req.Name); err != nil {
		return 0, err
	}
	m, err := d.envs()
	if err != nil {
		return 0, err
	}
	req.Test = true
	return m.RunOneOff(d.withIdentity(ctx), req)
}

func (d *Daemon) Run(ctx context.Context, req env.RunRequest) (int, error) {
	if err := d.forwardEnv(ctx, req.App, req.Name); err != nil {
		return 0, err
	}
	m, err := d.envs()
	if err != nil {
		return 0, err
	}
	req.Test = false
	return m.RunOneOff(d.withIdentity(ctx), req)
}

func (d *Daemon) Logs(ctx context.Context, req env.LogsRequest) error {
	if req.Edge {

		return d.edgeLogs(d.withIdentity(ctx), req)
	}

	if req.Name != "" {
		if err := d.forwardEnv(ctx, req.App, req.Name); err != nil {
			return err
		}
	}
	m, err := d.envs()
	if err != nil {
		return err
	}
	return m.Logs(d.withIdentity(ctx), req)
}

func (d *Daemon) EffectiveConfig(ctx context.Context, app, name string, reveal bool) (*api.EffectiveConfig, error) {
	if err := d.forwardEnv(ctx, app, name); err != nil {
		return nil, err
	}
	m, err := d.envs()
	if err != nil {
		return nil, err
	}
	ctx = d.withIdentity(ctx)

	dir, cleanup, err := m.ConfigDir(ctx, app, name)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	file, err := config.LoadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s of app %q: %w", config.FileName, app, err)
	}

	file.Name = app
	guess, err := stack.Detect(dir)
	if err != nil {
		return nil, fmt.Errorf("detect the stack of app %q: %w", app, err)
	}
	eff := stack.Merge(file, guess)

	cfg, err := m.ConfigWithSecrets(ctx, app, name, eff.Config, reveal)
	if err != nil {
		return nil, err
	}
	out := &api.EffectiveConfig{App: app, Env: name, Stack: eff.Stack, Config: cfg}
	out.Fields = make([]api.ConfigField, 0, len(eff.Settings))
	for _, s := range eff.Settings {
		out.Fields = append(out.Fields, api.ConfigField{
			Key:      s.Key,
			Value:    s.Value,
			Source:   api.Source(s.Source),
			Evidence: s.Evidence,
		})
	}
	return out, nil
}

func (d *Daemon) URLs(ctx context.Context, app, name, target string) ([]env.URL, error) {
	if err := d.forwardEnv(ctx, app, name); err != nil {
		return nil, err
	}
	m, err := d.envs()
	if err != nil {
		return nil, err
	}
	return m.URLs(d.withIdentity(ctx), app, name, target)
}
