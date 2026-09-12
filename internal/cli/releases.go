package cli

import (
	"io"

	"github.com/spf13/cobra"
)

func (a *app) releasesCmd() *cobra.Command {
	t := &envTargets{a: a}

	t.machineWide = func() bool { return true }
	var limit int
	cmd := &cobra.Command{
		Use:   "releases [ENV]",
		Short: "An environment's deploy history, or the app's releases",
		Long: `releases prints every deploy of an environment, newest first: the release
it carried, who asked for it, when it started and finished, and how it ended —
promoted, rolled back, or failed at a gate.

With no ENV it prints the app's releases instead: what each was built from,
which machine built it, and which machines hold its images, for which
architecture.

The release each line names can be rolled back to with caramelo rollback --to,
for as long as deploy.keep keeps its images.`,
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {

			name, _, err := t.targetOrMachine(cmd, args)
			if err != nil {
				return err
			}
			res, err := t.service().Releases(cmd.Context(), t.app, name, limit)
			if err != nil {
				return err
			}
			return t.a.printer().Result(res, func(w io.Writer) error {
				return writeReleasesResult(w, res)
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "at most this many deploys (default: all of them)")
	t.addTargetFlags(cmd)
	return cmd
}
