package env

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vault"
)

func (s *fakeStore) PutSecret(_ context.Context, e state.VaultEntry) (*state.VaultEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.secrets {
		h := &s.secrets[i]
		if h.Scope == e.Scope && h.App == e.App && h.Env == e.Env && h.Name == e.Name {
			e.Version = h.Version + 1
			*h = e
			out := *h
			return &out, nil
		}
	}
	e.Version = 1
	s.secrets = append(s.secrets, e)
	out := e
	return &out, nil
}

func (s *fakeStore) Secret(_ context.Context, scope, app, env, name string) (*state.VaultEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.secrets {
		h := s.secrets[i]
		if h.Scope == scope && h.App == app && h.Env == env && h.Name == name {
			return &h, nil
		}
	}
	return nil, state.ErrNotFound
}

func (s *fakeStore) Secrets(_ context.Context, scope, app, env string) ([]state.VaultEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []state.VaultEntry
	for _, h := range s.secrets {
		if h.Scope == scope && h.App == app && h.Env == env {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *fakeStore) SecretsFor(ctx context.Context, app, env string) ([]state.VaultEntry, error) {
	var out []state.VaultEntry
	for _, layer := range []struct{ scope, app, env string }{
		{state.VaultScopeMachine, "", ""},
		{state.VaultScopeApp, app, ""},
		{state.VaultScopeEnv, app, env},
	} {
		rows, err := s.Secrets(ctx, layer.scope, layer.app, layer.env)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

func (s *fakeStore) RemoveSecret(_ context.Context, scope, app, env, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, h := range s.secrets {
		if h.Scope == scope && h.App == app && h.Env == env && h.Name == name {
			s.secrets = append(s.secrets[:i], s.secrets[i+1:]...)
			return nil
		}
	}
	return state.ErrNotFound
}

func (h *harness) setSecret(t *testing.T, env, name, value string) {
	t.Helper()
	ref := vault.EnvRef("shop", env, name)
	if env == "" {
		ref = vault.AppRef("shop", name)
	}
	if _, err := h.m.Secrets.Set(context.Background(), ref, value); err != nil {
		t.Fatalf("set %s: %v", name, err)
	}
}

func (h *harness) specFor(name string) (runtime.ContainerSpec, bool) {
	h.driver.mu.Lock()
	defer h.driver.mu.Unlock()
	for i := len(h.driver.specs) - 1; i >= 0; i-- {
		if h.driver.specs[i].Name == name {
			return h.driver.specs[i], true
		}
	}
	return runtime.ContainerSpec{}, false
}

func (h *harness) captureEnvFiles() map[string]string {
	seen := map[string]string{}
	h.driver.RunErr = func(spec runtime.ContainerSpec) error {
		if spec.EnvFile == "" {
			return nil
		}
		raw, err := os.ReadFile(spec.EnvFile)
		if err != nil {
			h.t.Errorf("read the env-file of %s: %v", spec.Name, err)
			return nil
		}
		seen[spec.Name] = string(raw)
		return nil
	}
	return seen
}

func TestSecretsForMergesTheThreeLayers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	set := func(ref vault.Ref, value string) {
		if _, err := h.m.Secrets.Set(ctx, ref, value); err != nil {
			t.Fatalf("set %s: %v", ref.Name, err)
		}
	}
	set(vault.MachineRef("GREETING"), "machine")
	set(vault.AppRef("shop", "GREETING"), "app")
	set(vault.AppRef("shop", "STRIPE_KEY"), "sk_live")
	set(vault.EnvRef("shop", "production", "GREETING"), "env")

	got, err := h.m.SecretsFor(ctx, "shop", "production")
	if err != nil {
		t.Fatalf("SecretsFor: %v", err)
	}
	if got["GREETING"] != "env" {
		t.Errorf("GREETING = %q, want the environment's value", got["GREETING"])
	}
	if got["STRIPE_KEY"] != "sk_live" {
		t.Errorf("STRIPE_KEY = %q, want the app's value", got["STRIPE_KEY"])
	}

	other, err := h.m.SecretsFor(ctx, "shop", "staging")
	if err != nil {
		t.Fatalf("SecretsFor(staging): %v", err)
	}
	if other["GREETING"] != "app" {
		t.Errorf("staging GREETING = %q, want the app's value", other["GREETING"])
	}
}

func TestSecretsForWithNoVault(t *testing.T) {
	h := newHarness(t)
	h.m.Secrets = nil
	got, err := h.m.SecretsFor(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("SecretsFor: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a machine with no vault resolved %v", got)
	}
}

func TestSecretsReachAServiceThroughAnEnvFileAndNowhereElse(t *testing.T) {
	h := upHarness(t)
	h.cfg.Services[0].Env = map[string]string{"STRIPE_KEY": "${secrets.STRIPE_KEY}"}
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.setSecret(t, "feat-x", "STRIPE_KEY", "sk_live_42")
	h.setSecret(t, "feat-x", "EXTRA", "also-injected")

	files := h.captureEnvFiles()
	if _, err := h.m.Up(context.Background(), UpRequest{App: "shop", Name: "feat-x"}, &h.out); err != nil {
		t.Fatalf("Up: %v\n%s", err, h.out.String())
	}

	container := ReplicaContainerName("shop", "feat-x", "web", 1)
	spec, ok := h.specFor(container)
	if !ok {
		t.Fatalf("no spec for %s\n%s", container, h.out.String())
	}
	if spec.EnvFile == "" {
		t.Fatal("the replica was given no env-file, so it has no secrets")
	}

	for name, value := range spec.Env {
		if strings.Contains(value, "sk_live_42") {
			t.Errorf("the secret is in --env %s", name)
		}
	}

	content := files[container]
	for _, want := range []string{"STRIPE_KEY=sk_live_42\n", "EXTRA=also-injected\n"} {
		if !strings.Contains(content, want) {
			t.Errorf("the env-file does not carry %q; it holds %q", want, content)
		}
	}

	if _, err := os.Stat(spec.EnvFile); !os.IsNotExist(err) {
		t.Errorf("the env-file %s still exists after the run (%v)", spec.EnvFile, err)
	}
}

func TestSecretDerivedVariablesAreStoredRedacted(t *testing.T) {
	h := upHarness(t)
	h.cfg.Services[0].Env = map[string]string{
		"DATABASE_URL": "postgres://postgres:${deps.db.password}@${deps.db.host}:${deps.db.port}/app",
		"PLAIN":        "no secret here",
	}
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.setSecret(t, "feat-x", "DB_PASSWORD", "chosen-one")

	res, err := h.m.Up(context.Background(), UpRequest{App: "shop", Name: "feat-x"}, &h.out)
	if err != nil {
		t.Fatalf("Up: %v\n%s", err, h.out.String())
	}
	var web *Service
	for i := range res.Services {
		if res.Services[i].Name == "web" {
			web = &res.Services[i]
		}
	}
	if web == nil {
		t.Fatal("no web service in the result")
	}
	if got := web.Vars["DATABASE_URL"]; !strings.Contains(got, vault.Redacted) || strings.Contains(got, "chosen-one") {
		t.Errorf("the stored DATABASE_URL is %q", got)
	}
	if got := web.Vars["PLAIN"]; got != "no secret here" {
		t.Errorf("a variable with no secret in it was touched: %q", got)
	}
	if web.SecretsDigest == "" {
		t.Error("the service recorded no secrets digest")
	}

	rows, err := h.store.Services(context.Background(), 1)
	if err != nil {
		t.Fatalf("read services: %v", err)
	}
	for _, row := range rows {
		if strings.Contains(row.VarsJSON, "chosen-one") {
			t.Errorf("the stored definition of %s holds the plaintext: %s", row.Name, row.VarsJSON)
		}
	}
}

func TestAChangedSecretChangesTheDefinition(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.setSecret(t, "feat-x", "TOKEN", "first")
	if _, err := h.m.Up(context.Background(), UpRequest{App: "shop", Name: "feat-x"}, &h.out); err != nil {
		t.Fatalf("first Up: %v\n%s", err, h.out.String())
	}

	res, err := h.m.Up(context.Background(), UpRequest{App: "shop", Name: "feat-x"}, &h.out)
	if err != nil {
		t.Fatalf("second Up: %v\n%s", err, h.out.String())
	}
	for _, s := range res.Services {
		if s.Change != ChangeUnchanged {
			t.Errorf("%s was %s by an `up` that changed nothing", s.Name, s.Change)
		}
	}

	h.setSecret(t, "feat-x", "TOKEN", "second")
	res, err = h.m.Up(context.Background(), UpRequest{App: "shop", Name: "feat-x"}, &h.out)
	if err != nil {
		t.Fatalf("third Up: %v\n%s", err, h.out.String())
	}
	for _, s := range res.Services {
		if s.Change == ChangeUnchanged {
			t.Errorf("%s was not rolled after its secret changed", s.Name)
		}
	}
}

func TestSecretsFingerprintMovesWithoutTheValues(t *testing.T) {
	h := upHarness(t)
	ctx := context.Background()
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	digest := func() string {
		t.Helper()
		s, err := h.m.secretsOf(ctx, "shop", "feat-x")
		if err != nil {
			t.Fatalf("secretsOf: %v", err)
		}
		return s.digest
	}
	set := func(ref vault.Ref, value string) {
		t.Helper()
		if _, err := h.m.Secrets.Set(ctx, ref, value); err != nil {
			t.Fatalf("Set %s: %v", ref.Name, err)
		}
	}

	if got := digest(); got != "" {
		t.Errorf("an environment with no secrets has the fingerprint %q", got)
	}

	set(vault.EnvRef("shop", "feat-x", "STRIPE_KEY"), "sk_live_1")
	first := digest()
	if first == "" {
		t.Fatal("a secret was set and the fingerprint is still empty")
	}

	set(vault.EnvRef("shop", "feat-x", "STRIPE_KEY"), "sk_live_2")
	second := digest()
	if second == first {
		t.Error("a changed value did not change the fingerprint")
	}

	set(vault.AppRef("shop", "GREETING"), "hello")
	app := digest()
	set(vault.EnvRef("shop", "feat-x", "GREETING"), "hi")
	if over := digest(); over == app {
		t.Error("an override did not change the fingerprint")
	}

	if strings.Contains(second, "sk_live") {
		t.Errorf("the fingerprint looks like a value: %q", second)
	}
	sameAgain := digest()
	if sameAgain != digest() {
		t.Error("the fingerprint is not stable for an unchanged vault")
	}
}

func TestVarsChangedWhy(t *testing.T) {
	with := func(digest string, vars string) string {
		if digest == "" {
			return vars
		}
		return strings.TrimSuffix(vars, "}") + `,"` + SecretsDigestKey + `":"` + digest + `"}`
	}
	cases := []struct{ have, want, expect string }{
		{with("a", `{"X":"1"}`), with("b", `{"X":"1"}`), "a secret this environment uses changed"},
		{with("a", `{"X":"1"}`), with("b", `{"X":"2"}`), "the variables and a secret changed"},
		{with("a", `{"X":"1"}`), with("a", `{"X":"2"}`), "the variables changed"},
		{`not json`, `{"X":"1"}`, "the variables changed"},
	}
	for _, tc := range cases {
		if got := varsChangedWhy(tc.have, tc.want); got != tc.expect {
			t.Errorf("varsChangedWhy(%s, %s) = %q, want %q", tc.have, tc.want, got, tc.expect)
		}
	}
}

func TestADevEnvKeepsTheDevelopmentPassword(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	spec, ok := h.specFor(ContainerName("shop", "feat-x", "db"))
	if !ok {
		t.Fatal("no spec for the db container")
	}
	if got := spec.Env["POSTGRES_PASSWORD"]; got != vault.DevPassword {
		t.Errorf("POSTGRES_PASSWORD = %q, want %q", got, vault.DevPassword)
	}
	if spec.EnvFile != "" {
		t.Error("the development password was delivered as a secret")
	}

	entries, err := h.m.Secrets.(*vault.DB).List(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a dev env wrote %d secrets: %+v", len(entries), entries)
	}
}

func TestADependencyPasswordChosenBeforeCreate(t *testing.T) {
	h := newHarness(t)
	h.setSecret(t, "production", "DB_PASSWORD", "chosen")
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Release: true})

	spec, ok := h.specFor(ContainerName("shop", "production", "db"))
	if !ok {
		t.Fatal("no spec for the db container")
	}
	if _, inArgv := spec.Env["POSTGRES_PASSWORD"]; inArgv {
		t.Errorf("the dependency's password is in --env: %v", spec.Env)
	}
	if spec.EnvFile == "" {
		t.Error("the dependency's password did not travel in an env-file")
	}
}

func TestCreateWithSecretsFromSetsThemFirst(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Release: true,
		SecretsFrom: ".env.production",
		Secrets:     map[string]string{"DB_PASSWORD": "from-the-file", "API_KEY": "k"},
	})
	got, err := h.m.SecretsFor(context.Background(), "shop", "production")
	if err != nil {
		t.Fatalf("SecretsFor: %v", err)
	}
	if got["DB_PASSWORD"] != "from-the-file" || got["API_KEY"] != "k" {
		t.Errorf("the environment resolved %v", got)
	}
	if !strings.Contains(h.out.String(), "2 set for production before it was created") {
		t.Errorf("the progress stream does not say the secrets were set first:\n%s", h.out.String())
	}
}

