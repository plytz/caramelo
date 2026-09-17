package cli

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/env"

	"github.com/plytz/caramelo/internal/cli/ui"
)

func (e *envCmd) listCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the environments of an app",
		Long: `list shows the environments of the app you are standing in, or of --app.
With --all, or outside any checkout, it shows every environment on the
machine — and, on a hub, every environment of the fleet, with the machine that
holds it and the peer that owns it.

--mine narrows it to the environments this peer owns, which is how an agent
sharing a machine finds its own work again.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := e.app
			if e.all || e.mine {

				app = ""
			}
			envs, err := e.service().Envs(cmd.Context(), app)
			if err != nil {
				return err
			}
			if e.all {

				if envs, err = e.withFleet(cmd.Context(), envs); err != nil {
					return err
				}
			}
			if e.mine {
				if envs, err = e.onlyMine(envs); err != nil {
					return err
				}
			}
			if envs == nil {
				envs = []env.Env{}
			}
			return e.a.printer().Result(envs, func(w io.Writer) error {
				return writeEnvs(w, envs)
			})
		},
	}
	cmd.Flags().BoolVar(&e.all, "all", false, "every app's environments, not only this one's")
	cmd.Flags().BoolVar(&e.mine, "mine", false, "only the environments this peer owns")
	return cmd
}

func (e *envCmd) onlyMine(envs []env.Env) ([]env.Env, error) {
	me := strings.TrimSpace(e.a.session.Identity)
	if me == "" {
		return nil, &usageError{errors.New(
			"--mine needs an identity, and this command arrived over the local socket where there is none: " +
				"run it from a computer with a peer key, or use `env list --all`")}
	}
	mine := make([]env.Env, 0, len(envs))
	for _, ev := range envs {
		if owner, _, _ := envFleetFields(ev); owner == me {
			mine = append(mine, ev)
		}
	}
	return mine, nil
}

func envFleetFields(e env.Env) (owner, machine, via string) {
	v := reflect.ValueOf(e)
	get := func(name string) string {
		f := v.FieldByName(name)
		if !f.IsValid() || f.Kind() != reflect.String {
			return ""
		}
		return f.String()
	}
	return get("Owner"), get("Machine"), get("Via")
}

func listMode(e env.Env) string {
	mode := string(e.Mode)
	if mode == "" {
		mode = string(env.ModeDev)
	}
	if e.Protected {
		mode += "*"
	}
	return mode
}

func writeEnvs(w io.Writer, envs []env.Env) error { return envsView(envs).Write(w) }

func envsView(envs []env.Env) *ui.View {
	if len(envs) == 0 {
		return ui.NewView().Text("no environments")
	}
	return ui.NewView().Table(envsTable(envRows(envs)))
}

type envRow struct {
	env.Env
	Owner   string
	Machine string
	Via     string
}

func envRows(envs []env.Env) []envRow {
	rows := make([]envRow, 0, len(envs))
	for _, e := range envs {
		owner, machine, via := envFleetFields(e)
		rows = append(rows, envRow{Env: e, Owner: owner, Machine: machine, Via: via})
	}
	return rows
}

func envsTable(rows []envRow) *ui.Table {
	head := []string{"NAME", "APP", "MODE", "COMMIT", "STATUS", "PORTS", "DEPS", "CREATED", "BY"}
	hasSource := false
	for _, r := range rows {
		if r.Env.SourceBranch != "" {
			hasSource = true
			break
		}
	}
	if hasSource {
		head = insertAt(head, 4, "SOURCE")
	}
	hasFleet := false
	for _, r := range rows {
		if r.Owner != "" || r.Machine != "" {
			hasFleet = true
			break
		}
	}
	if hasFleet {
		head = append(head, "MACHINE", "OWNER")
	}
	t := ui.NewTable(head...)
	for _, r := range rows {
		e := r.Env
		created := "-"
		if !e.CreatedAt.IsZero() {
			created = e.CreatedAt.Local().Format(time.RFC3339)
		}
		deps := "-"
		if names := depNames(e); len(names) > 0 {
			deps = strings.Join(names, ",")
		}
		cells := []string{e.Name, e.App, listMode(e), shortCommit(e.Commit),
			string(e.Status), portRange(e), deps, created, strOrDash(e.CreatedBy)}
		if hasSource {
			cells = insertAt(cells, 4, strOrDash(e.SourceBranch))
		}
		if hasFleet {
			cells = append(cells, strOrDash(r.Machine), strOrDash(r.Owner))
		}
		t.Row(cells...)
	}
	return t
}

func insertAt(row []string, at int, cell string) []string {
	out := make([]string, 0, len(row)+1)
	out = append(out, row[:at]...)
	out = append(out, cell)
	return append(out, row[at:]...)
}

func (e *envCmd) withFleet(ctx context.Context, local []env.Env) ([]env.Env, error) {
	entries, err := e.service().EnvsAll(ctx, "")
	switch {
	case errors.Is(err, api.ErrNotImplemented):
		return local, nil
	case err != nil:
		return nil, err
	}
	have := make(map[string]bool, len(local))
	for _, l := range local {
		have[l.App+"/"+l.Name] = true
	}
	for _, d := range entries {
		if have[d.App+"/"+d.Env] {
			continue
		}
		local = append(local, env.Env{
			App: d.App, Name: d.Env, Branch: d.Env, VPNIP: d.Address,
			Mode: env.Mode(d.Mode), Machine: d.Machine, Owner: d.Owner,
			Via: config.Via(d.Via), UpdatedAt: d.UpdatedAt,
		})
	}
	sortEnvs(local)
	return local, nil
}

func sortEnvs(list []env.Env) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].App != list[j].App {
			return list[i].App < list[j].App
		}
		return list[i].Name < list[j].Name
	})
}
