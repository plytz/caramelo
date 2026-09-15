package env

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/git"
	"github.com/plytz/caramelo/internal/machine"
	"github.com/plytz/caramelo/internal/ports"
	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/stack"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vpn"
)

const (
	DefaultTimeout = 60 * time.Second

	ReadyInterval = 500 * time.Millisecond
)

const DepHost = "127.0.0.1"

type Dirs struct {
	Data string

	User string

	Run string
}

type Manager struct {
	Store  state.Store
	Driver runtime.Driver
	Git    git.Repo
	Ports  ports.Allocator
	Runner runner.Runner
	Dirs   Dirs

	VPNAlloc vpn.Allocator
	Net      Network

	Edge edge.Client

	Builder release.Builder

	Images release.ImageStore

	Notifier progress.Notifier

	PublicIP netip.Addr

	Version string

	Timeout time.Duration

	ReadyInterval time.Duration

	Now func() time.Time

	UpTimeout time.Duration

	StopTimeout time.Duration

	WatchInterval time.Duration

	LoadConfig func(path string) (*config.App, error)

	Expand func(vars map[string]string, ctx config.ExpandContext) (map[string]string, error)

	Detect func(dir string) (*stack.Guess, error)

	DialTCP    func(ctx context.Context, addr string, timeout time.Duration) error
	HTTPStatus func(ctx context.Context, url string) (int, error)

	Secrets SecretsProvider

	Log io.Writer

	wiringMu sync.Mutex
	wiring   atomic.Pointer[FleetWiring]

	Place func(candidates []fleet.Candidate, d fleet.Demand, pref fleet.Preference, now time.Time) (fleet.Decision, error)

	Background context.Context

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	busy map[string]int

	watches map[int64]*deployWait
}

func New(store state.Store, driver runtime.Driver, repo git.Repo, alloc ports.Allocator, run runner.Runner, dirs Dirs) *Manager {
	return &Manager{
		Store:   store,
		Driver:  driver,
		Git:     repo,
		Ports:   alloc,
		Runner:  run,
		Dirs:    dirs,
		Timeout: DefaultTimeout,
		Now:     time.Now,
	}
}

func (m *Manager) List(ctx context.Context, app string) ([]Env, error) {
	if app != "" {
		if err := ValidateName("app", app); err != nil {
			return nil, err
		}
	}
	recs, err := m.Store.Envs(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("list envs: %w", err)
	}

	rows, err := m.fleetRows(ctx, app)
	if err != nil {
		return nil, err
	}
	out := make([]Env, 0, len(recs))
	for i := range recs {
		e, err := recordToEnv(&recs[i])
		if err != nil {
			return nil, err
		}
		m.stampFleet(e, rows[e.ID])
		out = append(out, *e)
	}
	return out, nil
}

func (m *Manager) fleetRows(ctx context.Context, app string) (map[int64]FleetRow, error) {
	if m.fw().Fleet == nil {
		return nil, nil
	}
	rows, err := m.fw().Fleet.EnvFleetRows(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("read the fleet columns of %q: %w", app, err)
	}
	return rows, nil
}

func (m *Manager) stampFleet(e *Env, row FleetRow) {
	if e == nil {
		return
	}
	e.Owner, e.Via, e.Machine = row.Owner, row.Via, m.fw().Machine
}

func (m *Manager) Show(ctx context.Context, app, name string) (*Env, []DepState, []Event, error) {
	rec, err := m.env(ctx, app, name)
	if err != nil {
		return nil, nil, nil, err
	}
	e, err := recordToEnv(rec)
	if err != nil {
		return nil, nil, nil, err
	}
	m.stampFleet(e, m.fleetRow(ctx, rec.ID))
	deps, err := m.depStates(ctx, rec)
	if err != nil {
		return nil, nil, nil, err
	}
	events, err := m.events(ctx, rec.ID, eventTail)
	if err != nil {
		return nil, nil, nil, err
	}
	return e, deps, events, nil
}

