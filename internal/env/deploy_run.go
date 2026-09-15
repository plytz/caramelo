package env

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/state"
)

type deployRun struct {
	m   *Manager
	rec *state.EnvRecord
	d   *Deploy

	cfg    *config.App
	policy *config.Deploy
	layout portLayout

	pools []*deployPool

	flippedAt time.Time

	countedFrom time.Time
	progress    io.Writer

	up *upPlan

	wait *deployWait

	detached bool

	watchStarted bool

	replaced bool

	pushed bool
}

func (r *deployRun) waitService(ctx context.Context, svc *Service, s svcPlan) error {
	if svc.Health == HealthOK {
		return nil
	}
	if svc.Health == HealthNone {
		progressf(r.progress, "ok", "health", "%s has no check", svc.Name)
		svc.Status = ServiceRunning
		markReplicas(svc, ServiceRunning, ReplicaActive, HealthNone)
		return nil
	}
	return r.m.waitReplicas(ctx, r.rec, svc, s.health, r.m.now().Add(r.m.upTimeout(0)), r.progress)
}

func (m *Manager) missingImages(ctx context.Context, rel *release.Release) ([]string, error) {
	if rel == nil || m.Driver == nil {
		return nil, nil
	}
	var missing []string
	for _, ref := range rel.ImageList() {
		exists, err := m.Driver.ImageExists(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("look for image %s: %w", ref, err)
		}
		if !exists {
			missing = append(missing, ref)
		}
	}
	sort.Strings(missing)
	return missing, nil
}

type deployPool struct {
	service string

	route   route
	exposed bool

	roll *Rollout

	fresh []Replica
	held  []Replica
	drain time.Duration
}

type deployWait struct {
	promote  chan struct{}
	rollback chan string

	done chan struct{}
}

func newDeployWait() *deployWait {
	return &deployWait{promote: make(chan struct{}, 1), rollback: make(chan string, 1), done: make(chan struct{})}
}

const DefaultWatchInterval = 5 * time.Second

func (m *Manager) watchInterval() time.Duration {
	if m.WatchInterval > 0 {
		return m.WatchInterval
	}
	return DefaultWatchInterval
}

