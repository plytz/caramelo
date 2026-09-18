package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/env"

	"github.com/plytz/caramelo/internal/cli/ui"
)

type destroyFlags struct {
	deleteBranch bool
	yes          bool

	force bool
}

func (e *envCmd) destroyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "destroy NAME",
		Aliases: []string{"rm"},
		Short:   "Remove an environment: containers, volumes, worktree",
		Long: `destroy removes an environment's containers, its named volumes and its
worktree, then forgets it. It tolerates pieces that are already gone, so
destroying an environment twice is not an error.

The branch is kept: work committed inside an environment is never deleted by
a lifecycle command. --delete-branch removes it too.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := e.requireApp(); err != nil {
				return err
			}
			name := args[0]

			if !e.destroy.yes {
				return &usageError{fmt.Errorf("destroying %s/%s needs confirmation: re-run with --yes", e.app, name)}
			}
			if err := e.service().DestroyEnv(cmd.Context(), env.DestroyRequest{
				App:          e.app,
				Name:         name,
				DeleteBranch: e.destroy.deleteBranch,
				Force:        e.destroy.force,
			}, e.a.progressWriter()); err != nil {
				return err
			}
			out := destroyed{e.app, name, true, e.destroy.deleteBranch}
			return e.a.printer().Result(out, func(w io.Writer) error {
				return writeDestroyed(w, out)
			})
		},
	}
	cmd.Flags().BoolVar(&e.destroy.deleteBranch, "delete-branch", false, "delete the env's branch as well as its worktree")
	cmd.Flags().BoolVarP(&e.destroy.yes, "yes", "y", false, "do not ask for confirmation")
	cmd.Flags().BoolVar(&e.destroy.force, "force", false, "destroy a protected environment (--production, --protected)")
	return available(cmd, onCommander.or(onHub))
}

type destroyed struct {
	App           string `json:"app"`
	Name          string `json:"name"`
	Destroyed     bool   `json:"destroyed"`
	BranchDeleted bool   `json:"branch_deleted"`
}

func writeDestroyed(w io.Writer, d destroyed) error { return destroyedView(d).Write(w) }

func destroyedView(d destroyed) *ui.View {
	kept := "; branch kept"
	if d.BranchDeleted {
		kept = "; branch deleted"
	}
	return ui.NewView().Text("destroyed %s/%s%s", d.App, d.Name, kept)
}

func destroyPlan(app, name string, deleteBranch bool) string {
	branch := "the branch is kept"
	if deleteBranch {
		branch = "the branch is deleted too"
	}
	return fmt.Sprintf("destroy %s/%s: containers, volumes and worktree are removed (%s).\n", app, name, branch)
}
