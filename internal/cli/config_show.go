package cli

import (
	"errors"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/env"

	"github.com/plytz/caramelo/internal/cli/ui"
)

func init() {
	register(func(a *app) *cobra.Command {

		t := &envTargets{a: a, env: os.Getenv("CARAMELO_ENV")}
		var reveal bool
		group := commanderCmd(&cobra.Command{
			Use:   "config",
			Short: "Inspect the effective caramelo.yaml of an app",
		})
		show := &cobra.Command{
			Use:   "show [ENV]",
			Short: "Print the effective configuration, with the source of every field",
			Long: `show prints the configuration caramelo will actually use: what
caramelo.yaml says, with stack detection filling every gap, and for each field
where the value came from — the file, detection (with the evidence: "go.mod
says go 1.23"), or a built-in default.

With an environment, detection runs against that environment's worktree, which
is the code that will actually run. Without one, it runs on the app's default
branch.

A value env: builds out of a secret — ${secrets.NAME}, or a connection string
holding ${deps.db.password} — prints as <secret>. --reveal prints the real ones,
and the machine records an event naming you.`,
			Args: maxArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				name, err := configShowEnv(t, args)
				if err != nil {
					return err
				}
				cfg, err := a.service.EffectiveConfig(cmd.Context(), t.app, name, reveal)
				if err != nil {
					return err
				}
				return a.printer().Result(cfg, func(w io.Writer) error {
					return writeEffectiveConfig(w, cfg)
				})
			},
		}
		show.Flags().StringVar(&t.app, "app", os.Getenv("CARAMELO_APP"),
			"application to read (default: the checkout you are in)")
		show.Flags().BoolVar(&reveal, "reveal", false,
			"print the real values of variables that came from the vault (recorded as an event)")
		asGroup(group)
		group.AddCommand(show)

		group.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
			if a.service != nil {
				return nil
			}
			if err := resolveConfigApp(t, cmd); err != nil {
				return err
			}
			resolveConfigEnv(t, cmd, args)
			return a.forwardIfCommander(cmd)
		}
		return group
	})
}

func resolveConfigApp(t *envTargets, cmd *cobra.Command) error {
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

func resolveConfigEnv(t *envTargets, cmd *cobra.Command, args []string) {
	if len(args) > 0 {
		return
	}
	name := strings.TrimSpace(t.env)
	if name == "" {
		name = envFromCheckout(cmd.Context(), ".")
	}
	if name == "" {
		return
	}
	t.env = name

	t.a.args = inject(t.a.args, name)
}

func configShowEnv(t *envTargets, args []string) (string, error) {
	name := strings.TrimSpace(t.env)
	if len(args) > 0 {
		name = strings.TrimSpace(args[0])
	}
	if name == "" {
		return "", nil
	}
	if err := env.ValidateName("env", name); err != nil {
		return "", &usageError{err}
	}
	return name, nil
}

func writeEffectiveConfig(w io.Writer, c *api.EffectiveConfig) error {
	return configView(c).Write(w)
}

func configView(c *api.EffectiveConfig) *ui.View {
	if c == nil {
		return ui.NewView().Text("no configuration")
	}
	head := ui.NewTable()
	head.Row("APP", c.App)
	if c.Env != "" {
		head.Row("ENV", c.Env)
	} else {
		head.Row("ENV", "-", "(the app's default branch)")
	}
	if c.Stack != "" {
		head.Row("STACK", c.Stack)
	} else {
		head.Row("STACK", "-", "(nothing detected: caramelo.yaml must describe this app)")
	}
	v := ui.NewView().Table(head)
	if len(c.Fields) == 0 {
		return v.Blank().Text("nothing to run: caramelo.yaml says nothing and detection found nothing")
	}
	t := ui.NewTable("KEY", "VALUE", "SOURCE", "WHY")
	for _, f := range c.Fields {
		t.Row(f.Key, strOrDash(f.Value), string(f.Source), strOrDash(f.Evidence))
	}
	return v.Section(t)
}
