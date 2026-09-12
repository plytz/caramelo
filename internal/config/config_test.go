package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func golden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return b
}

func TestLoadFull(t *testing.T) {
	app, err := Load(filepath.Join("testdata", "full.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if app.Name != "shop" {
		t.Errorf("name = %q, want shop", app.Name)
	}
	if app.Path != filepath.Join("testdata", "full.yaml") {
		t.Errorf("path = %q", app.Path)
	}

	var names []string
	for _, d := range app.Deps {
		names = append(names, d.Name)
	}
	if want := []string{"db", "cache", "queue"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("dep order = %v, want %v", names, want)
	}
	want := map[string]Dep{

		"db": {
			Name: "db", Image: "postgres:16", Port: 5432,
			Ready: []string{"pg_isready", "-U", "postgres"},
			Env:   map[string]string{"POSTGRES_PASSWORD": "caramelo"},
			Data:  "/var/lib/postgresql/data",
		},
		"cache": {
			Name: "cache", Image: "redis:7", Port: 6379,
			Ready: []string{"redis-cli", "ping"}, Data: "/data",
		},

		"queue": {Name: "queue", Image: "nats:2", Port: 4222},
	}
	for name, w := range want {
		got, ok := app.Dep(name)
		if !ok {
			t.Fatalf("dep %q missing", name)
		}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("dep %q =\n %+v\nwant\n %+v", name, got, w)
		}
	}
	if got := app.Env["PRICE"]; got != "$$5" {
		t.Errorf("PRICE = %q before expansion, want the literal $$5", got)
	}
}

