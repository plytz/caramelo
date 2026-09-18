package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/sshapi"
)

func init() {
	registerHub(func(a *app) *cobra.Command {
		var configDir string
		cmd := &cobra.Command{
			Use:    "run",
			Short:  "Run caramelod, the control plane (started by systemd)",
			Hidden: true,
			Long: `Run the caramelod control plane in the foreground: open the state
store, load the SSH host key, gauge the machine and serve the API on the
configured port and local socket until told to stop.`,
			Args: exactArgs(0),
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := serverconfig.Load(configDir)
				if err != nil {
					return fmt.Errorf("load %s: %w", serverconfig.Path(configDir), err)
				}

				ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, os.Interrupt)
				defer stop()
				return sshapi.Run(ctx, cfg, configDir, version, a.stderr, execCommand)
			},
		}
		cmd.Flags().StringVar(&configDir, "config-dir", serverconfig.DefaultConfigDir,
			"directory holding config.yaml")
		return cmd
	})
}

func execCommand(ctx context.Context, c sshapi.Command) int {
	return RunWith(withStdin(ctx, c.Stdin), c.Args, c.Stdout, c.Stderr, Options{
		Service: c.Service,
		Session: c.Session,
	})
}
