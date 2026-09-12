package env

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
)

func TestConfigDirOfAnEnvIsItsWorktree(t *testing.T) {
	h := newHarness(t)
	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x", From: "main"})

	dir, cleanup, err := h.m.ConfigDir(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	defer cleanup()
	if dir != e.Worktree {
		t.Errorf("dir = %q, want the env's worktree %q", dir, e.Worktree)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the worktree is not there: %v", err)
	}
}

func TestConfigDirWithoutAnEnvMaterialisesTheDefaultBranch(t *testing.T) {
	h := newHarness(t)
	h.m.Runner = treeRunner(map[string]string{
		"caramelo.yaml": "name: shop\n",
		"go.mod":        "module shop\n\ngo 1.23\n",
	}, []string{"internal"})

	dir, cleanup, err := h.m.ConfigDir(context.Background(), "shop", "")
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	defer cleanup()

	for name, want := range map[string]string{"caramelo.yaml": "name: shop\n", "go.mod": "module shop\n\ngo 1.23\n"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	info, err := os.Stat(filepath.Join(dir, "internal"))
	if err != nil || !info.IsDir() {
		t.Errorf("the subdirectory is not there as a directory: %v", err)
	}

	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("cleanup left %s behind: %v", dir, err)
	}
}

func TestConfigDirOfAnUnknownApp(t *testing.T) {
	h := newHarness(t)
	_, cleanup, err := h.m.ConfigDir(context.Background(), "nosuchapp", "")
	cleanup()
	if err == nil || !strings.Contains(err.Error(), "push it to the machine first") {
		t.Errorf("err = %v, want it to name the fix", err)
	}
}

func treeRunner(blobs map[string]string, trees []string) runner.Runner {
	shas := map[string]string{}
	var listing strings.Builder
	for name, content := range blobs {
		sha := "blob-" + name
		shas[sha] = content
		listing.WriteString("100644 blob " + sha + " " + strconv.Itoa(len(content)) + "\t" + name + "\x00")
	}
	for _, name := range trees {
		listing.WriteString("040000 tree tree-" + name + "       -\t" + name + "\x00")
	}
	return runnerFunc(func(_ context.Context, c runner.Cmd) (runner.Result, error) {
		switch {
		case len(c.Args) > 0 && c.Args[0] == "ls-tree":
			return runner.Result{Stdout: listing.String()}, nil
		case len(c.Args) > 2 && c.Args[0] == "cat-file":
			return runner.Result{Stdout: shas[c.Args[2]]}, nil
		}
		return runner.Result{}, nil
	})
}
