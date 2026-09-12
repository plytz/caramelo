package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestM5CommandsAreRegistered(t *testing.T) {
	root := NewRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, path := range [][]string{
		{"vpn", "up"}, {"vpn", "down"}, {"vpn", "status"},
		{"vpn", "install"}, {"vpn", "uninstall"}, {"vpn", "config"},
		{"connect"},
		{"peer", "add"}, {"peer", "list"}, {"peer", "remove"},
		{"git-ssh"},
	} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Errorf("caramelo %s: %v", strings.Join(path, " "), err)
			continue
		}
		if got := strings.Fields(cmd.CommandPath()); !equal(got[1:], path) {
			t.Errorf("caramelo %s resolved to %q", strings.Join(path, " "), cmd.CommandPath())
		}
		if cmd.RunE == nil {
			t.Errorf("caramelo %s has no action", cmd.CommandPath())
		}
	}

	if cmd, _, err := root.Find([]string{"git-ssh"}); err == nil && !cmd.Hidden {
		t.Error("git-ssh is listed in help; it is only ever run by git")
	}
}

func TestM5StubArities(t *testing.T) {
	for _, args := range [][]string{
		{"peer", "add", "agent-7"},
		{"peer", "add", "agent-7", "key", "extra"},
		{"peer", "remove"},
		{"connect"},
		{"vpn", "up", "worker1"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != ExitUsage {
			t.Errorf("caramelo %s: exit %d, want %d (usage)", strings.Join(args, " "), code, ExitUsage)
		}
	}
}

func TestM5StubFlags(t *testing.T) {
	root := NewRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, tc := range []struct {
		path []string
		flag string
	}{
		{[]string{"vpn", "up"}, "name"},
		{[]string{"vpn", "up"}, "transparent"},
		{[]string{"vpn", "install"}, "interface"},
		{[]string{"connect"}, "listen"},
	} {
		cmd, _, err := root.Find(tc.path)
		if err != nil {
			t.Fatalf("%v: %v", tc.path, err)
		}
		if cmd.Flags().Lookup(tc.flag) == nil {
			t.Errorf("caramelo %s has no --%s", cmd.CommandPath(), tc.flag)
		}
	}

	if root.PersistentFlags().Lookup("json") == nil {
		t.Fatal("--json is missing")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
