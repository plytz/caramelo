package env

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vault"
)

func (s *fakeStore) AddRelease(ctx context.Context, r state.Release) (*state.Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, have := range s.releases {
		if have.App == r.App && have.Tree == r.Tree {
			return nil, state.ErrExists
		}
	}
	s.nextRelease++
	r.ID = s.nextRelease
	if r.BuiltAt.IsZero() {
		r.BuiltAt = time.Now()
	}
	s.releases = append(s.releases, r)
	out := r
	return &out, nil
}

func (s *fakeStore) Release(ctx context.Context, id int64) (*state.Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.releases {
		if s.releases[i].ID == id {
			out := s.releases[i]
			return &out, nil
		}
	}
	return nil, state.ErrNotFound
}

func (s *fakeStore) ReleaseByTree(ctx context.Context, app, tree string) (*state.Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.releases {
		if s.releases[i].App == app && s.releases[i].Tree == tree {
			out := s.releases[i]
			return &out, nil
		}
	}
	return nil, state.ErrNotFound
}

func (s *fakeStore) Releases(ctx context.Context, app string, limit int) ([]state.Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []state.Release{}
	for i := len(s.releases) - 1; i >= 0; i-- {
		if app != "" && s.releases[i].App != app {
			continue
		}
		out = append(out, s.releases[i])
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *fakeStore) DeleteRelease(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.releases[:0]
	for _, r := range s.releases {
		if r.ID != id {
			kept = append(kept, r)
		}
	}
	s.releases = kept
	return nil
}

func (s *fakeStore) CreateDeploy(ctx context.Context, d state.Deploy) (*state.Deploy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextDeploy++
	d.ID = s.nextDeploy
	if d.Kind == "" {
		d.Kind = state.DeployKindDeploy
	}
	s.deploys = append(s.deploys, d)
	out := d
	return &out, nil
}

func (s *fakeStore) UpdateDeploy(ctx context.Context, d state.Deploy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.deploys {
		if s.deploys[i].ID == d.ID {

			d.EnvID, d.StartedAt = s.deploys[i].EnvID, s.deploys[i].StartedAt
			s.deploys[i] = d
			return nil
		}
	}
	return state.ErrNotFound
}

func (s *fakeStore) Deploy(ctx context.Context, id int64) (*state.Deploy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.deploys {
		if s.deploys[i].ID == id {
			out := s.deploys[i]
			return &out, nil
		}
	}
	return nil, state.ErrNotFound
}

func (s *fakeStore) Deploys(ctx context.Context, envID int64, limit int) ([]state.Deploy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []state.Deploy{}
	for i := len(s.deploys) - 1; i >= 0; i-- {
		if envID != 0 && s.deploys[i].EnvID != envID {
			continue
		}
		out = append(out, s.deploys[i])
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *fakeStore) UnfinishedDeploys(ctx context.Context) ([]state.Deploy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []state.Deploy{}
	for _, d := range s.deploys {
		if !state.DeployDone(d.Status) {
			out = append(out, d)
		}
	}
	return out, nil
}

func (s *fakeStore) SetEnvRelease(ctx context.Context, envID, releaseID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.envs {
		if s.envs[i].ID == envID {
			s.envs[i].ReleaseID = releaseID
			return nil
		}
	}
	return state.ErrNotFound
}

func (s *fakeStore) SetEnvDeploy(ctx context.Context, envID, deployID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.envs {
		if s.envs[i].ID == envID {
			s.envs[i].DeployID = deployID
			return nil
		}
	}
	return state.ErrNotFound
}

func (s *fakeStore) deploy(id int64) (state.Deploy, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.deploys {
		if d.ID == id {
			return d, true
		}
	}
	return state.Deploy{}, false
}

type fakeVault struct {
	mu      sync.Mutex
	entries map[string]vault.Entry

	SetErr error
}

func newVault() *fakeVault { return &fakeVault{entries: map[string]vault.Entry{}} }

func vaultKey(r vault.Ref) string {
	return fmt.Sprintf("%s/%s/%s/%s", r.Scope, r.App, r.Env, r.Name)
}

func (v *fakeVault) Set(_ context.Context, r vault.Ref, value string) (*vault.Entry, error) {
	if v.SetErr != nil {
		return nil, v.SetErr
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	e := v.entries[vaultKey(r)]
	e.Ref, e.Value, e.Version = r, value, e.Version+1
	v.entries[vaultKey(r)] = e
	out := e
	return &out, nil
}

func (v *fakeVault) Get(_ context.Context, r vault.Ref) (*vault.Entry, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.entries[vaultKey(r)]
	if !ok {
		return nil, vault.ErrNotFound
	}
	out := e
	return &out, nil
}

func (v *fakeVault) List(_ context.Context, app, env string) ([]vault.Entry, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := []vault.Entry{}
	for _, e := range v.entries {
		if v.visible(e, app, env) {
			out = append(out, e.Redact())
		}
	}
	vault.Sort(out)
	return out, nil
}

func (v *fakeVault) Remove(_ context.Context, r vault.Ref) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.entries[vaultKey(r)]; !ok {
		return vault.ErrNotFound
	}
	delete(v.entries, vaultKey(r))
	return nil
}

func (v *fakeVault) Resolve(_ context.Context, app, env string) (map[string]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := map[string]string{}
	for _, scope := range []vault.Scope{vault.ScopeMachine, vault.ScopeApp, vault.ScopeEnv} {
		for _, e := range v.entries {
			if e.Scope == scope && v.visible(e, app, env) {
				out[e.Name] = e.Value
			}
		}
	}
	return out, nil
}

func (v *fakeVault) Sources(_ context.Context, app, env string) (map[string]vault.Scope, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := map[string]vault.Scope{}
	for _, scope := range []vault.Scope{vault.ScopeMachine, vault.ScopeApp, vault.ScopeEnv} {
		for _, e := range v.entries {
			if e.Scope == scope && v.visible(e, app, env) {
				out[e.Name] = scope
			}
		}
	}
	return out, nil
}

func (v *fakeVault) visible(e vault.Entry, app, env string) bool {
	switch e.Scope {
	case vault.ScopeMachine:
		return true
	case vault.ScopeApp:
		return e.App == app
	default:
		return e.App == app && e.Env == env
	}
}

type walkBuilder struct {
	mu sync.Mutex

	store *fakeStore

	driver *fakeDriver

	tree string

	cfg *config.App

	services []string

	Err error

	builds []string
	pruned []int
}

func newBuilder(s *fakeStore, tree string) *walkBuilder {
	return &walkBuilder{store: s, tree: tree}
}

func (b *walkBuilder) Build(ctx context.Context, req release.BuildRequest, _ io.Writer) (*release.BuildResult, error) {
	if b.Err != nil {
		return nil, b.Err
	}
	b.mu.Lock()
	b.builds = append(b.builds, b.tree)
	tree, cfg, only := b.tree, b.cfg, b.services
	b.mu.Unlock()

	images := map[string]string{}
	names := only
	if len(names) == 0 {
		for _, s := range cfg.Services {
			names = append(names, s.Name)
		}
	}
	for _, name := range names {
		images[name] = release.ImageRef(req.App, name, tree)
	}
	imagesJSON, err := json.Marshal(images)
	if err != nil {
		return nil, err
	}
	if b.driver != nil {
		b.driver.mu.Lock()
		for _, ref := range images {
			b.driver.images[ref] = true
		}
		b.driver.mu.Unlock()
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	row, err := b.store.AddRelease(ctx, state.Release{
		App: req.App, Commit: "commit-" + tree, Tree: tree, Ref: req.Ref,
		ConfigJSON: string(cfgJSON), ImagesJSON: string(imagesJSON), BuiltAt: time.Now(),
	})
	built := true
	if err != nil {

		if row, err = b.store.ReleaseByTree(ctx, req.App, tree); err != nil {
			return nil, err
		}
		built = false
	}
	rel, err := releaseFromRow(row)
	if err != nil {
		return nil, err
	}
	return &release.BuildResult{Release: rel, Built: built, Images: rel.ImageList()}, nil
}

func (b *walkBuilder) Prune(ctx context.Context, app string, keep int) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruned = append(b.pruned, keep)
	return nil, nil
}

func (b *walkBuilder) setTree(tree string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tree = tree
}

func (b *walkBuilder) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.builds)
}