func TestAReleaseEnvGeneratesADependencyPassword(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "production", Release: true})

	got, err := h.m.SecretsFor(context.Background(), "shop", "production")
	if err != nil {
		t.Fatalf("SecretsFor: %v", err)
	}
	pw := got[config.DepPasswordSecret("db")]
	if pw == "" {
		t.Fatal("no password was generated")
	}
	if pw == vault.DevPassword {
		t.Error("a release environment was given the development password")
	}
	if len(pw) < 20 {
		t.Errorf("the generated password %q is short", pw)
	}
	if !strings.Contains(h.out.String(), "DB_PASSWORD generated") {
		t.Errorf("the progress stream does not say a password was generated:\n%s", h.out.String())
	}

	if _, ok := got[config.DepPasswordSecret("cache")]; ok {
		t.Error("a password was generated for a dependency that takes none")
	}

	again, err := h.m.SecretsFor(context.Background(), "shop", "production")
	if err != nil {
		t.Fatalf("SecretsFor again: %v", err)
	}
	if again[config.DepPasswordSecret("db")] != pw {
		t.Error("the generated password moved")
	}
}

func TestAReleaseEnvWithNoVaultIsRefused(t *testing.T) {
	h := newHarness(t)
	h.m.Secrets = nil
	_, err := h.create(CreateRequest{App: "shop", Name: "production", Release: true})
	if err == nil {
		t.Fatal("a release environment was created with no vault")
	}
	for _, want := range []string{"release environment", "no vault", "fleet setup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not mention %q", err, want)
		}
	}
}

