package env

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"sort"
	"strings"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vault"
)

const SecretsDigestKey = "caramelo.secrets"

type SecretsProvider interface {
	Resolve(ctx context.Context, app, env string) (map[string]string, error)

	Sources(ctx context.Context, app, env string) (map[string]vault.Scope, error)

	Fingerprint(ctx context.Context, app, env string) (string, error)
	Set(ctx context.Context, r vault.Ref, value string) (*vault.Entry, error)
}

func (m *Manager) SecretsFor(ctx context.Context, app, name string) (map[string]string, error) {
	if m.Secrets == nil {
		return map[string]string{}, nil
	}
	values, err := m.Secrets.Resolve(ctx, app, name)
	if err != nil {
		return nil, fmt.Errorf("read the secrets of env %q: %w", name, err)
	}
	if values == nil {
		values = map[string]string{}
	}
	return values, nil
}

func (m *Manager) ExportSecrets(ctx context.Context, app, name string) (
	values map[string]string, sources map[string]vault.Scope, err error) {
	rec, err := m.env(ctx, app, name)
	if err != nil {
		return nil, nil, err
	}
	secrets, err := m.secretsOf(ctx, app, name)
	if err != nil {
		return nil, nil, err
	}
	sources = map[string]vault.Scope{}
	if m.Secrets != nil {
		if from, serr := m.Secrets.Sources(ctx, app, name); serr == nil {
			sources = from
		}
	}
	values = map[string]string{}
	for k, v := range secrets.values {
		values[k] = v
	}
	cfg, cerr := decodeConfig(rec)
	if cerr != nil {

		return values, sources, nil
	}
	secrets.withDepDefaults(cfg.Deps, Mode(rec.Mode).IsRelease())
	for dep, value := range secrets.defaults {
		nm := config.DepPasswordSecret(dep)
		if _, ok := values[nm]; ok {
			continue
		}
		values[nm], sources[nm] = value, vault.ScopeDefault
	}
	return values, sources, nil
}

type envSecrets struct {
	values map[string]string

	digest string

	defaults map[string]string
}

func (m *Manager) secretsOf(ctx context.Context, app, name string) (*envSecrets, error) {
	values, err := m.SecretsFor(ctx, app, name)
	if err != nil {
		return nil, err
	}
	digest := ""
	if m.Secrets != nil {
		if digest, err = m.Secrets.Fingerprint(ctx, app, name); err != nil {
			return nil, fmt.Errorf("fingerprint the secrets of env %q: %w", name, err)
		}
	}
	return newEnvSecrets(values, digest), nil
}

func newEnvSecrets(values map[string]string, digest string) *envSecrets {
	return &envSecrets{values: values, digest: digest}
}

func (s *envSecrets) password(dep string) (value string, secret, ok bool) {
	if s == nil {
		return "", false, false
	}
	if v, found := s.values[config.DepPasswordSecret(dep)]; found {
		return v, true, true
	}
	v, found := s.defaults[dep]
	return v, false, found
}

func (s *envSecrets) withDepDefaults(deps []config.Dep, release bool) *envSecrets {
	if s == nil || release {
		return s
	}
	for _, dep := range deps {
		if _, ok := s.values[config.DepPasswordSecret(dep.Name)]; ok {
			continue
		}
		if known, ok := config.Lookup(dep.Image); !ok || known.PasswordVar == "" {
			continue
		}
		if s.defaults == nil {
			s.defaults = map[string]string{}
		}
		s.defaults[dep.Name] = vault.DevPassword
	}
	return s
}

func (s *envSecrets) lookups() config.Secrets {
	if s == nil {
		return config.Secrets{}
	}
	return config.Secrets{
		Lookup: func(name string) (string, bool) {
			v, ok := s.values[name]
			return v, ok
		},
		DepPassword: func(dep string) (string, bool) {
			v, _, ok := s.password(dep)
			return v, ok
		},
	}
}

func (s *envSecrets) redacted() config.Secrets {
	if s == nil {
		return config.Secrets{}
	}
	return config.Secrets{
		Lookup: func(name string) (string, bool) {
			_, ok := s.values[name]
			return vault.Redacted, ok
		},

		DepPassword: func(dep string) (string, bool) {
			v, secret, ok := s.password(dep)
			if secret {
				return vault.Redacted, true
			}
			return v, ok
		},
	}
}

func (s *envSecrets) empty() bool { return s == nil || len(s.values) == 0 }