func (b *walkBuilder) keeps() []int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]int(nil), b.pruned...)
}

type oneOff struct {
	command string
	image   string
	env     map[string]string
}

func (d *fakeDriver) oneOffs() []oneOff {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]oneOff, 0, len(d.attached))
	for _, spec := range d.attached {
		out = append(out, oneOff{
			command: strings.Join(spec.Command, " "),
			image:   spec.Image,
			env:     spec.Env,
		})
	}
	return out
}

func (e *fakeEdge) setCounts(c *edge.Counts) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.counts = c
}

func (h *harness) hasEventWith(action, want string) bool {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	for _, e := range h.store.events {
		if e.Action == action && strings.Contains(string(e.JSON), want) {
			return true
		}
	}
	return false
}

func (h *harness) hasEvent(action, status, want string) bool {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	for _, e := range h.store.events {
		if e.Action == action && e.Status == status && strings.Contains(e.Detail, want) {
			return true
		}
	}
	return false
}

func (h *harness) clearEvents() {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	h.store.events = nil
}

func (d *fakeDriver) bumpRestarts(name string, n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.containers[name]
	c.Restarts += n
	d.containers[name] = c
}

func (h *harness) eventsText() string {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	b := &strings.Builder{}
	for _, e := range h.store.events {
		b.WriteString("  " + e.Action + " " + e.Step + " " + e.Status + ": " + e.Detail + " " + string(e.JSON) + "\n")
	}
	return b.String()
}
