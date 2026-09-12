package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func views() Views {
	return Views{
		Host: ExpandContext{
			App: "shop", Env: "feat-x", Port: 20000, View: ViewHost,
			Deps: map[string]DepAddr{
				"db":    {Host: "127.0.0.1", Port: 20001},
				"cache": {Host: "127.0.0.1", Port: 20002},
			},
			Services: map[string]DepAddr{"web": {Host: "127.0.0.1", Port: 20000}},
		},
		Network: ExpandContext{
			App: "shop", Env: "feat-x", Port: 20000, View: ViewNetwork,
			Deps: map[string]DepAddr{
				"db":    {Host: "db", Port: 5432},
				"cache": {Host: "cache", Port: 6379},
			},
			Services: map[string]DepAddr{"web": {Host: "web", Port: 20000}},
		},
	}
}

func TestParseView(t *testing.T) {
	for in, want := range map[string]View{"": ViewHost, "host": ViewHost, "network": ViewNetwork} {
		got, err := ParseView(in)
		if err != nil || got != want {
			t.Errorf("ParseView(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	_, err := ParseView("container")
	if err == nil {
		t.Fatal("ParseView(container) accepted")
	}
	for _, w := range []string{`"container"`, "host", "network"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not mention %q", err, w)
		}
	}
}

func TestResolveBothViews(t *testing.T) {
	app, err := Parse([]byte(`
name: shop
deps:
  db: postgres:16
  cache: redis:7
services:
  web:
    run: npm start
    env:
      SELF: http://${services.web.host}:${services.web.port}
env:
  DATABASE_URL: postgres://postgres:caramelo@${deps.db.host}:${deps.db.port}/postgres
  REDIS_URL: redis://${deps.cache.host}:${deps.cache.port}
  SELF: http://127.0.0.1:${port}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	v := views()

	host, err := app.Resolve(ViewHost, v)
	if err != nil {
		t.Fatalf("resolve host: %v", err)
	}
	if got, want := host.Env["DATABASE_URL"], "postgres://postgres:caramelo@127.0.0.1:20001/postgres"; got != want {
		t.Errorf("host DATABASE_URL = %q, want %q", got, want)
	}
	if got, want := host.Env["REDIS_URL"], "redis://127.0.0.1:20002"; got != want {
		t.Errorf("host REDIS_URL = %q, want %q", got, want)
	}
	if got, want := host.Services[0].Env["SELF"], "http://127.0.0.1:20000"; got != want {
		t.Errorf("host service SELF = %q, want %q", got, want)
	}

	network, err := app.Resolve(ViewNetwork, v)
	if err != nil {
		t.Fatalf("resolve network: %v", err)
	}
	if got, want := network.Env["DATABASE_URL"], "postgres://postgres:caramelo@db:5432/postgres"; got != want {
		t.Errorf("network DATABASE_URL = %q, want %q", got, want)
	}
	if got, want := network.Env["REDIS_URL"], "redis://cache:6379"; got != want {
		t.Errorf("network REDIS_URL = %q, want %q", got, want)
	}
	if got, want := network.Services[0].Env["SELF"], "http://web:20000"; got != want {
		t.Errorf("network service SELF = %q, want %q", got, want)
	}

	if host.Env["SELF"] != network.Env["SELF"] {
		t.Errorf("SELF differs between views: %q vs %q", host.Env["SELF"], network.Env["SELF"])
	}

	if got := app.Env["DATABASE_URL"]; !strings.Contains(got, "${deps.db.host}") {
		t.Errorf("the parsed config was modified: %q", got)
	}

	if v.Context("nonsense").Deps["db"].Host != "127.0.0.1" {
		t.Error("an unknown view is not the host view")
	}

	bare := Views{Network: ExpandContext{Deps: map[string]DepAddr{}}}
	if got := bare.Context(ViewNetwork).View; got != ViewNetwork {
		t.Errorf("Context(network).View = %q, want network", got)
	}
}

func TestResolveCopiesServices(t *testing.T) {
	app, err := Load(filepath.Join("testdata", "v1.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	v := views()
	v.Network.Services["web"] = DepAddr{Host: "web", Port: 3000}
	v.Host.Services["web"] = DepAddr{Host: "127.0.0.1", Port: 20000}
	out, err := app.Resolve(ViewNetwork, v)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	worker, _ := out.Service("worker")
	original, _ := app.Service("worker")
	if worker.Build == original.Build {
		t.Error("the build was shared with the parsed config")
	}
	worker.Health.Command[0] = "changed"
	if original.Health.Command[0] != "node" {
		t.Errorf("the health command was shared with the parsed config: %v", original.Health.Command)
	}
	if got, want := out.Services[0].Env["SELF"], "http://web:3000"; got != want {
		t.Errorf("web SELF = %q, want %q", got, want)
	}
}

func TestExpandServiceReferences(t *testing.T) {
	ctx := views().Network
	for in, want := range map[string]string{
		"${services.web.host}": "web",
		"${services.web.port}": "20000",
	} {
		got, err := ExpandString(in, ctx)
		if err != nil || got != want {
			t.Errorf("ExpandString(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"${services.api.host}", "${services.web.scheme}", "${services.web}", "${services.}"} {
		if got, err := ExpandString(in, ctx); err == nil {
			t.Errorf("ExpandString(%q) = %q, want an error", in, got)
		}
	}
}
