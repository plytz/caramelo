package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/env"

	"github.com/plytz/caramelo/internal/cli/ui"
)

func (e *envCmd) handoffCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "handoff ENV",
		Short: "Give an environment to another peer",
		Long: `handoff changes an environment's owner and records it in the feed.

The owner is the peer that created the environment — an identity 'caramelo peer
add' let in — and it is what 'env list --mine' filters on and 'env show' names.
Nothing is refused on its account: an environment somebody else owns can still
be brought up, tested and destroyed, because agents sharing a machine should be
able to help each other.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := e.requireApp(); err != nil {
				return err
			}
			ev, err := e.service().EnvHandoff(cmd.Context(), api.HandoffRequest{
				App: e.app, Name: args[0], To: e.handoffTo,
			})
			if err != nil {
				return fmt.Errorf("env handoff %s: %w", args[0], err)
			}
			return e.a.printer().Result(ev, func(w io.Writer) error {
				return writeHandoff(w, ev)
			})
		},
	}
	cmd.Flags().StringVar(&e.handoffTo, "to", "", "the peer that will own it")
	return available(cmd, onCommander.or(onHub))
}

func writeHandoff(w io.Writer, e *env.Env) error { return handoffView(e).Write(w) }

func handoffView(e *env.Env) *ui.View {
	if e == nil {
		return ui.NewView().Text("nothing changed")
	}
	rows := envRows([]env.Env{*e})
	v := ui.NewView()
	if owner := rows[0].Owner; owner == "" {
		v.Text("%s/%s handed off", e.App, e.Name)
	} else {
		v.Text("%s/%s is now owned by %s", e.App, e.Name, owner)
	}
	return v.Blank().Table(envsTable(rows))
}
