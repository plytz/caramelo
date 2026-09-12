package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func ctx() ExpandContext {
	return ExpandContext{
		App:  "shop",
		Env:  "feat-x",
		Port: 20000,
		Deps: map[string]DepAddr{
			"db":    {Host: "127.0.0.1", Port: 20001},
			"cache": {Host: "127.0.0.1", Port: 20002},
		},
	}
}

func TestExpandString(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"plain", "plain"},
		{"${port}", "20000"},
		{"${app.name}", "shop"},
		{"${env.name}", "feat-x"},
		{"${deps.db.host}", "127.0.0.1"},
		{"${deps.db.port}", "20001"},
		{"postgres://postgres:caramelo@${deps.db.host}:${deps.db.port}/postgres", "postgres://postgres:caramelo@127.0.0.1:20001/postgres"},
		{"redis://${deps.cache.host}:${deps.cache.port}", "redis://127.0.0.1:20002"},
		{"${app.name}-${env.name}", "shop-feat-x"},
		{"http://127.0.0.1:${port}/", "http://127.0.0.1:20000/"},

		{"$$5", "$5"},
		{"$${port}", "${port}"},
		{"$$$$", "$$"},
		{"$${port}${port}", "${port}20000"},

		{"pa$$word", "pa$word"},
		{"pa$word", "pa$word"},
		{"trailing$", "trailing$"},
		{"$", "$"},
	}
	for _, tt := range tests {
		got, err := ExpandString(tt.in, ctx())
		if err != nil {
			t.Errorf("ExpandString(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ExpandString(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestExpandStringErrors(t *testing.T) {
	tests := []struct{ in, want string }{
		{"${nope}", `unknown reference "${nope}"`},
		{"${}", `unknown reference "${}"`},
		{"${deps.nope.host}", `unknown reference "${deps.nope.host}"`},
		{"${deps.db.user}", `unknown reference "${deps.db.user}"`},
		{"${deps.db}", `unknown reference "${deps.db}"`},
		{"${deps}", `unknown reference "${deps}"`},
		{"${PORT}", `unknown reference "${PORT}"`},
		{"a ${port", "unterminated"},
	}
	for _, tt := range tests {
		got, err := ExpandString(tt.in, ctx())
		if err == nil {
			t.Errorf("ExpandString(%q) = %q, want an error", tt.in, got)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("ExpandString(%q) error = %q, want %q", tt.in, err, tt.want)
		}
	}
}

func TestExpand(t *testing.T) {
	in := map[string]string{
		"DATABASE_URL": "postgres://${deps.db.host}:${deps.db.port}/x",
		"SELF":         "http://127.0.0.1:${port}",
		"PLAIN":        "yes",
	}
	got, err := Expand(in, ctx())
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	want := map[string]string{
		"DATABASE_URL": "postgres://127.0.0.1:20001/x",
		"SELF":         "http://127.0.0.1:20000",
		"PLAIN":        "yes",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if in["DATABASE_URL"] != "postgres://${deps.db.host}:${deps.db.port}/x" {
		t.Error("Expand modified its input")
	}
	if got, err := Expand(nil, ctx()); err != nil || got != nil {
		t.Errorf("Expand(nil) = %v, %v", got, err)
	}

	_, err = Expand(map[string]string{"BAD": "${deps.nope.port}"}, ctx())
	if err == nil || !strings.Contains(err.Error(), "BAD") || !strings.Contains(err.Error(), "unknown reference") {
		t.Errorf("err = %v, want it to name BAD", err)
	}
}

func TestAppExpanded(t *testing.T) {
	app, err := Load(filepath.Join("testdata", "full.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c := ctx()
	c.Deps["queue"] = DepAddr{Host: "127.0.0.1", Port: 20003}
	out, err := app.Expanded(c)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	want := map[string]string{
		"DATABASE_URL": "postgres://postgres:caramelo@127.0.0.1:20001/postgres",
		"REDIS_URL":    "redis://127.0.0.1:20002",
		"NATS_URL":     "nats://127.0.0.1:20003",
		"SELF":         "http://127.0.0.1:20000",
		"TAG":          "shop-feat-x",
		"PRICE":        "$5",
	}
	if !reflect.DeepEqual(out.Env, want) {
		t.Errorf("env =\n %v\nwant\n %v", out.Env, want)
	}
	if len(out.Deps) != len(app.Deps) {
		t.Fatalf("deps = %v", out.Deps)
	}
	for i, d := range out.Deps {
		if d.Name != app.Deps[i].Name || d.Port != app.Deps[i].Port {
			t.Errorf("dep %d changed: %+v", i, d)
		}
	}
	if app.Env["PRICE"] != "$$5" {
		t.Error("Expanded modified the original config")
	}
	var nilApp *App
	if got, err := nilApp.Expanded(c); got != nil || err != nil {
		t.Errorf("nil.Expanded = %v, %v", got, err)
	}
}
