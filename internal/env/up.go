package env

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/stack"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vault"
)

const rootUser = "0"

const serviceLogTail = logTail

type UpResult struct {
	Env      Env       `json:"env"`
	Services []Service `json:"services"`

	Network string `json:"network,omitempty"`

	Image string `json:"image,omitempty"`

	Stack string `json:"stack,omitempty"`

	Rollouts []Rollout `json:"rollouts,omitempty"`

	Routes []edge.Route `json:"routes,omitempty"`

	URL string `json:"url,omitempty"`
}

func (m *Manager) Up(ctx context.Context, req UpRequest, progress io.Writer) (*UpResult, error) {
	if err := ValidateName("app", req.App); err != nil {
		return nil, err
	}
	if err := ValidateName("env", req.Name); err != nil {
		return nil, err
	}
	defer m.lockEnv(req.App, req.Name)()

	rec, err := m.env(ctx, req.App, req.Name)
	if err != nil {
		return nil, err
	}
	if rec.Status == state.EnvDestroying {
		return nil, fmt.Errorf("env %q is being destroyed", req.Name)
	}

	if err := requireDev(rec, "up"); err != nil {
		return nil, err
	}

	progress = m.feed(ctx, rec, progress)

	p, err := m.buildPlan(ctx, rec, progress, forUp)
	if err != nil {
		return nil, err
	}

	if err := m.saveEnv(ctx, rec, p.cfg, p.vars, rec.Status); err != nil {
		return nil, err
	}

	m.republish(ctx, rec, p.cfg, progress)

	wanted, err := p.pick(req.Services)
	if err != nil {
		return nil, err
	}
	m.event(ctx, rec.ID, "up", "started", plural(len(wanted), "service"))

	res := &UpResult{Network: NetworkName(rec.App, rec.Name), Stack: p.stack}
	if e, err := recordToEnv(rec); err == nil {
		res.Env = *e
	}

	if err := m.ensureNetwork(ctx, rec, p, progress); err != nil {
		return res, m.upFailed(ctx, rec, progress, err)
	}

	if err := m.planRoutes(ctx, rec, p, wanted, progress); err != nil {
		return res, m.upFailed(ctx, rec, progress, err)
	}

	if len(p.routes) > 0 {
		tree, err := m.treeHash(ctx, rec)
		if err != nil {
			return res, m.upFailed(ctx, rec, progress, err)
		}
		p.tree = tree
	}
	deadline := m.now().Add(m.upTimeout(req.Timeout))
	for _, s := range wanted {
		svc, roll, err := m.upService(ctx, rec, p, s, req, deadline, progress)
		if svc != nil {
			res.Services = append(res.Services, *svc)
			if svc.Image != "" && s.build != nil {
				res.Image = svc.Image
			}
		}
		if roll != nil {
			res.Rollouts = append(res.Rollouts, *roll)
			if err != nil {
				err = errRolloutFailed(s.name, roll, err)
			}
		}
		if err != nil {
			m.fillRoutes(ctx, rec, res, progress)
			return res, m.upFailed(ctx, rec, progress, err)
		}
	}
	if req.NoWait {
		progressf(progress, "skipped", "health", "--no-wait")
		m.event(ctx, rec.ID, "up", "ok", "started without waiting")
		m.fillRoutes(ctx, rec, res, progress)
		return res, nil
	}

	if err := m.waitServices(ctx, rec, res.Services, p.checks(), deadline, progress); err != nil {
		m.fillRoutes(ctx, rec, res, progress)
		return res, m.upFailed(ctx, rec, progress, err)
	}

	m.republish(ctx, rec, p.cfg, progress)
	m.fillRoutes(ctx, rec, res, progress)
	if res.URL != "" {
		progressf(progress, "ok", "url", "%s", res.URL)
	}
	m.event(ctx, rec.ID, "up", "ok", plural(len(res.Services), "service"))
	return res, nil
}

func (m *Manager) fillRoutes(ctx context.Context, rec *state.EnvRecord, res *UpResult, progress io.Writer) {
	if m.Edge == nil || res == nil {
		return
	}
	routes, err := m.envRoutes(context.WithoutCancel(ctx), rec)
	if err != nil {
		progressf(progress, "warning", "edge", "read the routes of env %q: %v", rec.Name, err)
		return
	}
	res.Routes = routes
	if len(routes) > 0 {
		res.URL = PublicURL(routes[0].Host)
	}
	for i := range res.Services {
		for _, r := range routes {
			if r.Service == res.Services[i].Name {
				res.Services[i].Host = r.Host
				res.Services[i].PublicURL = PublicURL(r.Host)
				res.Services[i].Expose = string(config.ExposeHTTPS)
				break
			}
		}
	}
}