const eventTail = 20

func (m *Manager) depStates(ctx context.Context, rec *state.EnvRecord) ([]DepState, error) {
	cfg, err := decodeConfig(rec)
	if err != nil {
		return nil, err
	}
	out := make([]DepState, 0, len(cfg.Deps))
	for i, dep := range cfg.Deps {
		st := DepState{
			Name:      dep.Name,
			Container: ContainerName(rec.App, rec.Name, dep.Name),
			Status:    DepMissing,
			Port:      depPort(rec.PortBase, i),
		}
		cs, err := m.Driver.Inspect(ctx, st.Container)
		switch {
		case errors.Is(err, runtime.ErrNotFound):
		case err != nil:
			return nil, fmt.Errorf("inspect %s: %w", st.Container, err)
		case cs.Running():
			st.Status = DepRunning
		default:
			st.Status = DepExited
		}
		out = append(out, st)
	}
	return out, nil
}

func (m *Manager) env(ctx context.Context, app, name string) (*state.EnvRecord, error) {
	if err := ValidateName("app", app); err != nil {
		return nil, err
	}
	if err := ValidateName("env", name); err != nil {
		return nil, err
	}
	rec, err := m.Store.Env(ctx, app, name)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return nil, fmt.Errorf("no such env %q in app %q", name, app)
	case err != nil:
		return nil, fmt.Errorf("read env %q: %w", name, err)
	}
	return rec, nil
}

func (m *Manager) events(ctx context.Context, envID int64, limit int) ([]Event, error) {
	rows, err := m.Store.Events(ctx, envID, limit)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	out := make([]Event, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Progress())
	}
	return out, nil
}

func (m *Manager) event(ctx context.Context, envID int64, action, status, detail string) {
	m.record(ctx, envID, Event{Action: action, Status: status, Detail: detail})
}

func (m *Manager) record(ctx context.Context, envID int64, e Event) {
	if e.At.IsZero() {
		e.At = m.now()
	}
	if e.Identity == "" {
		e.Identity = IdentityFrom(ctx)
	}

	_ = m.Store.AddEvent(ctx, state.EventFromProgress(envID, e))
}

func (m *Manager) lockEnv(app, name string) func() {
	return m.lockKey(app + "/" + name)
}

func (m *Manager) lockKey(key string) func() {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = map[string]*sync.Mutex{}
	}
	l, ok := m.locks[key]
	if !ok {
		l = &sync.Mutex{}
		m.locks[key] = l
	}
	m.mu.Unlock()
	l.Lock()
	m.hold(key, 1)
	return func() {
		m.hold(key, -1)
		l.Unlock()
	}
}

func (m *Manager) hold(key string, delta int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy == nil {
		m.busy = map[string]int{}
	}
	if m.busy[key] += delta; m.busy[key] <= 0 {
		delete(m.busy, key)
	}
}

func (m *Manager) envBusy(app, name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.busy[app+"/"+name] > 0
}

func (m *Manager) now() time.Time {
	if m.Now == nil {
		return time.Now()
	}
	return m.Now()
}

func (m *Manager) timeout(req time.Duration) time.Duration {
	switch {
	case req > 0:
		return req
	case m.Timeout > 0:
		return m.Timeout
	}
	return DefaultTimeout
}

func (m *Manager) readyInterval() time.Duration {
	if m.ReadyInterval > 0 {
		return m.ReadyInterval
	}
	return ReadyInterval
}

func (m *Manager) loadConfig(path string) (*config.App, error) {
	if m.LoadConfig != nil {
		return m.LoadConfig(path)
	}
	return config.Load(path)
}

func (m *Manager) expand(vars map[string]string, ctx config.ExpandContext) (map[string]string, error) {
	if m.Expand != nil {
		return m.Expand(vars, ctx)
	}
	return config.Expand(vars, ctx)
}

