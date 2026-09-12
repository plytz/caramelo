package env

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/plytz/caramelo/internal/state"
)

const maxConfigBlob = 1 << 20

func (m *Manager) ConfigDir(ctx context.Context, app, name string) (dir string, cleanup func(), err error) {
	noop := func() {}
	if err := ValidateName("app", app); err != nil {
		return "", noop, err
	}
	if name != "" {
		if err := ValidateName("env", name); err != nil {
			return "", noop, err
		}
		rec, err := m.env(ctx, app, name)
		if err != nil {
			return "", noop, err
		}
		return rec.Worktree, noop, nil
	}
	return m.defaultBranchTop(ctx, app)
}

func (m *Manager) defaultBranchTop(ctx context.Context, app string) (string, func(), error) {
	noop := func() {}
	rec, err := m.Store.App(ctx, app)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return "", noop, fmt.Errorf("no such app %q: push it to the machine first", app)
	case err != nil:
		return "", noop, fmt.Errorf("read app %q: %w", app, err)
	}
	repo := rec.RepoPath
	if repo == "" {
		repo = RepoPath(m.Dirs.Data, app)
	}
	branch, err := m.defaultBranch(ctx, rec, repo)
	if err != nil {
		return "", noop, err
	}
	entries, err := m.topLevel(ctx, repo, branch)
	if err != nil {
		return "", noop, err
	}

	dir, err := os.MkdirTemp("", "caramelo-config-")
	if err != nil {
		return "", noop, fmt.Errorf("make a directory to read app %q in: %w", app, err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	for _, e := range entries {
		if err := m.materialise(ctx, repo, dir, e); err != nil {
			cleanup()
			return "", noop, err
		}
	}
	return dir, cleanup, nil
}

type treeEntry struct {
	kind string
	sha  string
	size int64
	name string
}

func (m *Manager) topLevel(ctx context.Context, repo, branch string) ([]treeEntry, error) {
	out, err := m.git(ctx, repo, nil, "ls-tree", "--long", "-z", branch+"^{tree}")
	if err != nil {
		return nil, fmt.Errorf("list the top level of %s in %s: %w", branch, repo, err)
	}
	var entries []treeEntry
	for _, rec := range strings.Split(out, "\x00") {
		if strings.TrimSpace(rec) == "" {
			continue
		}
		meta, name, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) < 3 {
			continue
		}
		e := treeEntry{kind: fields[1], sha: fields[2], size: -1, name: name}
		if len(fields) > 3 {
			if n, err := strconv.ParseInt(fields[3], 10, 64); err == nil {
				e.size = n
			}
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (m *Manager) materialise(ctx context.Context, repo, dir string, e treeEntry) error {

	if e.name == "" || e.name == "." || e.name == ".." || strings.ContainsRune(e.name, os.PathSeparator) {
		return nil
	}
	path := filepath.Join(dir, e.name)
	switch {
	case e.kind == "tree":
		if err := os.Mkdir(path, 0o755); err != nil && !os.IsExist(err) {
			return fmt.Errorf("create %s: %w", e.name, err)
		}
		return nil
	case e.kind != "blob", e.size > maxConfigBlob:
		return nil
	}
	content, err := m.git(ctx, repo, nil, "cat-file", "blob", e.sha)
	if err != nil {
		return fmt.Errorf("read %s from %s: %w", e.name, repo, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", e.name, err)
	}
	return nil
}