func (m *Manager) upFailed(ctx context.Context, rec *state.EnvRecord, progress io.Writer, cause error) error {
	m.event(context.WithoutCancel(ctx), rec.ID, "up", "failed", cause.Error())
	progressf(progress, "failed", "up", "%v", cause)
	return cause
}

func (m *Manager) upTimeout(req time.Duration) time.Duration {
	if req > 0 {
		return req
	}
	if m.UpTimeout > 0 {
		return m.UpTimeout
	}
	return DefaultUpTimeout
}

type planMode int

const (
	forUp planMode = iota
	forOneOff
)

type upPlan struct {
	cfg      *config.App
	services []svcPlan
	layout   portLayout
	views    config.Views
	stack    string

	vars map[string]string

	test string

	routes map[string]route

	release *release.Release

	tree string

	secrets *envSecrets

	svcVars map[string]svcVars
}

type svcVars struct {
	plain map[string]string

	secret map[string]string

	stored map[string]string
}

func (m *Manager) secretVarsOf(rec *state.EnvRecord, p *upPlan, s svcPlan) (map[string]string, error) {
	v, err := m.varsOf(rec, p, s)
	if err != nil {
		return nil, err
	}
	return v.secret, nil
}

func (m *Manager) varsOf(rec *state.EnvRecord, p *upPlan, s svcPlan) (svcVars, error) {
	if v, ok := p.svcVars[s.name]; ok {
		return v, nil
	}
	plain, secret, stored, err := m.serviceVars(rec, p, s)
	if err != nil {
		return svcVars{}, err
	}
	v := svcVars{plain: plain, secret: secret, stored: stored}
	if p.svcVars == nil {
		p.svcVars = make(map[string]svcVars, len(p.services))
	}
	p.svcVars[s.name] = v
	return v, nil
}

type svcPlan struct {
	name    string
	image   string
	build   *config.Build
	install string
	run     string

	port     int
	portless bool
	protocol string
	health   *config.Health

	cache string
	env   map[string]string

	resources config.Resources

	replicas int
	expose   config.Expose
	drain    time.Duration
}

func (m *Manager) buildPlan(ctx context.Context, rec *state.EnvRecord, progress io.Writer, mode planMode) (*upPlan, error) {
	cfg, err := m.readConfig(rec.Worktree, rec.App, progress)
	if err != nil {
		return nil, err
	}

	cfg = cfg.ForEnv(rec.Name)
	guess, err := m.detect(rec.Worktree)
	if err != nil {
		return nil, fmt.Errorf("detect the stack of %s: %w", rec.Worktree, err)
	}
	p := &upPlan{cfg: cfg, test: cfg.Test}

	if p.secrets, err = m.secretsOf(vault.WithReason(ctx, "up"), rec.App, rec.Name); err != nil {
		return nil, err
	}

	p.secrets.withDepDefaults(cfg.Deps, Mode(rec.Mode).IsRelease())
	if guess != nil {
		p.stack = guess.Stack
		if p.test == "" {
			p.test = guess.Test.Value
		}
		progressf(progress, "ok", "stack", "%s", describeGuess(guess))
	}
	if p.services, err = mergeServices(rec, cfg, guess, mode); err != nil {
		return nil, err
	}
	if p.layout, err = layout(rec.PortBase, rec.PortCount, cfg.Deps, p.services); err != nil {
		return nil, err
	}
	p.views = p.layout.views(rec, cfg.Deps)

	p.cfg = withServices(cfg, p.services, p.layout)

	resolved, err := p.cfg.ResolveWith(config.ViewHost, p.views, p.secrets.redacted())
	if err != nil {
		return nil, fmt.Errorf("expand the variables of env %q: %w", rec.Name, err)
	}
	p.vars = managedVars(rec, resolved.Env, "")
	return p, nil
}

func (p *upPlan) checks() map[string]*config.Health {
	out := make(map[string]*config.Health, len(p.services))
	for _, s := range p.services {
		out[s.name] = s.health
	}
	return out
}

