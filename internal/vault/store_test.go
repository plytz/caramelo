package vault

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/state"
)

func TestCipherRoundTrip(t *testing.T) {
	c := testCipher(t)
	for _, plain := range []string{"", "sk_live_42", strings.Repeat("x", 4096), "πass\x00word\n"} {
		ct, nonce, err := c.Seal([]byte(plain))
		if err != nil {
			t.Fatalf("Seal(%q): %v", plain, err)
		}
		if plain != "" && bytes.Contains(ct, []byte(plain)) {
			t.Errorf("the ciphertext of %q contains the plaintext", plain)
		}
		out, err := c.Open(ct, nonce)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if string(out) != plain {
			t.Errorf("round trip gave %q, want %q", out, plain)
		}
	}
}

func TestCipherUsesAFreshNoncePerValue(t *testing.T) {
	c := testCipher(t)
	seen := map[string]bool{}
	var first []byte
	for i := 0; i < 64; i++ {
		ct, nonce, err := c.Seal([]byte("the same value every time"))
		if err != nil {
			t.Fatal(err)
		}
		if seen[string(nonce)] {
			t.Fatalf("a nonce was reused after %d values", i)
		}
		seen[string(nonce)] = true
		if len(nonce) != 12 {
			t.Errorf("the nonce is %d bytes", len(nonce))
		}
		if first == nil {
			first = ct
		} else if bytes.Equal(ct, first) {
			t.Fatal("two seals of one value produced the same ciphertext")
		}
	}
}

func TestCipherRefusesTamperedCiphertext(t *testing.T) {
	c := testCipher(t)
	ct, nonce, err := c.Seal([]byte("sk_live_42"))
	if err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), ct...)
	bad[0] ^= 0xff
	if _, err := c.Open(bad, nonce); err == nil {
		t.Error("a tampered ciphertext was opened")
	}
	if _, err := c.Open(ct, []byte("short")); err == nil {
		t.Error("a nonce of the wrong length was accepted")
	}

	other, err := NewCipher(bytes.Repeat([]byte{7}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(ct, nonce); err == nil {
		t.Error("a different key opened the value")
	} else if !strings.Contains(err.Error(), "machine's key") {
		t.Errorf("the error %q does not say why", err)
	}
}

func TestNewCipherRefusesTheWrongKeyLength(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33} {
		if _, err := NewCipher(make([]byte, n)); !errors.Is(err, ErrNoKey) {
			t.Errorf("NewCipher of %d bytes = %v", n, err)
		}
	}
}

func TestLoadKeyRefusesAReadableFile(t *testing.T) {
	dir := t.TempDir()
	path := KeyPath(dir)
	key, created, err := EnsureKey(path)
	if err != nil || !created || len(key) != KeySize {
		t.Fatalf("EnsureKey = %v, %v, %d bytes", err, created, len(key))
	}
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o666} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		_, err := LoadKey(path)
		if !errors.Is(err, ErrNoKey) {
			t.Errorf("mode %04o was accepted: %v", mode, err)
			continue
		}
		if !strings.Contains(err.Error(), "chmod 0600") {
			t.Errorf("the error for mode %04o does not say what to chmod: %v", mode, err)
		}
	}

	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(path); err != nil {
		t.Errorf("a 0400 key was refused: %v", err)
	}
}

func TestLoadKeyErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadKey(filepath.Join(dir, "nothing")); !errors.Is(err, ErrNoKey) {
		t.Errorf("a missing key = %v", err)
	} else if !strings.Contains(err.Error(), "server setup") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), KeyMode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, content := range map[string]string{
		"notbase64": "this is not base64 !!!",
		"tooshort":  "AAAA",
		"empty":     "",
	} {
		if _, err := LoadKey(write(name, content)); !errors.Is(err, ErrNoKey) {
			t.Errorf("%s was accepted: %v", name, err)
		} else if strings.Contains(err.Error(), content) && content != "" {
			t.Errorf("the error for %s quotes the file: %v", name, err)
		}
	}

	encoded, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, padded := range []string{encoded, encoded + "\n", "  " + encoded + "\n\n", encoded + "\r\n"} {
		if _, err := LoadKey(write("padded", padded)); err != nil {
			t.Errorf("a key with whitespace was refused: %v", err)
		}
	}
}