func (m *Manager) runDeploy(ctx context.Context, rec *state.EnvRecord, rel *release.Release,
	req DeployRequest, kind DeployKind, reason string, progress io.Writer, unlock func()) (*Deploy, error) {
	r := &deployRun{m: m, rec: rec, progress: progress, wait: newDeployWait()}
	done := false
	defer func() {
		if !done {
			unlock()
		}
	}()

	d := &Deploy{
		App: rec.App, Env: rec.Name, Kind: kind, Status: DeployBuilding,
		Reason: reason, Identity: IdentityFrom(ctx), StartedAt: m.now(),
	}
	r.d = d
	from, err := m.releaseOf(ctx, rec.ReleaseID)
	if err != nil {
		return nil, err
	}
	d.FromRelease = from

	row, err := m.Store.CreateDeploy(ctx, state.Deploy{
		EnvID: rec.ID, FromReleaseID: rec.ReleaseID, Kind: string(kind), Status: string(DeployBuilding),
		Reason: reason, Identity: d.Identity, StartedAt: d.StartedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("record the deploy of env %q: %w", rec.Name, err)
	}
	d.ID = row.ID
	if err := m.Store.SetEnvDeploy(ctx, rec.ID, d.ID); err != nil {
		return nil, fmt.Errorf("record the deploy of env %q: %w", rec.Name, err)
	}
	m.register(d.ID, r.wait)

	if err := r.build(ctx, rel, req); err != nil {
		return d, err
	}
	if err := r.migrate(ctx); err != nil {
		return d, err
	}
	if err := r.start(ctx, req); err != nil {
		return d, err
	}
	if err := r.check(ctx); err != nil {
		return d, err
	}
	if err := r.flip(ctx); err != nil {
		return d, err
	}
	if req.NoWatch {

		done = true
		return r.handOff(ctx, unlock, (*deployRun).watchAll), nil
	}
	if err := r.watch(ctx); err != nil {
		switch {
		case errors.Is(err, errHandedOff):

			done = true
			return r.handOff(ctx, unlock, (*deployRun).await), nil
		case errors.Is(err, errCommanderGone):

			done = true
			return r.handOff(ctx, unlock, (*deployRun).watchAll), nil
		default:
			return d, err
		}
	}
	return d, nil
}

func (r *deployRun) handOff(ctx context.Context, unlock func(), run func(*deployRun, context.Context) error) *Deploy {
	r.syncRollouts()
	snapshot := r.d.clone()
	r.detached = true
	go func() {
		defer unlock()
		bg, done := r.m.detach(ctx)
		defer done()

		_ = run(r, bg)
	}()
	return snapshot
}

func (r *deployRun) build(ctx context.Context, rel *release.Release, req DeployRequest) error {
	started := r.m.now()
	if rel != nil {

		if missing, err := r.m.missingImages(ctx, rel); err != nil {
			return r.fail(ctx, StepBuild, err)
		} else if len(missing) > 0 {
			return r.fail(ctx, StepBuild, fmt.Errorf(
				"release %s has been pruned: %s no longer on this machine. "+
					"`deploy.keep` in %s is how many releases keep their images; "+
					"deploy the commit again to build it back (`caramelo deploy %s --ref %s`)",
				rel.Short(), plural(len(missing), "image")+" "+strings.Join(missing, ", "),
				config.FileName, r.rec.Name, short(rel.Commit)))
		}
		r.d.Release = rel
		if err := r.setRelease(ctx); err != nil {
			return r.fail(ctx, StepBuild, err)
		}
		r.step(ctx, DeployStep{Step: StepBuild, Status: StepSkipped,
			Detail: rel.Short() + " was already built"})
		return r.plan(ctx)
	}
	if r.m.Builder == nil {
		return r.fail(ctx, StepBuild, errNoBuilder())
	}
	r.step(ctx, DeployStep{Step: StepBuild, Status: StepStarted, Detail: "building " + refOf(req.Ref)})

	res, err := r.m.Build(ctx, release.BuildRequest{
		App: r.rec.App, Env: r.rec.Name, Ref: req.Ref, Services: req.Services, Force: req.Force,
	}, r.progress)
	if err != nil {
		return r.fail(ctx, StepBuild, err)
	}
	if res == nil || res.Release == nil {
		return r.fail(ctx, StepBuild, fmt.Errorf("build %s: the builder produced no release", refOf(req.Ref)))
	}
	r.d.Release = res.Release
	if err := r.setRelease(ctx); err != nil {
		return r.fail(ctx, StepBuild, err)
	}
	detail := res.Release.Short() + " reused"
	if res.Built {
		detail = res.Release.Short() + " built"
	}

	if res.Plan.Why != "" {
		detail = res.Release.Short() + " " + string(res.Plan.Action) + ": " + res.Plan.Why
	}
	r.step(ctx, DeployStep{Step: StepBuild, Status: StepOK, Detail: detail,
		Duration: r.m.now().Sub(started)})
	return r.plan(ctx)
}

func (r *deployRun) plan(ctx context.Context) error {
	p, err := r.m.releasePlan(ctx, r.rec, r.d.Release, r.progress)
	if err != nil {
		return r.fail(ctx, StepBuild, err)
	}
	r.cfg, r.layout, r.up = p.cfg, p.layout, p
	r.policy = p.cfg.Deploy
	return nil
}

func (r *deployRun) migrate(ctx context.Context) error {
	if err := r.setStatus(ctx, DeployMigrating); err != nil {
		return err
	}
	cmd := ""
	if r.policy != nil {
		cmd = r.policy.Before
	}
	if cmd == "" {
		r.step(ctx, DeployStep{Step: StepBefore, Status: StepSkipped, Detail: "no `deploy.before`"})
		return nil
	}
	started := r.m.now()
	r.step(ctx, DeployStep{Step: StepBefore, Status: StepStarted, Detail: cmd})
	if err := r.m.deployOneOff(ctx, r.rec, r.up, cmd, nil, r.progress); err != nil {
		return r.fail(ctx, StepBefore, err)
	}
	r.step(ctx, DeployStep{Step: StepBefore, Status: StepOK, Detail: cmd,
		Duration: r.m.now().Sub(started)})
	return nil
}

func (r *deployRun) start(ctx context.Context, req DeployRequest) error {
	if err := r.setStatus(ctx, DeployStarting); err != nil {
		return err
	}
	p := r.up
	wanted, err := p.pick(req.Services)
	if err != nil {
		return r.fail(ctx, StepReplicas, err)
	}
	if err := r.m.planRoutes(ctx, r.rec, p, wanted, r.progress); err != nil {
		return r.fail(ctx, StepReplicas, err)
	}
	deadline := r.m.now().Add(r.m.upTimeout(req.Timeout))
	for _, s := range wanted {
		route, exposed := p.routes[s.name]
		pool := &deployPool{service: s.name, route: route, exposed: exposed,
			roll:  &Rollout{Service: s.name, Host: route.host, URL: PublicURL(route.host)},
			drain: drainOf(p.cfg, s.name)}
		r.pools = append(r.pools, pool)
		if !exposed {

			r.step(ctx, DeployStep{Step: StepReplicas, Status: StepSkipped, Service: s.name,
				Detail: "not behind the edge: it is replaced at the switch"})
			continue
		}
		want := replicaCount(p.cfg, s.name)
		if err := p.layout.fits(s.name, 2*want); err != nil {
			return r.fail(ctx, StepReplicas, err)
		}
		live, err := r.m.liveReplicas(ctx, r.rec, s.name)
		if err != nil {
			return r.fail(ctx, StepReplicas, err)
		}
		old := runningReplicas(numbered(live))
		pool.held = r.m.replicasOf(r.rec, s.name, p.layout, old, ReplicaDraining)

		svc, err := r.m.deployService(r.rec, p, s)
		if err != nil {
			return r.fail(ctx, StepReplicas, err)
		}
		started := r.m.now()
		r.step(ctx, DeployStep{Step: StepReplicas, Status: StepStarted, Service: s.name,
			Detail: fmt.Sprintf("%s from %s", plural(want, "replica"), svc.Image)})
		for _, index := range freeIndexes(old, want) {
			rep, err := r.m.bringUpReplica(ctx, r.rec, p, s, svc, pool.roll, index, deadline, r.progress)
			if rep != nil && err != nil {
				pool.fresh = append(pool.fresh, *rep)
			}
			if err != nil {

				return r.fail(ctx, StepReplicas, errRolloutFailed(s.name, pool.roll, err))
			}
			pool.fresh = append(pool.fresh, *rep)
		}
		r.step(ctx, DeployStep{Step: StepReplicas, Status: StepOK, Service: s.name,
			Detail:   plural(len(pool.fresh), "replica") + " healthy beside the old pool",
			Duration: r.m.now().Sub(started)})
	}
	return nil
}

func (r *deployRun) check(ctx context.Context) error {
	if err := r.setStatus(ctx, DeployChecking); err != nil {
		return err
	}
	cmd := ""
	if r.policy != nil {
		cmd = r.policy.Check
	}
	if cmd == "" {
		r.step(ctx, DeployStep{Step: StepCheck, Status: StepSkipped, Detail: "no `deploy.check`"})
		return nil
	}
	started := r.m.now()
	r.step(ctx, DeployStep{Step: StepCheck, Status: StepStarted, Detail: cmd})
	if err := r.m.deployOneOff(ctx, r.rec, r.up, cmd, r.checkVars(), r.progress); err != nil {
		return r.fail(ctx, StepCheck, err)
	}
	r.step(ctx, DeployStep{Step: StepCheck, Status: StepOK, Detail: cmd,
		Duration: r.m.now().Sub(started)})
	return nil
}

func (r *deployRun) checkVars() map[string]string {
	for _, pool := range r.pools {
		if !pool.exposed || len(pool.fresh) == 0 {
			continue
		}
		host := pool.fresh[0].Container
		port := r.layout.containerPorts[pool.service]
		out := map[string]string{"CARAMELO_CHECK_HOST": host}
		if port > 0 {
			out["CARAMELO_CHECK_URL"] = fmt.Sprintf("http://%s:%d", host, port)
		}
		return out
	}
	return nil
}

func (r *deployRun) flip(ctx context.Context) error {
	p := r.up

	for _, pool := range r.pools {
		if pool.exposed {
			continue
		}
		s, err := p.svc(pool.service)
		if err != nil {
			return r.fail(ctx, StepSwitch, err)
		}
		svc, err := r.m.deployService(r.rec, p, s)
		if err != nil {
			return r.fail(ctx, StepSwitch, err)
		}

		r.replaced = true
		if err := r.m.recreateService(ctx, r.rec, p, s, svc, r.progress); err != nil {
			return r.fail(ctx, StepSwitch, err)
		}

		if err := r.waitService(ctx, svc, s); err != nil {
			return r.fail(ctx, StepSwitch, err)
		}

		_ = r.m.putService(context.WithoutCancel(ctx), r.rec, svc, s.health,
			replicaCount(p.cfg, s.name), p.layout.services[s.name])
		pool.fresh = svc.Replicas
	}

	if err := r.setStatus(ctx, DeployWatching); err != nil {
		return err
	}
	for _, pool := range r.pools {
		if !pool.exposed {
			continue
		}
		s, err := p.svc(pool.service)
		if err != nil {
			return r.failFlip(ctx, err)
		}
		live := oldOf(pool.held)
		table, draining, at := r.m.flipTargets(r.rec, p, s, pool.fresh, live)
		pool.held = draining
		if r.flippedAt.IsZero() {
			r.flippedAt = at
		}
		if err := r.m.recordFlip(ctx, pool.route, table); err != nil {
			return r.failFlip(ctx, err)
		}
	}
	if r.flippedAt.IsZero() {
		r.flippedAt = r.m.now()
	}
	if err := r.m.pushRoutes(ctx, r.progress); err != nil {
		return r.failFlip(ctx, err)
	}

	r.pushed = true
	for _, pool := range r.pools {
		if !pool.exposed {
			continue
		}
		s, _ := p.svc(pool.service)
		r.m.flipSteps(ctx, r.rec, pool.roll, pool.route, pool.fresh, pool.held, r.flippedAt, r.progress)

		svc, err := r.m.deployService(r.rec, p, s)
		if err == nil {
			svc.Replicas, svc.Status, svc.Health = pool.fresh, ServiceRunning, HealthOK

			_ = r.m.putService(context.WithoutCancel(ctx), r.rec, svc, s.health,
				replicaCount(p.cfg, s.name), p.layout.services[s.name])
		}
	}
	r.fillRoutes(ctx)
	r.step(ctx, DeployStep{Step: StepSwitch, Status: StepOK, Detail: r.switchDetail(), At: r.flippedAt})
	return r.hold(ctx)
}

func (r *deployRun) failFlip(ctx context.Context, cause error) error {
	r.unrecordFlip(context.WithoutCancel(ctx))
	return r.fail(ctx, StepSwitch, cause)
}

func (r *deployRun) unrecordFlip(ctx context.Context) {
	at := r.m.now()
	for _, pool := range r.pools {
		if !pool.exposed {
			continue
		}
		table := make([]Replica, 0, len(pool.held))
		for i := range pool.held {
			pool.held[i].State, pool.held[i].Since = ReplicaActive, at
			table = append(table, pool.held[i])
		}
		if err := r.m.recordFlip(ctx, pool.route, table); err != nil {
			progressf(r.progress, "warning", "deploy", "put the route of %s back: %v", pool.service, err)
		}
	}
	if err := r.m.pushRoutes(ctx, r.progress); err != nil {
		progressf(r.progress, "warning", "deploy", "%v", err)
	}
}

func (r *deployRun) hold(ctx context.Context) error {
	held := 0
	for _, pool := range r.pools {
		if !pool.exposed || len(pool.held) == 0 {
			continue
		}
		for i := range pool.held {
			old := pool.held[i]
			started := r.m.now()
			inflight, byDeadline, err := r.m.waitDrain(ctx, pool.route.host, old.Index, pool.drain, r.flippedAt)
			switch {
			case err != nil:
				r.m.step(ctx, r.rec, pool.roll, r.progress, RolloutStep{Replica: old.Index, Step: StepDrain,
					Status: StepSkipped, State: ReplicaDraining, Host: pool.route.host, Detail: err.Error()})
			case byDeadline:
				r.m.step(ctx, r.rec, pool.roll, r.progress, RolloutStep{Replica: old.Index, Step: StepDrain,
					Status: StepOK, State: ReplicaDraining, Host: pool.route.host, Inflight: inflight,
					Detail:   fmt.Sprintf("the %s drain deadline passed with %d still in flight", pool.drain, inflight),
					Duration: r.m.now().Sub(started)})
			default:
				r.m.step(ctx, r.rec, pool.roll, r.progress, RolloutStep{Replica: old.Index, Step: StepDrain,
					Status: StepOK, State: ReplicaDraining, Host: pool.route.host,
					Detail: "the last request finished", Duration: r.m.now().Sub(started)})
			}
			pool.held[i].State, pool.held[i].Since = ReplicaHeld, r.m.now()
			held++
		}
	}
	if held == 0 {
		return nil
	}
	if err := r.pushPools(ctx); err != nil {

		progressf(r.progress, "warning", "deploy", "record the held pool: %v", err)
		r.step(ctx, DeployStep{Step: StepSwitch, Status: StepSkipped,
			Detail: fmt.Sprintf("%s drained; the hold could not be recorded: %v", plural(held, "replica"), err)})
		return nil
	}
	r.step(ctx, DeployStep{Step: StepSwitch, Status: StepOK,
		Detail: fmt.Sprintf("%s drained and held for the watch", plural(held, "replica"))})
	return nil
}

func (r *deployRun) pushPools(ctx context.Context) error {
	for _, pool := range r.pools {
		if !pool.exposed {
			continue
		}
		table := make([]Replica, 0, len(pool.fresh)+len(pool.held))
		table = append(table, pool.fresh...)
		table = append(table, pool.held...)
		if err := r.m.recordFlip(ctx, pool.route, table); err != nil {
			return err
		}
	}
	return r.m.pushRoutes(ctx, r.progress)
}

func (r *deployRun) switchDetail() string {
	fresh, held := 0, 0
	for _, pool := range r.pools {
		fresh += len(pool.fresh)
		held += len(pool.held)
	}
	if held == 0 {
		return fmt.Sprintf("%s serving", plural(fresh, "replica"))
	}
	return fmt.Sprintf("%s serving, %s held", plural(fresh, "replica"), plural(held, "replica"))
}

func (r *deployRun) fillRoutes(ctx context.Context) {
	routes, err := r.m.envRoutes(context.WithoutCancel(ctx), r.rec)
	if err != nil {
		return
	}
	r.d.Routes = nil
	for _, one := range routes {
		r.d.Routes = append(r.d.Routes, one.Host)
	}
	if len(routes) > 0 {
		r.d.URL = PublicURL(routes[0].Host)
	}
}

func (r *deployRun) step(ctx context.Context, s DeployStep) {
	if s.At.IsZero() {
		s.At = r.m.now()
	}
	if s.Status == "" {
		s.Status = StepOK
	}
	r.d.Steps = append(r.d.Steps, s)
	e := deployEvent(r.d, s)
	switch s.Step {
	case StepWatch, StepPromote, StepRollback:

		if r.d.Watch != nil {
			if raw, err := json.Marshal(r.d.Watch); err == nil {
				e.JSON = raw
			}
		}
	}
	r.m.record(context.WithoutCancel(ctx), r.rec.ID, e)

	_ = progress.Emit(r.progress, e)
}

func (r *deployRun) setStatus(ctx context.Context, status DeployStatus) error {
	r.d.Status = status
	if err := r.update(ctx); err != nil {
		return r.fail(ctx, r.lastStep(), err)
	}
	return nil
}

func (r *deployRun) setRelease(ctx context.Context) error {
	if r.d.Release == nil {
		return nil
	}
	return r.update(ctx)
}

func (r *deployRun) update(ctx context.Context) error {
	row := state.Deploy{
		ID: r.d.ID, EnvID: r.rec.ID, Kind: string(r.d.Kind), Status: string(r.d.Status),
		Reason: r.d.Reason, Identity: r.d.Identity, StartedAt: r.d.StartedAt,
		FinishedAt: r.d.FinishedAt, Error: r.d.Error,
	}
	if r.d.Release != nil {
		row.ReleaseID = r.d.Release.ID
	}
	if r.d.FromRelease != nil {
		row.FromReleaseID = r.d.FromRelease.ID
	}
	if err := r.m.Store.UpdateDeploy(ctx, row); err != nil {
		return fmt.Errorf("record deploy %d of env %q: %w", r.d.ID, r.rec.Name, err)
	}
	return nil
}

func (r *deployRun) lastStep() DeployStepName {
	if len(r.d.Steps) == 0 {
		return StepBuild
	}
	return r.d.Steps[len(r.d.Steps)-1].Step
}

func (r *deployRun) fail(ctx context.Context, step DeployStepName, cause error) error {
	if r.pushed {
		return r.rollback(ctx, fmt.Sprintf("%s failed after the switch: %v", step, cause))
	}
	bg := context.WithoutCancel(ctx)
	r.d.Status, r.d.Error, r.d.FinishedAt = DeployFailed, cause.Error(), r.m.now()
	r.step(bg, DeployStep{Step: step, Status: StepFailed, Detail: cause.Error()})
	r.restorePrevious(bg)
	r.removeFresh(bg)
	r.finish(bg)
	return fmt.Errorf("deploy %s to env %q stopped at %s: %w",
		releaseName(r.d.Release), r.rec.Name, step, cause)
}

func (r *deployRun) removeFresh(ctx context.Context) {
	for _, pool := range r.pools {
		if !pool.exposed {
			continue
		}
		for _, rep := range pool.fresh {
			if rep.Container == "" {
				continue
			}
			if err := r.m.stopReplica(ctx, rep.Container); err != nil {
				progressf(r.progress, "warning", "deploy", "%v", err)
			}
		}
		pool.fresh = nil
	}
}

func (r *deployRun) finish(ctx context.Context) {
	if r.d.FinishedAt.IsZero() {
		r.d.FinishedAt = r.m.now()
	}
	r.syncRollouts()
	if err := r.update(ctx); err != nil {
		progressf(r.progress, "warning", "deploy", "%v", err)
	}
	if err := r.m.Store.SetEnvDeploy(ctx, r.rec.ID, 0); err != nil {
		progressf(r.progress, "warning", "deploy", "clear the deploy of env %q: %v", r.rec.Name, err)
	}
	r.m.unregister(r.d.ID)
	if r.wait != nil {
		select {
		case <-r.wait.done:
		default:
			close(r.wait.done)
		}
	}
}

func (r *deployRun) syncRollouts() {
	out := make([]Rollout, 0, len(r.pools))
	for _, pool := range r.pools {
		if pool.roll == nil || len(pool.roll.Steps) == 0 {
			continue
		}
		out = append(out, *pool.roll)
	}
	r.d.Rollouts = out
}

func (m *Manager) register(id int64, w *deployWait) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.watches == nil {
		m.watches = map[int64]*deployWait{}
	}
	m.watches[id] = w
}

