package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestM8CommandsAreRegistered(t *testing.T) {
	root := NewRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, path := range [][]string{
		{"member", "add"}, {"member", "token"}, {"member", "join"},
		{"member", "list"}, {"member", "show"}, {"member", "remove"},
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
		{[]string{"member", "add"}, false},
		{[]string{"member", "join"}, false},
		{[]string{"member", "token"}, true},
		{[]string{"member", "list"}, true},
		{[]string{"member", "show"}, true},
		{[]string{"member", "remove"}, true},
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

	cmd, _, err := root.Find([]string{"member"})
	if err != nil || !isCommander(cmd) {
		t.Errorf("the member group stopped being a commander group (%v)", err)
	}
}

func TestM8StubFlags(t *testing.T) {
	root := NewRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, tc := range []struct {
		path []string
		flag string
	}{
		{[]string{"member", "add"}, "name"},
		{[]string{"member", "add"}, "edge"},
		{[]string{"member", "add"}, "private"},
		{[]string{"member", "add"}, "binary"},
		{[]string{"member", "token"}, "ttl"},
		{[]string{"member", "join"}, "token"},
		{[]string{"member", "join"}, "name"},
		{[]string{"member", "join"}, "private"},
		{[]string{"member", "remove"}, "force"},
		{[]string{"member", "remove"}, "yes"},
		{[]string{"env", "handoff"}, "to"},
		{[]string{"env", "create"}, "on"},
		{[]string{"env", "expose"}, "via"},
		{[]string{"env", "list"}, "mine"},
		{[]string{"env", "list"}, "all"},

		{[]string{"hub", "setup"}, "binary"},
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
		where func(t *testing.T)
		args  []string
		code  int
		want  string
	}{
		{hubPlace, []string{"member", "join", "hub.example.com", "--token", "t"}, ExitUsage, "caramelo-join-v1."},
		{hubPlace, []string{"member", "join", "hub.example.com"}, ExitUsage, "needs a token"},
		{commanderPlace, []string{"member", "add", "admin@nx2.local"}, ExitError, "take a join token from the hub"},
		{commanderPlace, []string{"member", "add", "admin@nx2.local", "--edge", "--private"}, ExitUsage, "opposites"},
	} {
		tc.where(t)
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
	initializedCommander(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"env", "handoff", "feat-x", "--app", "shop"}, &stdout, &stderr); code != ExitUsage {
		t.Errorf("exit = %d, want %d (usage)", code, ExitUsage)
	}
	if !strings.Contains(stderr.String(), "--to") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestExposeViaIsValidatedHere(t *testing.T) {
	initializedCommander(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"env", "expose", "feat-x", "--via", "sideways", "--app", "shop"},
		&stdout, &stderr); code != ExitUsage {
		t.Errorf("exit = %d, want %d (usage)", code, ExitUsage)
	}
	if !strings.Contains(stderr.String(), "unknown via") {
		t.Errorf("stderr = %q", stderr.String())
	}
}
