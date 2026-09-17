package sshapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vault"
)

func (d *Daemon) vaultStore() (vault.Store, error) {
	if d.Vault == nil {
		return nil, errors.New("this machine has no vault: nothing can hold a secret " +
			"(run `caramelo hub setup` again to create the vault key)")
	}
	return d.Vault, nil
}

func (d *Daemon) Build(ctx context.Context, req release.BuildRequest, progress io.Writer) (*api.BuildResult, error) {
	if err := d.forwardEnv(ctx, req.App, req.Env); err != nil {
		return nil, err
	}
	m, err := d.envs()
	if err != nil {
		return nil, err
	}
	if m.Builder == nil {
		return nil, errors.New("this machine cannot build releases")
	}

	res, err := m.Build(d.withIdentity(ctx), req, progress)
	if res == nil {
		return nil, err
	}
	return &api.BuildResult{
		App:     req.App,
		Env:     req.Env,
		Release: res.Release,
		Built:   res.Built,
		Images:  res.Images,
	}, err
}

func (d *Daemon) Deploy(ctx context.Context, req env.DeployRequest, progress io.Writer) (*api.DeployResult, error) {
	if err := d.forwardEnv(ctx, req.App, req.Env); err != nil {
		return nil, err
	}
	m, err := d.envs()
	if err != nil {
		return nil, err
	}
	dep, walkErr := m.Deploy(d.withIdentity(ctx), req, progress)
	return d.deployResult(ctx, req.App, req.Env, dep, walkErr)
}

func (d *Daemon) Promote(ctx context.Context, req env.PromoteRequest, progress io.Writer) (*api.DeployResult, error) {
	if err := d.forwardEnv(ctx, req.App, req.Env); err != nil {
		return nil, err
	}
	m, err := d.envs()
	if err != nil {
		return nil, err
	}
	dep, walkErr := m.Promote(d.withIdentity(ctx), req, progress)
	return d.deployResult(ctx, req.App, req.Env, dep, walkErr)
}

func (d *Daemon) Rollback(ctx context.Context, req env.RollbackRequest, progress io.Writer) (*api.DeployResult, error) {
	if err := d.forwardEnv(ctx, req.App, req.Env); err != nil {
		return nil, err
	}
	m, err := d.envs()
	if err != nil {
		return nil, err
	}
	dep, walkErr := m.Rollback(d.withIdentity(ctx), req, progress)
	return d.deployResult(ctx, req.App, req.Env, dep, walkErr)
}

func (d *Daemon) deployResult(ctx context.Context, app, name string, dep *env.Deploy, walkErr error) (*api.DeployResult, error) {
	if dep == nil {
		return nil, walkErr
	}
	out := &api.DeployResult{Deploy: dep}
	if m, err := d.envs(); err == nil {
		if envs, err := m.List(ctx, app); err == nil {
			for i := range envs {
				if envs[i].Name == name {
					out.Env = &envs[i]
					break
				}
			}
		}
	}
	return out, walkErr
}

func (d *Daemon) Releases(ctx context.Context, app, name string, limit int) (*api.ReleasesResult, error) {
	if err := d.forwardEnv(ctx, app, name); err != nil {
		return nil, err
	}
	m, err := d.envs()
	if err != nil {
		return nil, err
	}
	ctx = d.withIdentity(ctx)
	out := &api.ReleasesResult{App: app, Env: name}
	if rels, err := m.AppReleases(ctx, app, limit); err != nil {
		return nil, err
	} else {
		out.Releases = rels
	}
	if name == "" {

		return out, nil
	}
	deploys, err := m.Releases(ctx, app, name, limit)
	if err != nil {
		return nil, err
	}
	out.Deploys = deploys

	for i := range deploys {
		if deploys[i].Status == env.DeployPromoted {
			out.Current = deploys[i].Release
			break
		}
	}
	return out, nil
}

