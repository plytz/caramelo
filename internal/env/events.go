package env

import (
	"context"
	"fmt"
	"io"

	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/state"
)

const EventBuffer = progress.DefaultBuffer

func (m *Manager) Events(ctx context.Context, req EventsRequest, out io.Writer) error {
	filter, err := m.eventFilter(ctx, req)
	if err != nil {
		return err
	}

	var (
		live <-chan progress.Event
		stop = func() {}
	)
	if req.Follow {
		live, stop = m.notifier().Subscribe(EventBuffer)
		defer stop()
	}

	rows, err := m.Store.QueryEvents(ctx, filter)
	if err != nil {
		return fmt.Errorf("read the event feed: %w", err)
	}

	var seen int64
	for i := len(rows) - 1; i >= 0; i-- {
		e := rows[i].Progress()
		if e.Seq > seen {
			seen = e.Seq
		}
		if err := progress.Emit(out, e); err != nil {
			return fmt.Errorf("write the event feed: %w", err)
		}
	}
	if !req.Follow {
		return nil
	}

	for {
		select {
		case <-ctx.Done():

			return nil
		case e, ok := <-live:
			if !ok {

				return nil
			}
			if e.Seq != 0 && e.Seq <= seen {
				continue
			}
			if e.Seq > seen {
				seen = e.Seq
			}
			if !eventMatches(req, e) {
				continue
			}
			if err := progress.Emit(out, e); err != nil {
				return fmt.Errorf("write the event feed: %w", err)
			}
		}
	}
}

func (m *Manager) eventFilter(ctx context.Context, req EventsRequest) (state.EventFilter, error) {
	f := state.EventFilter{Since: req.Since, Limit: req.Limit}
	if f.Limit <= 0 {
		f.Limit = DefaultEventLimit
	}
	if req.Env == "" {

		if req.App != "" {
			if err := ValidateName("app", req.App); err != nil {
				return f, err
			}
			f.App = req.App
		}
		return f, nil
	}
	if err := ValidateName("app", req.App); err != nil {
		return f, err
	}
	if err := ValidateName("env", req.Env); err != nil {
		return f, err
	}
	rec, err := m.env(ctx, req.App, req.Env)
	if err != nil {
		return f, err
	}
	f.EnvID = rec.ID
	return f, nil
}

func eventMatches(req EventsRequest, e progress.Event) bool {
	if !req.Since.IsZero() && e.At.Before(req.Since) {
		return false
	}
	switch {
	case req.Env != "":
		return e.App == req.App && e.Env == req.Env
	case req.App != "":
		return e.App == req.App
	default:
		return true
	}
}

func (m *Manager) notifier() progress.Notifier {
	if m == nil || m.Notifier == nil {
		return progress.Nop{}
	}
	return m.Notifier
}
