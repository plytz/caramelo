package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/place"
)

func init() {
	register(func(a *app) *cobra.Command {
		e := &envCmd{a: a}
		cmd := commanderCmd(&cobra.Command{
			Use:   "env",
			Short: "Create, inspect and destroy isolated environments of an app",
			Long: `An environment is an isolated, disposable copy of an app on the machine:
a git worktree on its own branch, its own block of ports, a container and a
named volume per dependency, and the variables that tie them together. Many
of them can exist at once without sharing anything.

The app is the repository you are standing in; --app (or CARAMELO_APP) names
it explicitly when you are not.`,
		})
		cmd.PersistentFlags().StringVar(&e.app, "app", os.Getenv("CARAMELO_APP"),
			"application these environments belong to (default: the checkout you are in)")

		cmd.PersistentPreRunE = e.preRun
		asGroup(cmd)
		cmd.AddCommand(
			e.createCmd(),
			e.listCmd(),
			e.showCmd(),
			e.destroyCmd(),
			e.execCmd(),
			e.exportCmd(),
			e.urlCmd(),
			e.exposeCmd(),
			e.unexposeCmd(),

			e.handoffCmd(),
			e.worktreeStatusCmd(),
			e.syncCmd(),
		)
		return cmd
	})
}

type envCmd struct {
	a *app

	app string

	all bool

	mine bool

	create createFlags

	destroy destroyFlags

	handoffTo string
	exposeVia string
}

func (e *envCmd) preRun(cmd *cobra.Command, args []string) error {

	if err := e.a.beforeRun(cmd); err != nil {
		return err
	}
	if e.a.service != nil {
		return nil
	}
	if err := e.resolveApp(cmd); err != nil {
		return err
	}

	e.a.render = renderInfo{app: e.app, env: firstArg(args), args: args}
	if cmd.Name() == "create" {
		e.resolveFrom(cmd)

		if err := e.setSecretsBeforeCreate(cmd, args); err != nil {
			return err
		}
		if err := e.pushBeforeCreate(cmd); err != nil {
			return err
		}
	}
	if cmd.Name() == "destroy" {
		if err := e.confirmDestroy(cmd, args); err != nil {
			return err
		}
	}
	if err := e.checkFleetFlags(cmd); err != nil {
		return err
	}
	return e.a.forwardIfCommander(cmd)
}

func (e *envCmd) resolveFrom(cmd *cobra.Command) {
	if cmd.Flags().Changed("from") || strings.TrimSpace(e.create.from) != "" {
		return
	}
	branch := currentBranch(cmd.Context())
	if branch == "" {
		return
	}
	e.create.from = branch
	e.a.args = injectFlag(e.a.args, "--from", branch)
}

func (e *envCmd) confirmDestroy(cmd *cobra.Command, args []string) error {
	if e.destroy.yes {
		return nil
	}
	name := "this environment"
	if len(args) > 0 {
		name = args[0]
	}

	needs := &usageError{fmt.Errorf("destroying %s/%s needs confirmation: re-run with --yes", e.app, name)}
	plan := destroyPlan(e.app, name, e.destroy.deleteBranch)
	ctx := cmd.Context()
	var proceed bool
	var err error
	if e.destroy.force {

		proceed, err = e.a.confirmName(ctx, needs, plan, fmt.Sprintf("Type %q to confirm: ", name), name)
	} else {
		proceed, err = e.a.confirmWith(ctx, needs, plan)
	}
	switch {
	case err != nil:
		return err
	case !proceed:
		return errors.New("cancelled")
	}
	e.destroy.yes = true
	e.a.args = injectSwitch(e.a.args, "--yes")
	return nil
}

func (e *envCmd) resolveApp(cmd *cobra.Command) error {
	if cmd.Name() == "list" && (e.all || e.mine) {

		return nil
	}
	name := strings.TrimSpace(e.app)
	if name == "" {
		name = appFromCheckout(cmd.Context(), ".")
	}
	if name == "" {
		if cmd.Name() == "list" {

			return nil
		}
		return &usageError{errors.New(
			"cannot tell which app this is: run this inside the app's checkout, " +
				"or name it with --app NAME (or CARAMELO_APP)")}
	}
	if err := env.ValidateName("app", name); err != nil {
		return &usageError{err}
	}
	e.app = name
	if !cmd.Flags().Changed("app") {
		e.a.args = injectFlag(e.a.args, "--app", name)
	}
	return nil
}

func injectFlag(args []string, flag, value string) []string {
	return inject(args, flag, value)
}

func injectSwitch(args []string, flag string) []string {
	return inject(args, flag)
}

func inject(args []string, tokens ...string) []string {
	at := len(args)
	for i, a := range args {
		if a == "--" {
			at = i
			break
		}
	}
	out := make([]string, 0, len(args)+len(tokens))
	out = append(out, args[:at]...)
	out = append(out, tokens...)
	return append(out, args[at:]...)
}

func currentBranch(ctx context.Context) string {
	if _, err := runGit(ctx, ".", "rev-parse", "--git-dir"); err != nil {
		return ""
	}
	branch, err := runGit(ctx, ".", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || branch == "HEAD" {
		return ""
	}
	return branch
}

var runGit place.GitFunc = place.ExecGit

func appFromCheckout(ctx context.Context, dir string) string {
	return place.AppFromCheckout(ctx, runGit, dir)
}

func (e *envCmd) requireApp() error {
	if strings.TrimSpace(e.app) == "" {
		return &usageError{errors.New("no app: pass --app NAME (or run the command inside the app's checkout)")}
	}
	return env.ValidateName("app", e.app)
}

func (e *envCmd) service() api.Service { return e.a.service }

func minArgs(n int) cobra.PositionalArgs {
	inner := cobra.MinimumNArgs(n)
	return func(cmd *cobra.Command, args []string) error {
		if err := inner(cmd, args); err != nil {
			return &usageError{err}
		}
		return nil
	}
}

func rangeArgs(low, high int) cobra.PositionalArgs {
	inner := cobra.RangeArgs(low, high)
	return func(cmd *cobra.Command, args []string) error {
		if err := inner(cmd, args); err != nil {
			return &usageError{fmt.Errorf("%s: %w", cmd.CommandPath(), err)}
		}
		return nil
	}
}

func shortCommit(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	if sha == "" {
		return "-"
	}
	return sha
}

func portRange(e env.Env) string {
	if e.PortCount <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d-%d", e.PortBase, e.PortBase+e.PortCount-1)
}

func depNames(e env.Env) []string {
	if len(e.Config) == 0 {
		return nil
	}
	var doc struct {
		Deps []struct {
			Name string `json:"name"`
		} `json:"deps"`
	}
	if err := json.Unmarshal(e.Config, &doc); err != nil {
		return nil
	}
	out := make([]string, 0, len(doc.Deps))
	for _, d := range doc.Deps {
		out = append(out, d.Name)
	}
	return out
}

func strOrDash(s string) string { return strOr(s, "-") }

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func (e *envCmd) checkFleetFlags(cmd *cobra.Command) error {
	switch cmd.Name() {
	case "handoff":
		if strings.TrimSpace(e.handoffTo) == "" {
			return &usageError{errors.New("handoff needs a peer to hand it to: --to PEER")}
		}
	case "expose":
		if v := strings.TrimSpace(e.exposeVia); v != "" {
			if _, err := config.ParseVia(v); err != nil {
				return &usageError{err}
			}
		}
	}
	return nil
}
