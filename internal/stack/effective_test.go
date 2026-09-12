package stack

import (
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/config"
)

func settings(e *Effective) map[string]Setting {
	m := make(map[string]Setting, len(e.Settings))
	for _, s := range e.Settings {
		m[s.Key] = s
	}
	return m
}

func want(t *testing.T, e *Effective, key, value string, source Source) Setting {
	t.Helper()
	s, ok := settings(e)[key]
	if !ok {
		t.Fatalf("no setting %q; got %v", key, e.Settings)
	}
	if s.Value != value || s.Source != source {
		t.Errorf("%s = %q (%s), want %q (%s)", key, s.Value, s.Source, value, source)
	}
	return s
}

func TestMergeSynthesisesTheSingleServiceFromDetection(t *testing.T) {
	file := &config.App{Name: "shop"}
	g, err := Detect("testdata/go")
	if err != nil {
		t.Fatal(err)
	}
	e := Merge(file, g)

	if e.Stack != Go {
		t.Errorf("stack = %q, want %q", e.Stack, Go)
	}
	if e.Cache != cacheGo {
		t.Errorf("cache = %q, want %q", e.Cache, cacheGo)
	}
	if len(e.Config.Services) != 1 || e.Config.Services[0].Name != config.DefaultServiceName {
		t.Fatalf("services = %+v, want one called %q", e.Config.Services, config.DefaultServiceName)
	}
	svc := e.Config.Services[0]
	if svc.Image != "golang:1.23" || svc.Run != "go run ." || svc.Install != "go mod download" {
		t.Errorf("service = %+v, want the detected image, install and run", svc)
	}
	if e.Config.Test != "go test ./..." {
		t.Errorf("test = %q, want the detected one", e.Config.Test)
	}
	s := want(t, e, "services.web.image", "golang:1.23", SourceDetected)
	if s.Evidence != "go.mod says go 1.23" {
		t.Errorf("evidence = %q, want the go.mod directive", s.Evidence)
	}
	want(t, e, "services.web.run", "go run .", SourceDetected)
	want(t, e, "test", "go test ./...", SourceDetected)

	want(t, e, "services.web.port", PortReference, SourceDefault)
	want(t, e, "services.web.protocol", string(config.ProtocolTCP), SourceDefault)
	want(t, e, "services.web.health", "tcp connect", SourceDefault)
}

func TestMergeTheFileWinsFieldByField(t *testing.T) {
	file := &config.App{
		Name: "shop",
		Services: []config.Service{{
			Name:     "web",
			Run:      "go run ./cmd/server",
			Port:     8080,
			Protocol: config.ProtocolUDP,
			Health:   &config.Health{Path: "/healthz"},
		}},
		Test: "go test -race ./...",
	}
	g, err := Detect("testdata/go")
	if err != nil {
		t.Fatal(err)
	}
	e := Merge(file, g)

	want(t, e, "services.web.run", "go run ./cmd/server", SourceFile)
	want(t, e, "services.web.port", "8080", SourceFile)
	want(t, e, "services.web.protocol", "udp", SourceFile)
	want(t, e, "services.web.health", "GET /healthz", SourceFile)
	want(t, e, "test", "go test -race ./...", SourceFile)

	want(t, e, "services.web.image", "golang:1.23", SourceDetected)
	want(t, e, "services.web.install", "go mod download", SourceDetected)

	if file.Services[0].Image != "" || file.Test != "go test -race ./..." {
		t.Errorf("Merge modified the file's App: %+v", file)
	}
}

func TestMergeDockerStackBuildsFromTheRepo(t *testing.T) {
	g, err := Detect("testdata/docker")
	if err != nil {
		t.Fatal(err)
	}
	e := Merge(&config.App{Name: "sampleapp"}, g)

	if e.Stack != Docker {
		t.Fatalf("stack = %q, want docker", e.Stack)
	}
	svc := e.Config.Services[0]
	if svc.Build == nil || svc.Build.Context != "." {
		t.Fatalf("build = %+v, want the top-level context", svc.Build)
	}
	if svc.Image != "" {
		t.Errorf("image = %q, want empty: build and image are mutually exclusive", svc.Image)
	}
	s := want(t, e, "services.web.build", ".", SourceDetected)
	if s.Evidence != "Dockerfile at the top level" {
		t.Errorf("evidence = %q", s.Evidence)
	}

	run := want(t, e, "services.web.run", "", SourceDetected)
	if run.Evidence != "the image's own CMD" {
		t.Errorf("run evidence = %q", run.Evidence)
	}
	if _, ok := settings(e)["services.web.install"]; ok {
		t.Error("the docker stack reported an install command")
	}
}

