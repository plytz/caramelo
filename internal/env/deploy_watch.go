package env

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/state"
)

func (r *deployRun) watch(ctx context.Context) error {
	window := r.policy.WatchWindow()
	manual := r.policy.PromotePolicy() == config.PromoteManual
	if !r.watchStarted {
		r.watchStarted = true
		r.step(ctx, DeployStep{Step: StepWatch, Status: StepStarted,
			Detail: fmt.Sprintf("%s, rolling back over %s of at least %d requests",
				window, r.maxErrorsText(), config.MinErrorRequests)})
	}

	tick := time.NewTicker(r.m.watchInterval())
	defer tick.Stop()
	for {
		if err := r.poll(ctx); err != nil {
			return err
		}
		if w := r.d.Watch; w != nil && w.Breached() {
			return r.rollback(ctx, fmt.Sprintf("the edge counted %d errors in %d requests (%.1f%%, over %s)",
				w.Errors, w.Requests, w.Rate*100, r.maxErrorsText()))
		}
		if why := r.unhealthy(ctx); why != "" {
			return r.rollback(ctx, why)
		}

		elapsed, counted := r.elapsed(), r.counted()
		if w := r.d.Watch; w != nil {
			w.Elapsed, w.Counted, w.CountedFrom = elapsed, counted, r.countedFrom
		}

		over := counted >= window || elapsed >= 2*window
		if over && !manual {
			r.watchPassed(ctx)
			return r.promote(ctx, "the watch window passed")
		}
		if over {
			r.watchPassed(ctx)

			r.step(ctx, DeployStep{Step: StepWatch, Status: StepOK,
				Detail: "the watch window passed; `deploy.promote` is manual, so it is waiting for " +
					"`caramelo promote " + r.rec.Name + "` or `caramelo rollback " + r.rec.Name + "`"})
			return errHandedOff
		}
		select {
		case <-ctx.Done():
			return r.interrupted(ctx)
		case <-r.wait.promote:
			return r.promote(ctx, "promoted by hand")
		case why := <-r.wait.rollback:
			return r.rollback(ctx, why)
		case <-tick.C:
		}
	}
}

func (r *deployRun) interrupted(ctx context.Context) error {
	if r.detached {
		return r.abandon(ctx)
	}
	return errCommanderGone
}

func (r *deployRun) watchPassed(ctx context.Context) {
	detail := fmt.Sprintf("%s passed", r.policy.WatchWindow())
	if w := r.d.Watch; w != nil {
		detail = fmt.Sprintf("%s passed: the edge counted %d requests and %d errors on the new pool",
			w.Window, w.Requests, w.Errors)
		if w.Counted > 0 && w.Counted < w.Window {

			detail += fmt.Sprintf(", over the %s it has been counting for", w.Counted.Round(time.Second))
		}
	}
	r.step(ctx, DeployStep{Step: StepWatch, Status: StepOK, Detail: detail, Duration: r.elapsed()})
}

func (r *deployRun) await(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return r.interrupted(ctx)
		case <-r.wait.promote:
			return r.promote(ctx, "promoted by hand")
		case why := <-r.wait.rollback:
			return r.rollback(ctx, why)
		case <-time.After(r.m.watchInterval()):

			if err := r.poll(ctx); err != nil {
				return err
			}
			if w := r.d.Watch; w != nil && w.Breached() {
				return r.rollback(ctx, fmt.Sprintf("the edge counted %d errors in %d requests (%.1f%%, over %s)",
					w.Errors, w.Requests, w.Rate*100, r.maxErrorsText()))
			}
			if why := r.unhealthy(ctx); why != "" {
				return r.rollback(ctx, why)
			}
		}
	}
}

var errHandedOff = errors.New("the watch continues in the daemon")

var errCommanderGone = errors.New("the commander that started this deploy is gone")

func (r *deployRun) watchAll(ctx context.Context) error {
	err := r.watch(ctx)
	if errors.Is(err, errHandedOff) {
		return r.await(ctx)
	}
	if errors.Is(err, errCommanderGone) {

		r.detached = true
		return r.watchAll(ctx)
	}
	return err
}

