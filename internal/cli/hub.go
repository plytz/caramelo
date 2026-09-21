package cli

import "github.com/spf13/cobra"

var hubCommands []func(a *app) *cobra.Command

func registerHub(f func(a *app) *cobra.Command) { hubCommands = append(hubCommands, f) }

func init() {
	register(func(a *app) *cobra.Command {
		cmd := &cobra.Command{
			Use:   "hub",
			Short: "Commands the machine itself runs: uninstall, status, run",
			Long: `The hub namespace acts on this machine directly. It is never
forwarded to a daemon and caramelod refuses it over the SSH API.

Every machine starts as a hub of one, so these are also what a member types
about itself: 'hub status' and 'hub uninstall' answer for the box you are on.`,
		}
		for _, f := range hubCommands {
			asGroup(cmd)
			cmd.AddCommand(f(a))
		}
		return cmd
	})
}
