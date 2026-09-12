package vault

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestScopeOrder(t *testing.T) {
	if !reflect.DeepEqual(Scopes, []Scope{ScopeDefault, ScopeMachine, ScopeApp, ScopeEnv}) {
		t.Fatalf("Scopes = %v, want them widest first", Scopes)
	}

	if !reflect.DeepEqual(WritableScopes, []Scope{ScopeMachine, ScopeApp, ScopeEnv}) {
		t.Fatalf("WritableScopes = %v", WritableScopes)
	}
	if !ScopeMachine.Beats(ScopeDefault) {
		t.Error("a stored secret did not beat the published default")
	}
	if _, err := ParseScope("default"); err == nil {
		t.Error("`default` was accepted as a scope to write at")
	}
	if !ScopeEnv.Beats(ScopeApp) || !ScopeApp.Beats(ScopeMachine) || !ScopeEnv.Beats(ScopeMachine) {
		t.Error("the narrower scope does not win")
	}
	if ScopeMachine.Beats(ScopeApp) || ScopeApp.Beats(ScopeEnv) || ScopeEnv.Beats(ScopeEnv) {
		t.Error("a wider or equal scope won")
	}
	if Scope("nonsense").Rank() != -1 {
		t.Error("an unknown scope has a rank")
	}
}

func TestParseScope(t *testing.T) {
	for in, want := range map[string]Scope{
		"machine": ScopeMachine, "app": ScopeApp, "env": ScopeEnv, " ENV ": ScopeEnv,
	} {
		got, err := ParseScope(in)
		if err != nil || got != want {
			t.Errorf("ParseScope(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	for _, bad := range []string{"", "global", "production", "all"} {
		if _, err := ParseScope(bad); err == nil {
			t.Errorf("ParseScope(%q) was accepted", bad)
		}
	}
}

func TestRefValidate(t *testing.T) {
	for _, r := range []Ref{
		MachineRef("GREETING"),
		AppRef("shop", "STRIPE_KEY"),
		EnvRef("shop", "production", "DB_PASSWORD"),
	} {
		if err := r.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v", r, err)
		}
	}
	for _, r := range []Ref{
		{Scope: ScopeMachine, App: "shop", Name: "K"},
		{Scope: ScopeMachine, Env: "production", Name: "K"},
		{Scope: ScopeApp, Name: "K"},
		{Scope: ScopeApp, App: "shop", Env: "production", Name: "K"},
		{Scope: ScopeEnv, App: "shop", Name: "K"},
		{Scope: ScopeEnv, Env: "production", Name: "K"},
		{Scope: "global", Name: "K"},
		{Scope: ScopeMachine},
		{Scope: ScopeMachine, Name: "lower_case"},
	} {
		if err := r.Validate(); err == nil {
			t.Errorf("Validate(%+v) was accepted", r)
		}
	}
}

func TestRefWhere(t *testing.T) {
	for _, tc := range []struct {
		r    Ref
		want string
	}{
		{MachineRef("K"), "machine"},
		{AppRef("shop", "K"), "app shop"},
		{EnvRef("shop", "production", "K"), "env production"},
	} {
		if got := tc.r.Where(); got != tc.want {
			t.Errorf("Where(%+v) = %q, want %q", tc.r, got, tc.want)
		}
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"K", "DB_PASSWORD", "STRIPE_KEY", "API_KEY_2", "_PRIVATE"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "db_password", "DB-PASSWORD", "2FA", "DB PASSWORD", "DB.PASS", "ключ"} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) was accepted", bad)
		}
	}

	err := ValidateName("2FA_TOKEN")
	if err == nil || !strings.Contains(err.Error(), "starts with a digit") {
		t.Errorf("ValidateName(\"2FA_TOKEN\") = %v", err)
	}
	if err := ValidateName(strings.Repeat("A", MaxNameLen+1)); err == nil {
		t.Error("an over-long name was accepted")
	}
}

