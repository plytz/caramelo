//go:build integration

package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	capi "github.com/plytz/caramelo/internal/api"
	cprogress "github.com/plytz/caramelo/internal/progress"
	cvault "github.com/plytz/caramelo/internal/vault"
	"github.com/plytz/caramelo/test/integration/itest"
)

func TestZZVaultOverTheAPI(t *testing.T) {
	_, o := box(t)
	const name = "LAB_API_TOKEN"
	const value = "a-value-no-argv-may-carry"

	set := itest.SSHAPIRun(t, withStdin(o, name+"="+value+"\n"),
		"secrets", "set", "--machine-scope", "--stdin", "--json")
	if set.ExitCode != 0 {
		t.Fatalf("secrets set --machine-scope: exit %d\nstdout:%s\nstderr:%s",
			set.ExitCode, set.Stdout, set.Stderr)
	}
	var written capi.VaultResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(set.Stdout)), &written); err != nil {
		t.Fatalf("secrets set --json: %v (%q)", err, set.Stdout)
	}
	if !contains(written.Changed, name) {
		t.Errorf("secrets set wrote %v, want %s", written.Changed, name)
	}
	t.Cleanup(func() {
		opts := o
		opts.Timeout = time.Minute
		itest.SSHAPIRun(t, opts, "secrets", "rm", "--machine-scope", name, "--json")
	})

	t.Run("the answer carries no value", func(t *testing.T) {
		if strings.Contains(set.Stdout, value) {
			t.Errorf("secrets set printed the value:\n%s", set.Stdout)
		}
		list := itest.SSHAPIRun(t, o, "secrets", "list", "--machine-scope", "--json")
		if list.ExitCode != 0 {
			t.Fatalf("secrets list --machine: exit %d\nstderr:%s", list.ExitCode, list.Stderr)
		}
		if strings.Contains(list.Stdout, value) {
			t.Errorf("secrets list printed the value:\n%s", list.Stdout)
		}
		var out capi.VaultResult
		if err := json.Unmarshal([]byte(strings.TrimSpace(list.Stdout)), &out); err != nil {
			t.Fatalf("secrets list --json: %v (%q)", err, list.Stdout)
		}
		var found bool
		for _, e := range out.Entries {
			if e.Name == name {
				found = true
				if e.Scope != cvault.ScopeMachine {
					t.Errorf("%s is at scope %q, want %q", name, e.Scope, cvault.ScopeMachine)
				}
				if e.Version < 1 {
					t.Errorf("%s has version %d", name, e.Version)
				}
			}
		}
		if !found {
			t.Errorf("%s is not in the machine's scope: %+v", name, out.Entries)
		}
	})

	t.Run("a second write is a new version", func(t *testing.T) {
		res := itest.SSHAPIRun(t, withStdin(o, name+"=another\n"),
			"secrets", "set", "--machine-scope", "--stdin", "--json")
		if res.ExitCode != 0 {
			t.Fatalf("the second write: exit %d\nstderr:%s", res.ExitCode, res.Stderr)
		}
		var out capi.VaultResult
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &out); err != nil {
			t.Fatalf("secrets set --json: %v", err)
		}
		for _, e := range out.Entries {
			if e.Name == name && e.Version < 2 {
				t.Errorf("%s is still version %d after a second write", name, e.Version)
			}
		}
	})

	t.Run("and the daemon says who wrote it", func(t *testing.T) {
		list := itest.SSHAPIRun(t, o, "secrets", "list", "--machine-scope", "--json")
		var out capi.VaultResult
		if err := json.Unmarshal([]byte(strings.TrimSpace(list.Stdout)), &out); err != nil {
			t.Fatal(err)
		}
		for _, e := range out.Entries {
			if e.Name == name && e.UpdatedBy == "" {
				t.Errorf("%s has no updated_by: a write over the API is made by an identity", name)
			}
		}
	})
}

func TestZZProgressFormat(t *testing.T) {
	_, o := box(t)

	t.Run("json is accepted", func(t *testing.T) {
		res := itest.SSHAPIRun(t, o, "status", "--progress", "json", "--json")
		if res.ExitCode != 0 {
			t.Fatalf("status --progress json: exit %d\nstderr:%s", res.ExitCode, res.Stderr)
		}
		var st capi.Status
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &st); err != nil {
			t.Fatalf("status --json with --progress json: %v (%q)", err, res.Stdout)
		}
		for _, line := range strings.Split(strings.TrimSpace(res.Stderr), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var e cprogress.Event
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Errorf("--progress json wrote a line that is not an event: %q", line)
			}
		}
	})

	t.Run("an unknown format is a usage error", func(t *testing.T) {
		res := itest.SSHAPIRun(t, o, "status", "--progress", "yaml")
		if res.ExitCode != 2 {
			t.Errorf("exit = %d, want 2 (usage)\nstdout:%s\nstderr:%s",
				res.ExitCode, res.Stdout, res.Stderr)
		}
		if !strings.Contains(strings.ToLower(res.Stderr), "progress") {
			t.Errorf("the error does not name the flag: %q", res.Stderr)
		}
	})
}

func TestZZEventsOverBothTransports(t *testing.T) {
	m, o := box(t)
	const seeded = "ITEST_EVENT_SEED"

	set := itest.SSHAPIRun(t, withStdin(o, seeded+"=seeded\n"),
		"secrets", "set", "--machine-scope", "--stdin", "--json")
	if set.ExitCode != 0 {
		t.Fatalf("seed the feed with a vault write: exit %d\nstdout:%s\nstderr:%s",
			set.ExitCode, set.Stdout, set.Stderr)
	}
	t.Cleanup(func() {
		itest.SSHAPIRun(t, o, "secrets", "rm", "--machine-scope", seeded, "--json")
	})

	check := func(t *testing.T, what, stdout string) {
		t.Helper()
		trimmed := strings.TrimSpace(stdout)
		if trimmed == "" {
			t.Fatalf("%s: the feed is empty although a vault write was just audited", what)
		}
		if strings.HasPrefix(trimmed, "[") {
			t.Errorf("%s answered a JSON array; the feed is one object per line because a "+
				"followed feed has no end:\n%s", what, trimmed)
		}
		var sawTheWrite bool
		for i, line := range strings.Split(trimmed, "\n") {
			var e cprogress.Event
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Errorf("%s: line %d is not an event: %v\n%s", what, i+1, err, line)
				continue
			}
			if e.Action == "secrets" && strings.Contains(e.Detail, seeded) {
				sawTheWrite = true
			}
		}
		if !sawTheWrite {
			t.Errorf("%s: the vault write of %s is not in the feed:\n%s", what, seeded, trimmed)
		}
	}

	t.Run("over ssh", func(t *testing.T) {
		res := itest.SSHAPIRun(t, o, "events", "--limit", "20", "--json")
		if res.ExitCode != 0 {
			t.Fatalf("events: exit %d\nstderr:%s", res.ExitCode, res.Stderr)
		}
		check(t, "events over ssh", res.Stdout)
	})

	t.Run("over the local socket", func(t *testing.T) {
		res := itest.MustRunAsUser(t, m, itest.CarameloUser,
			itest.CarameloBinary+" events --limit 20 --json")
		if res.ExitCode != 0 {
			t.Fatalf("events on the box: exit %d\nstderr:%s", res.ExitCode, res.Stderr)
		}
		check(t, "events over the socket", res.Stdout)
	})
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
