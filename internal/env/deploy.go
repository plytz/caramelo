package env

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/state"
)

type Mode string

const (
	ModeDev Mode = "dev"

	ModeRelease Mode = "release"
)

var Modes = []Mode{ModeDev, ModeRelease}

func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case "", ModeDev:
		return ModeDev, nil
	case ModeRelease:
		return ModeRelease, nil
	default:
		return "", fmt.Errorf("unknown environment mode %q: want %s or %s", s, ModeDev, ModeRelease)
	}
}

func (m Mode) IsRelease() bool { return m == ModeRelease }

func (m Mode) String() string {
	if m == "" {
		return string(ModeDev)
	}
	return string(m)
}

type DeployKind string

const (
	DeployKindDeploy DeployKind = "deploy"

	DeployKindRollback DeployKind = "rollback"
)

type DeployStatus string

const (
	DeployBuilding  DeployStatus = state.DeployBuilding
	DeployMigrating DeployStatus = state.DeployMigrating
	DeployStarting  DeployStatus = state.DeployStarting
	DeployChecking  DeployStatus = state.DeployChecking

	DeployWatching DeployStatus = state.DeployWatching

	DeployPromoted DeployStatus = state.DeployPromoted

	DeployRolledBack DeployStatus = state.DeployRolledBack

	DeployFailed DeployStatus = state.DeployFailed
)

var DeployStatuses = []DeployStatus{
	DeployBuilding, DeployMigrating, DeployStarting, DeployChecking, DeployWatching,
	DeployPromoted, DeployRolledBack, DeployFailed,
}

func (s DeployStatus) Done() bool { return state.DeployDone(string(s)) }

func (s DeployStatus) Flipped() bool {
	switch s {
	case DeployWatching, DeployPromoted, DeployRolledBack:
		return true
	}
	return false
}

func (s DeployStatus) String() string { return string(s) }

type DeployStepName string

const (
	StepBuild DeployStepName = "build"

	StepBefore DeployStepName = "before"

	StepReplicas DeployStepName = "replicas"

	StepCheck DeployStepName = "check"

	StepSwitch DeployStepName = "switch"

	StepWatch DeployStepName = "watch"

	StepPromote DeployStepName = "promote"

	StepRollback DeployStepName = "rollback"
)

var DeployStepNames = []DeployStepName{
	StepBuild, StepBefore, StepReplicas, StepCheck, StepSwitch, StepWatch, StepPromote, StepRollback,
}

type DeployStep struct {
	Step DeployStepName `json:"step"`

	Status StepStatus `json:"status"`

	Service string `json:"service,omitempty"`

	Detail string `json:"detail,omitempty"`

	Duration time.Duration `json:"duration,omitempty"`
	At       time.Time     `json:"at"`
}

type DeployWatch struct {
	Window  time.Duration `json:"window"`
	Elapsed time.Duration `json:"elapsed"`

	Requests int `json:"requests"`
	Errors   int `json:"errors"`

	Rate    float64 `json:"rate"`
	MaxRate float64 `json:"max_rate"`

	MinRequests int `json:"min_requests"`

	CountedFrom time.Time     `json:"counted_from,omitempty"`
	Counted     time.Duration `json:"counted,omitempty"`
}

func (w DeployWatch) Breached() bool {
	if w.Requests < w.MinRequests || w.MaxRate < 0 {
		return false
	}
	return w.Rate > w.MaxRate
}

type Deploy struct {
	ID  int64  `json:"id"`
	App string `json:"app"`

	Env string `json:"env"`

	Kind DeployKind `json:"kind"`

	Status DeployStatus `json:"status"`

	Release     *release.Release `json:"release,omitempty"`
	FromRelease *release.Release `json:"from_release,omitempty"`

	Steps []DeployStep `json:"steps,omitempty"`

	Rollouts []Rollout `json:"rollouts,omitempty"`

	Watch *DeployWatch `json:"watch,omitempty"`

	Routes []string `json:"routes,omitempty"`

	URL string `json:"url,omitempty"`

	Reason string `json:"reason,omitempty"`

	Identity   string    `json:"identity,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`

	Error string `json:"error,omitempty"`
}

