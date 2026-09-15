package remote

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/serverconfig"
)

func TestParseTarget(t *testing.T) {
	const (
		user = serverconfig.DefaultUser
		port = serverconfig.DefaultSSHPort
	)
	cases := []struct {
		in   string
		want Target
	}{
		{"box", Target{user, "box", port}},
		{" box ", Target{user, "box", port}},
		{"box.example.com", Target{user, "box.example.com", port}},
		{"alex@box", Target{"alex", "box", port}},
		{"box:2222", Target{user, "box", 2222}},
		{"alex@box:2222", Target{"alex", "box", 2222}},
		{"192.168.56.11", Target{user, "192.168.56.11", port}},
		{"192.168.56.11:22", Target{user, "192.168.56.11", 22}},
		{"::1", Target{user, "::1", port}},
		{"[::1]", Target{user, "::1", port}},
		{"[::1]:4022", Target{user, "::1", 4022}},
		{"alex@[fe80::1]:22", Target{"alex", "fe80::1", 22}},
	}
	for _, c := range cases {
		got, err := ParseTarget(c.in)
		if err != nil {
			t.Errorf("ParseTarget(%q) failed: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseTarget(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestParseTargetErrors(t *testing.T) {
	for _, in := range []string{"", "   ", "@box", "box:", "box:0", "box:70000", "box:ssh", "[::1", "[::1]x", "a b"} {
		if got, err := ParseTarget(in); err == nil {
			t.Errorf("ParseTarget(%q) = %+v, want an error", in, got)
		}
	}
}

func TestTargetString(t *testing.T) {
	tg := Target{"alex", "box", 4022}
	if got, want := tg.String(), "alex@box:4022"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := tg.Destination(), "alex@box"; got != want {
		t.Errorf("Destination() = %q, want %q", got, want)
	}
	v6 := Target{"caramelo", "fe80::1", 22}
	if got, want := v6.String(), "caramelo@[fe80::1]:22"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestQuoteRoundTripShape(t *testing.T) {
	cases := map[string]string{
		"env":            "env",
		"--json":         "--json",
		"":               "''",
		"a b":            "'a b'",
		"it's":           `'it'"'"'s'`,
		"$HOME":          "'$HOME'",
		"a;rm -rf /":     "'a;rm -rf /'",
		"20000-20002":    "20000-20002",
		"user@host:/tmp": "user@host:/tmp",
	}
	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJoinArgs(t *testing.T) {
	got := JoinArgs([]string{"env", "list", "--name", "a b"})
	want := "env list --name 'a b'"
	if got != want {
		t.Errorf("JoinArgs = %q, want %q", got, want)
	}
}

func TestCommanderConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	got, err := LoadCommanderConfigFrom(path)
	if err != nil {
		t.Fatalf("loading a missing config must not fail: %v", err)
	}
	if !reflect.DeepEqual(got, CommanderConfig{}) {
		t.Errorf("missing config = %+v, want zero", got)
	}

	want := CommanderConfig{
		DefaultMachine: "box",
		Machines:       map[string]string{"box": "alex@192.168.56.11:4022"},
	}
	if err := SaveCommanderConfigTo(path, want); err != nil {
		t.Fatal(err)
	}
	got, err = LoadCommanderConfigFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	if fi, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestCommanderConfigResolve(t *testing.T) {
	c := CommanderConfig{Machines: map[string]string{
		"box":  "alex@192.168.56.11:4022",
		"prod": "prod.example.com",
	}}
	got, err := c.Resolve("box")
	if err != nil {
		t.Fatal(err)
	}
	if want := (Target{"alex", "192.168.56.11", 4022}); got != want {
		t.Errorf("Resolve(box) = %+v, want %+v", got, want)
	}

	got, err = c.Resolve("other@host:22")
	if err != nil {
		t.Fatal(err)
	}
	if want := (Target{"other", "host", 22}); got != want {
		t.Errorf("Resolve(address) = %+v, want %+v", got, want)
	}
	if _, err := c.Resolve("@bad"); err == nil {
		t.Error("Resolve(@bad) should fail")
	}
}

func TestCommanderConfigResolveBadEntry(t *testing.T) {
	c := CommanderConfig{Machines: map[string]string{"box": "alex@box:notaport"}}
	_, err := c.Resolve("box")
	if err == nil {
		t.Fatal("want an error for a broken machines entry")
	}
	if !strings.Contains(err.Error(), "commander config") {
		t.Errorf("error = %v, want it to blame the commander config", err)
	}
}

func TestCommanderConfigPathFollowsXDG(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	got, err := CommanderConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "caramelo", "config.yaml"); got != want {
		t.Errorf("CommanderConfigPath() = %q, want %q", got, want)
	}
}