func (p *upPlan) pick(names []string) ([]svcPlan, error) {
	if len(p.services) == 0 {
		return nil, errNothingToRun()
	}
	if len(names) == 0 {
		return p.services, nil
	}
	out := make([]svcPlan, 0, len(names))
	for _, n := range names {
		i := indexOfService(p.services, n)
		if i < 0 {
			return nil, fmt.Errorf("no such service %q: this app has %s", n, quoteNames(serviceNames(p.services)))
		}
		out = append(out, p.services[i])
	}
	return out, nil
}

func mergeServices(rec *state.EnvRecord, cfg *config.App, guess *stack.Guess, mode planMode) ([]svcPlan, error) {
	declared := cfg.Services
	if len(declared) == 0 {
		if guess == nil {
			if mode == forOneOff {

				return nil, nil
			}
			return nil, errNothingToRun()
		}

		declared = []config.Service{{Name: config.DefaultServiceName}}
	}
	out := make([]svcPlan, 0, len(declared))
	for _, s := range declared {
		p := svcPlan{
			name:     s.Name,
			image:    s.Image,
			build:    s.Build,
			install:  s.Install,
			run:      s.Run,
			port:     servicePort(s),
			portless: s.Port == config.PortNone,
			protocol: string(s.Protocol),
			health:   s.Health,
			env:      s.Env,
			replicas: s.ReplicaCount,
			expose:   s.Expose,
			drain:    s.Drain,
		}
		if s.Resources != nil {
			p.resources = *s.Resources
		}
		if p.protocol == "" {
			p.protocol = string(config.ProtocolTCP)
		}
		if guess != nil {
			if p.build == nil && p.image == "" && guess.Build.Value != "" {
				p.build = &config.Build{Context: guess.Build.Value, Dockerfile: guess.Dockerfile.Value}
			}
			if p.build == nil && p.image == "" {
				p.image = guess.Image.Value
			}
			if p.run == "" {
				p.run = guess.Run.Value
			}
			if p.build == nil {

				if p.install == "" {
					p.install = guess.Install.Value
				}
				p.cache = guess.Cache
			}
		}
		if p.run == "" && p.build == nil && mode == forUp {
			return nil, fmt.Errorf("service %q of env %q has no command: give it a `run:` in %s, "+
				"or a Dockerfile so it can be built and run from its own image",
				s.Name, rec.Name, config.FileName)
		}
		if p.image == "" && p.build == nil {
			return nil, fmt.Errorf("service %q of env %q has no image: give it an `image:` in %s, "+
				"or a Dockerfile so it can be built from the repository itself",
				s.Name, rec.Name, config.FileName)
		}
		out = append(out, p)
	}
	return out, nil
}

func errNothingToRun() error {
	return fmt.Errorf("nothing to run: %s has no `run:` and no `services:`, and no stack was detected in the worktree "+
		"(add `run: <command>` to %s, or a Dockerfile)", config.FileName, config.FileName)
}

func withServices(cfg *config.App, services []svcPlan, l portLayout) *config.App {
	out := *cfg
	out.Services = make([]config.Service, 0, len(services))
	for _, s := range services {
		svc := config.Service{
			Name:     s.name,
			Image:    s.image,
			Build:    s.build,
			Install:  s.install,
			Run:      s.run,
			Port:     l.containerPorts[s.name],
			Protocol: config.Protocol(s.protocol),
			Health:   s.health,
			Env:      s.env,

			ReplicaCount: s.replicas,
			Expose:       s.expose,
			Drain:        s.drain,
		}
		if !s.resources.Empty() {

			r := s.resources
			svc.Resources = &r
		}
		if s.portless {
			svc.Port = config.PortNone
		}
		out.Services = append(out.Services, svc)
	}
	return &out
}

func managedVars(rec *state.EnvRecord, vars map[string]string, service string) map[string]string {
	out := make(map[string]string, len(vars)+len(config.ManagedVars)+1)
	for k, v := range vars {
		if config.Managed(k) || k == "CARAMELO_SERVICE" {
			continue
		}
		out[k] = v
	}
	out["PORT"] = strconv.Itoa(rec.PortBase)
	out["CARAMELO_APP"] = rec.App
	out["CARAMELO_ENV"] = rec.Name
	if service != "" {
		out["CARAMELO_SERVICE"] = service
	}
	return out
}

