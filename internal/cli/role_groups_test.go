package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRoleGroupsCarryEveryLeaf(t *testing.T) {
	root := NewRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, path := range [][]string{
		{"hub", "setup"}, {"hub", "status"}, {"hub", "uninstall"}, {"hub", "run"}, {"hub", "probe"},
		{"member", "add"}, {"member", "join"}, {"member", "leave"}, {"member", "list"},
		{"member", "remove"}, {"member", "show"}, {"member", "token"},
		{"member", "announce"}, {"member", "redeem"}, {"member", "removed"},
		{"task", "list"}, {"task", "show"}, {"task", "run"},
		{"commander", "init"}, {"commander", "write-config"}, {"vpn", "keygen"},
	} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Errorf("caramelo %s: %v", strings.Join(path, " "), err)
			continue
		}
		if got := strings.Fields(cmd.CommandPath()); !equal(got[1:], path) {
			t.Errorf("caramelo %s resolved to %q", strings.Join(path, " "), cmd.CommandPath())
		}
	}
}

func TestTheOldGroupNamesAreGone(t *testing.T) {
	root := NewRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			for _, name := range append([]string{sub.Name()}, sub.Aliases...) {
				if name == "server" || name == "machine" {
					t.Errorf("%q still answers to the old group name %q", sub.CommandPath(), name)
				}
			}
			walk(sub)
		}
	}
	walk(root)

	var stdout, stderr bytes.Buffer
	for _, args := range [][]string{{"server", "setup"}, {"machine", "list"}} {
		stdout.Reset()
		stderr.Reset()
		if code := Run(args, &stdout, &stderr); code == ExitOK {
			t.Errorf("caramelo %s still runs", strings.Join(args, " "))
		}
	}
}

func TestExamplesUseTheRoleWords(t *testing.T) {
	for path, ex := range examples {
		for _, old := range []string{"caramelo server ", "caramelo machine "} {
			if strings.Contains(ex, old) {
				t.Errorf("the examples of %q still show %q", path, strings.TrimSpace(old))
			}
		}
		if strings.HasPrefix(path, "caramelo server") || strings.HasPrefix(path, "caramelo machine") {
			t.Errorf("examples are keyed on the retired command %q", path)
		}
	}
}