func TestMergeExplainsAMissingRunCommand(t *testing.T) {

	g, err := Detect("testdata/python")
	if err != nil {
		t.Fatal(err)
	}
	e := Merge(&config.App{Name: "lib"}, g)

	if len(e.Config.Services) != 1 || e.Config.Services[0].Name != config.DefaultServiceName {
		t.Fatalf("services = %+v, want the one service up would plan", e.Config.Services)
	}
	run := want(t, e, "services.web.run", "", SourceDetected)
	if !strings.Contains(run.Evidence, "manage.py") {
		t.Errorf("run evidence = %q, want it to say what the repo has to write down", run.Evidence)
	}

	want(t, e, "test", "pytest", SourceDetected)

	e = Merge(&config.App{
		Name:     "lib",
		Services: []config.Service{{Name: "web", Run: "python app.py"}},
	}, g)
	want(t, e, "services.web.run", "python app.py", SourceFile)
	want(t, e, "services.web.image", defaultPythonImage, SourceDetected)
	want(t, e, "services.web.install", "pip install -r requirements.txt", SourceDetected)
}

func TestMergeWithoutAGuess(t *testing.T) {
	e := Merge(&config.App{
		Name:     "mystery",
		Services: []config.Service{{Name: "web"}},
	}, nil)

	if e.Stack != "" || e.Cache != "" {
		t.Errorf("stack = %q, cache = %q, want both empty", e.Stack, e.Cache)
	}
	s := want(t, e, "services.web.run", "", SourceDefault)
	if s.Evidence == "" {
		t.Error("a service with nothing to run must say what to do about it")
	}
}

func TestMergeNilFileAndNilGuess(t *testing.T) {
	e := Merge(nil, nil)
	if e.Config == nil {
		t.Fatal("Config is nil")
	}

	if len(e.Settings) != 1 || e.Settings[0].Key != "placement" {
		t.Errorf("settings = %v, want the placement default alone", e.Settings)
	}
}

func TestMergePortNoneAndUDPHaveNoHealthCheck(t *testing.T) {
	e := Merge(&config.App{
		Services: []config.Service{
			{Name: "worker", Run: "npm run worker", Port: config.PortNone},
			{Name: "echo", Run: "python echo.py", Protocol: config.ProtocolUDP},
		},
	}, nil)

	want(t, e, "services.worker.port", "none", SourceFile)
	s := want(t, e, "services.worker.health", "none", SourceDefault)
	if s.Evidence != "the service has no port" {
		t.Errorf("evidence = %q", s.Evidence)
	}
	s = want(t, e, "services.echo.health", "none", SourceDefault)
	if s.Evidence != "a UDP service has nothing to connect to" {
		t.Errorf("evidence = %q", s.Evidence)
	}

	for _, svc := range e.Config.Services {
		if svc.Health != nil {
			t.Errorf("%s: Health = %+v, want nil", svc.Name, svc.Health)
		}
	}
}

func TestMergeReportsDepsAndTheKnownImageTable(t *testing.T) {
	e := Merge(&config.App{
		Deps: []config.Dep{
			{Name: "db", Image: "postgres:16-alpine", Port: 5432},
			{Name: "cache", Image: "redis:7-alpine", Port: 16379},
		},
	}, nil)

	want(t, e, "deps.db.image", "postgres:16-alpine", SourceFile)
	s := want(t, e, "deps.db.port", "5432", SourceDefault)
	if s.Evidence != "postgres is a known image" {
		t.Errorf("evidence = %q", s.Evidence)
	}

	want(t, e, "deps.cache.port", "16379", SourceFile)
}

func TestMergeSettingsAreStablyOrdered(t *testing.T) {
	g, err := Detect("testdata/node")
	if err != nil {
		t.Fatal(err)
	}
	file := &config.App{
		Name: "shop",
		Deps: []config.Dep{{Name: "db", Image: "postgres:16", Port: 5432}},
	}
	first := Merge(file, g)
	for i := 0; i < 5; i++ {
		other := Merge(file, g)
		if len(other.Settings) != len(first.Settings) {
			t.Fatalf("length changed: %d then %d", len(first.Settings), len(other.Settings))
		}
		for j := range first.Settings {
			if other.Settings[j] != first.Settings[j] {
				t.Fatalf("setting %d changed: %+v then %+v", j, first.Settings[j], other.Settings[j])
			}
		}
	}

	keys := make([]string, len(first.Settings))
	for i, s := range first.Settings {
		keys[i] = s.Key
	}
	wantOrder := []string{
		"services.web.image", "services.web.install", "services.web.run",
		"services.web.port", "services.web.protocol", "services.web.health",
		"test", "deps.db.image", "deps.db.port", "placement",
	}
	if len(keys) != len(wantOrder) {
		t.Fatalf("keys = %v, want %v", keys, wantOrder)
	}
	for i := range keys {
		if keys[i] != wantOrder[i] {
			t.Fatalf("keys = %v, want %v", keys, wantOrder)
		}
	}
}

func TestRenderBuild(t *testing.T) {
	tests := []struct {
		build *config.Build
		want  string
	}{
		{&config.Build{Context: "."}, "."},
		{&config.Build{}, "."},
		{&config.Build{Context: "svc/api", Dockerfile: "Dockerfile.dev"}, "svc/api (Dockerfile.dev)"},
	}
	for _, tt := range tests {
		if got := renderBuild(tt.build); got != tt.want {
			t.Errorf("renderBuild(%+v) = %q, want %q", tt.build, got, tt.want)
		}
	}
}
