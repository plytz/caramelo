package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/plytz/caramelo/internal/cli/ui"
	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/task"
	"github.com/plytz/caramelo/internal/userdir"
)

func init() {
	register(func(a *app) *cobra.Command {
		cmd := localCmd(&cobra.Command{
			Use:   "task",
			Short: "The tasks this binary carries: what they do and running one here",
			Long: `A task is a list of items in one YAML file, each item a check that says
whether the machine is already the way it should be and a command that makes it
so. The runner types them where the machine is: 'caramelo task run' on the box
it changes, never over a wire.

'task show' prints the file that will run, so what a person reviews is exactly
what the machine does.`,
		})
		asGroup(cmd)
		cmd.AddCommand(a.taskListCmd(), a.taskShowCmd(), a.taskRunCmd())
		return cmd
	})
}

func (a *app) taskListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the tasks this binary carries",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			list, err := task.List()
			if err != nil {
				return err
			}
			return a.printer().Result(list, func(w io.Writer) error {
				return tasksView(list).Write(w)
			})
		},
	}
	return available(cmd, always)
}

func tasksView(list []task.Info) *ui.View {
	if len(list) == 0 {
		return ui.NewView().Text("this binary carries no tasks")
	}
	table := ui.NewTable("TASK", "WHAT IT DOES")
	for _, t := range list {
		table.Row(t.Name, t.Desc)
	}
	return ui.NewView().Table(table)
}

func (a *app) taskShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show NAME",
		Short: "Print the task file that will run, byte for byte",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := task.Source(args[0])
			if err != nil {
				return &usageError{err}
			}
			_, err = a.stdout.Write(b)
			return err
		},
	}
	return available(cmd, always)
}

func (a *app) taskRunCmd() *cobra.Command {
	var dryRun, debug bool
	var vars []string
	cmd := &cobra.Command{
		Use:   "run NAME",
		Short: "Run a task on this machine",
		Long: `Runs every item of the task in the file's order, a 'parallel' block's items
together. An item whose check already passes is left alone; one whose check
fails runs its command and is checked again. A failure stops the run once the
block it is in has finished, and everything after it is reported 'not run'.

--dry-run stops at every check and says what would change. --debug prints every
check and command with its exit code, its output and the report it wrote.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			callerVars, err := parseTaskVars(vars)
			if err != nil {
				return err
			}
			f, err := task.Load(args[0])
			if err != nil {
				return &usageError{err}
			}
			store, err := a.taskReportStore(cmd.Context())
			if err != nil {
				return err
			}
			engine := &task.Engine{
				Runner:  runner.Exec{},
				Leaf:    a.taskLeaf(),
				Version: version,
				DryRun:  dryRun,
				Vars:    callerVars,
				Report:  store,
			}
			if a.progress == progress.FormatJSON {
				engine.Events = a.progressWriter()
			}
			var r *task.Renderer
			if !a.json {
				r = &task.Renderer{W: a.stdout, Colour: a.colour(), Debug: debug}
				plan, err := engine.Plan(f)
				if err != nil {
					return err
				}
				if err := r.Start(plan); err != nil {
					return err
				}
				engine.Observer = func(res task.Result) { _ = r.Result(res) }
			}
			report, err := engine.Execute(cmd.Context(), f)
			if err != nil {
				return err
			}
			if r != nil {
				if err := r.Finish(report); err != nil {
					return err
				}
			}
			if a.json {
				if err := a.printer().Result(report, nil); err != nil {
					return err
				}
			}
			if report.ReportErr != nil {
				fmt.Fprintf(a.stderr, "caramelo: the run finished and its report was not written: %v\n", report.ReportErr)
			}
			if report.Failed > 0 || report.ReportErr != nil {
				return &exitError{ExitError}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "stop at every check and say what would change")
	cmd.Flags().BoolVar(&debug, "debug", false, "print every check and command with its exit code and output")
	cmd.Flags().StringArrayVar(&vars, "var", nil, "a variable the task's templates can read, as k=v (repeatable)")
	return available(cmd, always)
}

func parseTaskVars(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range pairs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, &usageError{fmt.Errorf("--var %q: want k=v", kv)}
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, nil
}

func (a *app) taskReportStore(ctx context.Context) (task.ReportStore, error) {
	if c, err := a.place(ctx); err == nil && c.IsServer() && c.Server != nil && c.Server.Paths.State != "" {
		return task.ReportStore{
			Dir:  filepath.Join(c.Server.Paths.State, "task"),
			User: c.Server.User,
			Run:  runner.Exec{},
		}, nil
	}
	cache, err := userdir.Cache()
	if err != nil {
		return task.ReportStore{}, err
	}
	return task.ReportStore{Dir: filepath.Join(cache, remote.CommanderDirName, "runs")}, nil
}

var writerIsATerminal = func(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

func (a *app) colour() bool {
	if a.json || os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := a.stdout.(*os.File)
	if !ok {
		return false
	}
	return writerIsATerminal(f)
}

func (a *app) taskLeaf() task.Leaf {
	return func(ctx context.Context, argv []string) (runner.Result, error) {
		var stdout, stderr bytes.Buffer
		code := RunWith(ctx, argv, &stdout, &stderr, Options{})
		return runner.Result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code}, nil
	}
}
