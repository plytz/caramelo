package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/env"

	"github.com/plytz/caramelo/internal/cli/ui"
)

type createFlags struct {
	from    string
	reset   bool
	noPush  bool
	force   bool
	noDeps  bool
	timeout time.Duration

	release bool

	production bool

	protected bool

	on string

	secretsFrom string
}

func (e *envCmd) createCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create an environment: worktree, ports, dependency containers",
		Long: `create builds an environment on the machine and waits for it to be usable:
a branch named after the env, a worktree of it, a block of ports, a container
and a named volume per dependency in caramelo.yaml, and the expanded
variables. Progress goes to standard error, the environment to standard
output.

Run from a checkout and reached over SSH, it pushes the --from ref to the
machine first, so the code is there before the environment is built.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := e.requireApp(); err != nil {
				return err
			}
			name := args[0]
			if err := env.ValidateName("env", name); err != nil {
				return &usageError{err}
			}
			if e.create.on != "" {

				if err := env.ValidateName("machine", e.create.on); err != nil {
					return &usageError{err}
				}
			}

			secrets, err := createSecrets(cmd.Context(), e.create.secretsFrom)
			if err != nil {
				return err
			}
			ev, err := e.service().CreateEnv(cmd.Context(), env.CreateRequest{
				App:     e.app,
				Name:    name,
				From:    e.create.from,
				Reset:   e.create.reset,
				NoDeps:  e.create.noDeps,
				Timeout: e.create.timeout,

				Release:     e.create.release,
				Production:  e.create.production,
				Protected:   e.create.protected,
				SecretsFrom: e.create.secretsFrom,
				Secrets:     secrets,

				On: e.create.on,
			}, e.a.progressWriter())
			if err != nil {
				return err
			}
			return e.a.printer().Result(ev, func(w io.Writer) error {
				if err := writeEnv(w, ev, nil); err != nil {
					return err
				}
				return writeNextStep(w, ev)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&e.create.from, "from", "", "ref the env's branch is created from (default: the current branch, else the app's default branch)")
	f.BoolVar(&e.create.reset, "reset", false, "move an existing branch to --from instead of refusing")
	f.BoolVar(&e.create.noPush, "no-push", false, "do not push the code to the machine first")
	f.BoolVar(&e.create.force, "force", false, "force the push (the default is fast-forward only)")
	f.BoolVar(&e.create.noDeps, "no-deps", false, "skip the dependency containers")
	f.DurationVar(&e.create.timeout, "timeout", 0, "how long to wait for every dependency to become ready (default 1m)")
	f.BoolVar(&e.create.release, "release", false,
		"a deployed environment: its services run images built from a commit, and `caramelo deploy` changes them")
	f.BoolVar(&e.create.production, "production", false,
		"--release, plus protected, plus the hostnames envs.NAME.hosts lists, plus no default dependency passwords")
	f.BoolVar(&e.create.protected, "protected", false,
		"destroying this environment needs --force (--production implies it)")
	f.StringVar(&e.create.on, "on", "",
		"machine of the fleet to create it on, or `hub` (default: wherever it fits best)")
	f.StringVar(&e.create.secretsFrom, "secrets-from", "",
		"KEY=value file whose contents become this environment's secrets, before its dependencies start")
	return cmd
}

func createSecrets(ctx context.Context, path string) (map[string]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	values, err := readEnvFile(ctx, path)
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, &usageError{fmt.Errorf("%s has no KEY=value lines: nothing to put in the vault", path)}
	}
	return values, nil
}

func writeNextStep(w io.Writer, e *env.Env) error {
	if e == nil || e.Mode != env.ModeRelease {
		return nil
	}
	_, err := fmt.Fprintf(w,
		"\nrelease mode: nothing runs until `caramelo deploy %s` builds a release and walks it in\n", e.Name)
	return err
}

func describeVia(via string) string {
	switch config.Via(via) {
	case config.ViaHub:
		return "hub: the hub's edge terminates TLS and forwards through the tunnel"
	case config.ViaMember:
		return "member: the edge of the machine it runs on"
	}
	return via
}

func describeMode(e env.Env) string {
	mode := string(e.Mode)
	if mode == "" {

		mode = string(env.ModeDev)
	}
	if e.Protected {
		mode += ", protected (env destroy needs --force)"
	}
	return mode
}

func writeEnv(w io.Writer, e *env.Env, deps []env.DepState) error {
	return envRecordView(e, deps).Write(w)
}

func envRecordView(e *env.Env, deps []env.DepState) *ui.View {
	if e == nil {
		return ui.NewView().Text("no environment")
	}
	f := ui.NewFields(fmt.Sprintf("%s/%s  %s", e.App, e.Name, e.Status))

	f.Add("mode", "%s", describeMode(*e))
	f.Add("branch", "%s at %s", strOrDash(e.Branch), shortCommit(e.Commit))
	f.Add("worktree", "%s", strOrDash(e.Worktree))
	f.Add("ports", "%s  (PORT=%d)", portRange(*e), e.Port())
	if names := depNames(*e); len(names) > 0 {
		f.Add("deps", "%s", strings.Join(names, ", "))
	}

	if owner, machine, via := envFleetFields(*e); owner != "" || machine != "" || via != "" {
		if machine != "" {
			f.Add("machine", "%s", machine)
		}
		if owner != "" {
			f.Add("owner", "%s", owner)
		}
		if via != "" {
			f.Add("served", "%s", describeVia(via))
		}
	}
	if e.CreatedBy != "" {
		f.Add("created by", "%s", e.CreatedBy)
	}
	if !e.CreatedAt.IsZero() {
		f.Add("created", "%s", e.CreatedAt.Local().Format(time.RFC3339))
	}
	v := ui.NewView().Fields(f).Section(depsTable(deps))
	if len(e.Vars) > 0 {
		v.Blank().Text("%d variables; see `caramelo env export %s`", len(e.Vars), e.Name)
	}
	return v
}

func depsTable(deps []env.DepState) *ui.Table {
	t := ui.NewTable("DEP", "STATUS", "PORT", "CONTAINER")
	for _, d := range deps {
		t.Row(d.Name, string(d.Status), fmt.Sprint(d.Port), d.Container)
	}
	return t
}
