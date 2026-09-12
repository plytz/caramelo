package stack

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectFixtures(t *testing.T) {
	tests := []struct {
		dir     string
		stack   string
		image   string
		build   string
		install string
		run     string
		test    string
		cache   string
	}{
		{
			dir: "go", stack: Go,
			image: "golang:1.23", install: "go mod download",
			run: "go run .", test: "go test ./...", cache: cacheGo,
		},
		{

			dir: "go-noversion", stack: Go,
			image: defaultGoImage, install: "go mod download",
			run: "go run .", test: "go test ./...", cache: cacheGo,
		},
		{
			dir: "node", stack: Node,
			image: "node:20", install: "npm ci",
			run: "npm start", test: "npm test", cache: cacheNPM,
		},
		{

			dir: "node-pnpm", stack: Node,
			image: "node:22", install: "pnpm install --frozen-lockfile",
			run: "pnpm run dev", test: "pnpm test", cache: cachePNPM,
		},
		{

			dir: "node-yarn", stack: Node,
			image: "node:22", install: "yarn install --frozen-lockfile",
			run: "", test: "", cache: cacheYarn,
		},
		{

			dir: "node-nolock", stack: Node,
			image: "node:22", install: "npm install",
			run: "npm start", test: "", cache: cacheNPM,
		},
		{

			dir: "node-broken", stack: Node,
			image: "node:22", install: "npm install",
			run: "", test: "", cache: cacheNPM,
		},
		{
			dir: "python", stack: Python,
			image: defaultPythonImage, install: "pip install -r requirements.txt",
			run: "", test: "pytest", cache: cachePip,
		},
		{
			dir: "django", stack: Python,
			image: defaultPythonImage, install: "pip install -e .",
			run: "python manage.py runserver 0.0.0.0:$PORT", test: "pytest", cache: cachePip,
		},
		{
			dir: "rails", stack: Rails,
			image: "ruby:3.2.2", install: "bundle install",
			run: "bin/rails server -b 0.0.0.0 -p $PORT", test: "bin/rails test", cache: cacheBundle,
		},
		{

			dir: "rails-with-node", stack: Rails,
			image: "ruby:" + defaultRubyVersion, install: "bundle install",
			run: "bin/rails server -b 0.0.0.0 -p $PORT", test: "bin/rails test", cache: cacheBundle,
		},
		{
			dir: "docker", stack: Docker,
			build: ".", run: "", test: "", cache: "",
		},
		{

			dir: "both", stack: Docker,
			build: ".", cache: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			g, err := Detect(filepath.Join("testdata", tt.dir))
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if g == nil {
				t.Fatalf("Detect returned nil, want stack %s", tt.stack)
			}
			got := map[string]string{
				"stack":   g.Stack,
				"image":   g.Image.Value,
				"build":   g.Build.Value,
				"install": g.Install.Value,
				"run":     g.Run.Value,
				"test":    g.Test.Value,
				"cache":   g.Cache,
			}
			want := map[string]string{
				"stack":   tt.stack,
				"image":   tt.image,
				"build":   tt.build,
				"install": tt.install,
				"run":     tt.run,
				"test":    tt.test,
				"cache":   tt.cache,
			}
			for k, w := range want {
				if got[k] != w {
					t.Errorf("%s = %q, want %q", k, got[k], w)
				}
			}

			for name, f := range map[string]Field{
				"image": g.Image, "build": g.Build, "install": g.Install,
				"run": g.Run, "test": g.Test,
			} {
				if f.Value != "" && f.Evidence == "" {
					t.Errorf("%s = %q with no evidence", name, f.Value)
				}
			}
			if g.Run.Evidence == "" {
				t.Error("run has no evidence: config show could not explain it")
			}
		})
	}
}

func TestDetectRecognisesNothing(t *testing.T) {
	g, err := Detect(filepath.Join("testdata", "none"))
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if g != nil {
		t.Fatalf("Detect = %+v, want nil for a checkout nothing recognises", g)
	}
}

func TestDetectUnreadableDirectoryIsAnError(t *testing.T) {
	if _, err := Detect(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("Detect on a missing directory returned no error")
	}
}

func TestDetectEvidenceQuotesTheFile(t *testing.T) {
	g, err := Detect(filepath.Join("testdata", "go"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "go.mod says go 1.23"; g.Image.Evidence != want {
		t.Errorf("image evidence = %q, want %q", g.Image.Evidence, want)
	}
	g, err = Detect(filepath.Join("testdata", "go-noversion"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "go.mod has no go directive"; g.Image.Evidence != want {
		t.Errorf("image evidence = %q, want %q", g.Image.Evidence, want)
	}
	g, err = Detect(filepath.Join("testdata", "python"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "no manage.py: only caramelo.yaml can say how to run this"; g.Run.Evidence != want {
		t.Errorf("run evidence = %q, want %q", g.Run.Evidence, want)
	}
}

func TestGemfileWithoutRailsIsNotRails(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "Gemfile", "source \"https://rubygems.org\"\ngem \"sinatra\"\n")
	g, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if g != nil {
		t.Fatalf("Detect = %+v, want nil: a Gemfile without rails is not a Rails app", g)
	}
}

func TestRubyVersion(t *testing.T) {
	tests := map[string]string{
		"3.3.4\n":      "3.3.4",
		"ruby-3.2.2":   "3.2.2",
		"  3.1  ":      "3.1",
		"jruby-9.4":    "",
		"truffleruby":  "",
		"3.3.4-dev":    "",
		"not a number": "",
	}
	for in, want := range tests {
		if got := rubyVersion(in); got != want {
			t.Errorf("rubyVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNodeEnginesRange(t *testing.T) {
	tests := map[string]string{
		`{"engines":{"node":">=22"}}`:     "node:22",
		`{"engines":{"node":"^20.1.0"}}`:  "node:20",
		`{"engines":{"node":"18.x"}}`:     "node:18",
		`{"engines":{"node":"lts/*"}}`:    "node:" + defaultNodeVersion,
		`{"scripts":{"start":"node ."}}`:  "node:" + defaultNodeVersion,
		`{"engines":{"node":"22.11.0"}}`:  "node:22",
		`{"engines":{"node":">=20 <23"}}`: "node:20",
	}
	for body, want := range tests {
		dir := t.TempDir()
		write(t, dir, "package.json", body)
		g, err := Detect(dir)
		if err != nil {
			t.Fatal(err)
		}
		if g == nil || g.Image.Value != want {
			t.Errorf("package.json %s: image = %v, want %q", body, g, want)
		}
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
