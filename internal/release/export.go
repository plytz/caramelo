package release

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/plytz/caramelo/internal/git"
	"github.com/plytz/caramelo/internal/runner"
)

const exportDirName = "builds"

const exportMode fs.FileMode = 0o750

func (b *Docker) resolve(ctx context.Context, repo, branch, ref string) (commit, tree string, err error) {
	want := strings.TrimSpace(ref)
	if want == "" {
		want = branch
	}
	if want == "" {
		return "", "", errors.New("no ref to build: the environment has no branch and none was given")
	}
	commit, err = b.Git.RevParse(ctx, repo, want)
	switch {
	case errors.Is(err, git.ErrNotFound):
		return "", "", fmt.Errorf("no such commit, tag or branch %q in %s: push it first", want, repo)
	case err != nil:
		return "", "", fmt.Errorf("resolve %q in %s: %w", want, repo, err)
	}

	tree, err = b.Git.RevParse(ctx, repo, commit+"^{tree}")
	if err != nil {
		return "", "", fmt.Errorf("read the tree of %s in %s: %w", short(commit), repo, err)
	}
	if strings.TrimSpace(tree) == "" {
		return "", "", fmt.Errorf("read the tree of %s in %s: git said nothing", short(commit), repo)
	}
	return strings.TrimSpace(commit), ShortTree(tree), nil
}

func (b *Docker) export(ctx context.Context, envDir, repo, commit, tree string) (dir string, cleanup func(), err error) {
	noop := func() {}
	root := filepath.Join(envDir, exportDirName)
	if err := os.MkdirAll(root, exportMode); err != nil {
		return "", noop, fmt.Errorf("make the export directory %s: %w", root, err)
	}
	dir = filepath.Join(root, tree)
	archive := dir + ".tar"

	remove := func() {
		_ = os.RemoveAll(dir)
		_ = os.Remove(archive)
	}
	remove()
	if err := os.MkdirAll(dir, exportMode); err != nil {
		return "", noop, fmt.Errorf("make the export directory %s: %w", dir, err)
	}
	if err := b.gitArchive(ctx, repo, commit, archive); err != nil {
		remove()
		return "", noop, err
	}
	if err := extract(archive, dir); err != nil {
		remove()
		return "", noop, fmt.Errorf("export %s of %s: %w", short(commit), repo, err)
	}

	_ = os.Remove(archive)
	return dir, remove, nil
}

func (b *Docker) gitArchive(ctx context.Context, repo, commit, out string) error {
	if b.Runner == nil {
		return errors.New("export a commit: no command runner configured")
	}
	args := []string{"-C", repo, "archive", "--format=tar", "--output=" + out, commit}
	res, err := b.Runner.Run(ctx, runner.Cmd{Name: "git", Args: args, User: b.User})
	if err != nil {
		return fmt.Errorf("git archive %s in %s: %w", short(commit), repo, err)
	}
	if res.ExitCode != 0 {
		detail := firstLine(res.Stderr)
		if detail == "" {
			detail = firstLine(res.Stdout)
		}
		return fmt.Errorf("git archive %s in %s: exit %d: %s", short(commit), repo, res.ExitCode, detail)
	}
	return nil
}

func extract(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("read %s: %w", archive, err)
	}
	defer f.Close()

	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", archive, err)
		}
		path, err := insideDir(dir, h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, exportMode); err != nil {
				return fmt.Errorf("create %s: %w", h.Name, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), exportMode); err != nil {
				return fmt.Errorf("create the directory of %s: %w", h.Name, err)
			}
			if err := writeFile(path, tr, fileMode(h)); err != nil {
				return fmt.Errorf("write %s: %w", h.Name, err)
			}
		case tar.TypeSymlink:
			if err := symlink(dir, path, h); err != nil {
				return err
			}
		default:

		}
	}
}

func writeFile(path string, r io.Reader, mode fs.FileMode) error {

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func symlink(root, path string, h *tar.Header) error {
	if !targetInside(root, filepath.Dir(path), h.Linkname) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), exportMode); err != nil {
		return fmt.Errorf("create the directory of %s: %w", h.Name, err)
	}
	if err := os.Symlink(h.Linkname, path); err != nil {
		return fmt.Errorf("link %s: %w", h.Name, err)
	}
	return nil
}

func targetInside(root, linkDir, linkname string) bool {
	target := linkname
	if !filepath.IsAbs(target) {
		target = filepath.Join(linkDir, target)
	}
	target = filepath.Clean(target)
	return target == root || strings.HasPrefix(target, root+string(filepath.Separator))
}

func insideDir(root, name string) (string, error) {
	clean := strings.TrimSuffix(strings.TrimSpace(filepath.ToSlash(name)), "/")
	switch {
	case clean == "", clean == ".":
		return "", fmt.Errorf("refuse to unpack %q: it names no file", name)
	case strings.HasPrefix(clean, "/"):
		return "", fmt.Errorf("refuse to unpack %q: it is an absolute path", name)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".." {
			return "", fmt.Errorf("refuse to unpack %q: it climbs out of the export", name)
		}
	}
	path := filepath.Join(root, filepath.FromSlash(clean))
	if !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return "", fmt.Errorf("refuse to unpack %q: it is outside the export", name)
	}
	return path, nil
}

func fileMode(h *tar.Header) fs.FileMode {
	if h.FileInfo().Mode().Perm()&0o111 != 0 {
		return 0o750
	}
	return 0o640
}

func buildDir(export, sub, dockerfile string) (string, error) {
	root, err := filepath.EvalSymlinks(export)
	if err != nil {
		return "", fmt.Errorf("resolve the export %s: %w", export, err)
	}
	if strings.TrimSpace(sub) == "" {
		sub = "."
	}
	dir, err := realInside(root, sub, "build context", "the commit")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(dockerfile) != "" {
		if _, err := realInside(dir, dockerfile, "dockerfile", "the build context"); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func realInside(root, sub, what, where string) (string, error) {
	real, err := filepath.EvalSymlinks(filepath.Join(root, sub))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%s %q is not in %s", what, sub, where)
		}
		return "", fmt.Errorf("resolve the %s %q: %w", what, sub, err)
	}
	if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
		return "", fmt.Errorf("%s %q is outside %s", what, sub, where)
	}
	return real, nil
}

func short(hash string) string {
	if len(hash) > TreeLen {
		return hash[:TreeLen]
	}
	return hash
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func (b *Docker) ResolveTree(ctx context.Context, app, env, ref string) (commit, tree string, err error) {
	rec, repo, err := b.target(ctx, app, env)
	if err != nil {
		return "", "", err
	}
	return b.resolve(ctx, repo, rec.Branch, ref)
}
