package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/place"
	"github.com/plytz/caramelo/internal/progress"
	"github.com/spf13/cobra"
)

var version = "dev"

const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

type app struct {
	stdout io.Writer
	stderr io.Writer
	json   bool

	args []string

	stdin io.Reader

	machine   string
	fleet     string
	configDir string

	chosenFleet string
	appHint     func() string

	placeOnce sync.Once
	placeHere place.Context
	placeErr  error

	progressFlag string
	progress     progress.Format

	render renderInfo

	service api.Service
	session api.Session
}

type Options struct {
	Service api.Service
	Session api.Session
}

type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

const (
	kindCommander = "commander"
	kindLocal     = "local"
)

func commanderCmd(cmd *cobra.Command) *cobra.Command { return kindCmd(cmd, kindCommander) }

func localCmd(cmd *cobra.Command) *cobra.Command { return kindCmd(cmd, kindLocal) }

func kindCmd(cmd *cobra.Command, kind string) *cobra.Command {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations["kind"] = kind
	return cmd
}

func isCommander(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Annotations["kind"] {
		case kindCommander:
			return true
		case kindLocal:
			return false
		}
	}
	return false
}

var commands []func(a *app) *cobra.Command

func register(f func(a *app) *cobra.Command) { commands = append(commands, f) }

var forward = func(ctx context.Context, a *app) (int, error) {
	return ExitError, errors.New("no daemon transport available")
}

func (a *app) forwardIfCommander(cmd *cobra.Command) error {
	a.typedTargetWins(cmd)
	if !isCommander(cmd) || a.service != nil || cmd.HasSubCommands() {
		return nil
	}
	a.appHint = appOfCommand(cmd)
	code, err := a.forwardRendered(cmd.Context(), cmd)
	if err != nil {
		return err
	}
	if code == ExitOK {
		a.rememberAppFleet(cmd.Context())
	}
	return &exitError{code}
}

func (a *app) typedTargetWins(cmd *cobra.Command) {
	machineTyped, fleetTyped := flagTyped(cmd, "machine"), flagTyped(cmd, "fleet")
	switch {
	case machineTyped && !fleetTyped:
		a.fleet = ""
	case fleetTyped && !machineTyped:
		a.machine = ""
	}
}

func flagTyped(cmd *cobra.Command, name string) bool {
	f := cmd.Flags().Lookup(name)
	return f != nil && f.Changed
}

func appOfCommand(cmd *cobra.Command) func() string {
	if cmd.Flags().Lookup("app") == nil {
		return nil
	}
	var once sync.Once
	var name string
	return func() string {
		once.Do(func() {
			if f := cmd.Flags().Lookup("app"); f != nil && strings.TrimSpace(f.Value.String()) != "" {
				name = strings.TrimSpace(f.Value.String())
				return
			}
			name = appFromCheckout(cmd.Context(), ".")
		})
		return name
	}
}

func (a *app) printer() *Printer {
	return &Printer{Out: a.stdout, Err: a.stderr, JSON: a.json}
}

func (a *app) setProgress() error {
	f, err := progress.ParseFormat(a.progressFlag)
	if err != nil {
		return &usageError{err}
	}
	a.progress = f
	return nil
}

func (a *app) progressWriter() io.Writer {
	return progress.New(a.stderr, a.progress)
}

type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func exactArgs(n int) cobra.PositionalArgs {
	inner := cobra.ExactArgs(n)
	return func(cmd *cobra.Command, args []string) error {
		if err := inner(cmd, args); err != nil {
			return &usageError{err}
		}
		return nil
	}
}

func NewRootCmd(stdout, stderr io.Writer) *cobra.Command {
	return newRootCmd(&app{stdout: stdout, stderr: stderr})
}

func newRootCmd(a *app) *cobra.Command {

	root := &cobra.Command{
		Use:   "caramelo",
		Short: "Build, run, test, deploy and operate applications without a DevOps engineer",
		Long: `caramelo builds, tests, runs, deploys and operates applications from a single
caramelo.yaml, on machines it sets up itself: isolated environments for every
branch, previews on real hostnames, production with gated deploys and one-command
rollback, and a fleet of machines behaving as one.

It is built for agents first. Every command has --json output, stable exit codes
and never waits on a terminal, so an agent can drive it from a shell; a person
types the same commands. 'caramelo manual' is the reference for this machine,
and 'caramelo manual --role all' for every role there is.`,

		SilenceErrors: true,
		SilenceUsage:  true,
	}
	asGroup(root)
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().BoolVar(&a.json, "json", false, "print machine-readable JSON on stdout")
	root.PersistentFlags().StringVar(&a.machine, "machine", os.Getenv("CARAMELO_MACHINE"),
		"raw ssh target to talk to, for a machine that is in no fleet yet: user@host[:port], or 'local'")
	root.PersistentFlags().StringVar(&a.fleet, "fleet", os.Getenv("CARAMELO_FLEET"),
		"fleet to talk to, by its name in the commander config (default: the app's fleet, then the default fleet)")
	root.PersistentFlags().StringVar(&a.progressFlag, "progress", "",
		"how progress is written to stderr: text (default) or json, one event per line")
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if err := a.beforeRun(cmd); err != nil {
			return err
		}
		return a.forwardIfCommander(cmd)
	}
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &usageError{err}
	})

	root.AddCommand(a.versionCmd())
	for _, f := range commands {
		root.AddCommand(f(a))
	}
	attachExamples(root)
	a.installHelp(root)
	return root
}

func Run(args []string, stdout, stderr io.Writer) int {
	return RunWith(context.Background(), args, stdout, stderr, Options{})
}

func RunWith(ctx context.Context, args []string, stdout, stderr io.Writer, opts Options) int {
	if len(args) > 0 && args[0] == "caramelo" {
		args = args[1:]
	}
	a := &app{stdout: stdout, stderr: stderr, args: args, service: opts.Service, session: opts.Session}
	root := newRootCmd(a)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}
	var xe *exitError
	if errors.As(err, &xe) {
		return xe.code
	}

	var fe *api.ForwardedError
	if errors.As(err, &fe) {
		return fe.Code
	}

	var nh *notHereError
	if errors.As(err, &nh) {
		fmt.Fprintf(stderr, "caramelo: %v\n", err)
		return ExitUsage
	}
	fmt.Fprintf(stderr, "caramelo: %v\n", err)
	var ue *usageError
	if errors.As(err, &ue) {
		fmt.Fprintln(stderr, "Run 'caramelo --help' for usage.")
		return ExitUsage
	}
	return ExitError
}

func Execute() int {
	return Run(os.Args[1:], os.Stdout, os.Stderr)
}

func asGroup(cmd *cobra.Command) *cobra.Command {
	cmd.Args = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return nil
		}
		return &usageError{fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())}
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	}
	return cmd
}
