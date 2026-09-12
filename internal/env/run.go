package env

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/state"
)

func (m *Manager) RunOneOff(ctx context.Context, req RunRequest) (int, error) {
	rec, err := m.env(ctx, req.App, req.Name)
	if err != nil {
		return 0, err
	}
	if !req.Test && len(req.Argv) == 0 {
		return 0, fmt.Errorf("caramelo run: a command is required after --")
	}
	p, err := m.buildPlan(ctx, rec, nil, forOneOff)
	if err != nil {
		return 0, err
	}
	s, err := p.service(req.Service)
	if err != nil {
		return 0, err
	}
	command, err := oneOffCommand(req, s, p.test)
	if err != nil {
		return 0, err
	}

	if err := m.ensureNetwork(ctx, rec, p, req.Stderr); err != nil {
		return 0, err
	}
	if err := m.ensureCache(ctx, rec, s, req.Stderr); err != nil {
		return 0, err
	}
	image, err := m.serviceImage(ctx, rec, s, false, req.Stderr)
	if err != nil {
		return 0, err
	}
	spec, err := m.oneOffSpec(rec, p, s, image, command)
	if err != nil {
		return 0, err
	}

	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	action := "run"
	if req.Test {
		action = "test"
	}

	secret, err := m.secretVarsOf(rec, p, s)
	if err != nil {
		return 0, err
	}
	code, err := m.runAttached(ctx, spec, secret, runtime.Streams{
		Stdin: req.Stdin, Stdout: req.Stdout, Stderr: req.Stderr,
	})
	bg := context.WithoutCancel(ctx)
	if err != nil {
		m.event(bg, rec.ID, action, "failed", err.Error())
		return 0, fmt.Errorf("%s in env %q: %w", action, req.Name, err)
	}
	status := "ok"
	if code != 0 {
		status = "failed"
	}
	m.event(bg, rec.ID, action, status, fmt.Sprintf("%s (exit %d)", strings.Join(req.Argv, " "), code))
	return code, nil
}

func (p *upPlan) service(name string) (svcPlan, error) {
	if name != "" {
		if i := indexOfService(p.services, name); i >= 0 {
			return p.services[i], nil
		}
		return svcPlan{}, fmt.Errorf("no such service %q: this app has %s", name, quoteNames(serviceNames(p.services)))
	}
	if len(p.services) == 0 {
		return svcPlan{}, fmt.Errorf("this app has no services: add a `services:` entry with an `image:` to say which toolchain to run in")
	}
	return p.services[0], nil
}

func oneOffCommand(req RunRequest, s svcPlan, test string) ([]string, error) {
	script := ""
	if strings.TrimSpace(s.install) != "" {
		script = "set -e\n" + s.install + "\n"
	}
	switch {
	case req.Test && strings.TrimSpace(test) == "":
		return nil, fmt.Errorf("this app has no test command: add `test: <command>` to %s", config.FileName)
	case req.Test:

		script += test + ` "$@"`
	default:
		script += `exec "$@"`
	}
	argv := append([]string{"sh", "-c", script, "caramelo"}, req.Argv...)
	return argv, nil
}

func (m *Manager) oneOffSpec(rec *state.EnvRecord, p *upPlan, s svcPlan, image string, command []string) (runtime.ContainerSpec, error) {
	v, err := m.varsOf(rec, p, s)
	if err != nil {
		return runtime.ContainerSpec{}, err
	}
	name, err := oneOffName(rec.App, rec.Name, s.name)
	if err != nil {
		return runtime.ContainerSpec{}, err
	}
	spec := runtime.ContainerSpec{
		Name:       name,
		Image:      image,
		Env:        v.plain,
		Labels:     ServiceLabels(rec.App, rec.Name, s.name, m.Version),
		Network:    NetworkName(rec.App, rec.Name),
		Command:    command,
		AutoRemove: true,
	}
	if p.release != nil || s.build != nil {

		return spec, nil
	}

	spec.Binds = []runtime.BindMount{{Host: rec.Worktree, Path: MountPath}}
	spec.WorkDir = MountPath
	spec.User = rootUser
	if s.cache != "" {
		spec.Volumes = []runtime.VolumeMount{{Volume: CacheVolumeName(rec.App, rec.Name), Path: s.cache}}
	}
	return spec, nil
}

func oneOffName(app, env, service string) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("name a one-off container: %w", err)
	}
	return ServiceContainerName(app, env, service) + "-run-" + hex.EncodeToString(b[:]), nil
}
