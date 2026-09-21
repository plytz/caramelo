package sshapi

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vault"
)

func TestProductionCommandsSayWhatTheMachineIsMissing(t *testing.T) {
	d, _ := newTestDaemon(t)
	if d.EnvManager != nil {
		t.Fatal("the test daemon was built with an env manager")
	}
	if d.Vault != nil {
		t.Fatal("the test daemon was built with a vault")
	}
	ctx := context.Background()

	forEnvs := map[string]func() error{
		"Build": func() error {
			_, err := d.Build(ctx, release.BuildRequest{App: "shop", Env: "production"}, io.Discard)
			return err
		},
		"Deploy": func() error {
			_, err := d.Deploy(ctx, env.DeployRequest{App: "shop", Env: "production"}, io.Discard)
			return err
		},
		"Promote": func() error {
			_, err := d.Promote(ctx, env.PromoteRequest{App: "shop", Env: "production"}, io.Discard)
			return err
		},
		"Rollback": func() error {
			_, err := d.Rollback(ctx, env.RollbackRequest{App: "shop", Env: "production"}, io.Discard)
			return err
		},
		"Releases": func() error { _, err := d.Releases(ctx, "shop", "production", 0); return err },
		"Events": func() error {
			return d.Events(ctx, env.EventsRequest{}, io.Discard, false)
		},
	}
	for name, call := range forEnvs {
		err := call()
		if err == nil {
			t.Errorf("%s on a daemon with no environments succeeded", name)
			continue
		}
		if !strings.Contains(err.Error(), "environments are not available") &&
			!strings.Contains(err.Error(), "cannot build releases") {
			t.Errorf("%s said %q, want it to name what is missing", name, err)
		}
	}

	forVault := map[string]func() error{
		"VaultSet": func() error {
			_, err := d.VaultSet(ctx, api.VaultSetRequest{Scope: vault.ScopeEnv, App: "shop", Env: "production",
				Values: map[string]string{"DB_PASSWORD": "chosen"}})
			return err
		},
		"VaultList": func() error {
			_, err := d.VaultList(ctx, api.VaultListRequest{App: "shop", Env: "production"})
			return err
		},
		"VaultRemove": func() error {
			_, err := d.VaultRemove(ctx, api.VaultRemoveRequest{Scope: vault.ScopeEnv, App: "shop",
				Env: "production", Names: []string{"DB_PASSWORD"}})
			return err
		},
		"VaultExport": func() error {
			_, err := d.VaultExport(ctx, api.VaultExportRequest{App: "shop", Env: "production"})
			return err
		},
	}
	for name, call := range forVault {
		err := call()
		if err == nil {
			t.Errorf("%s on a daemon with no vault succeeded", name)
			continue
		}
		if !strings.Contains(err.Error(), "no vault") || !strings.Contains(err.Error(), "fleet setup") {
			t.Errorf("%s said %q, want it to name the vault and the way to get one", name, err)
		}
	}
}

func TestVaultResultsNeverCarryAValue(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.Vault = leakyVault{}
	ctx := context.Background()

	res, err := d.VaultList(ctx, api.VaultListRequest{App: "shop", Env: "production"})
	if err != nil {
		t.Fatalf("VaultList: %v", err)
	}
	if len(res.Entries) == 0 {
		t.Fatal("no entries")
	}
	for _, e := range res.Entries {
		if e.Value != "" {
			t.Errorf("the API handed out the value of %s: %q", e.Name, e.Value)
		}
	}

	if res.Entries[0].Scope != vault.ScopeMachine {
		t.Errorf("the first entry is %s, want the machine's", res.Entries[0].Scope)
	}
	if res.Resolved["GREETING"] != vault.ScopeEnv {
		t.Errorf("GREETING resolved from %q, want the environment's", res.Resolved["GREETING"])
	}
}

func TestVaultExportRedactsUnlessRevealed(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.Vault = leakyVault{}
	ctx := context.Background()

	hidden, err := d.VaultExport(ctx, api.VaultExportRequest{App: "shop", Env: "production"})
	if err != nil {
		t.Fatal(err)
	}
	if hidden.Revealed {
		t.Error("an export nobody asked to reveal says it was revealed")
	}
	for k, v := range hidden.Values {
		if v != vault.Redacted {
			t.Errorf("%s = %q, want %q", k, v, vault.Redacted)
		}
	}
	if hidden.Sources["GREETING"] != vault.ScopeEnv {
		t.Errorf("GREETING came from %q", hidden.Sources["GREETING"])
	}

	shown, err := d.VaultExport(ctx, api.VaultExportRequest{App: "shop", Env: "production", Reveal: true})
	if err != nil {
		t.Fatal(err)
	}
	if !shown.Revealed || shown.Values["GREETING"] != "hi" {
		t.Errorf("revealed export = %+v", shown)
	}
}