func depPort(base, i int) int { return base + 1 + i }

func recordToEnv(r *state.EnvRecord) (*Env, error) {
	e := &Env{
		ID:        r.ID,
		App:       r.App,
		Name:      r.Name,
		Branch:    r.Branch,
		Commit:    r.Commit,
		Worktree:  r.Worktree,
		PortBase:  r.PortBase,
		PortCount: r.PortCount,
		VPNIP:     r.VPNIP,
		Status:    Status(r.Status),
		Protected: r.Protected,
		ReleaseID: r.ReleaseID,
		DeployID:  r.DeployID,
		CreatedBy: r.CreatedBy,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}

	mode, err := ParseMode(r.Mode)
	if err != nil {
		return nil, fmt.Errorf("env %s/%s: %w", r.App, r.Name, err)
	}
	e.Mode = mode
	if r.ConfigJSON != "" {
		e.Config = json.RawMessage(r.ConfigJSON)
	}
	if r.VarsJSON != "" {
		if err := json.Unmarshal([]byte(r.VarsJSON), &e.Vars); err != nil {
			return nil, fmt.Errorf("env %s/%s: decode variables: %w", r.App, r.Name, err)
		}
	}
	return e, nil
}

func decodeConfig(r *state.EnvRecord) (*config.App, error) {
	cfg := &config.App{}
	if r.ConfigJSON == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(r.ConfigJSON), cfg); err != nil {
		return nil, fmt.Errorf("env %s/%s: decode config: %w", r.App, r.Name, err)
	}
	return cfg, nil
}

func (m *Manager) recordedConfig(rec *state.EnvRecord) *config.App {
	if rec == nil {
		return nil
	}
	cfg, err := decodeConfig(rec)
	if err != nil {
		m.logf("read the drain of env %s/%s from its recorded config: %v", rec.App, rec.Name, err)
		return nil
	}
	return cfg
}

func progressf(w io.Writer, status, action, format string, args ...any) {
	if w == nil {
		return
	}

	_ = progress.Emit(w, progress.Event{
		Status: status,
		Action: action,
		Detail: fmt.Sprintf(format, args...),
	})
}

func emit(w io.Writer, e Event) {

	_ = progress.Emit(w, e)
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

func (m *Manager) detach(ctx context.Context) (context.Context, context.CancelFunc) {
	base, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if m.Background == nil {
		return base, cancel
	}
	stop := context.AfterFunc(m.Background, cancel)
	return base, func() {
		stop()
		cancel()
	}
}

func (m *Manager) logf(format string, args ...any) {
	if m == nil || m.Log == nil {
		return
	}
	fmt.Fprintf(m.Log, "caramelo: "+format+"\n", args...)
}

type FleetWiring struct {
	Machine string

	Role fleet.Role

	Private bool

	Fleet FleetState

	Announce Announcer

	Gauge func(ctx context.Context) *machine.Record

	Arch string

	OpenIngress func(ctx context.Context, ing *edge.Ingress) error

	Mirror func(ctx context.Context, app, branch string) error

	RemoteWorktree RemoteWorktree

	Supply *release.Supply
}

func (m *Manager) SetFleetWiring(w FleetWiring) {
	m.wiringMu.Lock()
	defer m.wiringMu.Unlock()
	m.wiring.Store(&w)
}

func (m *Manager) WithFleet(w FleetWiring) *Manager {
	m.SetFleetWiring(w)
	return m
}

func (m *Manager) UpdateFleetWiring(fn func(*FleetWiring)) {
	m.wiringMu.Lock()
	defer m.wiringMu.Unlock()
	w := *m.fw()
	fn(&w)
	m.wiring.Store(&w)
}

func (m *Manager) FleetWiringOf() FleetWiring { return *m.fw() }

var noFleet FleetWiring

func (m *Manager) fw() *FleetWiring {
	if w := m.wiring.Load(); w != nil {
		return w
	}
	return &noFleet
}
