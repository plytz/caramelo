package cli

import (
	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/env"
)

func init() {
	register(func(a *app) *cobra.Command {

		t := &envTargets{a: a, takesServices: true}
		var (
			follow bool
			since  string
			tail   int
			deps   bool
			edge   bool
		)
		cmd := &cobra.Command{
			Use:   "logs [ENV] [SERVICE...]",
			Short: "Show an environment's service logs, merged and prefixed",
			Long: `logs prints what the environment's services have written, one line at a
time with the service name in front:

  caramelo logs feat-x -f            # follow
  caramelo logs feat-x web --tail 50 # one service, the last 50 lines
  caramelo logs feat-x --deps        # the dependency containers too
  caramelo logs feat-x --json        # one JSON object per line
  caramelo logs feat-x --edge        # the public requests the edge served

Inside an environment — an 'env exec' session, or a service container —
CARAMELO_ENV already names it, so every argument is a service name there:
'caramelo logs web'.`,
			Args: cobra.ArbitraryArgs,
			RunE: func(cmd *cobra.Command, args []string) error {

				name, services, err := t.targetOrMachine(cmd, args)
				if err != nil {
					return err
				}
				return t.service().Logs(cmd.Context(), env.LogsRequest{
					App:      t.app,
					Name:     name,
					Services: services,
					Deps:     deps,
					Follow:   follow,
					Since:    since,
					Tail:     tail,
					Edge:     edge,
					JSON:     t.a.json,
					Stdout:   t.a.stdout,
					Stderr:   t.a.stderr,
				})
			},
		}
		t.machineWide = func() bool { return edge }
		t.addTargetFlags(cmd)
		cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming new lines")
		cmd.Flags().StringVar(&since, "since", "", "only lines since this duration (10m) or timestamp")
		cmd.Flags().IntVar(&tail, "tail", 0, "start from this many trailing lines (default: all of them)")
		cmd.Flags().BoolVar(&deps, "deps", false, "include the dependency containers")
		cmd.Flags().BoolVar(&edge, "edge", false,
			"read the machine's edge access log instead: one line per public request, with the target that answered")
		return cmd
	})
}
