package place

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/env"
)

type GitFunc func(ctx context.Context, dir string, args ...string) (string, error)

func ExecGit(ctx context.Context, dir string, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func Checkout(ctx context.Context, git GitFunc, dir string) (app, environment string) {
	if git == nil {
		git = ExecGit
	}
	common, err := git(ctx, dir, "rev-parse", "--git-common-dir")
	if err == nil {
		app = appFromRepoPath(absFrom(dir, common))
	}
	top, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return app, ""
	}
	if app == "" {
		app = appFromTree(top)
	}
	return app, envFromTree(top)
}

func AppFromCheckout(ctx context.Context, git GitFunc, dir string) string {
	if git == nil {
		git = ExecGit
	}
	if common, err := git(ctx, dir, "rev-parse", "--git-common-dir"); err == nil {
		if app := appFromRepoPath(absFrom(dir, common)); app != "" {
			return app
		}
	}
	top, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return ""
	}
	return appFromTree(top)
}

func EnvFromCheckout(ctx context.Context, git GitFunc, dir string) string {
	if git == nil {
		git = ExecGit
	}
	top, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return ""
	}
	return envFromTree(top)
}

func appFromTree(top string) string {
	if name := nameFromConfig(filepath.Join(top, config.FileName)); name != "" {
		return name
	}
	return config.DefaultName(top)
}

func absFrom(dir, path string) string {
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	abs, err := filepath.Abs(filepath.Join(dir, path))
	if err != nil {
		return filepath.Clean(filepath.Join(dir, path))
	}
	return abs
}

func appFromRepoPath(gitDir string) string {
	if gitDir == "" || filepath.Base(gitDir) != "repo.git" {
		return ""
	}
	appDir := filepath.Dir(gitDir)
	if filepath.Base(filepath.Dir(appDir)) != "apps" {
		return ""
	}
	name := filepath.Base(appDir)
	if !env.ValidSlug(name) {
		return ""
	}
	return name
}

func nameFromConfig(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return ""
	}
	name := strings.TrimSpace(doc.Name)
	if !env.ValidSlug(name) {
		return ""
	}
	return name
}

func envFromTree(top string) string {
	if filepath.Base(top) != "src" {
		return ""
	}
	envDir := filepath.Dir(top)
	if filepath.Base(filepath.Dir(envDir)) != "envs" {
		return ""
	}
	name := filepath.Base(envDir)
	if !env.ValidSlug(name) {
		return ""
	}
	return name
}
