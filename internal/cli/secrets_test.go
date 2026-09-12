package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/vault"
)

func TestStripSecretValues(t *testing.T) {
	got := stripSecretValues([]string{
		"secrets", "set", "production", "STRIPE_KEY=sk_live_x", "--from-file", "/tmp/.env",
		"--app", "shop", "--from-file=/tmp/other", "OTHER=1", "--json",
	})
	want := []string{"secrets", "set", "production", "--app", "shop", "--json"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("forwarded argv = %v, want %v", got, want)
	}
	for _, tok := range got {
		if strings.Contains(tok, "sk_live_x") {
			t.Fatalf("a value survived in argv: %v", got)
		}
	}
}

func TestStripFlagWithValue(t *testing.T) {
	for _, args := range [][]string{
		{"env", "create", "production", "--secrets-from", ".env.production", "--production"},
		{"env", "create", "production", "--secrets-from=.env.production", "--production"},
	} {
		got := stripFlagWithValue(args, "--secrets-from")
		want := []string{"env", "create", "production", "--production"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%v -> %v, want %v", args, got, want)
		}
	}
}

func TestSplitSecretNames(t *testing.T) {
	names, rest := splitSecretNames([]string{"production", "DB_PASSWORD", "pr-41", "STRIPE_KEY"})
	if !reflect.DeepEqual(names, []string{"DB_PASSWORD", "STRIPE_KEY"}) {
		t.Errorf("names = %v", names)
	}
	if !reflect.DeepEqual(rest, []string{"production", "pr-41"}) {
		t.Errorf("rest = %v", rest)
	}
}

func TestParseEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env.production")
	const file = `# the production environment
DB_PASSWORD=chosen

export STRIPE_KEY="sk_live with spaces"
GREETING='hi'
`
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readEnvFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"DB_PASSWORD": "chosen",
		"STRIPE_KEY":  "sk_live with spaces",
		"GREETING":    "hi",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsed %v, want %v", got, want)
	}

	bad := filepath.Join(dir, "bad.env")
	if err := os.WriteFile(bad, []byte("DB_PASSWORD=ok\nnot a pair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readEnvFile(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "bad.env:2") {
		t.Errorf("err = %v, want one naming line 2", err)
	}

	lower := filepath.Join(dir, "lower.env")
	if err := os.WriteFile(lower, []byte("db_password=ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readEnvFile(context.Background(), lower); err == nil {
		t.Error("a lower-case name was accepted")
	}
}

func TestParseSecretsJSON(t *testing.T) {
	const pem = "-----BEGIN KEY-----\nabc\n-----END KEY-----\n"
	text := renderSecretsJSON(map[string]string{"TLS_KEY": pem})
	got, err := parseSecrets(strings.NewReader(text), "standard input")
	if err != nil {
		t.Fatal(err)
	}
	if got["TLS_KEY"] != pem {
		t.Errorf("round trip = %q", got["TLS_KEY"])
	}

	if _, err := parseSecrets(strings.NewReader("\n  "+text), "in"); err != nil {
		t.Errorf("whitespace before the object: %v", err)
	}

	if got, err := parseSecrets(strings.NewReader("   \n"), "in"); err != nil || len(got) != 0 {
		t.Errorf("empty input = %v, %v", got, err)
	}

	if _, err := parseSecrets(strings.NewReader(`{"nope":"x"}`), "in"); err == nil {
		t.Error("a lower-case name was accepted from JSON")
	}
}

func TestRenderEnvFileIsSorted(t *testing.T) {
	got := renderEnvFile(map[string]string{"B": "2", "A": "1"})
	if got != "A=1\nB=2\n" {
		t.Errorf("rendered %q", got)
	}
}

func TestParseEnvFileRefusesAnOversizeValue(t *testing.T) {
	line := "BIG=" + strings.Repeat("x", vault.MaxValueLen+1) + "\n"
	if _, err := parseSecrets(strings.NewReader(line), "in"); err == nil {
		t.Error("an oversize value was accepted")
	}
}

type vaultService struct {
	api.Service
	set    []api.VaultSetRequest
	remove []api.VaultRemoveRequest
	list   []api.VaultListRequest
}

func (v *vaultService) VaultRemove(_ context.Context, req api.VaultRemoveRequest) (*api.VaultResult, error) {
	v.remove = append(v.remove, req)
	return &api.VaultResult{App: req.App, Env: req.Env, Changed: req.Names}, nil
}

func (v *vaultService) VaultList(_ context.Context, req api.VaultListRequest) (*api.VaultResult, error) {
	v.list = append(v.list, req)
	return &api.VaultResult{App: req.App, Env: req.Env}, nil
}

func (v *vaultService) VaultSet(_ context.Context, req api.VaultSetRequest) (*api.VaultResult, error) {
	v.set = append(v.set, req)
	entries := make([]vault.Entry, 0, len(req.Values))
	changed := make([]string, 0, len(req.Values))
	for name := range req.Values {
		entries = append(entries, vault.Entry{
			Ref: vault.Ref{Scope: req.Scope, App: req.App, Env: req.Env, Name: name}, Version: 1,
		})
		changed = append(changed, name)
	}
	vault.Sort(entries)
	sort.Strings(changed)
	return &api.VaultResult{App: req.App, Env: req.Env, Entries: entries, Changed: changed}, nil
}

func TestSecretsSetReadsTheSessionsStdin(t *testing.T) {
	svc := &vaultService{}
	ctx := withStdin(context.Background(), strings.NewReader(`{"STRIPE_KEY":"sk_live_x"}`+"\n"))
	var stdout, stderr bytes.Buffer
	code := RunWith(ctx, []string{"secrets", "set", "--machine-scope", "--stdin", "--json"},
		&stdout, &stderr, Options{Service: svc})
	if code != ExitOK {
		t.Fatalf("exit %d\nstdout:%s\nstderr:%s", code, stdout.String(), stderr.String())
	}
	if len(svc.set) != 1 {
		t.Fatalf("%d write(s), want 1", len(svc.set))
	}
	req := svc.set[0]
	if req.Scope != vault.ScopeMachine {
		t.Errorf("scope = %q, want %q", req.Scope, vault.ScopeMachine)
	}
	if req.Values["STRIPE_KEY"] != "sk_live_x" {
		t.Errorf("values = %v", req.Values)
	}

	if strings.Contains(stdout.String(), "sk_live_x") {
		t.Errorf("the answer leaked the value:\n%s", stdout.String())
	}

	svc.set = nil
	stdout.Reset()
	ctx = withStdin(context.Background(), strings.NewReader("# a comment\nA_TOKEN=abc\n"))
	if code := RunWith(ctx, []string{"secrets", "set", "--machine-scope", "--stdin", "--json"},
		&stdout, &stderr, Options{Service: svc}); code != ExitOK {
		t.Fatalf("exit %d\nstderr:%s", code, stderr.String())
	}
	if len(svc.set) != 1 || svc.set[0].Values["A_TOKEN"] != "abc" {
		t.Errorf("values = %+v", svc.set)
	}
}

func TestSecretsSetWithNothingToSet(t *testing.T) {
	svc := &vaultService{}
	ctx := withStdin(context.Background(), strings.NewReader("   \n"))
	var stdout, stderr bytes.Buffer
	if code := RunWith(ctx, []string{"secrets", "set", "--machine-scope", "--stdin"},
		&stdout, &stderr, Options{Service: svc}); code != ExitUsage {
		t.Errorf("exit = %d, want %d (usage)\nstderr:%s", code, ExitUsage, stderr.String())
	}
	if len(svc.set) != 0 {
		t.Errorf("it wrote %+v", svc.set)
	}
}

func TestSecretsScopeRefusesAnEnvironmentItCannotUse(t *testing.T) {
	svc := &vaultService{}
	ctx := withStdin(context.Background(), strings.NewReader("A=b\n"))
	var stdout, stderr bytes.Buffer
	code := RunWith(ctx, []string{"secrets", "set", "--machine-scope", "--stdin", "production"},
		&stdout, &stderr, Options{Service: svc})
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d (usage)\nstderr:%s", code, ExitUsage, stderr.String())
	}
	if len(svc.set) != 0 {
		t.Errorf("it wrote %+v at the machine's scope", svc.set)
	}
	if !strings.Contains(stderr.String(), "production") {
		t.Errorf("the error does not name the argument it refused: %q", stderr.String())
	}
}

func TestSecretsScopeFlagsDoNotShadowTheMachineFlag(t *testing.T) {
	t.Setenv("CARAMELO_APP", "shop")
	t.Setenv("CARAMELO_ENV", "production")
	t.Setenv("CARAMELO_MACHINE", "")
	fakeGit(t, nil)
	for _, args := range [][]string{
		{"--machine", "prod1", "secrets", "list"},
		{"--machine", "prod1", "secrets", "set", "TOKEN=abc"},
	} {
		fwd := &fakeForward{}
		fwd.install(t)
		if code, _, stderr := run(t, args...); code != ExitOK {
			t.Fatalf("%v: exit = %d (%s)", args, code, stderr)
		}
		if !fwd.called {
			t.Fatalf("%v was not forwarded", args)
		}

		if got := strings.Join(fwd.args, " "); !strings.Contains(got, "--env production") {
			t.Errorf("%v forwarded %q, want the resolved environment in it", args, got)
		}
		if fwd.machine != "prod1" {
			t.Errorf("%v went to machine %q, want prod1", args, fwd.machine)
		}
	}
}

