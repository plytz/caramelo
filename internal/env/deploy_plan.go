package env

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vault"
)

func (m *Manager) releasePlan(ctx context.Context, rec *state.EnvRecord, rel *release.Release,
	progress io.Writer) (*upPlan, error) {
	if rel == nil {
		return nil, fmt.Errorf("deploy env %q: there is no release to run", rec.Name)
	}
	cfg := rel.Config
	if cfg == nil {

		var err error
		if cfg, err = m.readConfig(rec.Worktree, rec.App, progress); err != nil {
			return nil, err
		}
	}
	cfg = cfg.ForEnv(rec.Name)

	guess, _ := m.detect(rec.Worktree)

	p := &upPlan{cfg: cfg, test: cfg.Test, release: rel, tree: release.ShortTree(rel.Tree)}

	secrets, err := m.secretsOf(vault.WithReason(ctx, "deploy"), rec.App, rec.Name)
	if err != nil {
		return nil, err
	}
	p.secrets = secrets
	p.secrets.withDepDefaults(cfg.Deps, Mode(rec.Mode).IsRelease())
	if guess != nil {
		p.stack = guess.Stack
		if p.test == "" {
			p.test = guess.Test.Value
		}
	}
	services, err := mergeServices(rec, cfg, guess, forUp)
	if err != nil {
		return nil, err
	}

	for i := range services {
		ref, ok := rel.Image(services[i].name)
		if !ok {
			return nil, fmt.Errorf("release %s of app %q has no image for service %q: "+
				"build it again with `caramelo build %s`", rel.Short(), rec.App, services[i].name, rec.Name)
		}
		services[i].image, services[i].build = ref, nil

		services[i].install, services[i].cache = "", ""
	}
	p.services = services
	if p.layout, err = layout(rec.PortBase, rec.PortCount, cfg.Deps, p.services); err != nil {
		return nil, err
	}
	p.views = p.layout.views(rec, cfg.Deps)
	p.cfg = withServices(cfg, p.services, p.layout)

	p.cfg.Deploy = cfg.Deploy
	p.cfg.Envs = cfg.Envs

	resolved, err := p.cfg.ResolveWith(config.ViewHost, p.views, p.secrets.redacted())
	if err != nil {
		return nil, fmt.Errorf("expand the variables of env %q: %w", rec.Name, err)
	}
	p.vars = managedVars(rec, resolved.Env, "")
	return p, nil
}

func (p *upPlan) svc(name string) (svcPlan, error) {
	if i := indexOfService(p.services, name); i >= 0 {
		return p.services[i], nil
	}
	return svcPlan{}, fmt.Errorf("no such service %q: this app has %s", name, quoteNames(serviceNames(p.services)))
}

func (m *Manager) deployService(rec *state.EnvRecord, p *upPlan, s svcPlan) (*Service, error) {
	svc := &Service{
		Name:          s.name,
		Container:     ReplicaContainerName(rec.App, rec.Name, s.name, 1),
		Image:         s.image,
		Port:          p.layout.services[s.name],
		ContainerPort: p.layout.containerPorts[s.name],
		Protocol:      s.protocol,
		Status:        ServiceStarting,
		Health:        healthOf(s, p.layout.services[s.name]),
		Expose:        string(config.ExposeNone),
		UpdatedAt:     m.now(),
	}
	svc.URL = serviceURL(svc.Port, s.protocol)
	spec, err := m.serviceSpec(rec, p, s, s.image, 1, svc.Port)
	if err != nil {
		return nil, err
	}
	svc.Command, svc.Vars = spec.Command, spec.Env
	if r, ok := p.routes[s.name]; ok {
		svc.Expose, svc.Host, svc.PublicURL = string(config.ExposeHTTPS), r.host, PublicURL(r.host)
	}
	return svc, nil
}

func (m *Manager) deployOneOff(ctx context.Context, rec *state.EnvRecord, p *upPlan, command string,
	extra map[string]string, progress io.Writer) error {
	if len(p.services) == 0 {
		return fmt.Errorf("env %q has no services, so there is nothing to run %q in", rec.Name, command)
	}

	s := p.services[0]
	for _, cand := range p.services {
		if r, ok := p.routes[cand.name]; ok && r.host != "" {
			s = cand
			break
		}
	}
	spec, err := m.oneOffSpec(rec, p, s, s.image, []string{"sh", "-c", command})
	if err != nil {
		return err
	}
	for k, v := range extra {
		if spec.Env == nil {
			spec.Env = map[string]string{}
		}
		spec.Env[k] = v
	}

	secret, err := m.secretVarsOf(rec, p, s)
	if err != nil {
		return err
	}
	out := &strings.Builder{}
	code, err := m.runAttached(ctx, spec, secret, runtime.Streams{Stdout: out, Stderr: out})
	if text := strings.TrimRight(out.String(), "\n"); text != "" {
		progressf(progress, "ok", "deploy", "%s:\n%s", command, text)
	}
	if err != nil {
		return fmt.Errorf("run %q in env %q: %w", command, rec.Name, err)
	}
	if code != 0 {
		return fmt.Errorf("%q exited %d", command, code)
	}
	return nil
}
