package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/api"

	"github.com/plytz/caramelo/internal/cli/ui"
)

func init() {
	register(func(a *app) *cobra.Command {
		cmd := commanderCmd(&cobra.Command{
			Use:   "app",
			Short: "Inspect the applications on the machine",
			Long: `An application is a bare git repository on the machine. It comes into
existence on the first push to it — there is no create command — and its
environments are worktrees of it.`,
		})
		asGroup(cmd)
		cmd.AddCommand(a.appListCmd())
		return cmd
	})
}

func (a *app) appListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the applications, their default branch and their size",
		Args:    exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			apps, err := a.service.Apps(cmd.Context())
			if err != nil {
				return err
			}
			if apps == nil {
				apps = []api.AppInfo{}
			}
			return a.printer().Result(apps, func(w io.Writer) error {
				return writeApps(w, apps)
			})
		},
	}
}

func writeApps(w io.Writer, apps []api.AppInfo) error { return appsView(apps).Write(w) }

func appsView(apps []api.AppInfo) *ui.View {
	if len(apps) == 0 {
		return ui.NewView().Text("no apps: push one with `git push ssh://caramelo@MACHINE:4022/NAME BRANCH`")
	}
	t := ui.NewTable("NAME", "DEFAULT BRANCH", "ENVS", "REPO")
	for _, app := range apps {
		t.Row(app.Name, strOrDash(app.DefaultBranch), fmt.Sprint(app.EnvCount), fmtBytesIEC(app.RepoBytes))
	}
	return ui.NewView().Table(t)
}
