package vault

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrHub = errors.New("vault: the fleet's secrets live on the hub")

type BundleRequest struct {
	App string `json:"app"`
	Env string `json:"env"`

	Machine string `json:"machine,omitempty"`

	Reason string `json:"reason,omitempty"`
}

type Fetcher interface {
	Bundle(ctx context.Context, req BundleRequest) (*Bundle, error)
}

type Fetch struct {
	App string `json:"app"`
	Env string `json:"env"`

	Machine string `json:"machine"`

	Reason string `json:"reason,omitempty"`

	Names       []string  `json:"names,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	At          time.Time `json:"at"`
}

type AuditFunc func(ctx context.Context, f Fetch)

type Hub struct {
	Store Store

	Machine string

	Asker IdentityFunc

	Audit AuditFunc

	Now func() time.Time
}

var _ Fetcher = (*Hub)(nil)

func (h *Hub) Bundle(ctx context.Context, req BundleRequest) (*Bundle, error) {
	if h.Store == nil {
		return nil, fmt.Errorf("serve the secrets of env %q of app %q: this machine has no vault: %w",
			req.Env, req.App, ErrNoKey)
	}
	app, env := strings.TrimSpace(req.App), strings.TrimSpace(req.Env)
	if app == "" || env == "" {
		return nil, errors.New("serve a secrets bundle: an app and an environment are required")
	}
	member := strings.TrimSpace(req.Machine)
	if member == "" && h.Asker != nil {
		member = strings.TrimSpace(h.Asker(ctx))
	}
	if member == "" {

		return nil, fmt.Errorf("serve the secrets of env %q of app %q: "+
			"nothing said which machine is asking, and a bundle is only ever answered to a named one", env, app)
	}
	values, err := h.Store.Resolve(ctx, app, env)
	if err != nil {
		return nil, fmt.Errorf("resolve the secrets of env %q of app %q for machine %q: %w", env, app, member, err)
	}
	if values == nil {
		values = map[string]string{}
	}
	sources, err := h.Store.Sources(ctx, app, env)
	if err != nil {
		return nil, fmt.Errorf("resolve the secrets of env %q of app %q for machine %q: %w", env, app, member, err)
	}
	digest, err := h.Store.Fingerprint(ctx, app, env)
	if err != nil {
		return nil, fmt.Errorf("fingerprint the secrets of env %q of app %q for machine %q: %w", env, app, member, err)
	}
	b := &Bundle{
		App: app, Env: env, Values: values, Sources: sources,
		Fingerprint: digest, Machine: h.Machine, FetchedAt: h.now(),
	}
	h.record(ctx, Fetch{
		App: app, Env: env, Machine: member, Reason: strings.TrimSpace(req.Reason),
		Names: b.Names(), Fingerprint: digest, At: b.FetchedAt,
	})
	return b, nil
}

func (h *Hub) record(ctx context.Context, f Fetch) {
	if h.Audit == nil {
		return
	}
	h.Audit(ctx, f)
}

func (h *Hub) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

type Remote struct {
	Hub string

	Fetch Fetcher

	Machine string

	Reason string
}

var _ Store = (*Remote)(nil)

func (r *Remote) Set(_ context.Context, ref Ref, _ string) (*Entry, error) {
	return nil, r.refuse("set the %s secret %s", ref.Where(), ref.Name)
}

func (r *Remote) Get(_ context.Context, ref Ref) (*Entry, error) {
	return nil, r.refuse("read the %s secret %s", ref.Where(), ref.Name)
}

func (r *Remote) List(_ context.Context, app, env string) ([]Entry, error) {
	return nil, r.refuse("list the secrets of %s", envOrApp(app, env))
}

func (r *Remote) Remove(_ context.Context, ref Ref) error {
	return r.refuse("remove the %s secret %s", ref.Where(), ref.Name)
}

func (r *Remote) Resolve(ctx context.Context, app, env string) (map[string]string, error) {
	b, err := r.bundle(ctx, app, env, "resolve")
	if err != nil {
		return nil, err
	}
	return b.Values, nil
}

func (r *Remote) Sources(ctx context.Context, app, env string) (map[string]Scope, error) {
	b, err := r.bundle(ctx, app, env, "sources")
	if err != nil {
		return nil, err
	}
	return b.Sources, nil
}

func (r *Remote) Fingerprint(ctx context.Context, app, env string) (string, error) {
	b, err := r.bundle(ctx, app, env, "fingerprint")
	if err != nil {
		return "", err
	}
	return b.Fingerprint, nil
}

func (r *Remote) bundle(ctx context.Context, app, env, reason string) (*Bundle, error) {
	if r.Fetch == nil {
		return nil, fmt.Errorf("read the secrets of env %q of app %q: "+
			"this machine is a member of %s and cannot reach it: %w", env, app, r.hub(), ErrHub)
	}
	if why := strings.TrimSpace(r.Reason); why != "" {
		reason = why
	}
	if why := ReasonFrom(ctx); why != "" {
		reason = why
	}
	b, err := r.Fetch.Bundle(ctx, BundleRequest{App: app, Env: env, Machine: r.Machine, Reason: reason})
	if err != nil {
		return nil, fmt.Errorf("read the secrets of env %q of app %q from %s: %w", env, app, r.hub(), err)
	}
	if b == nil {
		return nil, fmt.Errorf("read the secrets of env %q of app %q from %s: it answered with nothing",
			env, app, r.hub())
	}
	if b.Values == nil {
		b.Values = map[string]string{}
	}
	return b, nil
}

func (r *Remote) refuse(what string, args ...any) error {
	return fmt.Errorf("%s: this machine is a member of %s, and the fleet's secrets live there — "+
		"run it against the hub: %w", fmt.Sprintf(what, args...), r.hub(), ErrHub)
}

func (r *Remote) hub() string {
	if name := strings.TrimSpace(r.Hub); name != "" {
		return "the fleet hub " + name
	}
	return "a fleet hub"
}

func envOrApp(app, env string) string {
	switch {
	case env != "":
		return fmt.Sprintf("env %q of app %q", env, app)
	case app != "":
		return fmt.Sprintf("app %q", app)
	default:
		return "this machine"
	}
}

type reasonKey struct{}

func WithReason(ctx context.Context, reason string) context.Context {
	if strings.TrimSpace(reason) == "" {
		return ctx
	}
	return context.WithValue(ctx, reasonKey{}, strings.TrimSpace(reason))
}

func ReasonFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(reasonKey{}).(string)
	return s
}
