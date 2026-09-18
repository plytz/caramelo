package cli

import (
	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/env"
)

func (a *app) promoteCmd() *cobra.Command {
	t := &envTargets{a: a}
	cmd := &cobra.Command{
		Use:   "promote [ENV]",
		Short: "End a deploy's watch: stop the old replicas and prune",
		Long: `promote finishes a deploy that is waiting in its watch window — because
deploy.promote is manual, or because the deploy was started with --no-watch.

The replicas the deploy is holding are stopped and removed, and releases past
deploy.keep are pruned. After this there is no instant way back: a rollback
becomes a deploy of the previous release, which is the same walk.`,
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _, err := t.target(cmd, args)
			if err != nil {
				return err
			}
			res, err := t.service().Promote(cmd.Context(), env.PromoteRequest{
				App: t.app,
				Env: name,
			}, t.a.progressWriter())
			return finishDeploy(t.a, res, err)
		},
	}
	t.addTargetFlags(cmd)
	return available(cmd, onCommander.or(onHub))
}
