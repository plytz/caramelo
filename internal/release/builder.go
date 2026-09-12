package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/git"
	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/stack"
	"github.com/plytz/caramelo/internal/state"
)

type Docker struct {
	Config

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

type Store interface {
	App(ctx context.Context, name string) (*state.App, error)
	Env(ctx context.Context, app, name string) (*state.EnvRecord, error)
	Envs(ctx context.Context, app string) ([]state.EnvRecord, error)
	AddRelease(ctx context.Context, r state.Release) (*state.Release, error)
	ReleaseByTree(ctx context.Context, app, tree string) (*state.Release, error)
	Releases(ctx context.Context, app string, limit int) ([]state.Release, error)
	Deploys(ctx context.Context, envID int64, limit int) ([]state.Deploy, error)
	UnfinishedDeploys(ctx context.Context) ([]state.Deploy, error)
}

type Config struct {
	Store Store

	Driver runtime.Driver

	Git git.Repo

	Runner runner.Runner

	User string

	Version string

	Identity func(ctx context.Context) string

	Now func() time.Time

	LoadConfig func(path string) (*config.App, error)
	Detect     func(dir string) (*stack.Guess, error)

	Machine string

	Arch string

	Images ImageStore
}

func New(cfg Config) *Docker { return &Docker{Config: cfg} }

var _ Builder = (*Docker)(nil)

func (b *Docker) Build(ctx context.Context, req BuildRequest, out io.Writer) (*BuildResult, error) {
	if err := b.ready(); err != nil {
		return nil, err
	}
	rec, repo, err := b.target(ctx, req.App, req.Env)
	if err != nil {
		return nil, err
	}
	commit, tree, err := b.resolve(ctx, repo, rec.Branch, req.Ref)
	if err != nil {
		return nil, err
	}

	defer b.lock(req.App + "\x00" + tree)()

	if !req.Force {
		if res, err := b.cached(ctx, req, commit, tree, out); err != nil || res != nil {
			return res, err
		}
	}
	b.emit(out, progress.Event{
		App: req.App, Env: req.Env, Action: ActionBuild, Status: progress.StatusStarted,
		Detail: fmt.Sprintf("release %s from %s", tree, short(commit)),
	})

	dir, cleanup, err := b.export(ctx, filepath.Dir(rec.Worktree), repo, commit, tree)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	plan, err := b.plan(dir, req.App, tree)
	if err != nil {
		return nil, err
	}
	if err := plan.only(req.Services); err != nil {
		return nil, err
	}
	images, built, err := b.images(ctx, req, plan, dir, tree, out)
	if err != nil {
		return nil, err
	}
	rel, err := b.record(ctx, req, plan.cfg, commit, tree, images)
	if err != nil {
		return nil, err
	}
	if err := b.recordImages(ctx, rel); err != nil {
		return nil, err
	}
	status := progress.StatusOK
	if built {
		status = progress.StatusChanged
	}
	b.emit(out, progress.Event{
		App: req.App, Env: req.Env, Action: ActionBuild, Status: status,
		Detail: fmt.Sprintf("release %s: %s", tree, plural(len(rel.Images), "image")),
	})
	return &BuildResult{Release: rel, Built: built, Images: rel.ImageList()}, nil
}

const ActionBuild = "build"

func (b *Docker) cached(ctx context.Context, req BuildRequest, commit, tree string, out io.Writer) (*BuildResult, error) {
	row, err := b.Store.ReleaseByTree(ctx, req.App, tree)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read the release of tree %s of app %q: %w", tree, req.App, err)
	}
	rel, err := FromRecord(row)
	if err != nil {
		return nil, err
	}
	if len(rel.Images) == 0 {
		return nil, nil
	}
	for _, ref := range rel.ImageList() {
		exists, err := b.Driver.ImageExists(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("look for image %s: %w", ref, err)
		}
		if !exists {
			return nil, nil
		}
	}

	if err := b.recordImages(ctx, rel); err != nil {
		return nil, err
	}
	b.emit(out, progress.Event{
		App: req.App, Env: req.Env, Action: ActionBuild, Status: progress.StatusOK,
		Detail: fmt.Sprintf("release %s of %s is built already", tree, short(commit)),
	})
	return &BuildResult{Release: rel, Built: false, Images: rel.ImageList()}, nil
}