type leakyVault struct {
	vault.NotImplemented
}

func (leakyVault) List(context.Context, string, string) ([]vault.Entry, error) {
	return []vault.Entry{
		{Ref: vault.EnvRef("shop", "production", "GREETING"), Value: "hi", Version: 1},
		{Ref: vault.MachineRef("GREETING"), Value: "hello", Version: 1},
		{Ref: vault.AppRef("shop", "STRIPE_KEY"), Value: "sk_live", Version: 1},
	}, nil
}

func (leakyVault) Sources(context.Context, string, string) (map[string]vault.Scope, error) {
	return map[string]vault.Scope{"GREETING": vault.ScopeEnv, "STRIPE_KEY": vault.ScopeApp}, nil
}

func (leakyVault) Resolve(context.Context, string, string) (map[string]string, error) {
	return map[string]string{"GREETING": "hi", "STRIPE_KEY": "sk_live"}, nil
}

func TestVaultWritesAreCheckedBeforeAnyOfThemHappens(t *testing.T) {
	d, _ := newTestDaemon(t)
	rec := &recordingVault{}
	d.Vault = rec
	ctx := context.Background()

	if _, err := d.VaultSet(ctx, api.VaultSetRequest{
		Scope: vault.ScopeEnv, App: "shop", Env: "production",

		Values: map[string]string{"AAA": "fine", "not a name": "bad"},
	}); err == nil {
		t.Fatal("a set with an illegal name succeeded")
	}
	if len(rec.set) != 0 {
		t.Errorf("a refused set wrote %v", rec.set)
	}

	if _, err := d.VaultRemove(ctx, api.VaultRemoveRequest{
		Scope: vault.ScopeEnv, App: "shop", Env: "production",
		Names: []string{"AAA", "not a name"},
	}); err == nil {
		t.Fatal("a remove with an illegal name succeeded")
	}
	if len(rec.removed) != 0 {
		t.Errorf("a refused remove removed %v", rec.removed)
	}
}

type recordingVault struct {
	leakyVault

	set     []string
	removed []string
}

func (v *recordingVault) Set(_ context.Context, r vault.Ref, _ string) (*vault.Entry, error) {
	v.set = append(v.set, r.Name)
	return &vault.Entry{Ref: r, Version: 1}, nil
}

func (v *recordingVault) Remove(_ context.Context, r vault.Ref) error {
	v.removed = append(v.removed, r.Name)
	return nil
}

func TestVaultExportRevealIsAnEvent(t *testing.T) {
	d, store := newTestDaemon(t)
	d.Vault = leakyVault{}
	store.envs = []state.EnvRecord{{ID: 3, App: "shop", Name: "production", Mode: state.EnvModeRelease}}
	ctx := WithSession(context.Background(), api.Session{Transport: "ssh", Identity: "commander"})

	if _, err := d.VaultExport(ctx, api.VaultExportRequest{App: "shop", Env: "production"}); err != nil {
		t.Fatal(err)
	}
	if len(store.events) != 0 {
		t.Fatalf("a redacted export wrote %d event(s)", len(store.events))
	}

	if _, err := d.VaultExport(ctx, api.VaultExportRequest{App: "shop", Env: "production", Reveal: true}); err != nil {
		t.Fatal(err)
	}
	if len(store.events) != 1 {
		t.Fatalf("a revealed export wrote %d event(s), want 1", len(store.events))
	}
	ev := store.events[0]
	if ev.EnvID != 3 || ev.Action != "secrets" || ev.Step != "export" || ev.Identity != "commander" {
		t.Errorf("event = %+v", ev)
	}
	if !strings.Contains(ev.Detail, "secret(s)") {
		t.Errorf("detail = %q", ev.Detail)
	}
}

func TestProductionStatus(t *testing.T) {
	d, store := newTestDaemon(t)
	ctx := context.Background()

	p := d.productionStatus(ctx)
	if p == nil {
		t.Fatal("no production section")
	}
	if p.Envs != 0 || p.Release != 0 || p.Vault {
		t.Errorf("empty machine = %+v", p)
	}

	store.envs = []state.EnvRecord{
		{ID: 1, App: "shop", Name: "feat-x", Mode: state.EnvModeDev},
		{ID: 2, App: "shop", Name: "production", Mode: state.EnvModeRelease},
	}
	store.deploys = []state.Deploy{{ID: 9, EnvID: 2, Status: state.DeployWatching}}
	d.Vault = leakyVault{}
	p = d.productionStatus(ctx)
	if p.Envs != 2 || p.Release != 1 || p.Deploying != 1 || !p.Vault || p.Secrets != 3 {
		t.Errorf("production = %+v", p)
	}
}