func (m *Manager) detect(dir string) (*stack.Guess, error) {
	if m.Detect != nil {
		return m.Detect(dir)
	}
	return stack.Detect(dir)
}

func describeGuess(g *stack.Guess) string {
	switch {
	case g.Image.Evidence != "":
		return fmt.Sprintf("%s (%s)", g.Stack, g.Image.Evidence)
	case g.Build.Evidence != "":
		return fmt.Sprintf("%s (%s)", g.Stack, g.Build.Evidence)
	}
	return g.Stack
}

func (m *Manager) upService(ctx context.Context, rec *state.EnvRecord, p *upPlan, s svcPlan, req UpRequest,
	deadline time.Time, progress io.Writer) (*Service, *Rollout, error) {
	image, err := m.serviceImage(ctx, rec, s, req.Build, progress)
	if err != nil {
		return nil, nil, err
	}
	if err := m.ensureCache(ctx, rec, s, progress); err != nil {
		return nil, nil, err
	}
	svc := &Service{
		Name:          s.name,
		Container:     ReplicaContainerName(rec.App, rec.Name, s.name, 1),
		Image:         image,
		Port:          p.layout.services[s.name],
		ContainerPort: p.layout.containerPorts[s.name],
		Protocol:      s.protocol,
		Status:        ServiceStarting,
		Health:        healthOf(s, p.layout.services[s.name]),
		Expose:        string(config.ExposeNone),
		UpdatedAt:     m.now(),
	}
	svc.URL = serviceURL(svc.Port, s.protocol)

	spec, err := m.serviceSpec(rec, p, s, image, 1, svc.Port)
	if err != nil {
		return nil, nil, err
	}

	vars, err := m.varsOf(rec, p, s)
	if err != nil {
		return nil, nil, err
	}
	svc.Command, svc.Vars, svc.SecretsDigest = spec.Command, vars.stored, p.secrets.digest

	r, exposed := p.routes[s.name]
	if exposed {
		svc.Expose, svc.Host, svc.PublicURL = string(config.ExposeHTTPS), r.host, PublicURL(r.host)
		if req.NoWait {

			progressf(progress, "warning", "health", "%s is behind %s, so it is rolled out and waited for anyway",
				s.name, r.host)
		}
		roll, err := m.rollService(ctx, rec, p, s, svc, r, deadline, progress)

		_ = m.putService(context.WithoutCancel(ctx), rec, svc, s.health, replicaCount(p.cfg, s.name), p.layout.services[s.name])
		if err != nil {
			return svc, roll, err
		}
		svc.Change = changeOf(roll, svc.Change)
		return svc, roll, nil
	}
	if err := m.recreateService(ctx, rec, p, s, svc, progress); err != nil {
		return svc, nil, err
	}
	if err := m.putService(ctx, rec, svc, s.health, replicaCount(p.cfg, s.name), p.layout.services[s.name]); err != nil {
		return svc, nil, err
	}
	return svc, nil, nil
}

func changeOf(roll *Rollout, current Change) Change {
	if roll == nil || len(roll.Steps) == 0 {
		if current == "" {
			return ChangeUnchanged
		}
		return current
	}
	return ChangeRecreated
}

func (m *Manager) recreateService(ctx context.Context, rec *state.EnvRecord, p *upPlan, s svcPlan,
	svc *Service, progress io.Writer) error {
	want := replicaCount(p.cfg, s.name)
	if err := p.layout.fits(s.name, want); err != nil && p.layout.services[s.name] > 0 {
		return err
	}
	live, err := m.liveReplicas(ctx, rec, s.name)
	if err != nil {
		return err
	}
	same, why, err := m.unchanged(ctx, rec, svc, s.health, want)
	if err != nil {
		return err
	}
	if !same {
		svc.Detail = why
	}

	replicas := make([]Replica, 0, want)
	for index := 1; index <= want; index++ {
		rep, change, err := m.reconcileReplica(ctx, rec, p, s, svc, index, same, progress)
		if err != nil {
			return err
		}
		if svc.Change == "" || change != ChangeUnchanged {
			svc.Change = change
		}
		replicas = append(replicas, *rep)
	}

	for _, o := range live {
		if o.index >= 1 && o.index <= want {
			continue
		}
		if err := m.Driver.Remove(ctx, o.container, true); err != nil {
			return fmt.Errorf("remove %s: %w", o.container, err)
		}
		progressf(progress, "changed", "replica", "%s removed", o.container)
		svc.Change = ChangeRecreated
	}
	svc.Replicas = replicas
	fillFromFirst(svc)
	return nil
}

