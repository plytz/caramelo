package cli

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/env"
)

func (a *app) deployCmd() *cobra.Command {
	t := &envTargets{a: a}
	f := &deployFlags{}
	cmd := &cobra.Command{
		Use:   "deploy [ENV]",
		Short: "Deploy a release to an environment, with gates and a way back",
		Long: `deploy walks a release into a release-mode environment:

  build     the release, or reuse the one this tree already has
  before    deploy.before (the migration) as a one-off in the new image
  replicas  every new replica beside the old pool, health-checked and probed
  check     deploy.check (the smoke test) against the new pool, by name
  switch    one pushed table: the new pool active, the old one held
  watch     deploy.watch, with the old replicas drained but still running
  promote   stop what was held and prune — or roll back, if the edge counted
            more errors than deploy.max_errors allows

Nothing a client can see changes until the switch, and for the length of the
watch the way back is one pushed table. --no-watch returns at the switch and
leaves the watch to the daemon, which still promotes or rolls back; env show
says where it got to.

It is refused on a development environment: use caramelo up there.`,
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _, err := t.target(cmd, args)
			if err != nil {
				return err
			}
			res, err := t.service().Deploy(cmd.Context(), env.DeployRequest{
				App:      t.app,
				Env:      name,
				Ref:      f.ref,
				Services: f.services,
				NoWatch:  f.noWatch,
				Timeout:  f.timeout,
				Force:    f.force,
			}, t.a.progressWriter())
			return finishDeploy(t.a, res, err)
		},
	}
	cmd.Flags().StringVar(&f.ref, "ref", "", "commit, tag or branch to deploy (default: the env's branch head)")
	cmd.Flags().BoolVar(&f.noWatch, "no-watch", false, "return at the switch and leave the watch to the daemon")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 0, "bound the whole walk (default 30m)")
	cmd.Flags().BoolVar(&f.force, "force", false, "rebuild even when this tree already has a release")
	cmd.Flags().StringSliceVar(&f.services, "service", nil, "only these services (repeatable)")
	cmd.Flags().BoolVar(&f.noPush, "no-push", false, "do not push the code to the machine first")
	t.addTargetFlags(cmd)
	t.beforeForward = func(cmd *cobra.Command) error { return t.pushBeforeRelease(cmd, f) }
	return cmd
}

func finishDeploy(a *app, res *api.DeployResult, walkErr error) error {
	if walkErr != nil {
		if res != nil && a.json {

			_ = a.printer().Result(res, func(io.Writer) error { return nil })
		}
		return walkErr
	}
	return a.printer().Result(res, func(w io.Writer) error {
		return writeDeployResult(w, res)
	})
}