func TestAOneOffGetsTheSecrets(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.setSecret(t, "feat-x", "TOKEN", "one-off-value")

	var seen runtime.ContainerSpec
	var content string
	h.driver.Attached = func(spec runtime.ContainerSpec, _ runtime.Streams) (int, error) {
		seen = spec
		if spec.EnvFile != "" {
			raw, err := os.ReadFile(spec.EnvFile)
			if err != nil {
				t.Errorf("read the env-file: %v", err)
			}
			content = string(raw)
		}
		return 0, nil
	}
	if _, err := h.m.RunOneOff(context.Background(), RunRequest{
		App: "shop", Name: "feat-x", Argv: []string{"env"},
	}); err != nil {
		t.Fatalf("RunOneOff: %v", err)
	}
	if seen.EnvFile == "" {
		t.Fatal("the one-off was given no env-file")
	}
	if !strings.Contains(content, "TOKEN=one-off-value\n") {
		t.Errorf("the env-file held %q", content)
	}
	if _, err := os.Stat(seen.EnvFile); !os.IsNotExist(err) {
		t.Errorf("the env-file %s outlived the one-off", seen.EnvFile)
	}
}

func TestNoSecretsMeansNoEnvFile(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	if _, err := h.m.Up(context.Background(), UpRequest{App: "shop", Name: "feat-x"}, &h.out); err != nil {
		t.Fatalf("Up: %v\n%s", err, h.out.String())
	}
	h.driver.mu.Lock()
	defer h.driver.mu.Unlock()
	for _, spec := range h.driver.specs {
		if spec.EnvFile != "" {
			t.Errorf("%s was given the env-file %s", spec.Name, spec.EnvFile)
		}
	}

	dir, err := vault.SecretsDir(h.run)
	if err != nil {
		t.Fatalf("SecretsDir(%q): %v", h.run, err)
	}
	entries, err := os.ReadDir(dir)
	if err == nil && len(entries) != 0 {
		t.Errorf("%s holds %d files", dir, len(entries))
	}
}

