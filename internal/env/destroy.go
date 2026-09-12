package env

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/plytz/caramelo/internal/state"
)

func (m *Manager) Destroy(ctx context.Context, req DestroyRequest, progress io.Writer) error {
	if err := ValidateName("app", req.App); err != nil {
		return err
	}
	if err := ValidateName("env", req.Name); err != nil {
		return err
	}
	defer m.lockEnv(req.App, req.Name)()

	rec, err := m.Store.Env(ctx, req.App, req.Name)
	switch {
	case errors.Is(err, state.ErrNotFound):
	case err != nil:
		return fmt.Errorf("read env %q: %w", req.Name, err)
	case rec.Protected && !req.Force:
		return fmt.Errorf("env %q of app %q is protected, so it is not destroyed by accident: "+
			"re-run with --force if you mean it", req.Name, req.App)
	}
	return m.destroyLocked(ctx, req.App, req.Name, req.DeleteBranch, progress)
}

func (m *Manager) destroyLocked(ctx context.Context, app, name string, deleteBranch bool, progress io.Writer) error {
	rec, err := m.Store.Env(ctx, app, name)
	switch {
	case errors.Is(err, state.ErrNotFound):
		rec = nil
	case err != nil:
		return fmt.Errorf("read env %q: %w", name, err)
	}

	var resources []state.EnvResource
	if rec != nil {
		if err := m.Store.UpdateEnvStatus(ctx, rec.ID, state.EnvDestroying); err != nil {
			return fmt.Errorf("mark env %q destroying: %w", name, err)
		}
		m.event(ctx, rec.ID, "destroy", "started", "")

		progress = m.feed(ctx, rec, progress)
		if resources, err = m.Store.Resources(ctx, rec.ID); err != nil {
			return fmt.Errorf("read the resources of env %q: %w", name, err)
		}
	}

	var problems []error
	fail := func(err error) {
		problems = append(problems, err)
		progressf(progress, "failed", "destroy", "%v", err)
	}

	if err := m.unpublish(ctx, rec, progress); err != nil {
		fail(err)
	}

	if err := m.dropRoutes(ctx, rec, progress); err != nil {
		fail(err)
	}

	containers, volumes, networks, err := m.leftovers(ctx, app, name, rec, resources)
	if err != nil {
		fail(err)
	}
	for _, c := range containers {
		if err := m.Driver.Remove(ctx, c, true); err != nil {
			fail(fmt.Errorf("remove container %s: %w", c, err))
			continue
		}
		progressf(progress, "changed", "container", "%s removed", c)
	}
	for _, v := range volumes {
		if err := m.Driver.RemoveVolume(ctx, v); err != nil {
			fail(fmt.Errorf("remove volume %s: %w", v, err))
			continue
		}
		progressf(progress, "changed", "volume", "%s removed", v)
	}

	for _, n := range networks {
		if err := m.Driver.RemoveNetwork(ctx, n); err != nil {
			fail(fmt.Errorf("remove network %s: %w", n, err))
			continue
		}
		progressf(progress, "changed", "network", "%s removed", n)
	}

	if err := m.removeWorktree(ctx, app, name, rec, progress); err != nil {
		fail(err)
	}
	if deleteBranch {
		branch := name
		if rec != nil && rec.Branch != "" {
			branch = rec.Branch
		}
		if err := m.Git.DeleteBranch(ctx, RepoPath(m.Dirs.Data, app), branch, true); err != nil {
			fail(fmt.Errorf("delete branch %s: %w", branch, err))
		} else {
			progressf(progress, "changed", "branch", "%s deleted", branch)
		}
	}

	if len(problems) > 0 {

		if rec != nil {
			m.event(ctx, rec.ID, "destroy", "failed", errors.Join(problems...).Error())
		}
		return fmt.Errorf("destroy env %q: %w", name, errors.Join(problems...))
	}

	if rec == nil {
		if len(containers)+len(volumes)+len(networks) == 0 {
			progressf(progress, "ok", "env", "%s is already gone", name)
		}
		return nil
	}
	if err := m.forget(ctx, rec, resources); err != nil {
		return err
	}
	progressf(progress, "changed", "env", "%s destroyed", name)

	m.announceGone(ctx, app, name)
	return nil
}

