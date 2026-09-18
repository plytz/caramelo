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
			Use:   "test [ENV] [-- ARG...]",
			Short: "Run the app's tests inside an environment",
			Long: `test runs the app's test command in a fresh container built from the
same definition the services use: the app's toolchain, the environment's
worktree mounted, its dependencies reachable by name, the install step first.

Anything after -- is appended to the test command:

  caramelo test feat-x -- ./internal/env/...

The exit code is the test command's own.`,
			Args:                  cobra.ArbitraryArgs,
			DisableFlagsInUseLine: true,
			RunE: func(cmd *cobra.Command, args []string) error {
				name, rest, err := t.target(cmd, args)
				if err != nil {
					return err
				}

				if len(rest) > 0 {
					return &usageError{errors.New("usage: caramelo test [ENV] [-- ARG...]")}
				}
				return t.oneOff(cmd, env.RunRequest{
					App:     t.app,
					Name:    name,
					Service: service,
					Test:    true,
					Argv:    afterDash(cmd, args),
					Timeout: timeout,
				})
			},
		}
		t.addTargetFlags(cmd)
		cmd.Flags().StringVar(&service, "service", "",
			"service whose toolchain to use (default: the first one)")
		cmd.Flags().DurationVar(&timeout, "timeout", 0, "give up after this long (default: no limit)")
		return available(cmd, onCommander.or(onHub))
	})
}

func (t *envTargets) oneOff(cmd *cobra.Command, req env.RunRequest) error {
	req.Stdin = stdinFrom(cmd.Context())
	req.Stdout = t.a.stdout
	req.Stderr = t.a.stderr
	call := t.service().Run
	if req.Test {
		call = t.service().Test
	}
	code, err := call(cmd.Context(), req)
	if err != nil {
		return err
	}
	if code != 0 {
		return &exitError{code}
	}
	return nil
}
