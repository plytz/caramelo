package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/plytz/caramelo/internal/cli/ui"
)

type Renderer func(w io.Writer, result json.RawMessage) error

func registerRender(commandPath string, fn Renderer) {
	if fn == nil {
		panic("cli: registerRender(" + normalizeCommandPath(commandPath) + ") with no renderer")
	}
	registerRenderer(commandPath, func(*app) Renderer { return fn })
}

func renderFor(commandPath string) Renderer { return rendererFor(nil, commandPath) }

func render(commandPath string, w io.Writer, result json.RawMessage) (bool, error) {
	return renderWith(nil, commandPath, w, result)
}

func registeredRenderers() []string {
	renderers.mu.RLock()
	defer renderers.mu.RUnlock()
	out := make([]string, 0, len(renderers.m))
	for k := range renderers.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func normalizeCommandPath(path string) string {
	fields := strings.Fields(path)
	if len(fields) > 0 && fields[0] == "caramelo" {
		fields = fields[1:]
	}
	return strings.Join(fields, " ")
}

type renderInfo struct {
	app string
	env string

	args []string
}

func (r renderInfo) arg(n int) string {
	if n < 0 || n >= len(r.args) {
		return ""
	}
	return r.args[n]
}

var renderers struct {
	mu sync.RWMutex
	m  map[string]func(*app) Renderer
}

func registerRenderer(commandPath string, make func(a *app) Renderer) {
	path := normalizeCommandPath(commandPath)
	if path == "" {
		panic("cli: registerRender with no command path")
	}
	if make == nil {
		panic("cli: registerRender(" + path + ") with no renderer")
	}
	renderers.mu.Lock()
	defer renderers.mu.Unlock()
	if renderers.m == nil {
		renderers.m = map[string]func(*app) Renderer{}
	}
	if _, dup := renderers.m[path]; dup {
		panic("cli: two renderers for " + path)
	}
	renderers.m[path] = make
}

func rendererFor(a *app, commandPath string) Renderer {
	path := normalizeCommandPath(commandPath)
	renderers.mu.RLock()
	make := renderers.m[path]
	renderers.mu.RUnlock()
	if make == nil {
		return nil
	}
	return make(a)
}

func renderWith(a *app, commandPath string, w io.Writer, result json.RawMessage) (bool, error) {
	fn := rendererFor(a, commandPath)
	if fn == nil {
		return false, nil
	}
	if err := fn(w, result); err != nil {
		return true, fmt.Errorf("render %s: %w", normalizeCommandPath(commandPath), err)
	}
	return true, nil
}

func viewRenderer[T any](build func(T) *ui.View) Renderer {
	return func(w io.Writer, result json.RawMessage) error {
		var v T
		if err := json.Unmarshal(result, &v); err != nil {
			return fmt.Errorf("decode the result: %w", err)
		}
		return build(v).Write(w)
	}
}