func secretVars(real, redacted map[string]string) (plain, secret, stored map[string]string) {
	plain = make(map[string]string, len(real))
	secret = make(map[string]string)
	stored = make(map[string]string, len(real))
	for name, value := range real {
		if r, ok := redacted[name]; ok && r != value {
			secret[name] = value
			stored[name] = r
			continue
		}
		plain[name] = value
		stored[name] = value
	}
	return plain, secret, stored
}

func (m *Manager) withSecrets(spec *runtime.ContainerSpec, secret map[string]string) (*vault.EnvFile, error) {
	if len(secret) == 0 {
		return &vault.EnvFile{}, nil
	}
	dir, err := m.secretsDir()
	if err != nil {
		return nil, fmt.Errorf("deliver the secrets of %s: %w", spec.Name, err)
	}
	f, err := vault.WriteEnvFile(dir, spec.Name, secret)
	if err != nil {
		return nil, err
	}
	spec.EnvFile = f.Path
	return f, nil
}

func (m *Manager) secretsDir() (string, error) {
	return vault.SecretsDir(m.Dirs.Run)
}

func (m *Manager) runContainer(ctx context.Context, spec runtime.ContainerSpec, secret map[string]string) (string, error) {
	f, err := m.withSecrets(&spec, secret)
	if err != nil {
		return "", err
	}
	defer m.removeEnvFile(f)
	return m.Driver.Run(ctx, spec)
}

func (m *Manager) runAttached(ctx context.Context, spec runtime.ContainerSpec, secret map[string]string,
	streams runtime.Streams) (int, error) {
	f, err := m.withSecrets(&spec, secret)
	if err != nil {
		return 0, err
	}
	defer m.removeEnvFile(f)
	return m.Driver.RunAttached(ctx, spec, streams)
}

func (m *Manager) removeEnvFile(f *vault.EnvFile) {
	if err := f.Remove(); err != nil && m.Log != nil {
		fmt.Fprintf(m.Log, "caramelo: could not remove a secrets file: %v\n", err)
	}
}

func (m *Manager) ensureDepPasswords(ctx context.Context, rec *state.EnvRecord, deps []config.Dep,
	progress io.Writer) (*envSecrets, error) {
	secrets, err := m.secretsOf(ctx, rec.App, rec.Name)
	if err != nil {
		return nil, err
	}
	release := Mode(rec.Mode).IsRelease()

	secrets.withDepDefaults(deps, release)
	for _, dep := range deps {
		name := config.DepPasswordSecret(dep.Name)
		if _, ok := secrets.values[name]; ok {
			continue
		}

		if known, ok := config.Lookup(dep.Image); !ok || known.PasswordVar == "" {
			continue
		}
		if !release {
			continue
		}
		if m.Secrets == nil {
			return nil, fmt.Errorf("env %q is a release environment and dependency %q has no password: "+
				"this machine has no vault to generate one in "+
				"(run `caramelo server setup` again to create the vault key)", rec.Name, dep.Name)
		}
		value, err := vault.GeneratePassword()
		if err != nil {
			return nil, err
		}
		if _, err := m.Secrets.Set(ctx, vault.EnvRef(rec.App, rec.Name, name), value); err != nil {
			return nil, fmt.Errorf("store the generated password of dependency %q: %w", dep.Name, err)
		}
		secrets.values[name] = value
		progressf(progress, "changed", "secret", "%s generated (read it with `caramelo secrets export %s --reveal`)",
			name, rec.Name)
	}

	if m.Secrets != nil {
		digest, err := m.Secrets.Fingerprint(ctx, rec.App, rec.Name)
		if err != nil {
			return nil, fmt.Errorf("fingerprint the secrets of env %q: %w", rec.Name, err)
		}
		secrets.digest = digest
	}
	return secrets, nil
}

func depEnv(dep config.Dep, secrets *envSecrets) map[string]string {
	out := maps.Clone(dep.Env)
	if out == nil {
		out = map[string]string{}
	}
	known, ok := config.Lookup(dep.Image)
	if !ok || known.PasswordVar == "" {
		return out
	}
	password, _, ok := secrets.password(dep.Name)
	if !ok {
		return out
	}
	out[known.PasswordVar] = password
	return out
}

func depPasswordIsSecret(dep config.Dep, secrets *envSecrets) bool {
	if secrets == nil {
		return false
	}
	known, ok := config.Lookup(dep.Image)
	if !ok || known.PasswordVar == "" {
		return false
	}
	_, secret, ok := secrets.password(dep.Name)
	return ok && secret
}