func TestASecretIsNeverWrittenBesideTheDataDirectory(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.setSecret(t, "feat-x", "TOKEN", "beside-the-data-dir")
	files := h.captureEnvFiles()
	h.m.Dirs.Run = ""

	_, err := h.m.Up(context.Background(), UpRequest{App: "shop", Name: "feat-x"}, &h.out)
	if err == nil {
		t.Fatalf("Up delivered a secret with no run directory\n%s", h.out.String())
	}
	if !errors.Is(err, vault.ErrNoRunDir) {
		t.Errorf("Up failed with %v, want an error that is %v", err, vault.ErrNoRunDir)
	}
	if len(files) != 0 {
		t.Errorf("an env-file was written and read back: %v", files)
	}

	stray := filepath.Join(h.data, vault.SecretsDirName)
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Errorf("%s exists beside the data directory (%v)", stray, err)
	}

	h.driver.mu.Lock()
	defer h.driver.mu.Unlock()
	for _, spec := range h.driver.specs {
		if spec.EnvFile != "" {
			t.Errorf("%s was given the env-file %s", spec.Name, spec.EnvFile)
		}
	}
}

func TestAnEnvironmentWithNoSecretComesUpWithoutARunDirectory(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.m.Dirs.Run = ""

	if _, err := h.m.Up(context.Background(), UpRequest{App: "shop", Name: "feat-x"}, &h.out); err != nil {
		t.Fatalf("Up refused an environment that has no secret: %v\n%s", err, h.out.String())
	}

	spec, ok := h.specFor(ReplicaContainerName("shop", "feat-x", "web", 1))
	if !ok {
		t.Fatal("no spec for the web container")
	}
	if spec.EnvFile != "" {
		t.Errorf("%s was given the env-file %s", spec.Name, spec.EnvFile)
	}

	stray := filepath.Join(h.data, vault.SecretsDirName)
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Errorf("%s exists beside the data directory (%v)", stray, err)
	}
}