func (d *Daemon) VaultSet(ctx context.Context, req api.VaultSetRequest) (*api.VaultResult, error) {
	store, err := d.vaultStore()
	if err != nil {
		return nil, err
	}
	ctx = d.withIdentity(ctx)

	names := sortedKeys(req.Values)
	refs := make([]vault.Ref, 0, len(names))
	for _, name := range names {
		ref := vault.Ref{Scope: req.Scope, App: req.App, Env: req.Env, Name: name}
		if err := ref.Validate(); err != nil {
			return nil, err
		}
		if err := vault.ValidateValue(name, req.Values[name]); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	changed := make([]string, 0, len(refs))
	for _, ref := range refs {
		if _, err := store.Set(ctx, ref, req.Values[ref.Name]); err != nil {
			return nil, err
		}
		changed = append(changed, ref.Name)
	}

	d.recordVault(ctx, req.App, req.Env, "set", progress.StatusChanged,
		fmt.Sprintf("%d secret(s) set at %s: %s", len(changed),
			vaultScopeText(req.Scope, req.App, req.Env), strings.Join(changed, ", ")))
	res, err := d.vaultResult(ctx, store, req.App, req.Env)
	if err != nil {
		return nil, err
	}
	res.Changed = changed

	if req.Scope != vault.ScopeMachine {
		if m, mErr := d.envs(); mErr == nil {
			res.Warnings = m.SecretWarnings(ctx, req.App, req.Env, changed)
		}
	}
	return res, nil
}

func (d *Daemon) VaultList(ctx context.Context, req api.VaultListRequest) (*api.VaultResult, error) {
	store, err := d.vaultStore()
	if err != nil {
		return nil, err
	}
	return d.vaultLayers(d.withIdentity(ctx), store, req.Scope, req.App, req.Env)
}

func (d *Daemon) VaultRemove(ctx context.Context, req api.VaultRemoveRequest) (*api.VaultResult, error) {
	store, err := d.vaultStore()
	if err != nil {
		return nil, err
	}
	ctx = d.withIdentity(ctx)

	refs := make([]vault.Ref, 0, len(req.Names))
	for _, name := range req.Names {
		ref := vault.Ref{Scope: req.Scope, App: req.App, Env: req.Env, Name: name}
		if err := ref.Validate(); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	changed := make([]string, 0, len(refs))
	for _, ref := range refs {
		if err := store.Remove(ctx, ref); err != nil {
			return nil, err
		}
		changed = append(changed, ref.Name)
	}
	d.recordVault(ctx, req.App, req.Env, "rm", progress.StatusChanged,
		fmt.Sprintf("%d secret(s) removed from %s: %s", len(changed),
			vaultScopeText(req.Scope, req.App, req.Env), strings.Join(changed, ", ")))
	res, err := d.vaultResult(ctx, store, req.App, req.Env)
	if err != nil {
		return nil, err
	}
	res.Changed = changed
	return res, nil
}

func (d *Daemon) vaultResult(ctx context.Context, store vault.Store, app, name string) (*api.VaultResult, error) {
	return d.vaultLayers(ctx, store, "", app, name)
}

func (d *Daemon) vaultLayers(ctx context.Context, store vault.Store, scope vault.Scope,
	app, name string) (*api.VaultResult, error) {
	entries, err := store.List(ctx, app, name)
	if err != nil {
		return nil, err
	}
	if scope != "" {
		kept := entries[:0]
		for _, e := range entries {
			if e.Scope == scope {
				kept = append(kept, e)
			}
		}
		entries = kept
	}

	for i := range entries {
		entries[i] = entries[i].Redact()
	}
	vault.Sort(entries)
	out := &api.VaultResult{App: app, Env: name, Entries: entries}

	if name != "" && scope == "" {

		_, sources, err := d.exportValues(ctx, store, app, name)
		if err != nil {
			return nil, err
		}
		out.Resolved = sources
	}
	return out, nil
}

func (d *Daemon) VaultExport(ctx context.Context, req api.VaultExportRequest) (*api.VaultExportResult, error) {
	store, err := d.vaultStore()
	if err != nil {
		return nil, err
	}
	ctx = d.withIdentity(ctx)

	values, sources, err := d.exportValues(ctx, store, req.App, req.Env)
	if err != nil {
		return nil, err
	}
	if !req.Reveal {
		for k := range values {
			values[k] = vault.Redacted
		}
	}

	text, err := vault.Render(values, req.Format)
	if err != nil {
		return nil, err
	}
	out := &api.VaultExportResult{
		App: req.App, Env: req.Env,
		Values: values, Sources: sources, Revealed: req.Reveal, Text: text,
	}
	if req.Reveal {

		d.recordReveal(ctx, req.App, req.Env, len(values))
	}
	return out, nil
}

func (d *Daemon) exportValues(ctx context.Context, store vault.Store, app, name string) (
	map[string]string, map[string]vault.Scope, error) {
	if m, err := d.envs(); err == nil && name != "" {
		values, sources, verr := m.ExportSecrets(ctx, app, name)
		if verr == nil {
			return values, sources, nil
		}

	}
	values, err := store.Resolve(ctx, app, name)
	if err != nil {
		return nil, nil, err
	}
	sources, err := store.Sources(ctx, app, name)
	if err != nil {
		return nil, nil, err
	}
	return values, sources, nil
}

func (d *Daemon) recordReveal(ctx context.Context, app, name string, count int) {
	d.recordVault(ctx, app, name, "export", progress.StatusWarning,
		fmt.Sprintf("revealed %d secret(s)", count))
}

func (d *Daemon) recordVault(ctx context.Context, app, name, step, status, detail string) {
	if d.Store == nil {
		return
	}
	envID := int64(0)
	if name != "" {
		if rec, err := d.Store.Env(ctx, app, name); err == nil && rec != nil {
			envID = rec.ID
		}
	}
	identity := ""
	if sess, ok := SessionFrom(ctx); ok {
		identity = sess.Identity
	}

	_ = d.Store.AddEvent(ctx, state.EventFromProgress(envID, progress.Event{
		App: app, Env: name,
		Action: "secrets", Step: step, Status: status,
		Detail:   detail,
		Identity: identity,
		At:       time.Now().UTC(),
	}))
}

func vaultScopeText(scope vault.Scope, app, name string) string {
	switch scope {
	case vault.ScopeMachine:
		return "machine scope"
	case vault.ScopeApp:
		return "app " + app
	default:
		if name == "" {
			return string(scope)
		}
		return "env " + name
	}
}

func sortedScopeKeys(m map[string]vault.Scope) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (d *Daemon) Events(ctx context.Context, req env.EventsRequest, out io.Writer, follow bool) error {
	if req.Env != "" {
		if err := d.forwardEnv(ctx, req.App, req.Env); err != nil {
			return err
		}
	}
	m, err := d.envs()
	if err != nil {
		return err
	}
	req.Follow = follow
	return m.Events(d.withIdentity(ctx), req, out)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var _ api.ProductionService = (*Daemon)(nil)

func (d *Daemon) envProduction(ctx context.Context, detail *api.EnvDetail) {
	if detail == nil || d.Store == nil {
		return
	}
	rec, err := d.Store.Env(ctx, detail.Env.App, detail.Env.Name)
	if err != nil || rec == nil {
		return
	}
	if rec.ReleaseID != 0 {
		if rel, err := d.Store.Release(ctx, rec.ReleaseID); err == nil && rel != nil {
			detail.Release = releaseFromRow(*rel)
		}
	}
	if rec.DeployID == 0 {
		return
	}
	dep, err := d.Store.Deploy(ctx, rec.DeployID)
	if err != nil || dep == nil || dep.Done() {

		return
	}
	detail.Deploy = &env.Deploy{
		ID:         dep.ID,
		App:        detail.Env.App,
		Env:        detail.Env.Name,
		Kind:       env.DeployKind(dep.Kind),
		Status:     env.DeployStatus(dep.Status),
		Reason:     dep.Reason,
		Identity:   dep.Identity,
		StartedAt:  dep.StartedAt,
		FinishedAt: dep.FinishedAt,
		Error:      dep.Error,
	}
	if dep.ReleaseID != 0 {
		if rel, err := d.Store.Release(ctx, dep.ReleaseID); err == nil && rel != nil {
			detail.Deploy.Release = releaseFromRow(*rel)
		}
	}
	if dep.FromReleaseID != 0 {
		if rel, err := d.Store.Release(ctx, dep.FromReleaseID); err == nil && rel != nil {
			detail.Deploy.FromRelease = releaseFromRow(*rel)
		}
	}
}

func releaseFromRow(r state.Release) *release.Release {
	out := &release.Release{
		ID:      r.ID,
		App:     r.App,
		Commit:  r.Commit,
		Tree:    r.Tree,
		Ref:     r.Ref,
		BuiltBy: r.BuiltBy,
		BuiltAt: r.BuiltAt,
	}
	if r.ImagesJSON != "" {
		images := map[string]string{}
		if err := json.Unmarshal([]byte(r.ImagesJSON), &images); err == nil {
			out.Images = images
		}
	}
	return out
}

func (d *Daemon) productionStatus(ctx context.Context) *api.ProductionStatus {
	if d.Store == nil {
		return nil
	}
	out := &api.ProductionStatus{Vault: d.Vault != nil}
	if envs, err := d.Store.Envs(ctx, ""); err == nil {
		out.Envs = len(envs)
		for _, e := range envs {
			if e.Mode == state.EnvModeRelease {
				out.Release++
			}
		}
	}
	if deploys, err := d.Store.UnfinishedDeploys(ctx); err == nil {
		out.Deploying = len(deploys)
	}
	if d.Vault != nil {
		if entries, err := d.Vault.List(ctx, "", ""); err == nil {
			out.Secrets = len(entries)
		}
	}
	if d.Feed != nil {
		out.Watching, out.FeedDropped = d.Feed.Subscribers(), d.Feed.Dropped()
	}

	for _, r := range d.edgeRouteTable(ctx) {
		for _, t := range r.Targets {
			switch t.State {
			case edge.TargetUnhealthy:
				out.Unhealthy++
			case edge.TargetHeld:
				out.Held++
			}
		}
	}
	return out
}

func (d *Daemon) edgeRouteTable(ctx context.Context) []edge.Route {
	client, err := d.edgeControl()
	if err != nil || client == nil {
		return nil
	}
	st, err := client.Status(ctx)
	if err != nil || st == nil {
		return nil
	}
	return st.Routes
}
