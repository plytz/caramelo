package env

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/plytz/caramelo/internal/git"
	"github.com/plytz/caramelo/internal/state"
)

type Dirtier interface {
	Dirty(ctx context.Context, worktree string) (bool, []string, error)
}

type WorktreeStatus struct {
	App string `json:"app"`
	Env string `json:"env"`

	Machine string `json:"machine,omitempty"`

	Worktree string `json:"worktree,omitempty"`

	Exists bool `json:"exists"`

	Dirty   bool     `json:"dirty"`
	Changed []string `json:"changed,omitempty"`
}

type RemoteWorktree func(ctx context.Context, machine, app, env string) (*WorktreeStatus, error)

func (m *Manager) WorktreeStatusOf(ctx context.Context, app, name string) (*WorktreeStatus, error) {
	st := &WorktreeStatus{App: app, Env: name, Machine: m.fw().Machine}
	rec, err := m.Store.Env(ctx, app, name)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return st, nil
	case err != nil:
		return nil, fmt.Errorf("read env %q of app %q: %w", name, app, err)
	}
	st.Exists, st.Worktree = true, rec.Worktree
	d, ok := m.Git.(Dirtier)
	if !ok {

		return st, nil
	}
	dirty, changed, err := d.Dirty(ctx, rec.Worktree)
	if err != nil {
		return nil, fmt.Errorf("read the state of the worktree of env %q: %w", name, err)
	}
	st.Dirty, st.Changed = dirty, changed
	return st, nil
}

func (m *Manager) CheckPush(ctx context.Context, app string, branches []string) error {
	if m.fw().Fleet == nil || len(branches) == 0 {
		return nil
	}
	for _, branch := range branches {
		branch = strings.TrimSpace(branch)
		if branch == "" {
			continue
		}
		machine, err := m.machineOfEnv(ctx, app, branch)
		if err != nil {
			return err
		}
		if machine == "" || machine == m.fw().Machine {
			continue
		}
		if m.fw().RemoteWorktree == nil {
			m.logf("push into %s/%s: machine %q holds it and this machine cannot ask it; allowing the push",
				app, branch, machine)
			continue
		}
		st, err := m.fw().RemoteWorktree(ctx, machine, app, branch)
		if err != nil {

			m.logf("ask machine %q about the worktree of %s/%s: %v; allowing the push", machine, app, branch, err)
			continue
		}
		if st == nil || !st.Exists || !st.Dirty {
			continue
		}
		return fmt.Errorf("refusing to update %s on machine %s: it has uncommitted changes (%s): "+
			"commit or discard them in the environment (caramelo env exec %s -- git status), then push again",
			worktreeName(st), machine, strings.Join(st.Changed, ", "), branch)
	}
	return nil
}

func (m *Manager) machineOfEnv(ctx context.Context, app, name string) (string, error) {
	entries, err := m.fw().Fleet.DirectoryEntries(ctx, "")
	if err != nil {
		return "", fmt.Errorf("read the environment directory: %w", err)
	}
	for _, e := range entries {
		if e.App == app && e.Env == name {
			return e.Machine, nil
		}
	}
	return "", nil
}

func worktreeName(st *WorktreeStatus) string {
	if st.Worktree != "" {
		return st.Worktree
	}
	return st.App + "/" + st.Env
}

func (m *Manager) SyncBranch(ctx context.Context, app, name string) (*Env, error) {
	defer m.lockEnv(app, name)()
	rec, err := m.env(ctx, app, name)
	if err != nil {
		return nil, err
	}
	mirror, ok := m.Git.(git.Mirrorer)
	if !ok {
		return nil, fmt.Errorf("sync %s/%s: this machine's git driver cannot follow a mirror", app, name)
	}
	repo := RepoPath(m.Dirs.Data, app)

	fetch := func() error { return mirror.FetchBranch(ctx, repo, rec.Branch) }
	if from := m.fw().Mirror; from != nil {
		fetch = func() error { return from(ctx, app, rec.Branch) }
	}
	if err := fetch(); err != nil {
		return nil, fmt.Errorf("fetch branch %s of app %q: %w", rec.Branch, app, err)
	}
	if err := mirror.UpdateWorktree(ctx, repo, rec.Worktree, rec.Branch); err != nil {
		return nil, fmt.Errorf("update the checkout of %s/%s: %w", app, name, err)
	}
	commit, err := m.Git.RevParse(ctx, repo, rec.Branch)
	if err == nil && commit != "" && commit != rec.Commit {
		rec.Commit = commit
		if err := m.Store.UpdateEnv(ctx, *rec); err != nil {
			return nil, fmt.Errorf("record the new commit of %s/%s: %w", app, name, err)
		}
	}
	m.event(ctx, rec.ID, "push", "changed", "checkout updated to "+short(rec.Commit))
	e, err := recordToEnv(rec)
	if err != nil {
		return nil, err
	}
	m.stampFleet(e, m.fleetRow(ctx, rec.ID))
	return e, nil
}
