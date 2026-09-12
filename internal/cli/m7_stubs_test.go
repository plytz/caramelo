package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestM7CommandsAreRegistered(t *testing.T) {
	root := NewRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, path := range [][]string{
		{"build"}, {"deploy"}, {"promote"}, {"rollback"}, {"releases"}, {"events"},
		{"secrets"}, {"secrets", "set"}, {"secrets", "list"}, {"secrets", "rm"}, {"secrets", "export"},
	} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Errorf("caramelo %s: %v", strings.Join(path, " "), err)
			continue
		}
		if got := strings.Fields(cmd.CommandPath()); !equal(got[1:], path) {
			t.Errorf("caramelo %s resolved to %q", strings.Join(path, " "), cmd.CommandPath())
		}
		if cmd.RunE == nil && cmd.Name() != "secrets" {
			t.Errorf("caramelo %s has no action", cmd.CommandPath())
		}
	}
}

func TestM7CommandsAreClientCommands(t *testing.T) {
	root := NewRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, path := range [][]string{
		{"build"}, {"deploy"}, {"promote"}, {"rollback"}, {"releases"}, {"events"},
		{"secrets", "set"}, {"secrets", "list"}, {"secrets", "rm"}, {"secrets", "export"},
	} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatalf("%v: %v", path, err)
		}
		if !isClient(cmd) {
			t.Errorf("caramelo %s is not a client command", cmd.CommandPath())
		}
	}
}

func TestM7StubFlags(t *testing.T) {
	root := NewRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, tc := range []struct {
		path []string
		flag string
	}{
		{[]string{"build"}, "ref"},
		{[]string{"build"}, "app"},
		{[]string{"deploy"}, "ref"},
		{[]string{"deploy"}, "no-watch"},
		{[]string{"deploy"}, "timeout"},
		{[]string{"deploy"}, "app"},
		{[]string{"deploy"}, "env"},
		{[]string{"rollback"}, "to"},
		{[]string{"releases"}, "limit"},
		{[]string{"events"}, "follow"},
		{[]string{"events"}, "since"},
		{[]string{"secrets", "set"}, "from-file"},

		{[]string{"secrets", "set"}, "machine-scope"},
		{[]string{"secrets", "set"}, "app-scope"},
		{[]string{"secrets", "export"}, "reveal"},
		{[]string{"secrets", "export"}, "format"},

		{[]string{"env", "create"}, "production"},
		{[]string{"env", "create"}, "release"},
		{[]string{"env", "create"}, "protected"},
		{[]string{"env", "create"}, "secrets-from"},

		{[]string{"env", "destroy"}, "force"},
	} {
		cmd, _, err := root.Find(tc.path)
		if err != nil {
			t.Fatalf("%v: %v", tc.path, err)
		}
		if cmd.Flags().Lookup(tc.flag) == nil {
			t.Errorf("caramelo %s has no --%s", cmd.CommandPath(), tc.flag)
		}
	}

	for _, path := range [][]string{
		{"secrets", "set"}, {"secrets", "list"}, {"secrets", "rm"}, {"secrets", "export"},
	} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, flag := range []string{"app", "env"} {
			if cmd.Flags().Lookup(flag) == nil && cmd.InheritedFlags().Lookup(flag) == nil {
				t.Errorf("caramelo %s cannot be given --%s", cmd.CommandPath(), flag)
			}
		}
	}
}

func TestProgressFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"version", "--progress", "yaml"}, &stdout, &stderr); code != ExitUsage {
		t.Errorf("exit = %d, want %d (usage)", code, ExitUsage)
	}
	if !strings.Contains(stderr.String(), "unknown progress format") {
		t.Errorf("stderr = %q", stderr.String())
	}
	for _, v := range []string{"text", "json", ""} {
		args := []string{"version"}
		if v != "" {
			args = append(args, "--progress", v)
		}
		stdout.Reset()
		stderr.Reset()
		if code := Run(args, &stdout, &stderr); code != ExitOK {
			t.Errorf("caramelo %v: exit %d (%s)", args, code, stderr.String())
		}
	}
}

func TestAppProgressWriter(t *testing.T) {
	var stderr bytes.Buffer
	a := &app{stdout: io.Discard, stderr: &stderr}
	if err := a.setProgress(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(a.progressWriter(), "[ok] health\n"); err != nil {
		t.Fatal(err)
	}
	if got := stderr.String(); got != "[ok] health\n" {
		t.Errorf("text progress = %q", got)
	}

	stderr.Reset()
	a.progressFlag = "json"
	if err := a.setProgress(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(a.progressWriter(), "[ok] health\n"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stderr.String(), `{"action":"message"`) {
		t.Errorf("json progress = %q", stderr.String())
	}
}

func TestRendererRegistry(t *testing.T) {
	before := registeredRenderers()
	registerRender("test renderer path", func(w io.Writer, result json.RawMessage) error {
		_, err := w.Write(result)
		return err
	})
	t.Cleanup(func() {
		renderers.mu.Lock()
		delete(renderers.m, "test renderer path")
		renderers.mu.Unlock()
	})

	if renderFor("caramelo test renderer path") == nil {
		t.Error("a path with the program name did not match")
	}
	if renderFor("  test   renderer  path  ") == nil {
		t.Error("a path with odd spacing did not match")
	}
	if renderFor("test renderer") != nil {
		t.Error("a prefix of a path matched")
	}

	var buf bytes.Buffer
	had, err := render("test renderer path", &buf, json.RawMessage(`{"ok":true}`))
	if err != nil || !had {
		t.Fatalf("render = %v, %v", had, err)
	}
	if buf.String() != `{"ok":true}` {
		t.Errorf("rendered %q", buf.String())
	}

	buf.Reset()
	if had, err := render("no such command", &buf, json.RawMessage(`{}`)); had || err != nil {
		t.Errorf("render of an unregistered path = %v, %v", had, err)
	}

	if got := len(registeredRenderers()); got != len(before)+1 {
		t.Errorf("%d renderers, want %d", got, len(before)+1)
	}
}

func TestRendererRegistryRefusesDuplicates(t *testing.T) {
	fn := func(io.Writer, json.RawMessage) error { return nil }
	registerRender("duplicate path", fn)
	t.Cleanup(func() {
		renderers.mu.Lock()
		delete(renderers.m, "duplicate path")
		renderers.mu.Unlock()
	})
	defer func() {
		if recover() == nil {
			t.Error("a second renderer for one command was accepted")
		}
	}()
	registerRender("duplicate path", fn)
}
