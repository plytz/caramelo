package env

import (
	"context"
	"io"

	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/state"
)

func (m *Manager) feed(ctx context.Context, rec *state.EnvRecord, w io.Writer) io.Writer {
	if rec == nil || rec.ID == 0 || m == nil || m.Store == nil {
		return w
	}
	if f, ok := w.(*feedWriter); ok && f.envID == rec.ID {

		return f
	}
	return &feedWriter{m: m, ctx: ctx, envID: rec.ID, app: rec.App, env: rec.Name, w: w}
}

type feedWriter struct {
	m     *Manager
	ctx   context.Context
	envID int64
	app   string
	env   string
	w     io.Writer
}

func (f *feedWriter) Emit(e progress.Event) error {
	if f == nil {
		return nil
	}
	if e.App == "" {
		e.App = f.app
	}
	if e.Env == "" {
		e.Env = f.env
	}
	if e.At.IsZero() {
		e.At = f.m.now()
	}
	if e.Identity == "" {
		e.Identity = IdentityFrom(f.ctx)
	}
	f.m.record(context.WithoutCancel(f.ctx), f.envID, e)
	return progress.Emit(f.w, e)
}

func (f *feedWriter) Write(b []byte) (int, error) {
	if f == nil || f.w == nil {
		return len(b), nil
	}
	return f.w.Write(b)
}

var _ progress.Emitter = (*feedWriter)(nil)