func (b *Docker) images(ctx context.Context, req BuildRequest, plan *buildPlan, dir, tree string, out io.Writer) (map[string]string, bool, error) {
	images := make(map[string]string, len(plan.services))
	built := false
	for _, s := range plan.services {
		ref := ImageRef(req.App, s.name, tree)
		images[s.name] = ref
		force := req.Force && s.picked
		if !force {
			exists, err := b.Driver.ImageExists(ctx, ref)
			if err != nil {
				return nil, false, fmt.Errorf("look for image %s: %w", ref, err)
			}
			if exists {
				b.emit(out, progress.Event{
					App: req.App, Env: req.Env, Service: s.name, Action: ActionBuild,
					Status: progress.StatusOK, Detail: ref + " (the tree has not changed)",
				})
				continue
			}
		}
		if err := b.build(ctx, req, plan, s, dir, ref, force, out); err != nil {
			return nil, false, err
		}
		built = true
		b.emit(out, progress.Event{
			App: req.App, Env: req.Env, Service: s.name, Action: ActionBuild,
			Status: progress.StatusChanged, Detail: ref,
		})
	}
	return images, built, nil
}

func (b *Docker) build(ctx context.Context, req BuildRequest, plan *buildPlan, s servicePlan, dir, ref string, noCache bool, out io.Writer) error {
	spec := runtime.BuildSpec{
		Tag:      ref,
		Labels:   ImageLabels(req.App, s.name, plan.tree, b.Version),
		NoCache:  noCache,
		Progress: out,
	}
	switch {
	case s.build != nil:

		ctxDir, err := buildDir(dir, s.build.Context, s.build.Dockerfile)
		if err != nil {
			return fmt.Errorf("build service %q of release %s: %w", s.name, plan.tree, err)
		}
		spec.Context, spec.Dockerfile = ctxDir, s.build.Dockerfile
	default:
		path, err := b.writeDockerfile(plan, s, dir)
		if err != nil {
			return err
		}
		spec.Context, spec.Dockerfile = dir, path
	}
	if _, err := b.Driver.Build(ctx, spec); err != nil {
		return fmt.Errorf("build %s: %w", ref, err)
	}
	return nil
}

const generatedDir = ".caramelo"

func (b *Docker) writeDockerfile(plan *buildPlan, s servicePlan, dir string) (string, error) {
	manifests, err := stack.ManifestsIn(plan.stack, dir)
	if err != nil {
		return "", err
	}
	df := Dockerfile{
		App: plan.app, Service: s.name, Stack: plan.stack, Tree: plan.tree, Version: b.Version,
		Image: s.image, Manifests: manifests, Install: s.install, Build: s.buildCmd, Run: s.run,
	}
	body, err := df.Render()
	if err != nil {
		return "", err
	}
	rel := filepath.Join(generatedDir, "Dockerfile."+s.name)
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), exportMode); err != nil {
		return "", fmt.Errorf("make %s in the export: %w", generatedDir, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		return "", fmt.Errorf("write %s: %w", rel, err)
	}
	return rel, nil
}

func (b *Docker) record(ctx context.Context, req BuildRequest, cfg *config.App, commit, tree string, images map[string]string) (*Release, error) {
	encoded, err := json.Marshal(images)
	if err != nil {
		return nil, fmt.Errorf("encode the images of release %s: %w", tree, err)
	}
	row := state.Release{
		App: req.App, Commit: commit, Tree: tree, Ref: strings.TrimSpace(req.Ref),
		ImagesJSON: string(encoded), BuiltBy: b.identity(ctx), BuiltAt: b.now(),

		Machine: b.Machine,
	}
	if cfg != nil {
		body, err := json.Marshal(cfg)
		if err != nil {
			return nil, fmt.Errorf("encode the configuration of release %s: %w", tree, err)
		}
		row.ConfigJSON = string(body)
	}
	out, err := b.Store.AddRelease(ctx, row)
	if errors.Is(err, state.ErrExists) {

		out, err = b.Store.ReleaseByTree(ctx, req.App, tree)
	}
	if err != nil {
		return nil, fmt.Errorf("record release %s of app %q: %w", tree, req.App, err)
	}
	return FromRecord(out)
}

