package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/plytz/caramelo/internal/api"
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

	machine string

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
	kindClient = "client"
	kindLocal  = "local"
)

func clientCmd(cmd *cobra.Command) *cobra.Command { return kindCmd(cmd, kindClient) }

func localCmd(cmd *cobra.Command) *cobra.Command { return kindCmd(cmd, kindLocal) }

func kindCmd(cmd *cobra.Command, kind string) *cobra.Command {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations["kind"] = kind
	return cmd
}

func isClient(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Annotations["kind"] {
		case kindClient:
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

func (a *app) forwardIfClient(cmd *cobra.Command) error {
	if !isClient(cmd) || a.service != nil || cmd.HasSubCommands() {
		return nil
	}
	code, err := a.forwardRendered(cmd.Context(), cmd)
	if err != nil {
		return err
	}
	return &exitError{code}
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
types the same commands. 'caramelo manual' is the complete reference.`,

		SilenceErrors: true,
		SilenceUsage:  true,
	}
	asGroup(root)
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().BoolVar(&a.json, "json", false, "print machine-readable JSON on stdout")
	root.PersistentFlags().StringVar(&a.machine, "machine", os.Getenv("CARAMELO_MACHINE"),
		"machine to talk to: name, user@host[:port], or 'local' (default: auto-detect)")
	root.PersistentFlags().StringVar(&a.progressFlag, "progress", "",
		"how progress is written to stderr: text (default) or json, one event per line")
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if err := a.setProgress(); err != nil {
			return err
		}
		return a.forwardIfClient(cmd)
	}
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &usageError{err}
	})

	root.AddCommand(a.versionCmd())
	for _, f := range commands {
		root.AddCommand(f(a))
	}
	attachExamples(root)
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