func TestVaultWritesAreEvents(t *testing.T) {
	d, store := newTestDaemon(t)
	d.Vault = &recordingVault{}
	store.envs = []state.EnvRecord{{ID: 3, App: "shop", Name: "production", Mode: state.EnvModeRelease}}
	ctx := WithSession(context.Background(), api.Session{Transport: "ssh", Identity: "commander"})

	if _, err := d.VaultSet(ctx, api.VaultSetRequest{
		Scope: vault.ScopeEnv, App: "shop", Env: "production",
		Values: map[string]string{"STRIPE_KEY": "sk_live_1", "DB_PASSWORD": "hunter2"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(store.events) != 1 {
		t.Fatalf("a set wrote %d event(s), want 1", len(store.events))
	}
	set := store.events[0]
	if set.EnvID != 3 || set.Action != "secrets" || set.Step != "set" || set.Identity != "commander" {
		t.Errorf("event = %+v", set)
	}
	for _, want := range []string{"STRIPE_KEY", "DB_PASSWORD", "env production"} {
		if !strings.Contains(set.Detail, want) {
			t.Errorf("detail %q does not name %q", set.Detail, want)
		}
	}

	for _, secret := range []string{"sk_live_1", "hunter2"} {
		if strings.Contains(set.Detail, secret) {
			t.Fatalf("the event carries a value: %q", set.Detail)
		}
	}

	if _, err := d.VaultRemove(ctx, api.VaultRemoveRequest{
		Scope: vault.ScopeEnv, App: "shop", Env: "production",
		Names: []string{"STRIPE_KEY"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(store.events) != 2 {
		t.Fatalf("a remove wrote %d event(s) in all, want 2", len(store.events))
	}
	if rm := store.events[1]; rm.Step != "rm" || !strings.Contains(rm.Detail, "STRIPE_KEY") {
		t.Errorf("event = %+v", rm)
	}
}

func TestVaultWritesWithNoEnvironmentAreStillEvents(t *testing.T) {
	d, store := newTestDaemon(t)
	d.Vault = &recordingVault{}
	ctx := WithSession(context.Background(), api.Session{Transport: "ssh", Identity: "commander"})

	if _, err := d.VaultSet(ctx, api.VaultSetRequest{
		Scope: vault.ScopeMachine, Values: map[string]string{"REGISTRY_TOKEN": "t"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(store.events) != 1 {
		t.Fatalf("a machine-scope set wrote %d event(s), want 1", len(store.events))
	}
	ev := store.events[0]
	if ev.EnvID != 0 || ev.Action != "secrets" || ev.Step != "set" || ev.Identity != "commander" {
		t.Errorf("event = %+v", ev)
	}
	if !strings.Contains(ev.Detail, "machine scope") || !strings.Contains(ev.Detail, "REGISTRY_TOKEN") {
		t.Errorf("detail = %q", ev.Detail)
	}
}

func TestRevealWithNoEnvironmentIsStillAnEvent(t *testing.T) {
	d, store := newTestDaemon(t)
	d.Vault = leakyVault{}
	ctx := WithSession(context.Background(), api.Session{Transport: "ssh", Identity: "commander"})

	if _, err := d.VaultExport(ctx, api.VaultExportRequest{
		App: "shop", Env: "no-such-env", Reveal: true,
	}); err != nil {
		t.Fatal(err)
	}
	if len(store.events) != 1 {
		t.Fatalf("a reveal of an environment with no row wrote %d event(s), want 1", len(store.events))
	}
	ev := store.events[0]
	if ev.EnvID != 0 || ev.Action != "secrets" || ev.Step != "export" || ev.Identity != "commander" {
		t.Errorf("event = %+v", ev)
	}
	if ev.App != "shop" || ev.Env != "no-such-env" {
		t.Errorf("event = %+v, want the scope it was asked about", ev)
	}
}

func TestVaultListNarrowsToOneScope(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.Vault = leakyVault{}
	ctx := context.Background()

	machine, err := d.VaultList(ctx, api.VaultListRequest{Scope: vault.ScopeMachine})
	if err != nil {
		t.Fatal(err)
	}
	if len(machine.Entries) != 1 || machine.Entries[0].Scope != vault.ScopeMachine {
		t.Fatalf("machine scope = %+v, want the machine's layer alone", machine.Entries)
	}
	if len(machine.Resolved) != 0 {
		t.Errorf("a listing of one layer reported provenance: %v", machine.Resolved)
	}

	app, err := d.VaultList(ctx, api.VaultListRequest{Scope: vault.ScopeApp, App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if len(app.Entries) != 1 || app.Entries[0].Scope != vault.ScopeApp {
		t.Fatalf("app scope = %+v, want the app's layer alone", app.Entries)
	}

	all, err := d.VaultList(ctx, api.VaultListRequest{App: "shop", Env: "production"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Entries) != 3 || len(all.Resolved) == 0 {
		t.Errorf("every layer = %+v, resolved %v", all.Entries, all.Resolved)
	}
}