func (m *Manager) reconcileReplica(ctx context.Context, rec *state.EnvRecord, p *upPlan, s svcPlan, svc *Service,
	index int, same bool, progress io.Writer) (*Replica, Change, error) {
	port := svc.Port
	if port > 0 {
		var err error
		if port, err = p.layout.replicaPort(s.name, index); err != nil {
			return nil, "", err
		}
	}
	spec, err := m.serviceSpec(rec, p, s, svc.Image, index, port)
	if err != nil {
		return nil, "", err
	}
	rep := &Replica{
		Service:   s.name,
		Index:     index,
		Container: spec.Name,
		Port:      port,
		State:     ReplicaStarting,
		Status:    ServiceStarting,
		Health:    healthOf(s, port),
		Since:     m.now(),
	}
	cur, err := m.inspectReplica(ctx, index, spec.Name)
	if err != nil {
		return rep, "", err
	}
	if same && cur.running {
		progressf(progress, "ok", "replica", "%s unchanged", replicaName(s.name, index))
		rep.ID, rep.Status, rep.State = cur.id, ServiceRunning, ReplicaActive
		return rep, ChangeUnchanged, nil
	}
	change := ChangeCreated
	if cur.container != "" && cur.status != "" {
		change = ChangeRecreated
		if err := m.Driver.Remove(ctx, spec.Name, true); err != nil {
			return rep, "", fmt.Errorf("replace %s: %w", spec.Name, err)
		}
	}
	secret, err := m.secretVarsOf(rec, p, s)
	if err != nil {
		return rep, "", err
	}
	id, err := m.runContainer(ctx, spec, secret)
	if err != nil {
		return rep, "", fmt.Errorf("start %s: %w", spec.Name, err)
	}
	rep.ID, rep.Status = id, ServiceRunning
	where := "no published port"
	if port > 0 {
		where = fmt.Sprintf("%s:%d", DepHost, port)
	}
	progressf(progress, "changed", "replica", "%s %s on %s", replicaName(s.name, index), change, where)
	m.addReplicaResource(ctx, rec.ID, spec.Name, s.name, index, port)
	return rep, change, nil
}

func (m *Manager) unchanged(ctx context.Context, rec *state.EnvRecord, want *Service, check *config.Health, replicas int) (bool, string, error) {
	rows, err := m.Store.Services(ctx, rec.ID)
	if err != nil {
		return false, "", fmt.Errorf("read the services of env %q: %w", rec.Name, err)
	}
	var have *state.EnvService
	for i := range rows {
		if rows[i].Name == want.Name {
			have = &rows[i]
			break
		}
	}
	if have == nil {
		return false, "it was not recorded", nil
	}
	wantCmd, wantVars, wantHealth, err := encodeDefinition(want, check)
	if err != nil {
		return false, "", err
	}
	haveReplicas := have.Replicas
	if haveReplicas <= 0 {
		haveReplicas = config.DefaultReplicas
	}
	switch {
	case have.Image != want.Image:
		return false, fmt.Sprintf("the image changed (%s → %s)", have.Image, want.Image), nil
	case have.Port != want.Port || have.ContainerPort != want.ContainerPort || have.Protocol != want.Protocol:
		return false, "the ports changed", nil
	case haveReplicas != replicas:
		return false, fmt.Sprintf("the replica count changed (%d → %d)", haveReplicas, replicas), nil
	case have.CommandJSON != wantCmd:
		return false, "the command changed", nil
	case have.VarsJSON != wantVars:
		return false, varsChangedWhy(have.VarsJSON, wantVars), nil
	case have.HealthJSON != wantHealth:
		return false, "the health check changed", nil
	}
	return true, "", nil
}

func varsChangedWhy(have, want string) string {
	var h, w map[string]string
	if json.Unmarshal([]byte(have), &h) != nil || json.Unmarshal([]byte(want), &w) != nil {
		return "the variables changed"
	}
	if h[SecretsDigestKey] == w[SecretsDigestKey] {
		return "the variables changed"
	}
	delete(h, SecretsDigestKey)
	delete(w, SecretsDigestKey)
	if maps.Equal(h, w) {
		return "a secret this environment uses changed"
	}
	return "the variables and a secret changed"
}