func TestADevEnvResolvesADependencyPasswordThatIsNotASecret(t *testing.T) {
	h := upHarness(t)
	h.cfg.Services[0].Env = map[string]string{
		"DATABASE_URL": "postgres://postgres:${deps.db.password}@${deps.db.host}:${deps.db.port}/x",
	}
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	spec, ok := h.specFor(ContainerName("shop", "feat-x", "web-1"))
	if !ok {
		t.Fatal("no spec for the web container")
	}
	if got := spec.Env["DATABASE_URL"]; !strings.Contains(got, ":"+vault.DevPassword+"@") {
		t.Errorf("DATABASE_URL = %q, want the development password in it", got)
	}
	if spec.EnvFile != "" {
		t.Errorf("a published default travelled in an env-file: %s", spec.EnvFile)
	}

	vars, err := h.m.VarsIn(context.Background(), "shop", "feat-x", config.ViewNetwork, false)
	if err != nil {
		t.Fatalf("VarsIn: %v", err)
	}
	if got := vars["DATABASE_URL"]; got != "" && !strings.Contains(got, ":"+vault.DevPassword+"@") {
		t.Errorf("the exported DATABASE_URL = %q", got)
	}

	secrets, err := h.m.secretsOf(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("secretsOf: %v", err)
	}
	secrets.withDepDefaults(h.cfg.Deps, false)
	if secrets.digest != "" {
		t.Errorf("a dev env with only the default password has digest %q", secrets.digest)
	}
}

