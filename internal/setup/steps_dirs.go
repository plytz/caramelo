package setup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
)

type DirsStep struct{}

func NewDirsStep() *DirsStep { return &DirsStep{} }

func (s *DirsStep) Name() string { return "dirs" }

func (s *DirsStep) dirs(env *Env) []dirSpec {
	cfg := env.Config
	user, group := cfg.User, cfg.Group
	return []dirSpec{
		{Path: env.ConfigDir, Mode: "0750", Owner: "root", Group: group},
		{Path: cfg.StateDir, Mode: "0750", Owner: user, Group: group},
		{Path: cfg.SSHDir(), Mode: "0700", Owner: user, Group: group, AsUser: user},
		{Path: setupRunsDir(cfg.StateDir), Mode: "0750", Owner: user, Group: group, AsUser: user},

		{Path: cfg.VPNDir(), Mode: "0700", Owner: user, Group: group, AsUser: user},

		{Path: cfg.DataDir, Mode: "0750", Owner: user, Group: group},
		{Path: cfg.AppsDir(), Mode: "0750", Owner: user, Group: group, AsUser: user},

		{Path: cfg.DockerDataRoot(), Mode: "0710", Owner: user, Group: group, AsUser: user},
	}
}

func setupRunsDir(stateDir string) string { return filepath.Join(stateDir, "setup") }

func (s *DirsStep) Check(ctx context.Context, env *Env) (bool, string, error) {
	var problems []string
	for _, d := range s.dirs(env) {
		okDir, detail, err := d.check(ctx, env)
		if err != nil {
			return false, "", err
		}
		if !okDir {
			problems = append(problems, detail)
		}
	}
	okCfg, detail, err := s.checkConfig(ctx, env)
	if err != nil {
		return false, "", err
	}
	if !okCfg {
		problems = append(problems, detail)
	}
	if len(problems) > 0 {
		return false, strings.Join(problems, "; "), nil
	}
	return true, fmt.Sprintf("%s, %s, %s", env.ConfigDir, env.Config.StateDir, env.Config.DataDir), nil
}

func (s *DirsStep) Apply(ctx context.Context, env *Env) error {
	for _, d := range s.dirs(env) {
		done, _, err := d.check(ctx, env)
		if err != nil {
			return err
		}
		if done {
			continue
		}
		if err := d.apply(ctx, env); err != nil {
			return fmt.Errorf("create %s: %w", d.Path, err)
		}
	}

	same := s.configMatches(env)
	if err := s.writeConfig(ctx, env); err != nil {
		return err
	}
	if !same {
		env.ConfigChanged = true
	}
	return nil
}

func (s *DirsStep) configMatches(env *Env) bool {
	got, err := serverconfig.Load(env.ConfigDir)
	return err == nil && got == env.Config
}

func (s *DirsStep) checkConfig(ctx context.Context, env *Env) (bool, string, error) {
	path := serverconfig.Path(env.ConfigDir)
	st, err := statPath(ctx, env, path)
	if err != nil {
		return false, "", err
	}
	if !st.Exists {
		return false, path + " missing", nil
	}
	if st.Owner != "root" || st.Group != env.Config.Group {
		return false, fmt.Sprintf("%s owned by %s:%s, want root:%s", path, st.Owner, st.Group, env.Config.Group), nil
	}
	if st.Mode != configMode {
		return false, fmt.Sprintf("%s mode %s, want %s", path, st.Mode, configMode), nil
	}
	got, err := serverconfig.Load(env.ConfigDir)
	if err != nil {
		return false, path + " unreadable", nil
	}
	if got != env.Config {
		return false, path + " differs", nil
	}
	return true, "", nil
}

const configMode = "0640"

func (s *DirsStep) writeConfig(ctx context.Context, env *Env) error {
	if err := serverconfig.Save(env.ConfigDir, env.Config, os.FileMode(0o640)); err != nil {
		return fmt.Errorf("write %s: %w", serverconfig.Path(env.ConfigDir), err)
	}
	path := serverconfig.Path(env.ConfigDir)
	if _, err := mustRun(ctx, env, runner.Cmd{Name: "chown", Args: []string{"root:" + env.Config.Group, "--", path}}); err != nil {
		return err
	}
	if _, err := mustRun(ctx, env, runner.Cmd{Name: "chmod", Args: []string{configMode, "--", path}}); err != nil {
		return err
	}
	return nil
}
