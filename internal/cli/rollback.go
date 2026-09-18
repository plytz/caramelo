package cli

import (
	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/env"
)

func (a *app) rollbackCmd() *cobra.Command {
	t := &envTargets{a: a}
	f := &deployFlags{}
	cmd := &cobra.Command{
		Use:   "rollback [ENV]",
		Short: "Put the previous release back",
		Long: `rollback deploys the release the environment was running before, or the
one named with --to, by the same walk as a deploy and with no build.

Inside a deploy's watch window it is the instant flip back to the replicas that
are still held: one pushed table, no containers started. After the watch it is an
ordinary deploy of an image that already exists.

Nothing is ever "the previous commit you have to know": caramelo releases ENV is
the history, and every line of it can be rolled back to.`,
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _, err := t.target(cmd, args)
			if err != nil {
				return err
			}
			res, err := t.service().Rollback(cmd.Context(), env.RollbackRequest{
				App:     t.app,
				Env:     name,
				To:      f.to,
				Timeout: f.timeout,
			}, t.a.progressWriter())
			return finishDeploy(t.a, res, err)
		},
	}
	cmd.Flags().StringVar(&f.to, "to", "", "release to go back to (default: the one before the current deploy)")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 0, "bound the whole walk (default 30m)")
	t.addTargetFlags(cmd)
	return available(cmd, onCommander.or(onHub))
}
