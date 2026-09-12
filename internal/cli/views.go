package cli

import (
	"sort"
	"sync"
)

type viewKind int

const (
	viewNone viewKind = iota

	viewFeed
)

var views struct {
	mu sync.RWMutex
	m  map[string]viewKind
}

func registerView(commandPath string, kind viewKind) {
	path := normalizeCommandPath(commandPath)
	if path == "" {
		panic("cli: registerView with no command path")
	}
	if kind == viewNone {
		panic("cli: registerView(" + path + ") with no view")
	}
	views.mu.Lock()
	defer views.mu.Unlock()
	if views.m == nil {
		views.m = map[string]viewKind{}
	}
	if _, dup := views.m[path]; dup {
		panic("cli: two views for " + path)
	}
	views.m[path] = kind
}

func viewFor(commandPath string) viewKind {
	path := normalizeCommandPath(commandPath)
	views.mu.RLock()
	defer views.mu.RUnlock()
	return views.m[path]
}

func registeredViews() []string {
	views.mu.RLock()
	defer views.mu.RUnlock()
	out := make([]string, 0, len(views.m))
	for k := range views.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func init() {
	registerView("events", viewFeed)
}
