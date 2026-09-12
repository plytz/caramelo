package env

import (
	"context"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/vault"
)

func TestSecretWarningsSayADatabaseWillNotPickThePasswordUp(t *testing.T) {
	h := newHarness(t)
	h.setSecret(t, "production", "DB_PASSWORD", "chosen")
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Release: true})

	got := h.m.SecretWarnings(context.Background(), "shop", "production", []string{"DB_PASSWORD"})
	if len(got) != 1 {
		t.Fatalf("SecretWarnings = %v, want one line", got)
	}
	for _, want := range []string{
		"DB_PASSWORD changed",
		"db of env production already exists",
		"postgres reads POSTGRES_PASSWORD only when it initialises",
		"ALTER USER postgres WITH PASSWORD",
	} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the warning does not say %q:\n%s", want, got[0])
		}
	}

	if strings.Contains(got[0], "chosen") {
		t.Errorf("the warning carries the value:\n%s", got[0])
	}
}

func TestSecretWarningsAreOnlyAboutDependencyPasswords(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Release: true})
	ctx := context.Background()

	if got := h.m.SecretWarnings(ctx, "shop", "production", []string{"STRIPE_KEY"}); len(got) != 0 {
		t.Errorf("a plain secret warned: %v", got)
	}

	if got := h.m.SecretWarnings(ctx, "shop", "production", []string{"CACHE_PASSWORD"}); len(got) != 0 {
		t.Errorf("redis warned: %v", got)
	}

	if got := h.m.SecretWarnings(ctx, "shop", "staging", []string{"DB_PASSWORD"}); len(got) != 0 {
		t.Errorf("an environment that does not exist warned: %v", got)
	}
}

func TestSecretWarningsCoverEveryEnvAtTheAppScope(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Release: true})
	h.mustCreate(CreateRequest{App: "shop", Name: "staging", Release: true})

	got := h.m.SecretWarnings(context.Background(), "shop", "", []string{"DB_PASSWORD"})
	if len(got) != 2 {
		t.Fatalf("SecretWarnings = %v, want one line per environment", got)
	}
	if !strings.Contains(got[0], "env production") || !strings.Contains(got[1], "env staging") {
		t.Errorf("the lines are not one per environment, in order:\n%s", strings.Join(got, "\n"))
	}
}

func TestConfigWithSecretsRedacts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.setSecret(t, "production", "STRIPE_KEY", "sk_live")
	h.setSecret(t, "production", "DB_PASSWORD", "chosen")

	cfg := &config.App{
		Name: "shop",
		Deps: []config.Dep{{Name: "db", Image: "postgres:16"}},
		Services: []config.Service{{Name: "web", Env: map[string]string{
			"STRIPE_KEY": "${secrets.STRIPE_KEY}",
			"DATABASE_URL": "postgres://postgres:${deps.db.password}@" +
				"${deps.db.host}:${deps.db.port}/shop",
			"PORT": "${port}",
		}}},
	}
	out, err := h.m.ConfigWithSecrets(ctx, "shop", "production", cfg, false)
	if err != nil {
		t.Fatalf("ConfigWithSecrets: %v", err)
	}
	vars := out.Services[0].Env
	if vars["STRIPE_KEY"] != vault.Redacted {
		t.Errorf("STRIPE_KEY = %q, want %q", vars["STRIPE_KEY"], vault.Redacted)
	}
	want := "postgres://postgres:" + vault.Redacted + "@${deps.db.host}:${deps.db.port}/shop"
	if vars["DATABASE_URL"] != want {
		t.Errorf("DATABASE_URL = %q, want %q", vars["DATABASE_URL"], want)
	}
	if vars["PORT"] != "${port}" {
		t.Errorf("PORT = %q, want it untouched", vars["PORT"])
	}
}

func TestConfigWithSecretsRevealsAndRecords(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	e := h.mustCreate(CreateRequest{App: "shop", Name: "production", Release: true})
	h.setSecret(t, "production", "STRIPE_KEY", "sk_live")

	cfg := &config.App{Name: "shop", Env: map[string]string{"K": "${secrets.STRIPE_KEY}"}}
	out, err := h.m.ConfigWithSecrets(ctx, "shop", "production", cfg, true)
	if err != nil {
		t.Fatalf("ConfigWithSecrets: %v", err)
	}
	if out.Env["K"] != "sk_live" {
		t.Errorf("K = %q, want the value", out.Env["K"])
	}

	events := h.eventsOf(e.ID, "secrets")
	if len(events) != 1 {
		t.Fatalf("reveal events = %d, want one: %v", len(events), events)
	}
	if events[0].Step != "reveal" {
		t.Errorf("step = %q, want %q", events[0].Step, "reveal")
	}
	if !strings.Contains(events[0].Detail, "STRIPE_KEY") {
		t.Errorf("the event does not name the secret: %q", events[0].Detail)
	}
	if strings.Contains(events[0].Detail, "sk_live") {
		t.Errorf("the event carries the value: %q", events[0].Detail)
	}

	if _, err := h.m.ConfigWithSecrets(ctx, "shop", "production", cfg, false); err != nil {
		t.Fatalf("ConfigWithSecrets: %v", err)
	}
	if got := h.eventsOf(e.ID, "secrets"); len(got) != 1 {
		t.Errorf("redacting recorded an event too: %v", got)
	}
}

func TestConfigWithSecretsWithNone(t *testing.T) {
	h := newHarness(t)
	cfg := &config.App{Name: "shop", Env: map[string]string{"K": "${secrets.NOPE}"}}
	out, err := h.m.ConfigWithSecrets(context.Background(), "shop", "feat-x", cfg, false)
	if err != nil {
		t.Fatalf("ConfigWithSecrets: %v", err)
	}
	if out != cfg {
		t.Error("a config with no secrets was copied")
	}
}