func TestEnsureKeyIsIdempotentAndNeverReplaces(t *testing.T) {
	path := KeyPath(t.TempDir())
	first, created, err := EnsureKey(path)
	if err != nil || !created {
		t.Fatalf("EnsureKey = %v, created %v", err, created)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != KeyMode {
		t.Errorf("the key was created mode %04o, want %04o", st.Mode().Perm(), KeyMode)
	}
	again, created, err := EnsureKey(path)
	if err != nil || created {
		t.Fatalf("the second EnsureKey = %v, created %v", err, created)
	}
	if !bytes.Equal(first, again) {
		t.Error("EnsureKey replaced the key")
	}

	if err := os.WriteFile(path, []byte("rubbish"), KeyMode); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureKey(path); !errors.Is(err, ErrNoKey) {
		t.Errorf("a broken key was repaired rather than reported: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "rubbish" {
		t.Errorf("the broken key was overwritten: %q, %v", raw, err)
	}
}

func TestPathsAgreeWithServerconfig(t *testing.T) {
	if KeyFile != serverconfig.VaultKeyFile {
		t.Errorf("KeyFile = %q, serverconfig says %q", KeyFile, serverconfig.VaultKeyFile)
	}
	if SecretsDirName != serverconfig.VaultSecretsDirName {
		t.Errorf("SecretsDirName = %q, serverconfig says %q", SecretsDirName, serverconfig.VaultSecretsDirName)
	}
	cfg := serverconfig.Config{StateDir: "/var/lib/caramelo", RunDir: "/run/caramelo"}
	if got, want := cfg.VaultKeyPath(), KeyPath("/var/lib/caramelo"); got != want {
		t.Errorf("VaultKeyPath = %q, want %q", got, want)
	}
	if got, want := cfg.SecretsDir(), SecretsDir("/run/caramelo"); got != want {
		t.Errorf("SecretsDir = %q, want %q", got, want)
	}
}

func TestStoreSetGetRemove(t *testing.T) {
	d, _ := testStore(t)
	ctx := WithTestIdentity(context.Background(), "alex@laptop")
	ref := EnvRef("shop", "production", "DB_PASSWORD")

	e, err := d.Set(ctx, ref, "chosen")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	if e.Value != "" {
		t.Errorf("Set returned the value %q", e.Value)
	}
	if e.Version != 1 || e.UpdatedBy != "alex@laptop" || e.UpdatedAt.IsZero() {
		t.Errorf("Set = %+v", e)
	}
	got, err := d.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Value != "chosen" {
		t.Errorf("Get = %q", got.Value)
	}

	if e, err = d.Set(ctx, ref, "second"); err != nil || e.Version != 2 {
		t.Errorf("the second Set = %+v, %v", e, err)
	}
	if got, _ = d.Get(ctx, ref); got.Value != "second" {
		t.Errorf("Get after the second Set = %q", got.Value)
	}

	if _, err := d.Get(ctx, AppRef("shop", "DB_PASSWORD")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get at a wider scope = %v, want ErrNotFound", err)
	}
	if err := d.Remove(ctx, ref); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := d.Remove(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing it twice = %v, want ErrNotFound", err)
	}
}

func TestStoreValidatesTheReference(t *testing.T) {
	d, rows := testStore(t)
	ctx := context.Background()
	for _, ref := range []Ref{
		{Scope: ScopeEnv, App: "shop", Name: "K"},
		{Scope: ScopeMachine, App: "shop", Name: "K"},
		{Scope: ScopeApp, Name: "K"},
		{Scope: "nonsense", Name: "K"},
		EnvRef("shop", "production", "lower_case"),
	} {
		if _, err := d.Set(ctx, ref, "v"); err == nil {
			t.Errorf("Set(%+v) was accepted", ref)
		}
		if _, err := d.Get(ctx, ref); err == nil {
			t.Errorf("Get(%+v) was accepted", ref)
		}
		if err := d.Remove(ctx, ref); err == nil {
			t.Errorf("Remove(%+v) was accepted", ref)
		}
	}
	if len(rows.entries) != 0 {
		t.Errorf("%d rows were written", len(rows.entries))
	}
	if _, err := d.Set(ctx, EnvRef("shop", "production", "BIG"), strings.Repeat("x", MaxValueLen+1)); err == nil {
		t.Error("an over-long value was stored")
	}
}

func TestStoreSetsSecretsForAnEnvThatDoesNotExist(t *testing.T) {
	d, _ := testStore(t)
	ctx := context.Background()
	if _, err := d.Set(ctx, EnvRef("shop", "production", "DB_PASSWORD"), "chosen"); err != nil {
		t.Fatalf("Set before the env exists: %v", err)
	}
	values, err := d.Resolve(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	if values["DB_PASSWORD"] != "chosen" {
		t.Errorf("Resolve = %v", values)
	}
}

func TestStoreResolveAndSources(t *testing.T) {
	d, _ := testStore(t)
	ctx := context.Background()
	set := func(ref Ref, value string) {
		if _, err := d.Set(ctx, ref, value); err != nil {
			t.Fatalf("set %s: %v", ref.Name, err)
		}
	}
	set(MachineRef("GREETING"), "from the machine")
	set(MachineRef("REGISTRY"), "machine only")
	set(AppRef("shop", "GREETING"), "from the app")
	set(AppRef("shop", "STRIPE_KEY"), "sk_live")
	set(EnvRef("shop", "production", "GREETING"), "from the env")
	set(EnvRef("shop", "production", "DB_PASSWORD"), "chosen")
	set(EnvRef("shop", "staging", "GREETING"), "staging's own")

	values, err := d.Resolve(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"GREETING":    "from the env",
		"REGISTRY":    "machine only",
		"STRIPE_KEY":  "sk_live",
		"DB_PASSWORD": "chosen",
	}
	if !reflect.DeepEqual(values, want) {
		t.Errorf("Resolve = %v, want %v", values, want)
	}
	sources, err := d.Sources(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	wantSources := map[string]Scope{
		"GREETING": ScopeEnv, "REGISTRY": ScopeMachine,
		"STRIPE_KEY": ScopeApp, "DB_PASSWORD": ScopeEnv,
	}
	if !reflect.DeepEqual(sources, wantSources) {
		t.Errorf("Sources = %v, want %v", sources, wantSources)
	}

	if err := d.Remove(ctx, EnvRef("shop", "production", "GREETING")); err != nil {
		t.Fatal(err)
	}
	if values, _ = d.Resolve(ctx, "shop", "production"); values["GREETING"] != "from the app" {
		t.Errorf("after rm, GREETING = %q", values["GREETING"])
	}

	appOnly, err := d.Resolve(ctx, "shop", "")
	if err != nil {
		t.Fatal(err)
	}
	if appOnly["GREETING"] != "from the app" || appOnly["DB_PASSWORD"] != "" {
		t.Errorf("the app's layers = %v", appOnly)
	}
	machineOnly, err := d.Resolve(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(machineOnly) != 2 || machineOnly["GREETING"] != "from the machine" {
		t.Errorf("the machine's layer = %v", machineOnly)
	}

	other, err := d.Resolve(ctx, "other", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 2 || other["GREETING"] != "from the machine" || other["STRIPE_KEY"] != "" {
		t.Errorf("another app's env = %v", other)
	}
}

func TestStoreListNeverCarriesAValue(t *testing.T) {
	d, _ := testStore(t)
	ctx := context.Background()
	for ref, value := range map[Ref]string{
		MachineRef("GREETING"):                      "m",
		AppRef("shop", "STRIPE_KEY"):                "a",
		EnvRef("shop", "production", "GREETING"):    "e",
		EnvRef("shop", "production", "DB_PASSWORD"): "e",
		EnvRef("shop", "other", "SOMEBODY_ELSES"):   "x",
	} {
		if _, err := d.Set(ctx, ref, value); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := d.List(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	var got [][2]string
	for _, e := range entries {
		if e.Value != "" {
			t.Errorf("List carried the value of %s", e.Name)
		}
		if e.Version != 1 {
			t.Errorf("%s has version %d", e.Name, e.Version)
		}
		got = append(got, [2]string{string(e.Scope), e.Name})
	}
	want := [][2]string{
		{"machine", "GREETING"},
		{"app", "STRIPE_KEY"},
		{"env", "DB_PASSWORD"},
		{"env", "GREETING"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List = %v, want %v", got, want)
	}
}

func TestResolveWithTheWrongKey(t *testing.T) {
	d, rows := testStore(t)
	ctx := context.Background()
	if _, err := d.Set(ctx, EnvRef("shop", "production", "DB_PASSWORD"), "chosen"); err != nil {
		t.Fatal(err)
	}
	other, err := NewCipher(bytes.Repeat([]byte{3}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := New(Options{Rows: rows, Cipher: other})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.Resolve(ctx, "shop", "production"); err == nil {
		t.Fatal("a database was read with the wrong key")
	} else if !strings.Contains(err.Error(), "DB_PASSWORD") {
		t.Errorf("the error %q does not name the secret", err)
	}

	if _, err := wrong.Sources(ctx, "shop", "production"); err != nil {
		t.Errorf("Sources needed the key: %v", err)
	}
}

func TestNewRefusesAnIncompleteVault(t *testing.T) {
	if _, err := New(Options{Cipher: testCipher(t)}); err == nil {
		t.Error("a vault with no store was built")
	}
	if _, err := New(Options{Rows: &fakeRows{}}); !errors.Is(err, ErrNoKey) {
		t.Error("a vault with no cipher was built")
	}
}

func testCipher(t *testing.T) Cipher {
	t.Helper()
	c, _, err := CipherFromKeyFile(KeyPath(t.TempDir()))
	if err != nil {
		t.Fatalf("build a cipher: %v", err)
	}
	return c
}

func testStore(t *testing.T) (*DB, *fakeRows) {
	t.Helper()
	rows := &fakeRows{}
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	d, err := New(Options{
		Rows: rows, Cipher: testCipher(t),
		Identity: func(ctx context.Context) string { s, _ := ctx.Value(testIdentity{}).(string); return s },
		Now:      func() time.Time { return at },
	})
	if err != nil {
		t.Fatalf("build the vault: %v", err)
	}
	return d, rows
}

type testIdentity struct{}

func WithTestIdentity(ctx context.Context, who string) context.Context {
	return context.WithValue(ctx, testIdentity{}, who)
}

type fakeRows struct{ entries []state.VaultEntry }

func (f *fakeRows) PutSecret(_ context.Context, e state.VaultEntry) (*state.VaultEntry, error) {
	for i := range f.entries {
		h := &f.entries[i]
		if h.Scope == e.Scope && h.App == e.App && h.Env == e.Env && h.Name == e.Name {
			e.Version = h.Version + 1
			*h = e
			out := *h
			return &out, nil
		}
	}
	e.Version = 1
	f.entries = append(f.entries, e)
	out := e
	return &out, nil
}

func (f *fakeRows) Secret(_ context.Context, scope, app, env, name string) (*state.VaultEntry, error) {
	for _, h := range f.entries {
		if h.Scope == scope && h.App == app && h.Env == env && h.Name == name {
			out := h
			return &out, nil
		}
	}
	return nil, state.ErrNotFound
}

func (f *fakeRows) Secrets(_ context.Context, scope, app, env string) ([]state.VaultEntry, error) {
	var out []state.VaultEntry
	for _, h := range f.entries {
		if h.Scope == scope && h.App == app && h.Env == env {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeRows) SecretsFor(ctx context.Context, app, env string) ([]state.VaultEntry, error) {
	var out []state.VaultEntry
	for _, layer := range []struct{ scope, app, env string }{
		{state.VaultScopeEnv, app, env},
		{state.VaultScopeMachine, "", ""},
		{state.VaultScopeApp, app, ""},
	} {
		rows, err := f.Secrets(ctx, layer.scope, layer.app, layer.env)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

func (f *fakeRows) RemoveSecret(_ context.Context, scope, app, env, name string) error {
	for i, h := range f.entries {
		if h.Scope == scope && h.App == app && h.Env == env && h.Name == name {
			f.entries = append(f.entries[:i], f.entries[i+1:]...)
			return nil
		}
	}
	return state.ErrNotFound
}

func TestFingerprintMovesOnEveryChangeAndHashesNoValue(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)

	empty, err := s.Fingerprint(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	if empty != "" {
		t.Errorf("an environment with no secrets fingerprints to %q", empty)
	}

	digest := func(what string) string {
		t.Helper()
		got, err := s.Fingerprint(ctx, "shop", "production")
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		return got
	}
	set := func(r Ref, value string) {
		t.Helper()
		if _, err := s.Set(ctx, r, value); err != nil {
			t.Fatalf("Set %s: %v", r.Name, err)
		}
	}

	set(MachineRef("SHARED"), "one")
	machine := digest("machine")
	set(AppRef("shop", "STRIPE_KEY"), "sk_live_1")
	app := digest("app")
	if app == machine {
		t.Error("a new name did not move the fingerprint")
	}

	set(AppRef("shop", "STRIPE_KEY"), "sk_live_2")
	rewritten := digest("rewritten")
	if rewritten == app {
		t.Error("a new value did not move the fingerprint")
	}

	set(EnvRef("shop", "production", "SHARED"), "mine")
	overridden := digest("overridden")
	if overridden == rewritten {
		t.Error("an override did not move the fingerprint")
	}

	if err := s.Remove(ctx, EnvRef("shop", "production", "SHARED")); err != nil {
		t.Fatal(err)
	}
	if back := digest("removed"); back == overridden {
		t.Error("a removed override did not move the fingerprint")
	}

	a, _ := testStore(t)
	b, _ := testStore(t)
	if _, err := a.Set(ctx, EnvRef("shop", "production", "DB_PASSWORD"), "hunter2"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Set(ctx, EnvRef("shop", "production", "DB_PASSWORD"), "correct horse"); err != nil {
		t.Fatal(err)
	}
	one, err := a.Fingerprint(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	two, err := b.Fingerprint(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	if one != two {
		t.Errorf("the fingerprint distinguishes two values: %q vs %q — it is derived from a plaintext", one, two)
	}
}