func (r *deployRun) poll(ctx context.Context) error {
	w := &DeployWatch{
		Window:      r.policy.WatchWindow(),
		Elapsed:     r.elapsed(),
		MaxRate:     r.policy.MaxErrorRate(),
		MinRequests: config.MinErrorRequests,
	}
	r.d.Watch = w
	if r.m.Edge == nil {
		return nil
	}
	counts, err := r.m.Edge.Counts(ctx, r.flippedAt)
	if err != nil {
		progressf(r.progress, "warning", "deploy", "read the edge's counts: %v", err)
		return nil
	}

	r.countFrom(ctx, counts.Since)
	w.CountedFrom, w.Counted = r.countedFrom, r.counted()
	for _, pool := range r.pools {
		if !pool.exposed {
			continue
		}
		host, ok := counts.Host(pool.route.host)
		if !ok {
			continue
		}

		mine := host.Replicas(indexesOf(pool.fresh)...)
		w.Requests += mine.Requests
		w.Errors += mine.Errors()

		w.Requests += host.Unrouted.Requests
		w.Errors += host.Unrouted.Errors()
	}
	if w.Requests > 0 {
		w.Rate = float64(w.Errors) / float64(w.Requests)
	}
	return nil
}

func (r *deployRun) countFrom(ctx context.Context, since time.Time) {
	if r.countedFrom.IsZero() {

		r.countedFrom = r.flippedAt
	}
	if since.IsZero() || !since.After(r.countedFrom) {
		return
	}

	if now := r.m.now(); since.After(now) {
		since = now
	}
	if !since.After(r.countedFrom) {
		return
	}
	r.countedFrom = since
	r.step(ctx, DeployStep{Step: StepWatch, Status: StepSkipped,
		Detail: fmt.Sprintf("the edge has only been counting since %s, so the window counts from there",
			since.Format(time.RFC3339))})
}

func (r *deployRun) counted() time.Duration {
	if r.countedFrom.IsZero() {
		return r.elapsed()
	}
	return r.m.now().Sub(r.countedFrom)
}

func (r *deployRun) unhealthy(ctx context.Context) string {
	for _, pool := range r.pools {
		for _, rep := range pool.fresh {
			if rep.Container == "" {
				continue
			}
			cur, err := r.m.Driver.Inspect(ctx, rep.Container)
			switch {
			case errors.Is(err, runtime.ErrNotFound):
				return fmt.Sprintf("replica %d of service %q is gone", rep.Index, pool.service)
			case err != nil:

				continue
			case !cur.Running():
				status := cur.Status
				if status == "" {
					status = "not running"
				}
				return fmt.Sprintf("replica %d of service %q is %s", rep.Index, pool.service, status)
			}
		}
	}
	return ""
}

func (r *deployRun) elapsed() time.Duration {
	if r.flippedAt.IsZero() {
		return 0
	}
	return r.m.now().Sub(r.flippedAt)
}

func (r *deployRun) maxErrorsText() string {
	if r.policy != nil && r.policy.MaxErrors != "" {
		return r.policy.MaxErrors
	}
	return config.DefaultMaxErrors
}

func (r *deployRun) promote(ctx context.Context, reason string) error {
	bg := context.WithoutCancel(ctx)
	started := r.m.now()
	r.step(bg, DeployStep{Step: StepPromote, Status: StepStarted, Detail: reason})

	stopped := 0
	for _, pool := range r.pools {
		for _, rep := range pool.held {
			if rep.Container == "" {
				continue
			}
			if err := r.m.stopReplica(bg, rep.Container); err != nil {

				progressf(r.progress, "warning", "deploy", "%v", err)
				continue
			}
			stopped++
		}
		pool.held = nil
	}
	if stopped > 0 {
		if err := r.pushPools(bg); err != nil {
			progressf(r.progress, "warning", "deploy", "%v", err)
		}
	}
	if r.d.Release != nil && r.d.Release.ID != 0 {
		if err := r.m.Store.SetEnvRelease(bg, r.rec.ID, r.d.Release.ID); err != nil {
			progressf(r.progress, "warning", "deploy", "%v", err)
		}
		r.rec.ReleaseID = r.d.Release.ID

		if r.d.Release.Commit != "" && r.up != nil {
			r.rec.Commit = r.d.Release.Commit

			_ = r.m.saveEnv(bg, r.rec, r.cfg, r.up.vars, r.rec.Status)
		}
	}
	r.prune(bg)
	r.d.Status, r.d.FinishedAt = DeployPromoted, r.m.now()
	if r.d.Reason == "" {
		r.d.Reason = reason
	}
	r.step(bg, DeployStep{Step: StepPromote, Status: StepOK,
		Detail:   fmt.Sprintf("%s, %s stopped", reason, plural(stopped, "held replica")),
		Duration: r.m.now().Sub(started)})
	r.finish(bg)
	return nil
}