func TestSecretsSetWithNoValueAsksForOne(t *testing.T) {
	t.Setenv("CARAMELO_APP", "shop")
	t.Setenv("CARAMELO_ENV", "production")
	fakeGit(t, nil)
	fwd := &fakeForward{}
	fwd.install(t)
	code, _, stderr := run(t, "secrets", "set", "STRIPE_KEY")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)\nstderr:%s", code, ExitUsage, stderr)
	}
	for _, want := range []string{"STRIPE_KEY=VALUE", "--from-file", "terminal"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal %q does not name %q", stderr, want)
		}
	}
	if fwd.called {
		t.Error("a set with no value was forwarded anyway")
	}
}

func TestMachineScopeSendsNoApp(t *testing.T) {
	t.Setenv("CARAMELO_APP", "shop")
	t.Setenv("CARAMELO_ENV", "production")
	for _, tc := range []struct {
		name  string
		args  []string
		stdin string
		got   func(v *vaultService) (vault.Scope, string, string)
	}{
		{"set", []string{"secrets", "set", "--machine-scope", "--stdin", "--json"},
			`{"STRIPE_KEY":"sk_live_x"}` + "\n",
			func(v *vaultService) (vault.Scope, string, string) {
				return v.set[0].Scope, v.set[0].App, v.set[0].Env
			}},
		{"rm", []string{"secrets", "rm", "--machine-scope", "STRIPE_KEY", "--json"}, "",
			func(v *vaultService) (vault.Scope, string, string) {
				return v.remove[0].Scope, v.remove[0].App, v.remove[0].Env
			}},
		{"list", []string{"secrets", "list", "--machine-scope", "--json"}, "",
			func(v *vaultService) (vault.Scope, string, string) {
				return v.list[0].Scope, v.list[0].App, v.list[0].Env
			}},
	} {
		svc := &vaultService{}
		ctx := withStdin(context.Background(), strings.NewReader(tc.stdin))
		var stdout, stderr bytes.Buffer
		if code := RunWith(ctx, tc.args, &stdout, &stderr, Options{Service: svc}); code != ExitOK {
			t.Fatalf("%s: exit = %d\nstderr:%s", tc.name, code, stderr.String())
		}
		scope, app, env := tc.got(svc)
		if scope != vault.ScopeMachine {
			t.Errorf("%s sent scope %q, want %q", tc.name, scope, vault.ScopeMachine)
		}
		if app != "" || env != "" {
			t.Errorf("%s sent app %q env %q; a machine secret belongs to neither", tc.name, app, env)
		}

		if err := (vault.Ref{Scope: scope, App: app, Env: env, Name: "STRIPE_KEY"}).Validate(); err != nil {
			t.Errorf("%s built a reference the daemon refuses: %v", tc.name, err)
		}
	}
}

func TestAppScopeSendsTheAppAndNoEnvironment(t *testing.T) {
	t.Setenv("CARAMELO_APP", "shop")
	t.Setenv("CARAMELO_ENV", "production")
	svc := &vaultService{}
	ctx := withStdin(context.Background(), strings.NewReader(""))
	var stdout, stderr bytes.Buffer
	if code := RunWith(ctx, []string{"secrets", "list", "--app-scope", "--json"},
		&stdout, &stderr, Options{Service: svc}); code != ExitOK {
		t.Fatalf("exit = %d\nstderr:%s", code, stderr.String())
	}
	if len(svc.list) != 1 {
		t.Fatalf("%d list calls", len(svc.list))
	}
	if got := svc.list[0]; got.Scope != vault.ScopeApp || got.App != "shop" || got.Env != "" {
		t.Errorf("list = %+v, want the app's layer alone", got)
	}
}

func TestEnvCreateWritesTheSecretsFileOnce(t *testing.T) {
	t.Setenv("CARAMELO_APP", "shop")
	t.Setenv("CARAMELO_ENV", "")
	fakeGit(t, nil)
	dir := t.TempDir()
	path := filepath.Join(dir, ".env.production")
	if err := os.WriteFile(path, []byte("DB_PASSWORD=chosen\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var forwarded [][]string
	old := forward
	forward = func(_ context.Context, a *app) (int, error) {
		forwarded = append(forwarded, append([]string(nil), a.args...))
		return ExitOK, nil
	}
	t.Cleanup(func() { forward = old })

	if code, _, stderr := run(t, "env", "create", "production", "--production",
		"--secrets-from", path); code != ExitOK {
		t.Fatalf("exit = %d\nstderr:%s", code, stderr)
	}

	sets := 0
	for _, args := range forwarded {
		if len(args) >= 2 && args[0] == "secrets" && args[1] == "set" {
			sets++
		}
		for _, arg := range args {
			if strings.HasPrefix(arg, "--secrets-from") {
				t.Errorf("the flag travelled to the daemon: %v", args)
			}
		}
	}
	if sets != 1 {
		t.Errorf("the vault was written %d times for one `env create`: %v", sets, forwarded)
	}
}