func encodeDefinition(s *Service, check *config.Health) (command, vars, health string, err error) {
	cmd, err := json.Marshal(s.Command)
	if err != nil {
		return "", "", "", fmt.Errorf("encode the command of service %q: %w", s.Name, err)
	}

	document := s.Vars
	if s.SecretsDigest != "" {
		document = maps.Clone(s.Vars)
		if document == nil {
			document = map[string]string{}
		}
		document[SecretsDigestKey] = s.SecretsDigest
	}
	v, err := json.Marshal(document)
	if err != nil {
		return "", "", "", fmt.Errorf("encode the variables of service %q: %w", s.Name, err)
	}
	h, err := json.Marshal(check)
	if err != nil {
		return "", "", "", fmt.Errorf("encode the health of service %q: %w", s.Name, err)
	}
	return string(cmd), string(v), string(h), nil
}

func (m *Manager) serviceSpec(rec *state.EnvRecord, p *upPlan, s svcPlan, image string, replica, port int) (runtime.ContainerSpec, error) {

	v, err := m.varsOf(rec, p, s)
	if err != nil {
		return runtime.ContainerSpec{}, err
	}
	spec := runtime.ContainerSpec{
		Name:    ReplicaContainerName(rec.App, rec.Name, s.name, replica),
		Image:   image,
		Env:     v.plain,
		Labels:  ReplicaLabels(rec.App, rec.Name, s.name, m.Version, p.tree),
		Restart: runtime.RestartUnlessStopped,
		Network: NetworkName(rec.App, rec.Name),
		Aliases: []string{s.name},
	}
	if port > 0 {
		spec.Publish = []runtime.PortMap{{
			HostIP:        DepHost,
			HostPort:      port,
			ContainerPort: p.layout.containerPorts[s.name],
			Protocol:      s.protocol,
		}}
	}

	spec.Memory, spec.CPU = s.resources.Memory, s.resources.CPU
	if p.release != nil || s.build != nil {

		if s.run != "" {
			spec.Command = shellCommand("", s.run)
		}
		return spec, nil
	}

	spec.Binds = []runtime.BindMount{{Host: rec.Worktree, Path: MountPath}}
	spec.WorkDir = MountPath
	spec.User = rootUser
	spec.Command = shellCommand(s.install, s.run)
	if s.cache != "" {
		spec.Volumes = []runtime.VolumeMount{{Volume: CacheVolumeName(rec.App, rec.Name), Path: s.cache}}
	}
	return spec, nil
}

func (m *Manager) serviceVars(rec *state.EnvRecord, p *upPlan, s svcPlan) (plain, secret, stored map[string]string, err error) {
	real, err := m.expandService(rec, p, s, p.secrets.lookups())
	if err != nil {
		return nil, nil, nil, err
	}
	if p.secrets.empty() {
		return real, nil, real, nil
	}
	hidden, err := m.expandService(rec, p, s, p.secrets.redacted())
	if err != nil {
		return nil, nil, nil, err
	}
	plain, secret, stored = secretVars(real, hidden)

	for name, value := range p.secrets.values {
		if _, taken := plain[name]; taken {
			continue
		}
		secret[name] = value
		stored[name] = vault.Redacted
	}
	return plain, secret, stored, nil
}

func (m *Manager) expandService(rec *state.EnvRecord, p *upPlan, s svcPlan,
	lookups config.Secrets) (map[string]string, error) {
	ctx := p.views.ContextWith(config.ViewNetwork, lookups)
	app, err := m.expand(p.cfg.Env, ctx)
	if err != nil {
		return nil, fmt.Errorf("expand the variables of env %q: %w", rec.Name, err)
	}
	own, err := m.expand(s.env, ctx)
	if err != nil {
		return nil, fmt.Errorf("expand the variables of service %q: %w", s.name, err)
	}
	merged := make(map[string]string, len(app)+len(own))
	for k, v := range app {
		merged[k] = v
	}
	for k, v := range own {
		merged[k] = v
	}
	vars := managedVars(rec, merged, s.name)
	if port := p.layout.containerPorts[s.name]; port > 0 {

		vars["PORT"] = strconv.Itoa(port)
	}
	return vars, nil
}