func TestValidateValue(t *testing.T) {
	if err := ValidateValue("K", strings.Repeat("x", MaxValueLen)); err != nil {
		t.Errorf("a value at the limit was refused: %v", err)
	}
	if err := ValidateValue("K", strings.Repeat("x", MaxValueLen+1)); err == nil {
		t.Error("an over-long value was accepted")
	}

	if err := ValidateValue("K", ""); err != nil {
		t.Errorf("an empty value was refused: %v", err)
	}
}

func TestRedaction(t *testing.T) {
	if Redacted != "<secret>" {
		t.Errorf("Redacted = %q", Redacted)
	}
	e := Entry{Ref: EnvRef("shop", "production", "DB_PASSWORD"), Value: "chosen", Version: 2}
	r := e.Redact()
	if r.Value != "" {
		t.Errorf("Redact left %q", r.Value)
	}
	if r.Version != 2 || r.Name != "DB_PASSWORD" {
		t.Errorf("Redact lost something: %+v", r)
	}

	if e.Value != "chosen" {
		t.Error("Redact mutated its receiver")
	}
}

func TestSort(t *testing.T) {
	in := []Entry{
		{Ref: EnvRef("shop", "production", "GREETING")},
		{Ref: MachineRef("GREETING")},
		{Ref: EnvRef("shop", "production", "DB_PASSWORD")},
		{Ref: AppRef("shop", "STRIPE_KEY")},
	}
	Sort(in)
	var got [][2]string
	for _, e := range in {
		got = append(got, [2]string{string(e.Scope), e.Name})
	}
	want := [][2]string{
		{"machine", "GREETING"},
		{"app", "STRIPE_KEY"},
		{"env", "DB_PASSWORD"},
		{"env", "GREETING"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Sort = %v, want %v", got, want)
	}
}

func TestKeyPathAndModes(t *testing.T) {
	if got := KeyPath("/var/lib/caramelo"); got != "/var/lib/caramelo/vault.key" {
		t.Errorf("KeyPath = %q", got)
	}
	if got := SecretsDir("/run/caramelo"); got != "/run/caramelo/secrets" {
		t.Errorf("SecretsDir = %q", got)
	}
	if KeyMode != 0o600 || SecretsFileMode != 0o600 || SecretsDirMode != 0o700 {
		t.Error("a vault file or directory is readable by somebody else")
	}
	if KeySize != 32 {
		t.Errorf("KeySize = %d, want 32 (AES-256)", KeySize)
	}
}

func TestNotImplemented(t *testing.T) {
	ctx := context.Background()
	var s Store = NotImplemented{}
	ref := EnvRef("shop", "production", "DB_PASSWORD")

	if _, err := s.Set(ctx, ref, "chosen"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Set = %v", err)
	} else if !strings.Contains(err.Error(), "env production") || !strings.Contains(err.Error(), "DB_PASSWORD") {
		t.Errorf("Set error %q does not say what it could not do", err)
	}
	if _, err := s.Get(ctx, ref); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Get = %v", err)
	}
	if _, err := s.List(ctx, "shop", "production"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("List = %v", err)
	}
	if err := s.Remove(ctx, ref); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Remove = %v", err)
	}
	if _, err := s.Resolve(ctx, "shop", "production"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Resolve = %v", err)
	}
	if _, err := s.Sources(ctx, "shop", "production"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Sources = %v", err)
	}

	var c Cipher = NotImplemented{}
	if _, _, err := c.Seal([]byte("x")); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Seal = %v", err)
	}
	if _, err := c.Open([]byte("x"), []byte("n")); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Open = %v", err)
	}
}

func TestEntryCarriesItsRef(t *testing.T) {
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	e := Entry{Ref: AppRef("shop", "STRIPE_KEY"), Version: 3, UpdatedBy: "alex@laptop", UpdatedAt: at}
	if e.Scope != ScopeApp || e.App != "shop" || e.Name != "STRIPE_KEY" {
		t.Errorf("entry = %+v", e)
	}
	if err := e.Validate(); err != nil {
		t.Errorf("an entry's own reference does not validate: %v", err)
	}
}