func (r *deployRun) prune(ctx context.Context) {
	if r.m.Builder == nil {
		return
	}
	keep := r.policy.KeepReleases()
	removed, err := r.m.Builder.Prune(ctx, r.rec.App, keep)
	switch {
	case err != nil:
		progressf(r.progress, "warning", "deploy", "prune the releases of app %q: %v", r.rec.App, err)
	case len(removed) > 0:
		r.step(ctx, DeployStep{Step: StepPromote, Status: StepOK,
			Detail: fmt.Sprintf("%s pruned past the %d kept", plural(len(removed), "image"), keep)})
	}
}

func (r *deployRun) rollback(ctx context.Context, reason string) error {
	bg := context.WithoutCancel(ctx)
	started := r.m.now()
	r.step(bg, DeployStep{Step: StepRollback, Status: StepStarted, Detail: reason})

	back := 0
	at := r.m.now()
	for _, pool := range r.pools {
		if !pool.exposed {
			continue
		}
		for i := range pool.held {
			pool.held[i].State, pool.held[i].Since = ReplicaActive, at
			back++
		}
		for i := range pool.fresh {
			pool.fresh[i].State, pool.fresh[i].Since = ReplicaDraining, at
		}
	}
	if back == 0 && r.d.Status.Flipped() {

		progressf(r.progress, "warning", "deploy", "there is no previous pool to go back to")
	}
	if err := r.pushPools(bg); err != nil {
		return r.rollbackFailed(bg, reason, err)
	}
	for _, pool := range r.pools {
		if !pool.exposed {
			continue
		}
		for _, rep := range pool.fresh {
			if rep.Container == "" {
				continue
			}

			_, _, _ = r.m.waitDrain(bg, pool.route.host, rep.Index, pool.drain, at)
		}
	}
	r.removeFresh(bg)

	for _, pool := range r.pools {
		pool.fresh, pool.held = pool.held, nil
	}
	if err := r.pushPools(bg); err != nil {
		progressf(r.progress, "warning", "deploy", "%v", err)
	}

	r.restorePrevious(bg)

	r.d.Status, r.d.Reason, r.d.FinishedAt = DeployRolledBack, reason, r.m.now()
	if err := r.m.Store.SetEnvDeploy(bg, r.rec.ID, 0); err != nil {
		progressf(r.progress, "warning", "deploy", "%v", err)
	}
	r.step(bg, DeployStep{Step: StepRollback, Status: StepOK,
		Detail:   fmt.Sprintf("%s: %s serving again", reason, plural(back, "replica")),
		Duration: r.m.now().Sub(started)})
	r.finish(bg)
	return nil
}

func (r *deployRun) rollbackFailed(ctx context.Context, reason string, cause error) error {
	r.d.Status, r.d.Error, r.d.FinishedAt = DeployFailed, cause.Error(), r.m.now()
	r.d.Reason = reason
	r.step(ctx, DeployStep{Step: StepRollback, Status: StepFailed, Detail: cause.Error()})
	r.finish(ctx)
	return fmt.Errorf("roll env %q back: %w", r.rec.Name, cause)
}

func (r *deployRun) restorePrevious(ctx context.Context) {
	if r.d.FromRelease == nil || (!r.replaced && !r.pushed) {

		return
	}
	p, err := r.m.releasePlan(ctx, r.rec, r.d.FromRelease, r.progress)
	if err != nil {
		progressf(r.progress, "warning", "deploy", "put the previous release back: %v", err)
		return
	}
	if err := r.m.saveEnv(ctx, r.rec, p.cfg, p.vars, r.rec.Status); err != nil {
		progressf(r.progress, "warning", "deploy", "record the previous release's config again: %v", err)
	}
	for _, pool := range r.pools {
		s, err := p.svc(pool.service)
		if err != nil {
			continue
		}
		svc, err := r.m.deployService(r.rec, p, s)
		if err != nil {
			continue
		}
		if pool.exposed {

			if !r.pushed {
				continue
			}
			svc.Replicas, svc.Status, svc.Health = pool.fresh, ServiceRunning, HealthOK

			_ = r.m.putService(ctx, r.rec, svc, s.health, replicaCount(p.cfg, s.name), p.layout.services[s.name])
			continue
		}
		if !r.replaced {
			continue
		}
		if err := r.m.recreateService(ctx, r.rec, p, s, svc, r.progress); err != nil {
			progressf(r.progress, "warning", "deploy", "put service %q back: %v", pool.service, err)
			continue
		}

		_ = r.m.putService(ctx, r.rec, svc, s.health, replicaCount(p.cfg, s.name), p.layout.services[s.name])
	}
}