func (m *Manager) unregister(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.watches, id)
}

func (m *Manager) waiter(id int64) (*deployWait, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.watches[id]
	return w, ok
}

func (m *Manager) releaseOf(ctx context.Context, id int64) (*release.Release, error) {
	if id == 0 {
		return nil, nil
	}
	row, err := m.Store.Release(ctx, id)
	switch {
	case errors.Is(err, state.ErrNotFound):

		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read release %d: %w", id, err)
	}
	return releaseFromRow(row)
}

func releaseFromRow(row *state.Release) (*release.Release, error) {
	if row == nil {
		return nil, nil
	}
	rel := &release.Release{
		ID: row.ID, App: row.App, Commit: row.Commit, Tree: row.Tree, Ref: row.Ref,
		BuiltBy: row.BuiltBy, BuiltAt: row.BuiltAt,
	}
	if row.ImagesJSON != "" {
		if err := json.Unmarshal([]byte(row.ImagesJSON), &rel.Images); err != nil {
			return nil, fmt.Errorf("decode the images of release %d: %w", row.ID, err)
		}
	}
	if row.ConfigJSON != "" {
		cfg := &config.App{}
		if err := json.Unmarshal([]byte(row.ConfigJSON), cfg); err != nil {
			return nil, fmt.Errorf("decode the config of release %d: %w", row.ID, err)
		}
		rel.Config = cfg
	}
	return rel, nil
}

func (d *Deploy) clone() *Deploy {
	out := *d
	out.Steps = append([]DeployStep(nil), d.Steps...)
	out.Rollouts = append([]Rollout(nil), d.Rollouts...)
	out.Routes = append([]string(nil), d.Routes...)
	if d.Watch != nil {
		w := *d.Watch
		out.Watch = &w
	}
	return &out
}

func oldOf(held []Replica) []replica {
	out := make([]replica, 0, len(held))
	for _, rep := range held {
		out = append(out, replica{index: rep.Index, container: rep.Container, id: rep.ID, running: true,
			status: string(ServiceRunning)})
	}
	return out
}

func refOf(ref string) string {
	if ref == "" {
		return "the environment's branch head"
	}
	return ref
}

func releaseName(rel *release.Release) string {
	if rel == nil {
		return "a release"
	}
	return rel.Short()
}

func errNoBuilder() error {
	return errors.New("this machine cannot build releases: caramelod has no builder " +
		"(run `caramelo server setup` again to upgrade it)")
}

func errNoEdgeForDeploy() error {
	return fmt.Errorf("%w: a deploy flips a pool at the edge and watches what it serves", errNoEdge())
}
