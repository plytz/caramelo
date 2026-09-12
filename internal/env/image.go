package env

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/state"
)

func (m *Manager) serviceImage(ctx context.Context, rec *state.EnvRecord, s svcPlan, force bool, progress io.Writer) (string, error) {
	if s.build == nil {
		if err := m.Driver.Pull(ctx, s.image); err != nil {
			return "", fmt.Errorf("pull %s: %w", s.image, err)
		}
		progressf(progress, "ok", "image", "%s", s.image)
		return s.image, nil
	}

	dir, err := buildContext(rec.Worktree, s.build.Context, s.build.Dockerfile)
	if err != nil {
		return "", err
	}
	hash, err := m.treeHash(ctx, rec)
	if err != nil {
		return "", err
	}
	ref := ImageRef(rec.App, hash)
	if !force {
		exists, err := m.Driver.ImageExists(ctx, ref)
		if err != nil {
			return "", fmt.Errorf("look for image %s: %w", ref, err)
		}
		if exists {
			m.addServiceResource(ctx, rec.ID, state.ResourceImage, ref, s.name, 0)
			progressf(progress, "ok", "image", "%s (the tree has not changed)", ref)
			return ref, nil
		}
	}
	spec := runtime.BuildSpec{
		Context:    dir,
		Dockerfile: s.build.Dockerfile,
		Tag:        ref,
		Labels:     ServiceLabels(rec.App, rec.Name, s.name, m.Version),
		NoCache:    force,
		Progress:   progress,
	}
	if _, err := m.Driver.Build(ctx, spec); err != nil {
		return "", fmt.Errorf("build %s: %w", ref, err)
	}
	m.addServiceResource(ctx, rec.ID, state.ResourceImage, ref, s.name, 0)
	progressf(progress, "changed", "image", "%s", ref)
	return ref, nil
}

func buildContext(worktree, sub, dockerfile string) (string, error) {
	root, err := realPath(worktree)
	if err != nil {
		return "", fmt.Errorf("resolve the worktree %s: %w", worktree, err)
	}
	if sub == "" {
		sub = "."
	}
	dir, err := pathInside(root, sub, "build context", "the worktree")
	if err != nil {
		return "", err
	}
	if dockerfile != "" {
		if _, err := pathInside(dir, dockerfile, "dockerfile", "the build context"); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func pathInside(root, sub, what, where string) (string, error) {
	real, err := realPath(filepath.Join(root, sub))
	if err != nil {
		return "", fmt.Errorf("resolve the %s %q: %w", what, sub, err)
	}
	if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
		return "", fmt.Errorf("%s %q is outside %s", what, sub, where)
	}
	return real, nil
}

func realPath(path string) (string, error) {
	path = filepath.Clean(path)
	rest := ""
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent, last := filepath.Split(path)
		parent = filepath.Clean(parent)
		if parent == path || last == "" {

			return "", err
		}
		rest = filepath.Join(last, rest)
		path = parent
	}
}

func (m *Manager) treeHash(ctx context.Context, rec *state.EnvRecord) (string, error) {

	defer m.lockKey("tree/" + rec.App + "/" + rec.Name)()
	index := filepath.Join(EnvDir(m.Dirs.Data, rec.App, rec.Name), "build-index")
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := m.git(ctx, rec.Worktree, env, "add", "--all"); err != nil {
		return "", err
	}
	out, err := m.git(ctx, rec.Worktree, env, "write-tree")
	if err != nil {
		return "", err
	}
	hash := strings.TrimSpace(out)
	if hash == "" {
		return "", fmt.Errorf("hash the worktree %s: git write-tree said nothing", rec.Worktree)
	}
	return release.ShortTree(hash), nil
}

func (m *Manager) git(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	if m.Runner == nil {
		return "", fmt.Errorf("git %s: no command runner configured", strings.Join(args, " "))
	}
	res, err := m.Runner.Run(ctx, runner.Cmd{
		Name: "git",
		Args: args,
		Dir:  dir,
		Env:  env,
		User: m.Dirs.User,
	})
	if err != nil {
		return "", fmt.Errorf("git %s in %s: %w", strings.Join(args, " "), dir, err)
	}
	if res.ExitCode != 0 {
		detail := firstLine(res.Stderr)
		if detail == "" {
			detail = firstLine(res.Stdout)
		}
		return "", fmt.Errorf("git %s in %s: exit %d: %s", strings.Join(args, " "), dir, res.ExitCode, detail)
	}
	return res.Stdout, nil
}
