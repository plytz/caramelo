package config

import "fmt"

type View string

const (
	ViewHost View = "host"

	ViewNetwork View = "network"
)

func ParseView(s string) (View, error) {
	switch View(s) {
	case "":
		return ViewHost, nil
	case ViewHost:
		return ViewHost, nil
	case ViewNetwork:
		return ViewNetwork, nil
	}
	return "", fmt.Errorf("unknown view %q: want %s or %s", s, ViewHost, ViewNetwork)
}

type Views struct {
	Host    ExpandContext `json:"host"`
	Network ExpandContext `json:"network"`
}

func (v Views) ContextWith(view View, s Secrets) ExpandContext {
	return v.Context(view).WithSecrets(s)
}

func (v Views) Context(view View) ExpandContext {
	ctx := v.Host
	if view == ViewNetwork {
		ctx = v.Network
	}
	if ctx.View == "" {
		ctx.View = view
	}
	return ctx
}

func (a *App) Resolve(view View, v Views) (*App, error) {
	return a.Expanded(v.Context(view))
}

func (a *App) ResolveWith(view View, v Views, s Secrets) (*App, error) {
	return a.Expanded(v.ContextWith(view, s))
}
