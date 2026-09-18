package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/env"
)

func (e *envCmd) execCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exec NAME -- COMMAND [ARG...]",
		Short: "Run a command inside an environment, on the machine",
		Long: `exec runs a command on the machine as the Caramelo user, in the
environment's worktree, with the environment's variables set. Standard input,
output and error are wired through and the exit code is the command's own.

The -- is required: everything after it is the command, not caramelo's flags.

This is the one command with no --json output: the child owns stdout, so its
exit code is the machine-readable result.`,
		Args: minArgs(1),

		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := e.requireApp(); err != nil {
				return err
			}

			if cmd.ArgsLenAtDash() != 1 || len(args) < 2 {
				return &usageError{errors.New("usage: caramelo env exec NAME -- COMMAND [ARG...]")}
			}
			name := args[0]
			if err := env.ValidateName("env", name); err != nil {
				return &usageError{err}
			}

			code, err := e.service().ExecEnv(cmd.Context(), env.ExecRequest{
				App:    e.app,
				Name:   name,
				Argv:   args[1:],
				Stdin:  stdinFrom(cmd.Context()),
				Stdout: e.a.stdout,
				Stderr: e.a.stderr,
			})
			if err != nil {
				return err
			}
			if code != 0 {
				return &exitError{code}
			}
			return nil
		},
	}
	return available(cmd, onCommander.or(onHub))
}
