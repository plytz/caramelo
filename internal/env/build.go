package env

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/release"
)

func (m *Manager) Build(ctx context.Context, req release.BuildRequest, out io.Writer) (*release.BuildResult, error) {
	if err := ValidateName("app", req.App); err != nil {
		return nil, err
	}
	if err := ValidateName("env", req.Env); err != nil {
		return nil, err
	}
	rec, err := m.env(ctx, req.App, req.Env)
	if err != nil {
		return nil, err
	}
	if m.Builder == nil {
		return nil, fmt.Errorf("this machine cannot build releases: caramelod was started without a builder")
	}
	res, err := m.build(ctx, req, out)
	if err != nil {
		m.record(context.WithoutCancel(ctx), rec.ID, Event{
			Action: release.ActionBuild, Status: progress.StatusFailed, Detail: err.Error(),
		})
		return nil, err
	}
	m.record(ctx, rec.ID, Event{
		Action: release.ActionBuild, Status: buildStatus(res), Detail: buildDetail(res),
	})
	return res, nil
}

func buildStatus(res *release.BuildResult) string {
	if res != nil && res.Built {
		return progress.StatusChanged
	}
	return progress.StatusOK
}

func buildDetail(res *release.BuildResult) string {
	if res == nil || res.Release == nil {
		return "no release"
	}
	return fmt.Sprintf("release %s: %s", res.Release.Short(), plural(len(res.Release.Images), "image"))
}

func (m *Manager) build(ctx context.Context, req release.BuildRequest, out io.Writer) (*release.BuildResult, error) {
	if m.fw().Supply == nil {
		return m.Builder.Build(ctx, req, out)
	}
	res, err := m.fw().Supply.Ensure(ctx, release.EnsureRequest{
		App: req.App, Env: req.Env, Ref: req.Ref, Services: req.Services, Force: req.Force,
	}, out)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, fmt.Errorf("build %s: the fleet answered with no release", req.App+"/"+req.Env)
	}

	b := &release.BuildResult{Release: res.Release, Built: res.Built, Plan: res.Plan}
	if res.Release != nil {
		services := make([]string, 0, len(res.Release.Images))
		for svc := range res.Release.Images {
			services = append(services, svc)
		}
		sort.Strings(services)
		for _, svc := range services {
			b.Images = append(b.Images, res.Release.Images[svc])
		}
	}
	return b, nil
}

func (m *Manager) AppReleases(ctx context.Context, app string, limit int) ([]release.Release, error) {
	if err := ValidateName("app", app); err != nil {
		return nil, err
	}
	rows, err := m.Store.Releases(ctx, app, limit)
	if err != nil {
		return nil, fmt.Errorf("read the releases of app %q: %w", app, err)
	}
	out := make([]release.Release, 0, len(rows))
	attach := make([]*release.Release, 0, len(rows))
	for i := range rows {
		rel, err := release.FromRecord(&rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, *rel)
	}
	for i := range out {
		attach = append(attach, &out[i])
	}
	if m.Images != nil {
		if err := release.Attach(ctx, m.Images, attach...); err != nil {
			return nil, err
		}
	}
	return out, nil
}
