package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/cli/ui"
	"github.com/plytz/caramelo/internal/progress"
)

func (a *app) forwardRendered(ctx context.Context, cmd *cobra.Command) (int, error) {
	path := normalizeCommandPath(cmd.CommandPath())
	render := rendererFor(a, path)
	kind := viewFor(path)
	if a.json || a.progressFlag != "" || (render == nil && kind == viewNone) {
		return forward(ctx, a)
	}
	if kind == viewFeed {
		return a.forwardFeed(ctx, path)
	}

	out, err := a.stdout, a.stderr
	old := &olderDaemon{Progress: a.newProgress(path)}
	sink := ui.SinkFor(old)

	a.args = inject(a.args, "--progress", string(progress.FormatJSON))
	var result bytes.Buffer
	if render != nil {
		a.args = inject(a.args, "--json")
		a.stdout = &result
	}
	a.stderr = sink
	code, ferr := forward(ctx, a)
	a.stdout, a.stderr = out, err

	_ = sink.Close()
	_ = old.Close(ferr, "")

	if old.saw() {
		return ExitError, errDaemonIsOlder(a)
	}
	if render == nil {
		return code, ferr
	}
	if rerr := writeResult(path, render, out, result.Bytes(), code, ferr); rerr != nil {
		return ExitError, rerr
	}
	return code, ferr
}

func (a *app) forwardFeed(ctx context.Context, path string) (int, error) {
	out, errw := a.stdout, a.stderr
	view := a.newProgress(path)
	events := ui.SinkFor(view)

	old := &olderDaemon{Progress: ui.NewPlain(errw)}
	lines := ui.SinkFor(old)

	a.args = inject(a.args, "--progress", string(progress.FormatJSON))
	a.args = inject(a.args, "--json")
	a.stdout, a.stderr = events, lines
	code, ferr := forward(ctx, a)
	a.stdout, a.stderr = out, errw

	_ = events.Close()
	_ = lines.Close()
	_ = view.Close(ferr, "")
	if old.saw() {
		return ExitError, errDaemonIsOlder(a)
	}
	return code, ferr
}

type olderDaemon struct {
	ui.Progress
	mu    sync.Mutex
	older bool
}

const unknownProgressFlag = "unknown flag: --progress"

func (o *olderDaemon) Line(s string) {
	if strings.Contains(s, unknownProgressFlag) {
		o.mu.Lock()
		o.older = true
		o.mu.Unlock()
	}
	o.Progress.Line(s)
}

func (o *olderDaemon) saw() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.older
}

func errDaemonIsOlder(a *app) error {
	target := "the machine"
	if a.machine != "" {
		target = a.machine
	}
	return fmt.Errorf("%s runs a caramelod older than this commander: it does not understand --progress, "+
		"which every command is forwarded with since M7. Upgrade it with "+
		"`caramelo fleet setup --target <user@host>` (or run the same version on both sides)", target)
}

func writeResult(path string, render Renderer, w io.Writer, raw []byte, code int, ferr error) error {
	body := bytes.TrimSpace(raw)
	if len(body) == 0 {
		return nil
	}
	if !json.Valid(body) {
		_, err := w.Write(raw)
		return err
	}
	if code != ExitOK || ferr != nil {
		return nil
	}
	if err := render(w, json.RawMessage(body)); err != nil {
		return fmt.Errorf("render %s: %w", path, err)
	}
	return nil
}

func (a *app) newProgress(path string) ui.Progress {
	if viewFor(path) == viewFeed {
		return ui.NewPlainFeed(a.stdout)
	}
	return ui.NewPlain(a.stderr)
}
