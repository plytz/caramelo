package cli

import (
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/cli/ui"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/progress"
)

func (a *app) eventsCmd() *cobra.Command {

	t := &envTargets{a: a, machineWide: func() bool { return true }}
	var (
		follow bool
		since  time.Duration
		limit  int
	)
	cmd := &cobra.Command{
		Use:   "events [ENV]",
		Short: "Everything the machine does, whoever started it",
		Long: `events is the machine's feed: every environment created, service rolled,
deploy walked, replica restarted and secret revealed, with who asked for it and
when. With an ENV it is that environment's alone.

It is one shape — the same record a command's own progress carries and the same
one env show lists — so --json prints one object per line and an agent can
follow a deploy it did not start.`,
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _, err := t.targetOrMachine(cmd, args)
			if err != nil {
				return err
			}
			req := env.EventsRequest{App: t.app, Env: name, Limit: limit, Follow: follow}
			if name == "" {

				req.App = ""
			}
			if since > 0 {

				req.Since = time.Now().Add(-since)
			}
			out, done := eventsWriter(t.a)
			err = t.service().Events(cmd.Context(), req, out, follow)
			if ferr := done(); err == nil {
				err = ferr
			}
			return err
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep the stream open and print events as they happen")
	cmd.Flags().DurationVar(&since, "since", 0, "only events this recent (default: the last 100)")
	cmd.Flags().IntVar(&limit, "limit", 0, "at most this many events before following (default 100)")
	t.addTargetFlags(cmd)
	return cmd
}

func eventsWriter(a *app) (io.Writer, func() error) {
	if a.json {
		w := progress.New(a.stdout, progress.FormatJSON)
		return w, func() error { return nil }
	}
	sink := ui.SinkFor(ui.NewPlainFeed(a.stdout))
	return progress.New(sink, progress.FormatJSON), sink.Close
}