func (m *Manager) SetSecretsBefore(ctx context.Context, app, name string, values map[string]string,
	progress io.Writer) error {
	if len(values) == 0 {
		return nil
	}
	if m.Secrets == nil {
		return errors.New("this machine has no vault, so secrets cannot be set " +
			"(run `caramelo server setup` again to create the vault key)")
	}
	names := make([]string, 0, len(values))
	for k := range values {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if err := vault.ValidateName(k); err != nil {
			return err
		}
		if _, err := m.Secrets.Set(ctx, vault.EnvRef(app, name, k), values[k]); err != nil {
			return err
		}
	}
	progressf(progress, "changed", "secrets", "%d set for %s before it was created: %s",
		len(names), name, strings.Join(names, ", "))
	return nil
}

func (m *Manager) RecordReveal(ctx context.Context, app, name string, names []string) {
	where := name
	if where == "" {
		where = "app " + app
	}
	detail := fmt.Sprintf("%d secret(s) of %s were read with their values", len(names), where)
	if len(names) > 0 {
		detail += ": " + strings.Join(names, ", ")
	}
	m.record(ctx, m.envIDOf(ctx, app, name), Event{
		App: app, Env: name,
		Action: "secrets", Step: "reveal", Status: progress.StatusWarning, Detail: detail,
	})
}

func (m *Manager) envIDOf(ctx context.Context, app, name string) int64 {
	if name == "" {
		return 0
	}
	rec, err := m.Store.Env(ctx, app, name)
	if err != nil || rec == nil {
		return 0
	}
	return rec.ID
}

func (m *Manager) ConfigWithSecrets(ctx context.Context, app, name string, cfg *config.App,
	reveal bool) (*config.App, error) {
	if cfg == nil {
		return nil, nil
	}
	secrets, err := m.secretsOf(ctx, app, name)
	if err != nil {
		return nil, err
	}

	secrets.withDepDefaults(cfg.Deps, m.isRelease(ctx, app, name))
	if secrets.empty() && len(secrets.defaults) == 0 {
		return cfg, nil
	}
	lookups := secrets.redacted()
	if reveal {
		lookups = secrets.lookups()
		m.RecordReveal(ctx, app, name, sortedSecretNames(secrets.values))
	}
	return cfg.WithSecretsExpanded(lookups), nil
}

func (m *Manager) isRelease(ctx context.Context, app, name string) bool {
	if name == "" || m.Store == nil {
		return false
	}
	rec, err := m.Store.Env(ctx, app, name)
	if err != nil {
		return false
	}
	return Mode(rec.Mode).IsRelease()
}

func sortedSecretNames(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for k := range values {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (m *Manager) SecretWarnings(ctx context.Context, app, name string, names []string) []string {
	if len(names) == 0 || app == "" || m.Store == nil {
		return nil
	}
	changed := make(map[string]bool, len(names))
	for _, n := range names {
		changed[n] = true
	}
	recs, err := m.Store.Envs(ctx, app)
	if err != nil {
		return nil
	}
	var out []string
	for i := range recs {
		if name != "" && recs[i].Name != name {
			continue
		}
		out = append(out, m.envSecretWarnings(ctx, &recs[i], changed)...)
	}
	sort.Strings(out)
	return out
}

func (m *Manager) envSecretWarnings(ctx context.Context, rec *state.EnvRecord, changed map[string]bool) []string {
	cfg, err := decodeConfig(rec)
	if err != nil || len(cfg.Deps) == 0 {
		return nil
	}
	resources, err := m.Store.Resources(ctx, rec.ID)
	if err != nil {
		return nil
	}
	started := map[string]bool{}
	for _, r := range resources {
		if r.Kind == state.ResourceContainer && r.Dep != "" {
			started[r.Dep] = true
		}
	}
	var out []string
	for _, dep := range cfg.Deps {
		if !started[dep.Name] || !changed[config.DepPasswordSecret(dep.Name)] {
			continue
		}
		known, ok := config.Lookup(dep.Image)
		if !ok || known.PasswordVar == "" {
			continue
		}
		msg := fmt.Sprintf("%s changed, but %s of env %s already exists and %s reads %s only when it "+
			"initialises its data directory: the new value is in the vault and in the next rollout's "+
			"environment, and the server still wants the old one",
			config.DepPasswordSecret(dep.Name), dep.Name, rec.Name,
			config.ImageName(dep.Image), known.PasswordVar)
		if known.PasswordChange != "" {
			msg += fmt.Sprintf(" — apply it with the app's own means (%s)", known.PasswordChange)
		}
		out = append(out, msg)
	}
	return out
}

func (m *Manager) RecordFetch(ctx context.Context, app, name, machine, detail string) {
	m.record(ctx, m.envIDOf(ctx, app, name), Event{
		App: app, Env: name, Machine: machine,
		Action: "secrets", Step: "bundle", Status: progress.StatusOK, Detail: detail,
	})
}
