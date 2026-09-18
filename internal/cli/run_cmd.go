package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/place"
)

type envTargets struct {
	a *app

	app string

	env string

	takesServices bool

	beforeForward func(cmd *cobra.Command) error

	machineWide func() bool
}

func (t *envTargets) addTargetFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&t.app, "app", os.Getenv("CARAMELO_APP"),
		"application this environment belongs to (default: the checkout you are in)")
	cmd.Flags().StringVar(&t.env, "env", os.Getenv("CARAMELO_ENV"),
		"environment to act on (default: the positional argument, or the worktree you are in)")

	cmd.PersistentPreRunE = t.preRun
	commanderCmd(cmd)
}

func (t *envTargets) preRun(cmd *cobra.Command, args []string) error {

	if err := t.a.setProgress(); err != nil {
		return err
	}
	if t.a.service != nil {
		return nil
	}
	wide := t.machineWide != nil && t.machineWide()
	if err := t.resolveApp(cmd); err != nil && !wide {
		return err
	}
	if err := t.injectEnv(cmd, args); err != nil && !wide {
		return err
	}

	t.a.render = renderInfo{app: t.app, env: t.env, args: beforeDash(cmd, args)}
	if t.beforeForward != nil {
		if err := t.beforeForward(cmd); err != nil {
			return err
		}
	}
	return t.a.forwardIfCommander(cmd)
}

func (t *envTargets) resolveApp(cmd *cobra.Command) error {
	name := strings.TrimSpace(t.app)
	if name == "" {
		name = appFromCheckout(cmd.Context(), ".")
	}
	if name == "" {
		return &usageError{errors.New(
			"cannot tell which app this is: run this inside the app's checkout, " +
				"or name it with --app NAME (or CARAMELO_APP)")}
	}
	if err := env.ValidateName("app", name); err != nil {
		return &usageError{err}
	}
	t.app = name
	if !cmd.Flags().Changed("app") {
		t.a.args = injectFlag(t.a.args, "--app", name)
	}
	return nil
}

func (t *envTargets) injectEnv(cmd *cobra.Command, args []string) error {
	name, _, fromArgv, err := t.resolveEnv(cmd, args)
	if err != nil {
		return err
	}
	t.env = name
	if !fromArgv {
		t.a.args = injectFlag(t.a.args, "--env", name)
	}
	return nil
}

func (t *envTargets) resolveEnv(cmd *cobra.Command, args []string) (name string, rest []string, fromArgv bool, err error) {
	positional := beforeDash(cmd, args)
	flagged := strings.TrimSpace(t.env)
	switch {
	case t.takesServices && flagged != "":
		name, rest, fromArgv = flagged, positional, cmd.Flags().Changed("env")
	case len(positional) > 0:
		name, rest, fromArgv = strings.TrimSpace(positional[0]), positional[1:], true
	case flagged != "":
		name, fromArgv = flagged, cmd.Flags().Changed("env")
	case t.a.service == nil:

		name = envFromCheckout(cmd.Context(), ".")
	}
	if name == "" {
		return "", nil, false, &usageError{fmt.Errorf(
			"cannot tell which environment to act on: name it (caramelo %s ENV), "+
				"pass --env NAME, set CARAMELO_ENV, or run this inside the environment's worktree",
			cmd.Name())}
	}
	if err := env.ValidateName("env", name); err != nil {
		return "", nil, false, &usageError{err}
	}
	return name, rest, fromArgv, nil
}

func beforeDash(cmd *cobra.Command, args []string) []string {
	if n := cmd.ArgsLenAtDash(); n >= 0 && n <= len(args) {
		return args[:n]
	}
	return args
}

func afterDash(cmd *cobra.Command, args []string) []string {
	n := cmd.ArgsLenAtDash()
	if n < 0 || n > len(args) {
		return nil
	}
	return args[n:]
}

func envFromCheckout(ctx context.Context, dir string) string {
	return place.EnvFromCheckout(ctx, runGit, dir)
}

func (t *envTargets) requireApp() error {
	if strings.TrimSpace(t.app) == "" {
		return &usageError{errors.New("no app: pass --app NAME (or run the command inside the app's checkout)")}
	}
	return env.ValidateName("app", t.app)
}

func (t *envTargets) target(cmd *cobra.Command, args []string) (string, []string, error) {
	if err := t.requireApp(); err != nil {
		return "", nil, err
	}
	name, rest, _, err := t.resolveEnv(cmd, args)
	if err != nil {
		return "", nil, err
	}
	return name, rest, nil
}

func (t *envTargets) targetOrMachine(cmd *cobra.Command, args []string) (string, []string, error) {
	name, rest, err := t.target(cmd, args)
	if err != nil {
		if t.machineWide == nil || !t.machineWide() {
			return "", nil, err
		}
		return "", beforeDash(cmd, args), nil
	}
	return name, rest, nil
}

func (t *envTargets) service() api.Service { return t.a.service }

func maxArgs(n int) cobra.PositionalArgs {
	inner := cobra.MaximumNArgs(n)
	return func(cmd *cobra.Command, args []string) error {
		if err := inner(cmd, args); err != nil {
			return &usageError{err}
		}
		return nil
	}
}
