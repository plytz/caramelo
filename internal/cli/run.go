package cli

import (
	"errors"
	"time"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/env"
)

func init() {
	register(func(a *app) *cobra.Command {
		t := &envTargets{a: a}
		var (
			service string
			timeout time.Duration
		)
		cmd := &cobra.Command{
			Use:   "run ENV -- COMMAND [ARG...]",
			Short: "Run a command in the app's toolchain inside an environment",
			Long: `run starts a one-off container from the service's image, on the
environment's network, with the worktree mounted and the environment's
variables set, runs the install step and then your command:

  caramelo run feat-x -- npm run lint
  caramelo run feat-x -- python manage.py migrate

The -- is required: everything after it is the command, not caramelo's flags.
Standard input, output and error are wired through and the exit code is the
command's own.

For a command on the machine itself — git, docker, a look at a file — use
'caramelo env exec' instead.`,
			Args:                  cobra.ArbitraryArgs,
			DisableFlagsInUseLine: true,
			RunE: func(cmd *cobra.Command, args []string) error {
				name, rest, err := t.target(cmd, args)
				if err != nil {
					return err
				}
				if len(rest) > 0 {
					return &usageError{errors.New("usage: caramelo run ENV -- COMMAND [ARG...]")}
				}
				argv := afterDash(cmd, args)
				if len(argv) == 0 {

					return &usageError{errors.New(
						"caramelo run needs a command after --, for example " +
							"`caramelo run " + name + " -- npm run lint`; " +
							"to start the app's own services, use `caramelo up " + name + "`")}
				}
				return t.oneOff(cmd, env.RunRequest{
					App:     t.app,
					Name:    name,
					Service: service,
					Argv:    argv,
					Timeout: timeout,
				})
			},
		}
		t.addTargetFlags(cmd)
		cmd.Flags().StringVar(&service, "service", "",
			"service whose toolchain to use (default: the first one)")
		cmd.Flags().DurationVar(&timeout, "timeout", 0, "give up after this long (default: no limit)")
		return cmd
	})
}
