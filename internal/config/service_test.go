package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadV1(t *testing.T) {
	app, err := Load(filepath.Join("testdata", "v1.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	var names []string
	for _, s := range app.Services {
		names = append(names, s.Name)
	}
	if want := []string{"web", "echo", "worker"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("service order = %v, want %v", names, want)
	}
	if app.Test != "npm test" {
		t.Errorf("test = %q, want npm test", app.Test)
	}
	want := map[string]Service{
		"web": {
			Name: "web", Image: "node:22", Install: "npm ci", Run: "npm start",
			Port:   3000,
			Health: &Health{Path: "/healthz"},
			Env: map[string]string{
				"LOG_LEVEL": "debug",
				"SELF":      "http://${services.web.host}:${services.web.port}",
			},
		},
		"echo": {Name: "echo", Run: "node echo.js", Port: 4000, Protocol: ProtocolUDP},
		"worker": {
			Name:   "worker",
			Build:  &Build{Context: ".", Dockerfile: "Dockerfile.worker"},
			Run:    "npm run worker",
			Port:   PortNone,
			Health: &Health{Command: []string{"node", "healthcheck.js"}},
		},
	}
	for name, w := range want {
		got, ok := app.Service(name)
		if !ok {
			t.Fatalf("service %q missing", name)
		}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("service %q =\n %+v\nwant\n %+v", name, got, w)
		}
	}
	if s, _ := app.FirstService(); s.Name != "web" {
		t.Errorf("FirstService = %q, want web", s.Name)
	}

	if s, _ := app.Service("web"); s.Protocol != "" {
		t.Errorf("web protocol = %q, want it unwritten", s.Protocol)
	}
	if p, err := ParseProtocol(""); err != nil || p != ProtocolTCP {
		t.Errorf("ParseProtocol(\"\") = %q, %v, want tcp", p, err)
	}
}

func TestShorthand(t *testing.T) {
	app, err := Load(filepath.Join("testdata", "shorthand.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []Service{{
		Name:    DefaultServiceName,
		Install: "go mod download",
		Run:     "go run .",
	}}
	if !reflect.DeepEqual(app.Services, want) {
		t.Errorf("services = %+v, want %+v", app.Services, want)
	}
	if app.Test != "go test ./..." {
		t.Errorf("test = %q", app.Test)
	}

	app, err = Parse([]byte("name: shop\nbuild: .\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want = []Service{{Name: DefaultServiceName, Build: &Build{Context: "."}}}
	if !reflect.DeepEqual(app.Services, want) {
		t.Errorf("services = %+v, want %+v", app.Services, want)
	}

	for _, doc := range []string{"name: shop\nservices:\n", "name: shop\nservices: {}\n"} {
		app, err = Parse([]byte(doc))
		if err != nil {
			t.Fatalf("parse %q: %v", doc, err)
		}
		if len(app.Services) != 0 {
			t.Errorf("parse %q: services = %+v, want none", doc, app.Services)
		}
	}

	app, err = Parse([]byte("name: lib\ntest: go test ./...\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(app.Services) != 0 {
		t.Errorf("services = %+v, want none", app.Services)
	}
	if _, ok := app.FirstService(); ok {
		t.Error("FirstService on an app with none")
	}
}

func TestParseServiceErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{
			name: "shorthand and services together",
			yaml: "run: npm start\nservices:\n  web:\n    run: npm start\n",
			want: []string{`"run"`, `"services"`, "mutually exclusive", "services.web"},
		},
		{
			name: "several shorthand keys and services",
			yaml: "install: npm ci\nbuild: .\nservices:\n  web:\n    run: npm start\n",
			want: []string{`"install"`, `"build"`, "mutually exclusive"},
		},
		{name: "services not a mapping", yaml: "services: [web]\n", want: []string{"services", "must be a mapping"}},
		{name: "protocol not a string", yaml: "services:\n  web:\n    run: x\n    protocol: [tcp]\n", want: []string{"services.web.protocol", "must be a string"}},
		{name: "service string form", yaml: "services:\n  web: npm start\n", want: []string{"services.web", "must be a mapping", `"run"`}},
		{name: "service unknown key", yaml: "services:\n  web:\n    run: x\n    command: y\n", want: []string{"services.web", "unknown key", `"command"`}},
		{name: "empty run", yaml: "run:\n", want: []string{"run", "must not be empty"}},
		{name: "empty service run", yaml: "services:\n  web:\n    run: \"\"\n", want: []string{"services.web.run", "must not be empty"}},
		{name: "empty test", yaml: "test:\n", want: []string{"test", "must not be empty"}},
		{name: "run not a string", yaml: "services:\n  web:\n    run: [npm, start]\n", want: []string{"services.web.run", "must be a string"}},
		{name: "port not a number", yaml: "services:\n  web:\n    run: x\n    port: soon\n", want: []string{"services.web.port", "must be a number", `"none"`}},
		{name: "port out of range", yaml: "services:\n  web:\n    run: x\n    port: 70000\n", want: []string{"services.web.port", "out of range"}},
		{name: "port zero", yaml: "services:\n  web:\n    run: x\n    port: 0\n", want: []string{"services.web.port", "out of range"}},
		{name: "unknown protocol", yaml: "services:\n  web:\n    run: x\n    protocol: sctp\n", want: []string{"services.web.protocol", `"sctp"`, "tcp", "udp"}},
		{name: "health not a path", yaml: "services:\n  web:\n    run: x\n    health: healthz\n", want: []string{"services.web.health", "HTTP path"}},
		{name: "health empty list", yaml: "services:\n  web:\n    run: x\n    health: []\n", want: []string{"services.web.health", "must not be empty"}},
		{name: "health list of lists", yaml: "services:\n  web:\n    run: x\n    health: [[curl]]\n", want: []string{"services.web.health", "list of arguments"}},
		{name: "health mapping", yaml: "services:\n  web:\n    run: x\n    health:\n      path: /x\n", want: []string{"services.web.health", "HTTP path"}},
		{name: "http health without a port", yaml: "services:\n  web:\n    run: x\n    port: none\n    health: /healthz\n", want: []string{"services.web.health", "needs a port", "command"}},
		{name: "http health over udp", yaml: "services:\n  web:\n    run: x\n    protocol: udp\n    health: /healthz\n", want: []string{"services.web.health", "TCP port", "command"}},
		{name: "image and build", yaml: "services:\n  web:\n    image: node:22\n    build: .\n    run: x\n", want: []string{"services.web", `"image"`, `"build"`, "mutually exclusive"}},
		{name: "build unknown key", yaml: "services:\n  web:\n    run: x\n    build:\n      target: dev\n", want: []string{"services.web.build", "unknown key", `"target"`}},
		{name: "build not a path", yaml: "services:\n  web:\n    run: x\n    build: [.]\n", want: []string{"services.web.build", "build context path"}},
		{name: "build without a context", yaml: "services:\n  web:\n    run: x\n    build:\n      dockerfile: Dockerfile.dev\n", want: []string{"services.web.build.context", "needs a context"}},
		{name: "absolute build context", yaml: "services:\n  web:\n    run: x\n    build: /src\n", want: []string{"services.web.build.context", "relative"}},
		{name: "escaping build context", yaml: "services:\n  web:\n    run: x\n    build: ../other\n", want: []string{"services.web.build.context", "inside the checkout"}},
		{name: "escaping dockerfile", yaml: "services:\n  web:\n    run: x\n    build:\n      context: .\n      dockerfile: ../Dockerfile\n", want: []string{"services.web.build.dockerfile", "inside the checkout"}},
		{name: "service name not a slug", yaml: "services:\n  Web_1:\n    run: x\n", want: []string{"services", `"Web_1"`, "lowercase"}},
		{name: "service and dep share a name", yaml: "deps:\n  db: postgres:16\nservices:\n  db:\n    run: x\n", want: []string{"services.db", "also a dependency"}},
		{name: "service env not a mapping", yaml: "services:\n  web:\n    run: x\n    env: LOG=1\n", want: []string{"services.web.env", "must be a mapping"}},
		{
			name: "unknown reference in a service's env",
			yaml: "services:\n  web:\n    run: x\n    env:\n      URL: ${services.api.host}\n",
			want: []string{"services.web.env.URL", "unknown reference"},
		},
		{
			name: "unknown reference to a service",
			yaml: "services:\n  web:\n    run: x\nenv:\n  URL: http://${services.api.host}\n",
			want: []string{"env.URL", "unknown reference", "${services.api.host}"},
		},
		{

			name: "port written as -1",
			yaml: "services:\n  web:\n    run: x\n    port: -1\n",
			want: []string{"services.web.port", "out of range", "none"},
		},
		{
			name: "an install beside a build",
			yaml: "services:\n  web:\n    build: .\n    install: npm ci\n",
			want: []string{"services.web", "mutually exclusive", "Dockerfile"},
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

func TestValidateServices(t *testing.T) {
	ok := func() *App {
		return &App{
			Name:     "shop",
			Path:     FileName,
			Deps:     []Dep{{Name: "db", Image: "postgres:16", Port: 5432}},
			Services: []Service{{Name: "web", Run: "npm start", Port: 3000}},
		}
	}
	if err := ok().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	tests := []struct {
		name string
		mut  func(*App)
		want string
	}{
		{"duplicate service", func(a *App) { a.Services = append(a.Services, a.Services[0]) }, "duplicate service"},
		{"empty service name", func(a *App) { a.Services[0].Name = "" }, "needs a name"},
		{"bad service name", func(a *App) { a.Services[0].Name = "Web" }, "invalid service name"},
		{"name shared with a dep", func(a *App) { a.Services[0].Name = "db" }, "also a dependency"},
		{"image and build", func(a *App) {
			a.Services[0].Image = "node:22"
			a.Services[0].Build = &Build{Context: "."}
		}, "mutually exclusive"},
		{"install and build", func(a *App) {
			a.Services[0].Build = &Build{Context: "."}
			a.Services[0].Install = "npm ci"
		}, "mutually exclusive"},
		{"an image that is a flag", func(a *App) { a.Services[0].Image = "--privileged" }, "does not start with"},
		{"an image with a shell in it", func(a *App) { a.Services[0].Image = "node:22; rm -rf /" }, "not an image reference"},
		{"negative port", func(a *App) { a.Services[0].Port = -2 }, "out of range"},
		{"bad protocol", func(a *App) { a.Services[0].Protocol = "sctp" }, "unknown protocol"},
		{"health both forms", func(a *App) {
			a.Services[0].Health = &Health{Path: "/x", Command: []string{"true"}}
		}, "not both"},
		{"health neither form", func(a *App) { a.Services[0].Health = &Health{} }, "must be an HTTP path"},
		{"health path without a slash", func(a *App) { a.Services[0].Health = &Health{Path: "healthz"} }, "starts with"},
		{"empty health argument", func(a *App) { a.Services[0].Health = &Health{Command: []string{"curl", ""}} }, "must not be empty"},
		{"bad reference in a service", func(a *App) {
			a.Services[0].Env = map[string]string{"X": "${deps.nope.port}"}
		}, "unknown reference"},
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
}

func TestNilApp(t *testing.T) {
	var nilApp *App
	if _, ok := nilApp.Service("web"); ok {
		t.Error("nil app has a service")
	}
	if _, ok := nilApp.FirstService(); ok {
		t.Error("nil app has a first service")
	}
}

func TestServicePorts(t *testing.T) {
	tests := []struct {
		name    string
		s       Service
		want    int
		hasPort bool
	}{
		{"written", Service{Port: 3000}, 3000, true},
		{"unwritten", Service{}, 20000, true},
		{"none", Service{Port: PortNone}, 0, false},
	}
	for _, tt := range tests {
		if got := tt.s.ContainerPort(20000); got != tt.want {
			t.Errorf("%s: ContainerPort(20000) = %d, want %d", tt.name, got, tt.want)
		}
		if got := tt.s.HasPort(); got != tt.hasPort {
			t.Errorf("%s: HasPort() = %v, want %v", tt.name, got, tt.hasPort)
		}
	}
}

func TestServiceEnv(t *testing.T) {
	app := &App{
		Env:      map[string]string{"LOG_LEVEL": "info", "DATABASE_URL": "postgres://db"},
		Services: []Service{{Name: "web", Env: map[string]string{"LOG_LEVEL": "debug"}}},
	}
	got := app.ServiceEnv(app.Services[0])
	want := map[string]string{"LOG_LEVEL": "debug", "DATABASE_URL": "postgres://db"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ServiceEnv = %v, want %v", got, want)
	}
	got["LOG_LEVEL"] = "trace"
	if app.Env["LOG_LEVEL"] != "info" || app.Services[0].Env["LOG_LEVEL"] != "debug" {
		t.Error("ServiceEnv shares its map with the config")
	}
	if got := app.ServiceEnv(Service{Name: "worker"}); !reflect.DeepEqual(got, app.Env) {
		t.Errorf("ServiceEnv(no vars of its own) = %v, want the app's %v", got, app.Env)
	}
	var nilApp *App
	if got := nilApp.ServiceEnv(Service{Env: map[string]string{"X": "1"}}); !reflect.DeepEqual(got, map[string]string{"X": "1"}) {
		t.Errorf("nil app ServiceEnv = %v", got)
	}
}

func TestParseProtocol(t *testing.T) {
	for in, want := range map[string]Protocol{"": ProtocolTCP, "tcp": ProtocolTCP, "udp": ProtocolUDP} {
		got, err := ParseProtocol(in)
		if err != nil || got != want {
			t.Errorf("ParseProtocol(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	if _, err := ParseProtocol("sctp"); err == nil {
		t.Error("ParseProtocol(sctp) accepted")
	}
}
