package vault

import (
	"reflect"
	"strings"
	"testing"
)

func TestBundleNamesAndRedacted(t *testing.T) {
	b := &Bundle{
		App: "shop", Env: "production", Machine: "hub",
		Values:  map[string]string{"STRIPE_KEY": "sk_live_1", "DB_PASSWORD": "hunter2"},
		Sources: map[string]Scope{"STRIPE_KEY": ScopeApp, "DB_PASSWORD": ScopeEnv},
	}
	if got := b.Names(); !reflect.DeepEqual(got, []string{"DB_PASSWORD", "STRIPE_KEY"}) {
		t.Errorf("Names() = %v", got)
	}
	red := b.Redacted()
	for name, v := range red.Values {
		if v != Redacted {
			t.Errorf("%s = %q, want %q", name, v, Redacted)
		}
	}
	if b.Values["DB_PASSWORD"] != "hunter2" {
		t.Error("Redacted modified the bundle it was given")
	}
	if red.Machine != "hub" || red.Sources["DB_PASSWORD"] != ScopeEnv {
		t.Error("Redacted dropped the provenance, which is the safe half")
	}
	if strings.Contains(strings.Join(red.Names(), " "), "hunter2") {
		t.Error("a name carried a value")
	}
	if (*Bundle)(nil).Redacted() != nil || (*Bundle)(nil).Names() != nil {
		t.Error("a nil bundle must survive both")
	}
}
