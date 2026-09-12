package cli

import "github.com/spf13/cobra"

var serverCommands []func(a *app) *cobra.Command

func registerServer(f func(a *app) *cobra.Command) { serverCommands = append(serverCommands, f) }

func init() {
	register(func(a *app) *cobra.Command {
		cmd := &cobra.Command{
			Use:   "server",
			Short: "Commands the machine itself runs: setup, uninstall, status, run",
			Long: `The server namespace acts on this machine directly. It is never
forwarded to a daemon and caramelod refuses it over the SSH API.`,
		}
		for _, f := range serverCommands {
			asGroup(cmd)
			cmd.AddCommand(f(a))
		}
		return cmd
	})
}
