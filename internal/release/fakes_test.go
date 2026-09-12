package release

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/git"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/state"
)

type fakeStore struct {
	mu       sync.Mutex
	apps     map[string]state.App
	envs     []state.EnvRecord
	releases []state.Release
	deploys  []state.Deploy
	nextRel  int64
}

func newFakeStore() *fakeStore { return &fakeStore{apps: map[string]state.App{}} }

func (s *fakeStore) App(_ context.Context, name string) (*state.App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[name]
	if !ok {
		return nil, state.ErrNotFound
	}
	return &a, nil
}

func (s *fakeStore) Env(_ context.Context, app, name string) (*state.EnvRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.envs {
		if s.envs[i].App == app && s.envs[i].Name == name {
			e := s.envs[i]
			return &e, nil
		}
	}
	return nil, state.ErrNotFound
}

func (s *fakeStore) Envs(_ context.Context, app string) ([]state.EnvRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []state.EnvRecord{}
	for _, e := range s.envs {
		if app == "" || e.App == app {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *fakeStore) AddRelease(_ context.Context, r state.Release) (*state.Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, have := range s.releases {
		if have.App == r.App && have.Tree == r.Tree {
			return nil, state.ErrExists
		}
	}
	s.nextRel++
	r.ID = s.nextRel
	s.releases = append(s.releases, r)
	out := r
	return &out, nil
}

func (s *fakeStore) ReleaseByTree(_ context.Context, app, tree string) (*state.Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.releases {
		if s.releases[i].App == app && s.releases[i].Tree == tree {
			r := s.releases[i]
			return &r, nil
		}
	}
	return nil, state.ErrNotFound
}

func (s *fakeStore) Releases(_ context.Context, app string, limit int) ([]state.Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []state.Release{}
	for i := len(s.releases) - 1; i >= 0; i-- {
		if app == "" || s.releases[i].App == app {
			out = append(out, s.releases[i])
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *fakeStore) Deploys(_ context.Context, envID int64, limit int) ([]state.Deploy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []state.Deploy{}
	for i := len(s.deploys) - 1; i >= 0; i-- {
		if s.deploys[i].EnvID == envID {
			out = append(out, s.deploys[i])
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *fakeStore) UnfinishedDeploys(_ context.Context) ([]state.Deploy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []state.Deploy{}
	for _, d := range s.deploys {
		if !d.Done() {
			out = append(out, d)
		}
	}
	return out, nil
}

type fakeDriver struct {
	runtime.Driver

	mu     sync.Mutex
	images map[string]bool
	builds []runtime.BuildSpec

	contexts map[string][]string

	dockerfiles map[string]string
	removed     []string
	buildErr    error

	arch string
}

func newFakeDriver() *fakeDriver {
	return &fakeDriver{images: map[string]bool{}, contexts: map[string][]string{}, dockerfiles: map[string]string{}}
}

func (d *fakeDriver) Build(_ context.Context, spec runtime.BuildSpec) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.buildErr != nil {
		return "", d.buildErr
	}
	d.builds = append(d.builds, spec)
	d.contexts[spec.Tag] = listDir(spec.Context)
	if spec.Dockerfile != "" {
		if b, err := os.ReadFile(filepath.Join(spec.Context, spec.Dockerfile)); err == nil {
			d.dockerfiles[spec.Tag] = string(b)
		}
	}
	d.images[spec.Tag] = true
	return "sha256:" + spec.Tag, nil
}

func (d *fakeDriver) ImageExists(_ context.Context, ref string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.images[ref], nil
}

func (d *fakeDriver) RemoveImage(_ context.Context, ref string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.images, ref)
	d.removed = append(d.removed, ref)
	return nil
}

func (d *fakeDriver) ImageInfo(_ context.Context, ref string) (runtime.ImageInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.images[ref] {
		return runtime.ImageInfo{}, fmt.Errorf("image %s: %w", ref, runtime.ErrNotFound)
	}
	return runtime.ImageInfo{Ref: ref, ID: "sha256:" + ref, Arch: d.arch, Size: 2048}, nil
}

func (d *fakeDriver) SaveImages(_ context.Context, refs []string, out io.Writer) (int64, error) {
	n, err := io.WriteString(out, "tar:"+strings.Join(refs, ","))
	return int64(n), err
}

func (d *fakeDriver) LoadImages(_ context.Context, in io.Reader) ([]string, error) {
	body, err := io.ReadAll(in)
	if err != nil {
		return nil, err
	}
	refs := strings.Split(strings.TrimPrefix(string(body), "tar:"), ",")
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ref := range refs {
		d.images[ref] = true
	}
	return refs, nil
}

func (d *fakeDriver) buildOf(tag string) (runtime.BuildSpec, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.builds) - 1; i >= 0; i-- {
		if d.builds[i].Tag == tag {
			return d.builds[i], true
		}
	}
	return runtime.BuildSpec{}, false
}

func (d *fakeDriver) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.builds)
}

func (d *fakeDriver) tags() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.images))
	for ref := range d.images {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

func listDir(dir string) []string {
	var out []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	sort.Strings(out)
	return out
}

type harness struct {
	t      *testing.T
	data   string
	repo   string
	src    string
	branch string
	store  *fakeStore
	driver *fakeDriver
	b      *Docker
	run    runner.Runner
}

type isolated struct {
	inner runner.Runner
	env   []string
}

func (i isolated) Run(ctx context.Context, c runner.Cmd) (runner.Result, error) {
	c.Env = append(append([]string(nil), i.env...), c.Env...)
	return i.inner.Run(ctx, c)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	data := t.TempDir()
	h := &harness{
		t:      t,
		data:   data,
		repo:   filepath.Join(data, "apps", "shop", "repo.git"),
		src:    filepath.Join(data, "src"),
		branch: "production",
		store:  newFakeStore(),
		driver: newFakeDriver(),
		run: isolated{inner: runner.Exec{}, env: []string{
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=Caramelo Test",
			"GIT_AUTHOR_EMAIL=test@caramelo.invalid",
			"GIT_COMMITTER_NAME=Caramelo Test",
			"GIT_COMMITTER_EMAIL=test@caramelo.invalid",

			"GIT_AUTHOR_DATE=2026-09-09T12:00:00Z",
			"GIT_COMMITTER_DATE=2026-09-09T12:00:00Z",
		}},
	}
	worktree := filepath.Join(data, "apps", "shop", "envs", h.branch, "src")
	if err := os.MkdirAll(worktree, 0o750); err != nil {
		t.Fatal(err)
	}
	h.git(t, "", "init", "--bare", h.repo)
	h.git(t, "", "init", "-b", h.branch, h.src)

	h.store.apps["shop"] = state.App{Name: "shop", RepoPath: h.repo}
	h.store.envs = append(h.store.envs, state.EnvRecord{
		ID: 1, App: "shop", Name: h.branch, Branch: h.branch, Worktree: worktree,
		Mode: state.EnvModeRelease,
	})
	h.b = New(Config{
		Store:    h.store,
		Driver:   h.driver,
		Git:      git.NewCLI(h.run, ""),
		Runner:   h.run,
		Version:  "0.7.0",
		Now:      func() time.Time { return time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC) },
		Identity: identityFrom,
	})
	return h
}

