package stack

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestManifestsIn(t *testing.T) {
	for _, tt := range []struct {
		dir         string
		stack       string
		want        []string
		needsSource bool
	}{

		{dir: "go", stack: Go, want: []string{"go.mod"}},
		{dir: "node", stack: Node, want: []string{"package.json", "package-lock.json"}},
		{dir: "node-pnpm", stack: Node, want: []string{"package.json", "pnpm-lock.yaml"}},
		{dir: "node-yarn", stack: Node, want: []string{"package.json", "yarn.lock"}},

		{dir: "node-nolock", stack: Node, want: []string{"package.json"}},

		{dir: "python", stack: Python, want: []string{"requirements.txt"}},

		{dir: "django", stack: Python, want: []string{"pyproject.toml"}, needsSource: true},
		{dir: "rails", stack: Rails, want: []string{"Gemfile", ".ruby-version"}},

		{dir: "rails-gemspec", stack: Rails,
			want: []string{"Gemfile", "Gemfile.lock"}, needsSource: true},

		{dir: "docker", stack: Docker},
		{dir: "none", stack: Go},

		{dir: "go", stack: "fortran"},
	} {
		t.Run(tt.dir+"/"+tt.stack, func(t *testing.T) {
			got, err := ManifestsIn(tt.stack, filepath.Join("testdata", tt.dir))
			if err != nil {
				t.Fatalf("ManifestsIn: %v", err)
			}
			if !reflect.DeepEqual(got.Files, tt.want) {
				t.Errorf("files = %v, want %v", got.Files, tt.want)
			}
			if got.NeedsSource != tt.needsSource {
				t.Errorf("NeedsSource = %v, want %v", got.NeedsSource, tt.needsSource)
			}
			if got.Empty() != (len(tt.want) == 0) {
				t.Errorf("Empty = %v with files %v", got.Empty(), got.Files)
			}
		})
	}
}

func TestManifestsAreDeclaredInCopyOrder(t *testing.T) {
	if got := Manifests(Go); !reflect.DeepEqual(got, []string{"go.mod", "go.sum"}) {
		t.Errorf("Manifests(go) = %v", got)
	}
	if got := Manifests(Docker); len(got) != 0 {
		t.Errorf("Manifests(docker) = %v, want none", got)
	}
	first := Manifests(Node)
	first[0] = "clobbered"
	if second := Manifests(Node); second[0] != "package.json" {
		t.Errorf("the caller mutated the table: %v", second)
	}

	for _, s := range []string{Go, Node, Python, Rails} {
		if len(Manifests(s)) == 0 {
			t.Errorf("the %s stack declares no manifests", s)
		}
	}
}

func TestManifestsIgnoreDirectoriesAndUnreadableFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "go.mod"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ManifestsIn(Go, dir)
	if err != nil {
		t.Fatalf("ManifestsIn: %v", err)
	}
	if !reflect.DeepEqual(got.Files, []string{"go.sum"}) {
		t.Errorf("files = %v, want only go.sum", got.Files)
	}
}

func TestManifestsInEmptyDir(t *testing.T) {
	got, err := ManifestsIn(Node, t.TempDir())
	if err != nil {
		t.Fatalf("ManifestsIn: %v", err)
	}
	if !got.Empty() || got.NeedsSource {
		t.Errorf("= %+v", got)
	}
}
