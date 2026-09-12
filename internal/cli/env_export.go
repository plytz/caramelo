package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/env"
)

func (e *envCmd) exportCmd() *cobra.Command {
	var format, view string
	var reveal bool
	cmd := &cobra.Command{
		Use:   "export NAME",
		Short: "Print an environment's variables, for eval, .env or JSON",
		Long: `export prints the environment's expanded variables:

  eval "$(caramelo env export feat-x)"        # export KEY='value' lines
  caramelo env export feat-x --format dotenv  # KEY=value, for a .env file
  caramelo env export feat-x --format json    # one object

--json is the same as --format json.

--view says whose addresses the variables carry. The default, host, is what a
process on the machine sees: 127.0.0.1 and the env's published ports, which is
what env exec runs with. --view network is what a service container sees: the
dependency's own name on the env's Docker network, and the port the image
listens on.

A value the vault contributed — ${secrets.NAME}, or a connection string holding
${deps.db.password} — prints as <secret>. --reveal prints the real ones, and the
machine records an event naming you.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := e.requireApp(); err != nil {
				return err
			}

			if e.a.json && !cmd.Flags().Changed("format") {
				format = string(env.FormatJSON)
			}
			f, err := env.ParseExportFormat(format)
			if err != nil {
				return &usageError{err}
			}
			v, err := config.ParseView(view)
			if err != nil {
				return &usageError{err}
			}
			out, err := e.service().ExportEnv(cmd.Context(), e.app, args[0], f, v, reveal)
			if err != nil {
				return err
			}

			if !strings.HasSuffix(out, "\n") {
				out += "\n"
			}
			if _, err := fmt.Fprint(e.a.stdout, out); err != nil {
				return fmt.Errorf("write variables: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "shell (default), dotenv or json")
	cmd.Flags().StringVar(&view, "view", "", "host (default: 127.0.0.1 and the published ports) or network (what a service container sees)")
	cmd.Flags().BoolVar(&reveal, "reveal", false, "print the real values of variables that came from the vault (recorded as an event)")
	return cmd
}