func shellCommand(install, run string) []string {
	if strings.TrimSpace(install) == "" {
		return []string{"sh", "-c", run}
	}
	return []string{"sh", "-c", "set -e\n" + install + "\nexec sh -c " + shellQuote(run)}
}

func (m *Manager) putService(ctx context.Context, rec *state.EnvRecord, svc *Service, check *config.Health,
	replicas, port int) error {
	cmd, vars, health, err := encodeDefinition(svc, check)
	if err != nil {
		return err
	}
	if replicas <= 0 {
		replicas = config.DefaultReplicas
	}
	if port <= 0 {
		port = svc.Port
	}
	row := state.EnvService{
		EnvID:         rec.ID,
		Name:          svc.Name,
		Image:         svc.Image,
		Container:     svc.Container,
		Port:          port,
		ContainerPort: svc.ContainerPort,
		Protocol:      svc.Protocol,
		CommandJSON:   cmd,
		VarsJSON:      vars,
		HealthJSON:    health,
		Status:        string(svc.Status),
		UpdatedAt:     m.now(),
		Replicas:      replicas,
	}
	if err := m.Store.PutService(ctx, row); err != nil {
		return fmt.Errorf("record service %q: %w", svc.Name, err)
	}
	return nil
}

func (m *Manager) addServiceResource(ctx context.Context, envID int64, kind, name, service string, port int) {

	_ = m.Store.AddResource(ctx, state.EnvResource{
		EnvID:     envID,
		Kind:      kind,
		Name:      name,
		Service:   service,
		Port:      port,
		CreatedAt: m.now(),
	})
}

func healthOf(s svcPlan, port int) HealthStatus {
	if noHealthCheck(s, port) {
		return HealthNone
	}
	return HealthWaiting
}

func noHealthCheck(s svcPlan, port int) bool {
	if s.health != nil && len(s.health.Command) > 0 {
		return false
	}
	if port == 0 {
		return true
	}
	return strings.EqualFold(s.protocol, string(config.ProtocolUDP))
}