func (b *Docker) Prune(ctx context.Context, app string, keep int) ([]string, error) {
	if err := b.ready(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(app) == "" {
		return nil, errors.New("prune releases: no app given")
	}
	keep = keepCount(keep)
	releases, err := b.Store.Releases(ctx, app, 0)
	if err != nil {
		return nil, fmt.Errorf("list the releases of app %q: %w", app, err)
	}
	keepIDs, err := b.keepers(ctx, app, keep)
	if err != nil {
		return nil, err
	}

	spared := map[string]bool{}
	var removable []*Release
	for i := range releases {
		rel, err := FromRecord(&releases[i])
		if err != nil {
			return nil, err
		}
		if keepIDs[rel.ID] {
			for _, ref := range rel.ImageList() {
				spared[ref] = true
			}
			continue
		}
		removable = append(removable, rel)
	}
	removed := map[string]bool{}
	for _, rel := range removable {
		for _, ref := range rel.ImageList() {
			if spared[ref] || removed[ref] {
				continue
			}

			exists, err := b.Driver.ImageExists(ctx, ref)
			if err != nil {
				return nil, fmt.Errorf("look for image %s: %w", ref, err)
			}
			if !exists {
				continue
			}
			if err := b.Driver.RemoveImage(ctx, ref); err != nil {
				return nil, fmt.Errorf("remove image %s of release %s: %w", ref, rel.Short(), err)
			}
			removed[ref] = true
		}
	}
	out := make([]string, 0, len(removed))
	for ref := range removed {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out, nil
}

func (b *Docker) keepers(ctx context.Context, app string, keep int) (map[int64]bool, error) {
	out := map[int64]bool{}
	envs, err := b.Store.Envs(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("list the envs of app %q: %w", app, err)
	}
	byEnv := map[int64]bool{}
	for _, e := range envs {
		byEnv[e.ID] = true
		if e.ReleaseID != 0 {
			out[e.ReleaseID] = true
		}
		deploys, err := b.Store.Deploys(ctx, e.ID, 0)
		if err != nil {
			return nil, fmt.Errorf("read the deploys of env %q: %w", e.Name, err)
		}

		n := 0
		seen := map[int64]bool{}
		for _, d := range deploys {
			if d.ReleaseID == 0 || seen[d.ReleaseID] {
				continue
			}
			seen[d.ReleaseID] = true
			if n >= keep {
				continue
			}
			out[d.ReleaseID] = true
			n++
		}
	}
	unfinished, err := b.Store.UnfinishedDeploys(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the deploys in progress: %w", err)
	}
	for _, d := range unfinished {
		if !byEnv[d.EnvID] {
			continue
		}
		if d.ReleaseID != 0 {
			out[d.ReleaseID] = true
		}

		if d.FromReleaseID != 0 {
			out[d.FromReleaseID] = true
		}
	}
	return out, nil
}

func keepCount(keep int) int {
	switch {
	case keep <= 0:
		return config.DefaultKeep
	case keep > config.MaxKeep:
		return config.MaxKeep
	}
	return keep
}

type buildPlan struct {
	app   string
	tree  string
	stack string

	cfg      *config.App
	services []servicePlan
}

type servicePlan struct {
	name string

	image string
	build *config.Build

	install  string
	buildCmd string
	run      string

	picked bool
}

func (b *Docker) plan(dir, app, tree string) (*buildPlan, error) {
	cfg, err := b.loadConfig(filepath.Join(dir, config.FileName))
	switch {
	case errors.Is(err, os.ErrNotExist):
		cfg = &config.App{Name: app}
	case err != nil:
		return nil, fmt.Errorf("read %s of release %s: %w", config.FileName, tree, err)
	}
	if cfg.Name == "" {
		cfg.Name = app
	}
	guess, err := b.detect(dir)
	if err != nil {
		return nil, fmt.Errorf("detect the stack of release %s: %w", tree, err)
	}
	eff := stack.Merge(cfg, guess)
	p := &buildPlan{app: app, tree: tree, stack: eff.Stack, cfg: eff.Config}
	for _, s := range eff.Config.Services {
		p.services = append(p.services, servicePlan{
			name: s.Name, image: s.Image, build: s.Build, install: s.Install, run: s.Run, picked: true,
		})
	}
	if len(p.services) == 0 {
		return nil, fmt.Errorf("nothing to build in release %s: %s declares no `services:` and no stack was "+
			"detected in the commit (add `run: <command>` to %s, or a Dockerfile)",
			tree, config.FileName, config.FileName)
	}
	return p, nil
}

func (p *buildPlan) only(names []string) error {
	if len(names) == 0 {
		return nil
	}
	want := map[string]bool{}
	for _, n := range names {
		found := false
		for _, s := range p.services {
			if s.name == n {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("no such service %q in release %s: it has %s", n, p.tree, quoted(p.names()))
		}
		want[n] = true
	}
	for i := range p.services {
		p.services[i].picked = want[p.services[i].name]
	}
	return nil
}

func (p *buildPlan) names() []string {
	out := make([]string, 0, len(p.services))
	for _, s := range p.services {
		out = append(out, s.name)
	}
	return out
}

func (b *Docker) ready() error {
	var missing []string
	if b.Store == nil {
		missing = append(missing, "state")
	}
	if b.Driver == nil {
		missing = append(missing, "a container runtime")
	}
	if b.Git == nil {
		missing = append(missing, "git")
	}
	if b.Runner == nil {
		missing = append(missing, "a command runner")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("this machine cannot build releases: the builder has no %s", strings.Join(missing, ", no "))
}

func (b *Docker) target(ctx context.Context, app, name string) (*state.EnvRecord, string, error) {
	if strings.TrimSpace(app) == "" || strings.TrimSpace(name) == "" {
		return nil, "", errors.New("build a release: an app and an env are required")
	}
	rec, err := b.Store.Env(ctx, app, name)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return nil, "", fmt.Errorf("no such env %q in app %q", name, app)
	case err != nil:
		return nil, "", fmt.Errorf("read env %q: %w", name, err)
	}
	if rec.Worktree == "" {
		return nil, "", fmt.Errorf("env %q of app %q has no worktree: it cannot be built from", name, app)
	}
	rows, err := b.Store.App(ctx, app)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return nil, "", fmt.Errorf("no such app %q: push it to the machine first", app)
	case err != nil:
		return nil, "", fmt.Errorf("read app %q: %w", app, err)
	}
	if rows.RepoPath == "" {
		return nil, "", fmt.Errorf("app %q has no repository on this machine: push it first", app)
	}
	return rec, rows.RepoPath, nil
}

func (b *Docker) lock(key string) func() {
	b.mu.Lock()
	if b.locks == nil {
		b.locks = map[string]*sync.Mutex{}
	}
	l, ok := b.locks[key]
	if !ok {
		l = &sync.Mutex{}
		b.locks[key] = l
	}
	b.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (b *Docker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Docker) identity(ctx context.Context) string {
	if b.Identity == nil {
		return ""
	}
	return b.Identity(ctx)
}

func (b *Docker) loadConfig(path string) (*config.App, error) {
	if b.LoadConfig != nil {
		return b.LoadConfig(path)
	}
	return config.Load(path)
}

func (b *Docker) detect(dir string) (*stack.Guess, error) {
	if b.Detect != nil {
		return b.Detect(dir)
	}
	return stack.Detect(dir)
}

func (b *Docker) emit(w io.Writer, e progress.Event) {
	if w == nil {
		return
	}
	if e.At.IsZero() {
		e.At = b.now()
	}

	_ = progress.Emit(w, e)
}

func quoted(names []string) string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	out := make([]string, 0, len(sorted))
	for _, n := range sorted {
		out = append(out, `"`+n+`"`)
	}
	return strings.Join(out, ", ")
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
