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
		{"hub", "status"}, {"hub", "uninstall"}, {"hub", "run"}, {"hub", "probe"},
		{"fleet", "setup"},
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

func TestTheFleetGroupIsNotForwarded(t *testing.T) {
	root := NewRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, path := range [][]string{{"fleet"}, {"fleet", "setup"}, {"hub"}} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatalf("caramelo %s: %v", strings.Join(path, " "), err)
		}
		if isCommander(cmd) {
			t.Errorf("caramelo %s is forwarded to a daemon; setup acts on the machine it is typed on",
				cmd.CommandPath())
		}
	}
}

var retiredSpellings = [][]string{{"hub", "setup"}}

func TestTheOldSpellingsAreGone(t *testing.T) {
	root := NewRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, retired := range retiredSpellings {
		group, leaf := retired[:len(retired)-1], retired[len(retired)-1]
		var walk func(c *cobra.Command, path []string)
		walk = func(c *cobra.Command, path []string) {
			for _, sub := range c.Commands() {
				if equal(path, group) {
					for _, name := range append([]string{sub.Name()}, sub.Aliases...) {
						if name == leaf {
							t.Errorf("%q still answers to the retired spelling %q",
								sub.CommandPath(), strings.Join(retired, " "))
						}
					}
				}
				walk(sub, append(append([]string{}, path...), sub.Name()))
			}
		}
		walk(root, nil)

		var stdout, stderr bytes.Buffer
		if code := Run(retired, &stdout, &stderr); code == ExitOK {
			t.Errorf("caramelo %s still runs", strings.Join(retired, " "))
		}
	}
}

func TestExamplesUseTheRoleWords(t *testing.T) {
	retired := []string{"caramelo server", "caramelo machine"}
	for _, path := range retiredSpellings {
		retired = append(retired, "caramelo "+strings.Join(path, " "))
	}
	for path, ex := range examples {
		for _, old := range retired {
			for _, line := range strings.Split(ex, "\n") {
				line = strings.TrimSpace(line)
				if strings.Contains(line, old+" ") || line == old || strings.HasSuffix(line, " "+old) {
					t.Errorf("the examples of %q still show %q", path, old)
					break
				}
			}
			if strings.HasPrefix(path, old) {
				t.Errorf("examples are keyed on the retired command %q", path)
			}
		}
	}
}
