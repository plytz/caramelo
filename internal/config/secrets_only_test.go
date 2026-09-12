package config

import (
	"reflect"
	"testing"
)

const redacted = "<secret>"

func secretsFor(values map[string]string) Secrets {
	return Secrets{
		Lookup: func(name string) (string, bool) {
			v, ok := values[name]
			return v, ok
		},
		DepPassword: func(dep string) (string, bool) {
			v, ok := values[DepPasswordSecret(dep)]
			return v, ok
		},
	}
}

func TestExpandSecretsOnly(t *testing.T) {
	deps := []Dep{{Name: "db", Image: "postgres:16"}}
	sec := secretsFor(map[string]string{"STRIPE_KEY": "sk_live", "DB_PASSWORD": "chosen"})
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"a secret", "${secrets.STRIPE_KEY}", "sk_live"},
		{"a dependency's password", "${deps.db.password}", "chosen"},
		{
			"a secret in the middle of a value",
			"postgres://postgres:${deps.db.password}@${deps.db.host}:${deps.db.port}/shop",
			"postgres://postgres:chosen@${deps.db.host}:${deps.db.port}/shop",
		},
		{"every other reference survives", "${deps.db.host}:${port} ${app.name} ${env.name}",
			"${deps.db.host}:${port} ${app.name} ${env.name}"},
		{"a secret nobody set survives", "${secrets.NOPE}", "${secrets.NOPE}"},
		{"a dependency nobody declared survives", "${deps.cache.password}", "${deps.cache.password}"},
		{"a literal dollar is untouched", "pa$$word $FOO", "pa$word $FOO"},
		{"an escaped reference is untouched", "$${secrets.STRIPE_KEY}", "${secrets.STRIPE_KEY}"},
		{"no reference at all", "plain", "plain"},

		{"an unterminated reference", "${secrets.STRIPE_KEY", "${secrets.STRIPE_KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExpandSecretsOnly(tc.in, deps, sec); got != tc.want {
				t.Errorf("ExpandSecretsOnly(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExpandSecretsOnlyWithNoVault(t *testing.T) {
	in := "${secrets.STRIPE_KEY}"
	if got := ExpandSecretsOnly(in, nil, Secrets{}); got != in {
		t.Errorf("with no vault = %q, want %q", got, in)
	}
}

func TestWithSecretsExpanded(t *testing.T) {
	app := &App{
		Name: "shop",
		Env:  map[string]string{"TOP": "${secrets.STRIPE_KEY}", "PLAIN": "${port}"},
		Deps: []Dep{{Name: "db", Image: "postgres:16", Env: map[string]string{
			"POSTGRES_PASSWORD": "${deps.db.password}",
		}}},
		Services: []Service{{Name: "web", Env: map[string]string{
			"URL": "postgres://postgres:${deps.db.password}@${deps.db.host}/shop",
		}}},
	}
	red := app.WithSecretsExpanded(secretsFor(map[string]string{
		"STRIPE_KEY": redacted, "DB_PASSWORD": redacted,
	}))
	if got := red.Env["TOP"]; got != redacted {
		t.Errorf("env TOP = %q, want %q", got, redacted)
	}
	if got := red.Env["PLAIN"]; got != "${port}" {
		t.Errorf("env PLAIN = %q, want it untouched", got)
	}
	if got := red.Deps[0].Env["POSTGRES_PASSWORD"]; got != redacted {
		t.Errorf("the dep's password = %q, want %q", got, redacted)
	}
	want := "postgres://postgres:" + redacted + "@${deps.db.host}/shop"
	if got := red.Services[0].Env["URL"]; got != want {
		t.Errorf("the service's URL = %q, want %q", got, want)
	}

	if app.Env["TOP"] != "${secrets.STRIPE_KEY}" ||
		app.Deps[0].Env["POSTGRES_PASSWORD"] != "${deps.db.password}" ||
		!reflect.DeepEqual(app.Services[0].Env,
			map[string]string{"URL": "postgres://postgres:${deps.db.password}@${deps.db.host}/shop"}) {
		t.Error("WithSecretsExpanded wrote through to the config it was given")
	}
	revealed := app.WithSecretsExpanded(secretsFor(map[string]string{
		"STRIPE_KEY": "sk_live", "DB_PASSWORD": "chosen",
	}))
	if got := revealed.Env["TOP"]; got != "sk_live" {
		t.Errorf("revealed TOP = %q, want the value", got)
	}
}

func TestWithSecretsExpandedWithNoSecrets(t *testing.T) {
	app := &App{Name: "shop", Env: map[string]string{"A": "${secrets.X}"}}
	if got := app.WithSecretsExpanded(Secrets{}); got != app {
		t.Error("a config with no vault attached was copied")
	}
	if app.WithSecretsExpanded(secretsFor(nil)) == app {
		t.Error("a config with a vault attached was not copied")
	}
}

func TestWithSecretsExpandedKeepsNilMapsNil(t *testing.T) {
	app := &App{Name: "shop", Services: []Service{{Name: "web"}}, Deps: []Dep{{Name: "db", Image: "redis:7"}}}
	out := app.WithSecretsExpanded(secretsFor(map[string]string{"X": "y"}))
	if out.Env != nil || out.Services[0].Env != nil || out.Deps[0].Env != nil {
		t.Errorf("a nil env map became %v / %v / %v", out.Env, out.Services[0].Env, out.Deps[0].Env)
	}
}

func TestCloneSharesNothingMutable(t *testing.T) {
	app := &App{
		Name:   "shop",
		Deploy: &Deploy{Watch: 10},
		Envs:   map[string]EnvOverride{"production": {Hosts: []string{"shop.test"}}},
		Env:    map[string]string{"A": "1"},
		Deps:   []Dep{{Name: "db", Image: "postgres:16", Ready: []string{"pg_isready"}, Env: map[string]string{"B": "2"}}},
		Services: []Service{{
			Name:      "web",
			Health:    &Health{Path: "/healthz"},
			Resources: &Resources{Memory: 1 << 20},
			Env:       map[string]string{"C": "3"},
		}},
	}
	out := app.clone()
	out.Deploy.Watch = 99
	out.Envs["production"].Hosts[0] = "elsewhere.test"
	out.Env["A"] = "changed"
	out.Deps[0].Ready[0] = "false"
	out.Deps[0].Env["B"] = "changed"
	out.Services[0].Health.Path = "/changed"
	out.Services[0].Resources.Memory = 1
	out.Services[0].Env["C"] = "changed"

	if app.Deploy.Watch != 10 || app.Envs["production"].Hosts[0] != "shop.test" ||
		app.Env["A"] != "1" || app.Deps[0].Ready[0] != "pg_isready" || app.Deps[0].Env["B"] != "2" ||
		app.Services[0].Health.Path != "/healthz" || app.Services[0].Resources.Memory != 1<<20 ||
		app.Services[0].Env["C"] != "3" {
		t.Errorf("clone shares state with the config it copied: %+v", app)
	}
}