func (d Deploy) Done() bool { return d.Status.Done() }

type DeployRequest struct {
	App string `json:"app"`

	Env string `json:"env"`

	Ref string `json:"ref,omitempty"`

	Services []string `json:"services,omitempty"`

	NoWatch bool `json:"no_watch,omitempty"`

	Timeout time.Duration `json:"timeout,omitempty"`

	Force bool `json:"force,omitempty"`
}

type PromoteRequest struct {
	App string `json:"app"`
	Env string `json:"env"`
}

type RollbackRequest struct {
	App string `json:"app"`
	Env string `json:"env"`

	To string `json:"to,omitempty"`

	Timeout time.Duration `json:"timeout,omitempty"`
}

type EventsRequest struct {
	App string `json:"app,omitempty"`
	Env string `json:"env,omitempty"`

	Since time.Time `json:"since,omitempty"`

	Limit int `json:"limit,omitempty"`

	Follow bool `json:"follow,omitempty"`
}

const DefaultEventLimit = 100

const DefaultDeployTimeout = 30 * time.Minute

func errDeployNotImplemented(what string) error {
	return fmt.Errorf("%s: %w", what, state.ErrNotImplemented)
}

func (m *Manager) Deploy(ctx context.Context, req DeployRequest, progress io.Writer) (*Deploy, error) {
	rec, err := m.deployable(ctx, req.App, req.Env, "deploy")
	if err != nil {
		return nil, err
	}

	if err := m.noDeployInFlight(ctx, rec); err != nil {
		return nil, err
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	rec, unlock, err := m.lockDeployable(ctx, req.App, req.Env, "deploy")
	if err != nil {
		return nil, err
	}
	return m.runDeployWith(ctx, rec, nil, req, DeployKindDeploy, "", progress, unlock)
}

func (m *Manager) lockDeployable(ctx context.Context, app, name, verb string) (
	*state.EnvRecord, func(), error) {
	unlock := m.lockEnv(app, name)
	rec, err := m.deployable(ctx, app, name, verb)
	if err == nil {
		err = m.noDeployInFlight(ctx, rec)
	}
	if err != nil {
		unlock()
		return nil, nil, err
	}
	return rec, unlock, nil
}

func (m *Manager) Promote(ctx context.Context, req PromoteRequest, progress io.Writer) (*Deploy, error) {
	rec, err := m.deployable(ctx, req.App, req.Env, "promote")
	if err != nil {
		return nil, err
	}
	return m.endWatching(ctx, rec, "promote", func(w *deployWait) {
		select {
		case w.promote <- struct{}{}:
		default:
		}
	})
}

func (m *Manager) Rollback(ctx context.Context, req RollbackRequest, progress io.Writer) (*Deploy, error) {
	rec, err := m.deployable(ctx, req.App, req.Env, "rollback")
	if err != nil {
		return nil, err
	}
	if req.To == "" && rec.DeployID != 0 {
		d, err := m.endWatching(ctx, rec, "rollback", func(w *deployWait) {
			select {
			case w.rollback <- "rolled back by hand":
			default:
			}
		})
		if err == nil {
			return d, nil
		}
		if !errors.Is(err, errNotWatching) {
			return nil, err
		}

	}

	if _, err := m.rollbackTarget(ctx, rec, req.To); err != nil {
		return nil, err
	}
	if err := m.noDeployInFlight(ctx, rec); err != nil {
		return nil, err
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	rec, unlock, err := m.lockDeployable(ctx, req.App, req.Env, "rollback")
	if err != nil {
		return nil, err
	}

	rel, err := m.rollbackTarget(ctx, rec, req.To)
	if err != nil {
		unlock()
		return nil, err
	}
	return m.runDeployWith(ctx, rec, rel, DeployRequest{App: req.App, Env: req.Env, Timeout: req.Timeout},
		DeployKindRollback, "rollback to "+rel.Short(), progress, unlock)
}

func (m *Manager) Releases(ctx context.Context, app, name string, limit int) ([]Deploy, error) {
	rec, err := m.env(ctx, app, name)
	if err != nil {
		return nil, err
	}
	rows, err := m.Store.Deploys(ctx, rec.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("read the deploys of env %q: %w", name, err)
	}
	out := make([]Deploy, 0, len(rows))
	for _, row := range rows {
		d := Deploy{
			ID: row.ID, App: rec.App, Env: rec.Name, Kind: DeployKind(row.Kind),
			Status: DeployStatus(row.Status), Reason: row.Reason, Identity: row.Identity,
			StartedAt: row.StartedAt, FinishedAt: row.FinishedAt, Error: row.Error,
		}

		if d.Release, err = m.releaseOf(ctx, row.ReleaseID); err != nil {
			return nil, err
		}
		if d.FromRelease, err = m.releaseOf(ctx, row.FromReleaseID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

var errNotWatching = errors.New("no deploy is waiting")

func (m *Manager) deployable(ctx context.Context, app, name, verb string) (*state.EnvRecord, error) {
	if err := ValidateName("app", app); err != nil {
		return nil, err
	}
	if err := ValidateName("env", name); err != nil {
		return nil, err
	}
	rec, err := m.env(ctx, app, name)
	if err != nil {
		return nil, err
	}
	if rec.Status == state.EnvDestroying {
		return nil, fmt.Errorf("env %q is being destroyed", name)
	}
	if err := requireRelease(rec, verb); err != nil {
		return nil, err
	}
	if m.Edge == nil {
		return nil, errNoEdgeForDeploy()
	}
	return rec, nil
}

func (m *Manager) noDeployInFlight(ctx context.Context, rec *state.EnvRecord) error {
	if rec.DeployID == 0 {
		return nil
	}
	row, err := m.Store.Deploy(ctx, rec.DeployID)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("read the deploy of env %q: %w", rec.Name, err)
	case row.Done():
		return nil
	}
	return fmt.Errorf("deploy %d of env %q is still %s: finish it with `caramelo promote %s` "+
		"or `caramelo rollback %s`", row.ID, rec.Name, row.Status, rec.Name, rec.Name)
}

func (m *Manager) runDeployWith(ctx context.Context, rec *state.EnvRecord, rel *release.Release,
	req DeployRequest, kind DeployKind, reason string, progress io.Writer, unlock func()) (*Deploy, error) {
	m.event(ctx, rec.ID, string(kind), "started", refOf(req.Ref))
	d, err := m.runDeploy(ctx, rec, rel, req, kind, reason, progress, unlock)
	if err != nil {
		return d, err
	}
	return d, nil
}

func (m *Manager) endWatching(ctx context.Context, rec *state.EnvRecord, verb string,
	signal func(*deployWait)) (*Deploy, error) {
	if rec.DeployID == 0 {
		return nil, fmt.Errorf("%w: env %q has no deploy in progress", errNotWatching, rec.Name)
	}
	w, ok := m.waiter(rec.DeployID)
	if !ok {

		w, ok = m.adopt(ctx, rec)
	}
	if !ok {
		return nil, fmt.Errorf("%w: deploy %d of env %q is not being watched by this daemon",
			errNotWatching, rec.DeployID, rec.Name)
	}
	signal(w)
	select {
	case <-w.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	row, err := m.Store.Deploy(ctx, rec.DeployID)
	if err != nil {
		return nil, fmt.Errorf("read deploy %d: %w", rec.DeployID, err)
	}
	d, err := m.deployOf(ctx, rec, row)
	if err != nil {
		return nil, err
	}
	m.event(ctx, rec.ID, verb, "ok", string(d.Status))
	return d, nil
}

func (m *Manager) deployOf(ctx context.Context, rec *state.EnvRecord, row *state.Deploy) (*Deploy, error) {
	d := &Deploy{
		ID: row.ID, App: rec.App, Env: rec.Name, Kind: DeployKind(row.Kind),
		Status: DeployStatus(row.Status), Reason: row.Reason, Identity: row.Identity,
		StartedAt: row.StartedAt, FinishedAt: row.FinishedAt, Error: row.Error,
	}
	var err error
	if d.Release, err = m.releaseOf(ctx, row.ReleaseID); err != nil {
		return nil, err
	}
	if d.FromRelease, err = m.releaseOf(ctx, row.FromReleaseID); err != nil {
		return nil, err
	}
	return d, nil
}

func (m *Manager) rollbackTarget(ctx context.Context, rec *state.EnvRecord, to string) (*release.Release, error) {
	if to != "" {
		return m.releaseNamed(ctx, rec.App, to)
	}
	rows, err := m.Store.Deploys(ctx, rec.ID, 0)
	if err != nil {
		return nil, fmt.Errorf("read the deploys of env %q: %w", rec.Name, err)
	}
	for _, row := range rows {
		if DeployStatus(row.Status) != DeployPromoted || row.ReleaseID == 0 || row.ReleaseID == rec.ReleaseID {
			continue
		}
		rel, err := m.releaseOf(ctx, row.ReleaseID)
		if err != nil {
			return nil, err
		}
		if rel != nil {
			return rel, nil
		}
	}
	return nil, fmt.Errorf("env %q has no release to go back to: `caramelo releases %s` lists what it has, "+
		"and `caramelo rollback %s --to <release>` names one", rec.Name, rec.Name, rec.Name)
}

func (m *Manager) releaseNamed(ctx context.Context, app, to string) (*release.Release, error) {
	if id, err := strconv.ParseInt(to, 10, 64); err == nil && id > 0 {
		row, err := m.Store.Release(ctx, id)
		switch {
		case errors.Is(err, state.ErrNotFound):
			return nil, fmt.Errorf("no such release %s of app %q", to, app)
		case err != nil:
			return nil, fmt.Errorf("read release %s: %w", to, err)
		}
		if row.App != app {
			return nil, fmt.Errorf("release %s belongs to app %q, not to %q", to, row.App, app)
		}
		return releaseFromRow(row)
	}
	row, err := m.Store.ReleaseByTree(ctx, app, release.ShortTree(to))
	switch {
	case errors.Is(err, state.ErrNotFound):
		return nil, fmt.Errorf("app %q has no release %q: `caramelo releases` lists what it has", app, to)
	case err != nil:
		return nil, fmt.Errorf("read release %q: %w", to, err)
	}
	return releaseFromRow(row)
}

func requireRelease(rec *state.EnvRecord, verb string) error {
	mode, err := ParseMode(rec.Mode)
	if err != nil {
		return err
	}
	if mode.IsRelease() {
		return nil
	}
	return fmt.Errorf("env %q of app %q is a development environment, so `caramelo %s` does not apply to it: "+
		"change it with `caramelo up`, or make a deployed one with `caramelo env create NAME --release`",
		rec.Name, rec.App, verb)
}

func requireDev(rec *state.EnvRecord, verb string) error {
	mode, err := ParseMode(rec.Mode)
	if err != nil {
		return err
	}
	if !mode.IsRelease() {
		return nil
	}
	return fmt.Errorf("env %q of app %q runs releases, so `caramelo %s` does not apply to it: "+
		"change it with `caramelo deploy %s`", rec.Name, rec.App, verb, rec.Name)
}

func IsNotImplemented(err error) bool { return errors.Is(err, state.ErrNotImplemented) }

func deployEvent(d *Deploy, s DeployStep) progress.Event {
	e := progress.Event{
		Action:  "deploy",
		Step:    string(s.Step),
		Status:  string(s.Status),
		Service: s.Service,
		Detail:  s.Detail,
		At:      s.At,
	}
	if d != nil {
		e.App, e.Env, e.Identity = d.App, d.Env, d.Identity
	}
	return e
}