func TestLoadOverrides(t *testing.T) {
	app, err := Load(filepath.Join("testdata", "overrides.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	db, _ := app.Dep("db")
	if db.Port != 5433 {
		t.Errorf("port = %d, want the overridden 5433", db.Port)
	}
	if len(db.Ready) != 0 {
		t.Errorf("ready = %v, want none: an explicit empty list turns the check off", db.Ready)
	}
	if db.Data != "" {
		t.Errorf("data = %q, want none: an explicit empty path keeps nothing", db.Data)
	}
	want := map[string]string{"POSTGRES_PASSWORD": "hunter2", "POSTGRES_DB": "shop"}
	if !reflect.DeepEqual(db.Env, want) {
		t.Errorf("env = %v, want %v (the file's value wins, its own keys are kept)", db.Env, want)
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load(filepath.Join("testdata", "nope.yaml"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
}

func TestParseEmpty(t *testing.T) {
	for _, name := range []string{"empty.yaml"} {
		app, err := Parse(golden(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if app.Name != "" || len(app.Deps) != 0 || len(app.Env) != 0 {
			t.Errorf("%s: %+v, want an empty app", name, app)
		}
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{
			name: "unknown top-level key",
			yaml: string(golden(t, "unknown-key.yaml")),
			want: []string{"unknown key", `"depends_on"`, `"deps"`},
		},
		{
			name: "unknown image without a port",
			yaml: string(golden(t, "unknown-image.yaml")),
			want: []string{"deps.queue", "port is required", "nats:2"},
		},
		{name: "not a mapping", yaml: "- shop\n", want: []string{"must be a mapping"}},
		{name: "broken yaml", yaml: "name: [\n", want: []string{"yaml"}},
		{name: "deps not a mapping", yaml: "deps: [postgres]\n", want: []string{"deps", "must be a mapping"}},
		{name: "dep with no value", yaml: "deps:\n  db:\n", want: []string{"deps.db", "image reference or a mapping"}},
		{name: "dep unknown key", yaml: "deps:\n  db:\n    image: postgres:16\n    volumes: [/data]\n", want: []string{"deps.db", "unknown key", `"volumes"`}},
		{name: "dep image not a string", yaml: "deps:\n  db:\n    image: [postgres]\n", want: []string{"deps.db.image", "must be a string"}},
		{name: "dep port not a number", yaml: "deps:\n  db:\n    image: nats:2\n    port: soon\n", want: []string{"deps.db.port", "must be a number"}},
		{name: "dep port out of range", yaml: "deps:\n  db:\n    image: nats:2\n    port: 70000\n", want: []string{"deps.db.port", "out of range"}},
		{name: "dep ready not a list", yaml: "deps:\n  db:\n    image: postgres:16\n    ready: pg_isready\n", want: []string{"deps.db.ready", "list of arguments"}},
		{name: "dep data relative", yaml: "deps:\n  db:\n    image: postgres:16\n    data: var/lib\n", want: []string{"deps.db.data", "absolute path"}},
		{name: "dep name not a slug", yaml: "deps:\n  My_DB: postgres:16\n", want: []string{"deps", `"My_DB"`, "lowercase"}},
		{name: "app name not a slug", yaml: "name: Shop App\n", want: []string{"name", `"Shop App"`, "lowercase"}},
		{name: "env not a mapping", yaml: "env: PORT=1\n", want: []string{"env", "must be a mapping"}},
		{name: "env value not a scalar", yaml: "env:\n  ARGS: [a, b]\n", want: []string{"env.ARGS", "must be a string"}},
		{name: "env name invalid", yaml: "env:\n  not-a-var: 1\n", want: []string{"env", `"not-a-var"`}},
		{
			name: "unknown reference",
			yaml: "deps:\n  db: postgres:16\nenv:\n  DATABASE_URL: postgres://${deps.dbb.host}/x\n",
			want: []string{"env.DATABASE_URL", "unknown reference", "${deps.dbb.host}"},
		},
		{
			name: "unknown reference field",
			yaml: "deps:\n  db: postgres:16\nenv:\n  X: ${deps.db.user}\n",
			want: []string{"env.X", "unknown reference", "${deps.db.user}"},
		},
		{name: "unknown bare reference", yaml: "env:\n  X: ${hostname}\n", want: []string{"env.X", "${hostname}"}},
		{name: "unterminated reference", yaml: "env:\n  X: ${port\n", want: []string{"env.X", "unterminated"}},
		{
			name: "reference in a dep's env",
			yaml: "deps:\n  db:\n    image: postgres:16\n    env:\n      HOST: ${deps.cache.host}\n",
			want: []string{"deps.db.env.HOST", "unknown reference"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatalf("parsed without error: %+v", app)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

func TestErrorsNameTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	for _, doc := range []string{
		"services:\n  web:\n    run: npm start\n    port: 0\n",
		string(golden(t, "unknown-key.yaml")),
		string(golden(t, "unknown-image.yaml")),
		"env:\n  X: ${nope}\n",
		"name: Shop\n",
	} {
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if err == nil {
			t.Fatalf("%q parsed without error", doc)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error %q does not name %s", err, path)
		}
		if _, err := Parse([]byte(doc)); err == nil || strings.Contains(err.Error(), path) {
			t.Errorf("Parse error %v should not name a file", err)
		}
	}
}

func TestReservedKeyError(t *testing.T) {
	e := &ReservedKeyError{Key: "secrets", Path: "caramelo.yaml"}
	if got, want := e.Error(), `caramelo.yaml: "secrets" is not supported yet`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	e.Path = ""
	if got, want := e.Error(), `"secrets" is not supported yet`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}

	var rke *ReservedKeyError
	if !errors.As(error(e), &rke) || rke.Key != "secrets" {
		t.Fatalf("errors.As did not match a *ReservedKeyError")
	}
	for _, k := range ReservedKeys {
		if !Reserved(k) {
			t.Errorf("Reserved(%q) = false", k)
		}
	}

	for _, k := range []string{"services", "run", "build", "test", "deps"} {
		if Reserved(k) {
			t.Errorf("Reserved(%q) = true, but v1 parses it", k)
		}
	}
}

func TestLoadDir(t *testing.T) {
	t.Run("no file", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "shop")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		app, err := LoadDir(dir)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if app.Name != "shop" || len(app.Deps) != 0 || len(app.Env) != 0 {
			t.Errorf("got %+v, want the directory's name and nothing else", app)
		}
	})
	t.Run("file without a name", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "shop-api")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte("deps:\n  db: postgres:16\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		app, err := LoadDir(dir)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if app.Name != "shop-api" {
			t.Errorf("name = %q, want the directory's name", app.Name)
		}
		if len(app.Deps) != 1 {
			t.Errorf("deps = %v", app.Deps)
		}
	})
	t.Run("file with a name", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, FileName), golden(t, "minimal.yaml"), 0o600); err != nil {
			t.Fatal(err)
		}
		app, err := LoadDir(dir)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if app.Name != "shop" {
			t.Errorf("name = %q, want the file's name", app.Name)
		}
	})
}

func TestDefaultName(t *testing.T) {
	tests := []struct{ dir, want string }{
		{"shop", "shop"},
		{"/home/dev/src/shop-api", "shop-api"},
		{"/home/dev/src/Shop_API/", "shop-api"},
		{"My App", "my-app"},
		{"shop.git", "shop-git"},
		{"-weird-", "weird"},
		{"_", "app"},
		{".", "app"},
		{"", "app"},
		{"9lives", "9lives"},
		{strings.Repeat("a", 40), strings.Repeat("a", 32)},
	}
	for _, tt := range tests {
		if got := DefaultName(tt.dir); got != tt.want {
			t.Errorf("DefaultName(%q) = %q, want %q", tt.dir, got, tt.want)
		}
		if got := DefaultName(tt.dir); !validSlug(got) {
			t.Errorf("DefaultName(%q) = %q, not a slug", tt.dir, got)
		}
	}
	if got := Default("/src/shop"); got.Name != "shop" || len(got.Deps) != 0 {
		t.Errorf("Default = %+v, want the name and no deps", got)
	}
}

func TestValidate(t *testing.T) {
	ok := func() *App {
		return &App{
			Name: "shop",
			Path: FileName,
			Deps: []Dep{{Name: "db", Image: "postgres:16", Port: 5432}},
			Env:  map[string]string{"DATABASE_URL": "postgres://${deps.db.host}:${deps.db.port}/x"},
		}
	}
	if err := ok().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	a := ok()
	a.Name = ""
	if err := a.Validate(); err != nil {
		t.Errorf("empty name rejected: %v", err)
	}
	tests := []struct {
		name string
		mut  func(*App)
		want string
	}{
		{"bad app name", func(a *App) { a.Name = "Shop" }, "name"},
		{"duplicate dep", func(a *App) { a.Deps = append(a.Deps, a.Deps[0]) }, "duplicate"},
		{"empty dep name", func(a *App) { a.Deps[0].Name = "" }, "needs a name"},
		{"bad dep name", func(a *App) { a.Deps[0].Name = "DB" }, "invalid dependency name"},
		{"no image", func(a *App) { a.Deps[0].Image = " " }, "image is required"},
		{"no port", func(a *App) { a.Deps[0].Port = 0 }, "out of range"},
		{"empty ready arg", func(a *App) { a.Deps[0].Ready = []string{"pg_isready", ""} }, "must not be empty"},
		{"relative data", func(a *App) { a.Deps[0].Data = "data" }, "absolute path"},
		{"bad reference", func(a *App) { a.Env["X"] = "${deps.nope.port}" }, "unknown reference"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := ok()
			tt.mut(a)
			err := a.Validate()
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tt.want) || !strings.HasPrefix(err.Error(), FileName+": ") {
				t.Errorf("err = %q, want %q and the file name", err, tt.want)
			}
		})
	}
	var nilApp *App
	if err := nilApp.Validate(); err == nil {
		t.Error("nil app validated")
	}
	if _, ok := nilApp.Dep("db"); ok {
		t.Error("nil app has a dep")
	}
}

func TestManagedVars(t *testing.T) {
	for _, k := range ManagedVars {
		if !Managed(k) {
			t.Errorf("Managed(%q) = false", k)
		}
	}
	if Managed("DATABASE_URL") {
		t.Error("Managed(DATABASE_URL) = true")
	}
}
