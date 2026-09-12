package config

import (
	"reflect"
	"testing"
)

func TestImageName(t *testing.T) {
	tests := []struct{ ref, want string }{
		{"postgres", "postgres"},
		{"postgres:16", "postgres"},
		{"postgres:16-alpine", "postgres"},
		{"library/postgres:16", "postgres"},
		{"docker.io/library/redis:7", "redis"},
		{"registry.example.com:5000/team/mariadb:11", "mariadb"},
		{"redis@sha256:0123456789abcdef", "redis"},
		{"ghcr.io/org/redis@sha256:0123456789abcdef", "redis"},
		{"Postgres:16", "postgres"},
		{"  redis:7  ", "redis"},
		{"nats:2", "nats"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := ImageName(tt.ref); got != tt.want {
			t.Errorf("ImageName(%q) = %q, want %q", tt.ref, got, tt.want)
		}
	}
}

func TestKnownLookup(t *testing.T) {
	tests := []struct {
		ref   string
		found bool
		port  int
		ready []string
		env   map[string]string
		data  string
	}{
		{ref: "postgres:16-alpine", found: true, port: 5432, ready: []string{"pg_isready", "-U", "postgres"}, env: map[string]string{"POSTGRES_PASSWORD": "caramelo"}, data: "/var/lib/postgresql/data"},
		{ref: "redis:7-alpine", found: true, port: 6379, ready: []string{"redis-cli", "ping"}, data: "/data"},
		{ref: "mysql:8", found: true, port: 3306, ready: []string{"mysqladmin", "ping"}, env: map[string]string{"MYSQL_ROOT_PASSWORD": "caramelo"}, data: "/var/lib/mysql"},
		{ref: "mariadb:11", found: true, port: 3306, ready: []string{"mysqladmin", "ping"}, env: map[string]string{"MYSQL_ROOT_PASSWORD": "caramelo"}, data: "/var/lib/mysql"},
		{ref: "mongo:7", found: true, port: 27017, ready: []string{"mongosh", "--eval", "db.runCommand('ping')"}, data: "/data/db"},
		{ref: "rabbitmq:3", found: true, port: 5672, ready: []string{"rabbitmq-diagnostics", "check_port_connectivity"}, data: "/var/lib/rabbitmq"},
		{ref: "docker.io/library/postgres", found: true, port: 5432, ready: []string{"pg_isready", "-U", "postgres"}, env: map[string]string{"POSTGRES_PASSWORD": "caramelo"}, data: "/var/lib/postgresql/data"},
		{ref: "nats:2"},
		{ref: "postgresql:16"},
		{ref: ""},
	}
	for _, tt := range tests {
		got, ok := Lookup(tt.ref)
		if ok != tt.found {
			t.Fatalf("Lookup(%q) found = %v, want %v", tt.ref, ok, tt.found)
		}
		if !ok {
			continue
		}
		if got.Port != tt.port {
			t.Errorf("%s: port = %d, want %d", tt.ref, got.Port, tt.port)
		}
		if !reflect.DeepEqual(got.Ready, tt.ready) {
			t.Errorf("%s: ready = %v, want %v", tt.ref, got.Ready, tt.ready)
		}
		if !reflect.DeepEqual(got.Env, tt.env) {
			t.Errorf("%s: env = %v, want %v", tt.ref, got.Env, tt.env)
		}
		if got.Data != tt.data {
			t.Errorf("%s: data = %q, want %q", tt.ref, got.Data, tt.data)
		}
	}
}

func TestKnownLookupCopies(t *testing.T) {
	got, ok := Lookup("postgres:16")
	if !ok {
		t.Fatal("postgres not known")
	}
	got.Env["POSTGRES_PASSWORD"] = "leaked"
	got.Ready[0] = "leaked"
	again, _ := Lookup("postgres:16")
	if again.Env["POSTGRES_PASSWORD"] != "caramelo" || again.Ready[0] != "pg_isready" {
		t.Fatalf("the table was mutated through a lookup: %+v", again)
	}
}

func TestKnownTableIsValid(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range Known {
		if seen[k.Image] {
			t.Errorf("%s: listed twice", k.Image)
		}
		seen[k.Image] = true
		if k.Image != ImageName(k.Image) {
			t.Errorf("%s: the table matches on the bare image name", k.Image)
		}
		app := &App{Name: "shop", Deps: []Dep{{Name: "d", Image: k.Image, Port: k.Port, Ready: k.Ready, Env: k.Env, Data: k.Data}}}
		if err := app.Validate(); err != nil {
			t.Errorf("%s: %v", k.Image, err)
		}
	}
	for _, want := range []string{"postgres", "redis", "mysql", "mariadb", "mongo", "rabbitmq"} {
		if !seen[want] {
			t.Errorf("%s is missing from the table", want)
		}
	}
}
