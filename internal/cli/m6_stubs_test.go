package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestM6CommandsAreRegistered(t *testing.T) {
	root := NewRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, path := range [][]string{
		{"edge"}, {"edge", "status"}, {"edge", "enable"}, {"edge", "disable"}, {"edge", "ca"},
		{"edge", "prune"},
		{"env", "expose"}, {"env", "unexpose"},
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
}

func TestM6EdgeCommandKinds(t *testing.T) {
	root := NewRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, tc := range []struct {
		path      []string
		commander bool
	}{
		{[]string{"edge", "status"}, true},
		{[]string{"edge", "ca"}, true},
		{[]string{"edge", "prune"}, true},
		{[]string{"edge", "enable"}, false},
		{[]string{"edge", "disable"}, false},
		{[]string{"edge"}, false},
	} {
		cmd, _, err := root.Find(tc.path)
		if err != nil {
			t.Fatalf("%v: %v", tc.path, err)
		}
		if got := isCommander(cmd); got != tc.commander {
			t.Errorf("caramelo %s: commander command = %v, want %v", cmd.CommandPath(), got, tc.commander)
		}
	}
}

func TestEdgeBoxCommandsRefuseOffAMachine(t *testing.T) {
	freshPlace(t)
	dir := t.TempDir()
	for _, tc := range []struct {
		args []string
		code int
		says string
	}{
		{[]string{"edge", "--config-dir", dir}, ExitError, "read the machine's configuration"},
		{[]string{"edge", "enable", "--config-dir", dir}, ExitUsage, "runs on (a hub or a member), as root"},
		{[]string{"edge", "disable", "--config-dir", dir}, ExitUsage, "runs on (a hub or a member), as root"},
	} {
		var stdout, stderr bytes.Buffer
		code := Run(tc.args, &stdout, &stderr)
		if code != tc.code {
			t.Errorf("caramelo %s: exit %d, want %d", strings.Join(tc.args, " "), code, tc.code)
		}
		if !strings.Contains(stderr.String(), tc.says) {
			t.Errorf("caramelo %s said %q, want it to mention %q",
				strings.Join(tc.args, " "), stderr.String(), tc.says)
		}
		if stdout.Len() != 0 {
			t.Errorf("caramelo %s wrote %q to stdout; diagnostics belong on stderr",
				strings.Join(tc.args, " "), stdout.String())
		}
	}
}

func TestM6StubArities(t *testing.T) {
	for _, args := range [][]string{
		{"edge", "status", "extra"},
		{"edge", "ca", "extra"},
		{"edge", "prune", "extra"},
		{"env", "expose"},
		{"env", "expose", "feat-x", "extra"},
		{"env", "unexpose"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != ExitUsage {
			t.Errorf("caramelo %s: exit %d, want %d (usage)", strings.Join(args, " "), code, ExitUsage)
		}
	}
}

func TestM6StubFlags(t *testing.T) {
	root := NewRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, tc := range []struct {
		path []string
		flag string
	}{
		{[]string{"env", "expose"}, "service"},
		{[]string{"env", "expose"}, "host"},
		{[]string{"env", "unexpose"}, "service"},
		{[]string{"env", "unexpose"}, "host"},
		{[]string{"edge", "enable"}, "acme-email"},
		{[]string{"edge", "enable"}, "acme-ca"},
		{[]string{"edge", "enable"}, "tls"},
		{[]string{"edge", "enable"}, "no-http3"},
		{[]string{"edge", "prune"}, "older-than"},
		{[]string{"edge", "prune"}, "dry-run"},
		{[]string{"logs"}, "edge"},
		{[]string{"fleet", "setup"}, "edge"},
		{[]string{"fleet", "setup"}, "acme-email"},
		{[]string{"fleet", "setup"}, "acme-ca"},
		{[]string{"fleet", "setup"}, "tls"},
		{[]string{"fleet", "setup"}, "no-http3"},
	} {
		cmd, _, err := root.Find(tc.path)
		if err != nil {
			t.Fatalf("%v: %v", tc.path, err)
		}
		if cmd.Flags().Lookup(tc.flag) == nil {
			t.Errorf("caramelo %s has no --%s", cmd.CommandPath(), tc.flag)
		}
	}
}
