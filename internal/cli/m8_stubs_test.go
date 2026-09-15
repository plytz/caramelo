package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestM8CommandsAreRegistered(t *testing.T) {
	root := NewRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, path := range [][]string{
		{"machine", "add"}, {"machine", "token"}, {"machine", "join"},
		{"machine", "list"}, {"machine", "show"}, {"machine", "remove"},
		{"env", "handoff"},
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

func TestM8LocalAndCommanderCommands(t *testing.T) {
	root := NewRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, tc := range []struct {
		path      []string
		commander bool
	}{
		{[]string{"machine", "add"}, false},
		{[]string{"machine", "join"}, false},
		{[]string{"machine", "token"}, true},
		{[]string{"machine", "list"}, true},
		{[]string{"machine", "show"}, true},
		{[]string{"machine", "remove"}, true},
		{[]string{"env", "handoff"}, true},
	} {
		cmd, _, err := root.Find(tc.path)
		if err != nil {
			t.Fatalf("%v: %v", tc.path, err)
		}
		if got := isCommander(cmd); got != tc.commander {
			t.Errorf("caramelo %s: forwarded = %v, want %v", cmd.CommandPath(), got, tc.commander)
		}
	}

	cmd, _, err := root.Find([]string{"machine"})
	if err != nil || !isCommander(cmd) {
		t.Errorf("the machine group stopped being a commander group (%v)", err)
	}
}

func TestM8StubFlags(t *testing.T) {
	root := NewRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, tc := range []struct {
		path []string
		flag string
	}{
		{[]string{"machine", "add"}, "name"},
		{[]string{"machine", "add"}, "edge"},
		{[]string{"machine", "add"}, "private"},
		{[]string{"machine", "add"}, "binary"},
		{[]string{"machine", "token"}, "ttl"},
		{[]string{"machine", "join"}, "token"},
		{[]string{"machine", "join"}, "name"},
		{[]string{"machine", "join"}, "private"},
		{[]string{"machine", "remove"}, "force"},
		{[]string{"machine", "remove"}, "yes"},
		{[]string{"env", "handoff"}, "to"},
		{[]string{"env", "create"}, "on"},
		{[]string{"env", "expose"}, "via"},
		{[]string{"env", "list"}, "mine"},
		{[]string{"env", "list"}, "all"},

		{[]string{"server", "setup"}, "binary"},
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

func TestTheTwoLocalFleetCommandsRefuseBeforeTheyTouchAnything(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"machine", "join", "hub.example.com", "--token", "t"}, ExitUsage, "caramelo-join-v1."},
		{[]string{"machine", "join", "hub.example.com"}, ExitUsage, "needs a token"},
		{[]string{"machine", "add", "admin@nx2.local"}, ExitError, "take a join token from the hub"},
		{[]string{"machine", "add", "admin@nx2.local", "--edge", "--private"}, ExitUsage, "opposites"},
	} {
		var stdout, stderr bytes.Buffer
		code := Run(tc.args, &stdout, &stderr)
		if code != tc.code {
			t.Errorf("caramelo %s: exit %d, want %d", strings.Join(tc.args, " "), code, tc.code)
		}
		if !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("caramelo %s: stderr = %q, want it to mention %q",
				strings.Join(tc.args, " "), stderr.String(), tc.want)
		}
	}
}

func TestHandoffNeedsAPeer(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"env", "handoff", "feat-x", "--app", "shop"}, &stdout, &stderr); code != ExitUsage {
		t.Errorf("exit = %d, want %d (usage)", code, ExitUsage)
	}
	if !strings.Contains(stderr.String(), "--to") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestExposeViaIsValidatedHere(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"env", "expose", "feat-x", "--via", "sideways", "--app", "shop"},
		&stdout, &stderr); code != ExitUsage {
		t.Errorf("exit = %d, want %d (usage)", code, ExitUsage)
	}
	if !strings.Contains(stderr.String(), "unknown via") {
		t.Errorf("stderr = %q", stderr.String())
	}
}