func (h *harness) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	res, err := h.run.Run(context.Background(), runner.Cmd{Name: "git", Args: args, Dir: dir})
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("git %s: exit %d: %s", strings.Join(args, " "), res.ExitCode, res.Stderr)
	}
	return strings.TrimSpace(res.Stdout)
}

func (h *harness) write(files map[string]string) {
	h.t.Helper()
	for name, body := range files {
		path := filepath.Join(h.src, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) commit(message string, files map[string]string) string {
	h.t.Helper()
	h.write(files)
	h.git(h.t, h.src, "add", "--all")
	h.git(h.t, h.src, "commit", "--allow-empty", "-m", message)
	h.push()
	return h.git(h.t, h.src, "rev-parse", "HEAD")
}

func (h *harness) push() {
	h.t.Helper()
	h.git(h.t, h.src, "push", "--force", h.repo, h.branch+":"+h.branch)
}

func (h *harness) tree(commit string) string {
	h.t.Helper()
	return ShortTree(h.git(h.t, h.src, "rev-parse", commit+"^{tree}"))
}

type identityKey struct{}

func identityFrom(ctx context.Context) string {
	s, _ := ctx.Value(identityKey{}).(string)
	return s
}

func withIdentity(ctx context.Context, who string) context.Context {
	return context.WithValue(ctx, identityKey{}, who)
}

const sampleYAML = `name: shop
services:
  web:
    run: go run ./cmd/web
    port: 8080
  worker:
    run: go run ./cmd/worker
    port: none
`