func (r *deployRun) abandon(ctx context.Context) error {
	bg := context.WithoutCancel(ctx)
	r.step(bg, DeployStep{Step: StepWatch, Status: StepSkipped,
		Detail: "the watch was interrupted; the old pool is still held and the next caramelod resumes it"})

	_ = r.update(bg)
	r.m.unregister(r.d.ID)
	return nil
}

func indexesOf(pool []Replica) []int {
	out := make([]int, 0, len(pool))
	for _, rep := range pool {
		out = append(out, rep.Index)
	}
	return out
}

func (m *Manager) ResumeDeploys(ctx context.Context) error {
	rows, err := m.Store.UnfinishedDeploys(ctx)
	if err != nil {
		return fmt.Errorf("list the deploys to resume: %w", err)
	}

	var failed []error
	for i := range rows {
		if err := m.resume(ctx, rows[i]); err != nil {
			failed = append(failed, fmt.Errorf("deploy %d of env %d: %w", rows[i].ID, rows[i].EnvID, err))
		}
	}
	return errors.Join(failed...)
}

func (m *Manager) resume(ctx context.Context, row state.Deploy) error {
	rec, err := m.envByID(ctx, row.EnvID)
	switch {
	case errors.Is(err, state.ErrNotFound):

		return m.endResumed(ctx, row, "the environment was destroyed")
	case err != nil:
		return err
	}
	r, err := m.resumeRun(ctx, rec, row)
	if err != nil {
		return err
	}
	if DeployStatus(row.Status) != DeployWatching {

		r.step(ctx, DeployStep{Step: StepRollback, Status: StepOK,
			Detail: "caramelod restarted before the switch, so this deploy never reached a commander"})
		r.d.Status, r.d.Reason, r.d.FinishedAt = DeployRolledBack,
			"caramelod restarted at "+row.Status, m.now()
		r.cleanStrays(ctx)
		r.finish(ctx)
		return nil
	}

	m.takeOverWatch(ctx, rec, r, "caramelod restarted during the watch; the window starts again")
	return nil
}

func (m *Manager) takeOverWatch(ctx context.Context, rec *state.EnvRecord, r *deployRun, detail string) {
	r.flippedAt = m.now()
	r.detached = true
	m.register(r.d.ID, r.wait)
	r.step(ctx, DeployStep{Step: StepWatch, Status: StepStarted, Detail: detail})
	r.watchStarted = true
	go func() {

		defer m.lockEnv(rec.App, rec.Name)()
		bg, done := m.detach(ctx)
		defer done()

		_ = r.watchAll(bg)
	}()
}

func (m *Manager) adopt(ctx context.Context, rec *state.EnvRecord) (*deployWait, bool) {
	if rec.DeployID == 0 {
		return nil, false
	}

	defer m.lockKey("adopt/" + rec.App + "/" + rec.Name)()
	if w, ok := m.waiter(rec.DeployID); ok {
		return w, true
	}
	row, err := m.Store.Deploy(ctx, rec.DeployID)
	if err != nil || row == nil || row.Done() || DeployStatus(row.Status) != DeployWatching {
		return nil, false
	}
	r, err := m.resumeRun(ctx, rec, *row)
	if err != nil {
		return nil, false
	}
	m.takeOverWatch(ctx, rec, r, "this deploy was left watching by a commander that is gone; caramelod picked it up")
	return r.wait, true
}

