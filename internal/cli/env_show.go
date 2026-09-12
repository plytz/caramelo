package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/env"

	"github.com/plytz/caramelo/internal/cli/ui"
)

func (e *envCmd) showCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show NAME",
		Short: "Show one environment and what its dependencies are doing",
		Long: `show prints the environment's record together with the live state of its
dependency containers, read from the container runtime rather than from the
store, and the tail of its audit trail.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := e.requireApp(); err != nil {
				return err
			}
			detail, err := e.service().Env(cmd.Context(), e.app, args[0])
			if err != nil {
				return err
			}
			return e.a.printer().Result(detail, func(w io.Writer) error {
				return writeEnvDetail(w, detail)
			})
		},
	}
}

func writeEnvDetail(w io.Writer, d *api.EnvDetail) error { return envDetailView(d).Write(w) }

func envDetailView(d *api.EnvDetail) *ui.View {
	if d == nil {
		return ui.NewView().Text("no environment")
	}
	e := d.Env
	v := envRecordView(&e, d.Deps)

	if d.Unreachable {
		v.Text("%s is unreachable: this is what the hub last knew", strOrDash(d.Machine))
		return v
	}

	v.Section(servicesTable(d.Services)).Section(replicasTable(d.Services))

	v.Section(routesTable(d.Routes)).Section(certificatesTable(d.Certificates))

	envRelease(v, d)
	if d.Network != "" {
		v.Blank().Text("network %s", d.Network)
	}
	return v.Section(eventsTable(d.Events))
}

func eventsTable(events []env.Event) *ui.Table {
	if len(events) == 0 {
		return nil
	}
	t := ui.NewTable("WHEN", "ACTION", "STATUS", "BY", "DETAIL")
	for _, ev := range events {
		when := "-"
		if !ev.At.IsZero() {
			when = ev.At.Local().Format(time.RFC3339)
		}
		t.Row(when, ev.Action, ev.Status, strOrDash(ev.Identity), ev.Detail)
	}
	return t
}

func envRelease(v *ui.View, d *api.EnvDetail) {
	release := d.Release
	if release == nil && d.Deploy == nil {
		if d.Env.Mode != env.ModeRelease {
			return
		}
		v.Blank().Text("no release yet: caramelo deploy %s builds one and walks it in", d.Env.Name)
		return
	}
	f := ui.NewFields("")
	if release != nil {
		line := fmt.Sprintf("%s  (commit %s", release.Short(), shortCommit(release.Commit))
		if release.Ref != "" {
			line += ", ref " + release.Ref
		}
		f.Add("release", "%s)", line)
		if release.BuiltBy != "" || !release.BuiltAt.IsZero() {
			built := strOrDash(release.BuiltBy)
			if !release.BuiltAt.IsZero() {
				built += "  " + release.BuiltAt.Local().Format(time.RFC3339)
			}
			f.Add("built", "%s", built)
		}
	}
	if dep := d.Deploy; dep != nil {

		line := fmt.Sprintf("%s %s", dep.Kind, dep.Status)
		if dep.Release != nil {
			line += "  release " + dep.Release.Short()
		}
		if dep.Identity != "" {
			line += "  by " + dep.Identity
		}
		if !dep.StartedAt.IsZero() {
			line += fmt.Sprintf("  (%s ago)", time.Since(dep.StartedAt).Round(time.Second))
		}
		f.Add("deploying", "%s", line)
		if dep.Status == env.DeployWatching {
			f.Add("", "caramelo promote %s ends the watch, caramelo rollback %s takes it back",
				d.Env.Name, d.Env.Name)
		}
	}
	v.Blank().Fields(f)
}