func (m *Manager) leftovers(ctx context.Context, app, name string, rec *state.EnvRecord, resources []state.EnvResource) (containers, volumes, networks []string, err error) {
	cset, vset, nset := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range resources {
		switch r.Kind {
		case state.ResourceContainer:
			cset[r.Name] = true
		case state.ResourceVolume, state.ResourceCache:
			vset[r.Name] = true
		case state.ResourceNetwork:
			nset[r.Name] = true
		}
	}

	ranUp := len(nset) > 0
	if rec != nil {

		vset[CacheVolumeName(app, name)] = true
		cfg, cerr := decodeConfig(rec)
		if cerr != nil {
			err = cerr
		} else {
			for _, d := range cfg.Deps {
				cset[ContainerName(app, name, d.Name)] = true
				vset[VolumeName(app, name, d.Name)] = true
			}
			for _, s := range cfg.Services {

				cset[ServiceContainerName(app, name, s.Name)] = true
				for i := 1; i <= 2*s.Replicas(); i++ {
					cset[ReplicaContainerName(app, name, s.Name, i)] = true
				}
				ranUp = true
			}
		}
	}

	filter := Labels(app, name, "", "")
	found, lerr := m.Driver.ListByLabel(ctx, filter)
	if lerr != nil {
		err = errors.Join(err, fmt.Errorf("list the containers of env %q: %w", name, lerr))
	}
	for _, c := range found {
		cset[c.Name] = true
		if c.Labels[LabelService] != "" {
			ranUp = true
		}
	}
	vols, verr := m.Driver.ListVolumesByLabel(ctx, filter)
	if verr != nil {
		err = errors.Join(err, fmt.Errorf("list the volumes of env %q: %w", name, verr))
	}
	for _, v := range vols {
		vset[v] = true
	}
	if ranUp {
		nset[NetworkName(app, name)] = true
	}
	return sortedKeys(cset), sortedKeys(vset), sortedKeys(nset), err
}

func (m *Manager) removeWorktree(ctx context.Context, app, name string, rec *state.EnvRecord, progress io.Writer) error {
	dir := EnvDir(m.Dirs.Data, app, name)
	path := WorktreePath(m.Dirs.Data, app, name)
	if rec != nil && rec.Worktree != "" {
		path = rec.Worktree
	}
	if rec == nil {

		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	repo := RepoPath(m.Dirs.Data, app)
	if err := m.Git.WorktreeRemove(ctx, repo, path, true); err != nil {
		return fmt.Errorf("remove worktree %s: %w", path, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove %s: %w", dir, err)
	}
	if err := m.Git.WorktreePrune(ctx, repo); err != nil {
		return fmt.Errorf("prune the worktrees of %s: %w", repo, err)
	}
	progressf(progress, "changed", "worktree", "%s removed", path)
	return nil
}

func (m *Manager) forget(ctx context.Context, rec *state.EnvRecord, resources []state.EnvResource) error {
	for _, r := range resources {
		if err := m.Store.DeleteResource(ctx, r.ID); err != nil {
			return fmt.Errorf("forget %s %s: %w", r.Kind, r.Name, err)
		}
	}
	if err := m.Store.DeleteEnv(ctx, rec.ID); err != nil {
		return fmt.Errorf("delete env %q: %w", rec.Name, err)
	}

	if err := m.Ports.Release(ctx, rec.ID); err != nil {
		return fmt.Errorf("release the ports of env %q: %w", rec.Name, err)
	}
	return m.releaseAddress(ctx, rec)
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		if k != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