func (m *Manager) resumeRun(ctx context.Context, rec *state.EnvRecord, row state.Deploy) (*deployRun, error) {
	r := &deployRun{m: m, rec: rec, wait: newDeployWait()}
	rel, err := m.releaseOf(ctx, row.ReleaseID)
	if err != nil {
		return nil, err
	}
	from, err := m.releaseOf(ctx, row.FromReleaseID)
	if err != nil {
		return nil, err
	}
	r.d = &Deploy{
		ID: row.ID, App: rec.App, Env: rec.Name, Kind: DeployKind(row.Kind), Status: DeployStatus(row.Status),
		Release: rel, FromRelease: from, Reason: row.Reason, Identity: row.Identity, StartedAt: row.StartedAt,
	}

	cfg, err := decodeConfig(rec)
	if err != nil {
		return nil, err
	}
	if rel != nil && rel.Config != nil {
		cfg = rel.Config
	}
	cfg = cfg.ForEnv(rec.Name)
	r.cfg, r.policy = cfg, cfg.Deploy
	if r.layout, err = m.layoutOf(rec, cfg); err != nil {
		return nil, err
	}
	if rel != nil {

		if p, perr := m.releasePlan(ctx, rec, rel, nil); perr == nil {
			r.up = p
		}
	}
	rows, err := m.Store.RoutesOfEnv(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("read the routes of env %q: %w", rec.Name, err)
	}
	byService := map[string]*deployPool{}
	normalised := false
	for _, one := range rows {
		pool, ok := byService[one.Service]
		if !ok {
			pool = &deployPool{service: one.Service, exposed: true,
				route: route{id: one.ID, host: one.Host},
				roll:  &Rollout{Service: one.Service, Host: one.Host, URL: PublicURL(one.Host)},
				drain: drainOf(cfg, one.Service)}
			byService[one.Service] = pool
			r.pools = append(r.pools, pool)
			targets, err := m.Store.Targets(ctx, one.ID)
			if err != nil {
				return nil, fmt.Errorf("read the targets of %s: %w", one.Host, err)
			}
			for _, t := range targets {
				rep := Replica{
					Service: one.Service, Index: t.Replica, Port: t.Port,
					Container: ReplicaContainerName(rec.App, rec.Name, one.Service, t.Replica),
					State:     ReplicaState(t.State), Since: t.UpdatedAt,
				}
				switch edge.TargetState(t.State) {
				case edge.TargetHeld, edge.TargetDraining:

					normalised = normalised || edge.TargetState(t.State) != edge.TargetHeld
					rep.State = ReplicaHeld
					pool.held = append(pool.held, rep)
				case edge.TargetActive:
					pool.fresh = append(pool.fresh, rep)
				}
			}
			continue
		}
		pool.route.more = append(pool.route.more, route{id: one.ID, host: one.Host})
	}
	r.fillRoutes(ctx)

	if normalised {
		if err := r.pushPools(ctx); err != nil {
			progressf(r.progress, "warning", "deploy", "record the held pool of env %q: %v", rec.Name, err)
		}
	}
	return r, nil
}

func (r *deployRun) cleanStrays(ctx context.Context) {
	if r.d.Release == nil {
		return
	}
	tree := release.ShortTree(r.d.Release.Tree)
	if tree == "" {
		return
	}

	routed := map[string]map[int]bool{}
	for _, pool := range r.pools {
		if routed[pool.service] == nil {
			routed[pool.service] = map[int]bool{}
		}
		for _, rep := range pool.fresh {
			routed[pool.service][rep.Index] = true
		}
		for _, rep := range pool.held {
			routed[pool.service][rep.Index] = true
		}
	}
	for _, service := range r.d.Release.Services() {
		live, err := r.m.liveReplicas(ctx, r.rec, service)
		if err != nil {
			continue
		}
		for _, one := range numbered(live) {
			if routed[service][one.index] || one.tree != tree {
				continue
			}
			if err := r.m.stopReplica(ctx, one.container); err != nil {
				progressf(r.progress, "warning", "deploy", "%v", err)
				continue
			}
			r.step(ctx, DeployStep{Step: StepRollback, Status: StepOK, Service: service,
				Detail: one.container + " removed: it was started by a deploy that never switched"})
		}
	}
}

func (m *Manager) endResumed(ctx context.Context, row state.Deploy, reason string) error {
	row.Status, row.Reason, row.FinishedAt = DeployRolledBack.String(), reason, m.now()
	if err := m.Store.UpdateDeploy(ctx, row); err != nil {
		return fmt.Errorf("end deploy %d: %w", row.ID, err)
	}
	return nil
}

func (m *Manager) envByID(ctx context.Context, id int64) (*state.EnvRecord, error) {
	recs, err := m.Store.Envs(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list envs: %w", err)
	}
	for i := range recs {
		if recs[i].ID == id {
			return &recs[i], nil
		}
	}
	return nil, state.ErrNotFound
}
