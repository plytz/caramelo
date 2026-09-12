package cli

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/env"

	"github.com/plytz/caramelo/internal/cli/ui"
)

func init() {
	register(func(a *app) *cobra.Command {
		t := &envTargets{a: a}
		var (
			services []string
			force    bool
		)
		cmd := &cobra.Command{
			Use:   "down [ENV]",
			Short: "Stop an environment's services, keeping its dependencies and data",
			Long: `down stops and removes the app's service containers. The environment's
dependencies keep running, its network, cache volume and every byte of data
stay, and 'caramelo up' brings the services back where they were.

An environment whose services are already down is a success, so down is safe
to run twice.

A protected environment (env create --production) needs --force: down is the one
verb that takes a production environment off the air without removing anything.`,
			Args: maxArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				name, _, err := t.target(cmd, args)
				if err != nil {
					return err
				}
				res, err := t.service().Down(cmd.Context(), env.DownRequest{
					App:      t.app,
					Name:     name,
					Services: services,
					Force:    force,
				}, t.a.progressWriter())
				if err != nil {
					return err
				}
				return t.a.printer().Result(res, func(w io.Writer) error {
					return writeDownResult(w, name, res)
				})
			},
		}
		t.addTargetFlags(cmd)
		cmd.Flags().StringSliceVar(&services, "service", nil,
			"only these services (default: all of them)")
		cmd.Flags().BoolVar(&force, "force", false,
			"stop a protected environment (env create --production makes one)")
		return cmd
	})
}

func writeDownResult(w io.Writer, name string, r *api.DownResult) error {
	return downView(name, r).Write(w)
}

func downView(name string, r *api.DownResult) *ui.View {
	if r == nil || len(r.Services) == 0 {
		return ui.NewView().Text("%s: no services were running", name)
	}
	return ui.NewView().Table(servicesTable(r.Services))
}