func (m *Manager) waitServices(ctx context.Context, rec *state.EnvRecord, services []Service, checks map[string]*config.Health, deadline time.Time, progress io.Writer) error {
	for i := range services {
		s := &services[i]
		if s.Health == HealthOK {
			continue
		}
		if s.Health == HealthNone {

			progressf(progress, "ok", "health", "%s has no check", s.Name)
			s.Status = ServiceRunning
			markReplicas(s, ServiceRunning, ReplicaActive, HealthNone)
			if err := m.putService(ctx, rec, s, checks[s.Name], len(s.Replicas), 0); err != nil {
				return err
			}
			continue
		}
		if err := m.waitReplicas(ctx, rec, s, checks[s.Name], deadline, progress); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) waitReplicas(ctx context.Context, rec *state.EnvRecord, s *Service, check *config.Health,
	deadline time.Time, progress io.Writer) error {
	for i := range s.Replicas {
		rep := &s.Replicas[i]
		probe := &Service{Name: replicaName(s.Name, rep.Index), Container: rep.Container, Port: rep.Port}
		err := m.waitService(ctx, probe, check, deadline)
		if err == nil {
			progressf(progress, "ok", "health", "%s", probe.Name)
			rep.Status, rep.Health, rep.State = ServiceRunning, HealthOK, ReplicaActive
			continue
		}
		rep.Status, rep.Health, rep.State, rep.Detail = ServiceFailed, HealthFailed, ReplicaFailed, err.Error()
		s.Status, s.Health, s.Detail = ServiceFailed, HealthFailed, err.Error()

		_ = m.putService(context.WithoutCancel(ctx), rec, s, check, len(s.Replicas), 0)
		if out, lerr := m.Driver.LogTail(context.WithoutCancel(ctx), rep.Container, serviceLogTail); lerr == nil && out != "" {
			progressf(progress, "warning", "logs", "last %d lines of %s:\n%s", serviceLogTail, rep.Container, out)
		}
		return fmt.Errorf("service %q was not healthy: %w", probe.Name, err)
	}
	s.Status, s.Health = ServiceRunning, HealthOK
	fillFromFirst(s)
	return m.putService(ctx, rec, s, check, len(s.Replicas), 0)
}

func markReplicas(s *Service, status ServiceStatus, state ReplicaState, health HealthStatus) {
	for i := range s.Replicas {
		s.Replicas[i].Status, s.Replicas[i].State, s.Replicas[i].Health = status, state, health
	}
}

func (m *Manager) waitService(ctx context.Context, s *Service, check *config.Health, deadline time.Time) error {
	var last error
	for {
		err := m.checkService(ctx, s, check)
		if err == nil {
			return nil
		}
		last = err
		if !m.now().Before(deadline) {
			return last
		}
		if err := sleep(ctx, m.readyInterval()); err != nil {
			return err
		}
	}
}

func (m *Manager) checkService(ctx context.Context, s *Service, check *config.Health) error {
	before, err := m.runningState(ctx, s)
	if err != nil {
		return err
	}
	if err := m.probeService(ctx, s, check); err != nil {
		return err
	}
	after, err := m.runningState(ctx, s)
	if err != nil {
		return err
	}
	if after.Restarts != before.Restarts {
		return fmt.Errorf("the container restarted while it was being checked (%d restarts): "+
			"the service is not staying up", after.Restarts)
	}
	return nil
}

func (m *Manager) runningState(ctx context.Context, s *Service) (runtime.ContainerState, error) {
	cur, err := m.Driver.Inspect(ctx, s.Container)
	switch {
	case errors.Is(err, runtime.ErrNotFound):
		return cur, fmt.Errorf("the container %s is gone", s.Container)
	case err != nil:
		return cur, fmt.Errorf("inspect %s: %w", s.Container, err)
	case !cur.Running():
		state := cur.Status
		if state == "" {
			state = "not running"
		}
		return cur, fmt.Errorf("the container is %s", state)
	}
	return cur, nil
}

func (m *Manager) probeService(ctx context.Context, s *Service, check *config.Health) error {
	switch {
	case check != nil && len(check.Command) > 0:
		res, err := m.Driver.Exec(ctx, s.Container, check.Command)
		switch {
		case err != nil:
			return err
		case res.ExitCode != 0:
			detail := firstLine(res.Stderr)
			if detail == "" {
				detail = firstLine(res.Stdout)
			}
			return fmt.Errorf("health command exited %d: %s", res.ExitCode, detail)
		}
		return nil
	case check != nil && check.Path != "":
		url := fmt.Sprintf("http://%s:%d%s", DepHost, s.Port, check.Path)
		code, err := m.httpStatus(ctx, url)
		if err != nil {
			return err
		}
		if code < 200 || code > 399 {
			return fmt.Errorf("GET %s answered %d", url, code)
		}
		return nil
	default:
		return m.dial(ctx, net.JoinHostPort(DepHost, strconv.Itoa(s.Port)))
	}
}

const probeRead = 250 * time.Millisecond

func (m *Manager) dial(ctx context.Context, addr string) error {
	if m.DialTCP != nil {
		return m.DialTCP(ctx, addr, m.readyInterval())
	}
	var d net.Dialer
	c, cancel := context.WithTimeout(ctx, m.readyInterval())
	defer cancel()
	conn, err := d.DialContext(c, "tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	wait := probeRead
	if half := m.readyInterval() / 2; half < wait {
		wait = half
	}
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		return err
	}
	var b [1]byte
	switch _, err := conn.Read(b[:]); {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrDeadlineExceeded):
		return nil
	default:
		return fmt.Errorf("%s accepted a connection and dropped it at once (%v): "+
			"nothing is listening on that port inside the container", addr, err)
	}
}

func (m *Manager) httpStatus(ctx context.Context, url string) (int, error) {
	if m.HTTPStatus != nil {
		return m.HTTPStatus(ctx, url)
	}
	c, cancel := context.WithTimeout(ctx, m.readyInterval())
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	return resp.StatusCode, nil
}

func serviceURL(port int, protocol string) string {
	if port == 0 {
		return ""
	}
	scheme := "http"
	if strings.EqualFold(protocol, string(config.ProtocolUDP)) {
		scheme = "udp"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, DepHost, port)
}

func indexOfService(services []svcPlan, name string) int {
	for i, s := range services {
		if s.name == name {
			return i
		}
	}
	return -1
}

func serviceNames(services []svcPlan) []string {
	out := make([]string, 0, len(services))
	for _, s := range services {
		out = append(out, s.name)
	}
	sort.Strings(out)
	return out
}

func quoteNames(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = strconv.Quote(n)
	}
	return strings.Join(out, ", ")
}