func TestAReleaseEnvHasNoDefaultDependencyPassword(t *testing.T) {
	h := newHarness(t)
	secrets := newEnvSecrets(map[string]string{}, "")
	secrets.withDepDefaults(h.cfg.Deps, true)
	if len(secrets.defaults) != 0 {
		t.Errorf("a release env fell back to %v", secrets.defaults)
	}
	if _, _, ok := secrets.password("db"); ok {
		t.Error("a release env resolved a dependency password nobody set")
	}
}

func TestExportSecretsIncludesTheDependencyDefault(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	ctx := context.Background()

	values, sources, err := h.m.ExportSecrets(ctx, "shop", "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	if values["DB_PASSWORD"] != vault.DevPassword {
		t.Errorf("DB_PASSWORD = %q, want the development default %q", values["DB_PASSWORD"], vault.DevPassword)
	}
	if sources["DB_PASSWORD"] != vault.ScopeDefault {
		t.Errorf("DB_PASSWORD came from %q, want %q", sources["DB_PASSWORD"], vault.ScopeDefault)
	}

	if _, err := h.m.Secrets.Set(ctx, vault.EnvRef("shop", "feat-x", "DB_PASSWORD"), "chosen"); err != nil {
		t.Fatal(err)
	}
	values, sources, err = h.m.ExportSecrets(ctx, "shop", "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	if values["DB_PASSWORD"] != "chosen" {
		t.Errorf("DB_PASSWORD = %q, want the value that was set", values["DB_PASSWORD"])
	}
	if sources["DB_PASSWORD"] != vault.ScopeEnv {
		t.Errorf("DB_PASSWORD came from %q, want %q", sources["DB_PASSWORD"], vault.ScopeEnv)
	}
}

func TestEnvExportRevealIsASecretsEvent(t *testing.T) {
	h := upHarness(t)
	ctx := context.Background()
	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	if _, err := h.m.Secrets.Set(ctx, vault.EnvRef("shop", "feat-x", "STRIPE_KEY"), "sk_live"); err != nil {
		t.Fatal(err)
	}

	if _, err := h.m.VarsIn(ctx, "shop", "feat-x", config.ViewNetwork, true); err != nil {
		t.Fatalf("VarsIn(reveal): %v", err)
	}
	events := h.eventsOf(e.ID, "secrets")
	if len(events) != 1 {
		t.Fatalf("reveal events = %d, want one under the one action every reveal shares: %v",
			len(events), h.eventsOf(e.ID, "reveal"))
	}
	if events[0].Step == "" {
		t.Errorf("event = %+v, want a step saying which read it was", events[0])
	}
	if !strings.Contains(events[0].Detail, "STRIPE_KEY") {
		t.Errorf("the event does not name what was read: %q", events[0].Detail)
	}
	if strings.Contains(events[0].Detail, "sk_live") {
		t.Errorf("the event carries the value: %q", events[0].Detail)
	}

	if _, err := h.m.VarsIn(ctx, "shop", "feat-x", config.ViewNetwork, false); err != nil {
		t.Fatal(err)
	}
	if got := h.eventsOf(e.ID, "secrets"); len(got) != 1 {
		t.Errorf("a redacted export recorded an event too: %v", got)
	}
}
