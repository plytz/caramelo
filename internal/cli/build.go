package cli

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/release"
)

func (a *app) buildCmd() *cobra.Command {
	t := &envTargets{a: a}
	f := &deployFlags{}
	cmd := &cobra.Command{
		Use:   "build [ENV]",
		Short: "Build a release: one image per service, from a commit",
		Long: `build makes a release of an environment's code: one container image per
service, named caramelo/<app>/<service>:<tree> and addressed by the hash of the
tree, so a tree that was already built is reported unchanged and nothing runs.

Run from a checkout it pushes HEAD to the machine first, so the code is there
before it is built. The release records the commit, the tree, the images and the
caramelo.yaml they were built from, which is what lets a rollback deploy the
definition that went with the code.`,
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _, err := t.target(cmd, args)
			if err != nil {
				return err
			}
			res, err := t.service().Build(cmd.Context(), release.BuildRequest{
				App:      t.app,
				Env:      name,
				Ref:      f.ref,
				Services: f.services,
				Force:    f.force,
			}, t.a.progressWriter())
			if err != nil {
				return err
			}
			return t.a.printer().Result(res, func(w io.Writer) error {
				return writeBuildResult(w, res)
			})
		},
	}
	cmd.Flags().StringVar(&f.ref, "ref", "", "commit, tag or branch to build (default: the env's branch head)")
	cmd.Flags().BoolVar(&f.force, "force", false, "rebuild even when this tree already has a release")
	cmd.Flags().StringSliceVar(&f.services, "service", nil, "only these services (repeatable)")
	cmd.Flags().BoolVar(&f.noPush, "no-push", false, "do not push the code to the machine first")
	t.addTargetFlags(cmd)
	t.beforeForward = func(cmd *cobra.Command) error { return t.pushBeforeRelease(cmd, f) }
	return available(cmd, onCommander.or(onHub))
}
