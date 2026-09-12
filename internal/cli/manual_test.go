package cli

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func manualRoot(t *testing.T) *cobra.Command {
	t.Helper()
	var out bytes.Buffer
	root := newRootCmd(&app{stdout: &out, stderr: &out})
	root.InitDefaultHelpCmd()
	return root
}

func visibleCommands(root *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, s := range manualChildren(c) {
			out = append(out, s)
			walk(s)
		}
	}
	walk(root)
	return out
}

func TestManualCoversEveryCommandInEveryFormat(t *testing.T) {
	root := manualRoot(t)
	cmds := visibleCommands(root)
	if len(cmds) < 60 {
		t.Fatalf("only %d commands found; the tree did not register", len(cmds))
	}
	m := buildManual(root)
	plain := renderPlain(m)
	md := renderManual(m, false)
	for _, c := range cmds {
		path := c.CommandPath()
		if !strings.Contains(plain, "\n"+strings.ToUpper(path)+"\n") {
			t.Errorf("plain manual lacks a section for %q", path)
		}
		if !strings.Contains(md, "\n## "+path+"\n") {
			t.Errorf("markdown manual lacks a heading for %q", path)
		}
		if strings.TrimSpace(c.Short) == "" {
			t.Errorf("%q has no Short description", path)
		}
	}
}

func TestManualCommandFormats(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"manual"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("manual: exit %d: %s", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "CARAMELO(1)") || !strings.Contains(stdout.String(), "\nNAME\n") {
		t.Errorf("plain manual does not look like a manual page:\n%s", firstLines(stdout.String(), 5))
	}
	if strings.Contains(stdout.String(), "\n## ") || strings.Contains(stdout.String(), "```") {
		t.Errorf("plain manual contains markdown syntax")
	}

	stdout.Reset()
	if code := Run([]string{"manual", "--markdown"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("manual --markdown: exit %d: %s", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "# caramelo manual\n") {
		t.Errorf("markdown manual starts with %q", firstLines(stdout.String(), 1))
	}

	stdout.Reset()
	if code := Run([]string{"manual", "--man"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("manual --man: exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `.TH CARAMELO 1 `) {
		t.Errorf("man output has no .TH header:\n%s", firstLines(stdout.String(), 3))
	}

	stdout.Reset()
	if code := Run([]string{"manual", "--markdown", "--man"}, &stdout, &stderr); code != ExitUsage {
		t.Errorf("manual --markdown --man: exit %d, want %d", code, ExitUsage)
	}

	stdout.Reset()
	if code := Run([]string{"manual", "--json"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("manual --json: exit %d: %s", code, stderr.String())
	}
	var m manual
	if err := json.Unmarshal(stdout.Bytes(), &m); err != nil {
		t.Fatalf("manual --json is not JSON: %v", err)
	}
	if len(m.Commands) == 0 || len(m.GlobalFlags) != 3 || m.ExitCodes["usage"] != ExitUsage {
		t.Errorf("manual --json = %d commands, %d global flags, exit codes %v", len(m.Commands), len(m.GlobalFlags), m.ExitCodes)
	}
}

var exampleCommand = regexp.MustCompile(`caramelo\s+([^#|)"']*)`)

func TestExamplesNameRealCommandsAndFlags(t *testing.T) {
	root := manualRoot(t)
	for path := range examples {
		if _, _, err := root.Find(strings.Fields(path)[1:]); err != nil {
			t.Errorf("examples has an entry for %q, which is not a command", path)
		}
	}
	var seen int
	for _, c := range visibleCommands(root) {
		if c.Example == "" {
			if len(c.Commands()) == 0 {
				t.Errorf("%q has no example", c.CommandPath())
			}
			continue
		}
		for _, line := range strings.Split(c.Example, "\n") {
			ms := exampleCommand.FindAllStringSubmatch(line, -1)
			if len(ms) == 0 {
				continue
			}
			seen++
			args := strings.Fields(ms[len(ms)-1][1])
			target, rest, err := root.Find(args)
			if err != nil {
				t.Errorf("%q example %q: %v", c.CommandPath(), line, err)
				continue
			}
			for _, tok := range rest {
				if tok == "--" {
					break
				}
				if !strings.HasPrefix(tok, "-") || tok == "-" {
					continue
				}
				name := strings.TrimLeft(strings.SplitN(tok, "=", 2)[0], "-")
				var found bool
				if strings.HasPrefix(tok, "--") {
					found = target.Flags().Lookup(name) != nil || target.InheritedFlags().Lookup(name) != nil
				} else {
					found = target.Flags().ShorthandLookup(name) != nil || target.InheritedFlags().ShorthandLookup(name) != nil
				}
				if !found {
					t.Errorf("%q example %q uses %s, which %q does not have", c.CommandPath(), strings.TrimSpace(line), tok, target.CommandPath())
				}
			}
		}
	}
	if seen < 100 {
		t.Errorf("only %d example lines checked", seen)
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
