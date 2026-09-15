package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/vault"
)

type secretsFlags struct {
	app string
	env string

	machine  bool
	appScope bool
	fromFile string
	reveal   bool
	format   string

	stdin bool
}

type secretsCmd struct {
	a *app
	f secretsFlags
}

func (a *app) secretsCmd() *cobra.Command {
	s := &secretsCmd{a: a}
	cmd := commanderCmd(&cobra.Command{
		Use:   "secrets",
		Short: "The machine's vault: scoped secrets, encrypted at rest",
		Long: `A secret belongs to one of three scopes: the machine (every app), an app
(every environment of it) or one environment. An environment resolves all three
with the narrowest winning, and every secret it resolves is injected into its
containers and available as ${secrets.NAME} in caramelo.yaml.

Values are encrypted under a key file the daemon owns, beside the state
database, so a copy of the database is not a compromise. Nothing prints a value
unless you ask for it with secrets export --reveal, which the machine records as
an event naming you.

A dependency's password is the secret <DEP>_PASSWORD of its environment, and it
can be set before the environment exists — which it has to be, because Postgres
and MySQL read their password only when they first initialise their data.`,
	})
	cmd.PersistentFlags().StringVar(&s.f.app, "app", os.Getenv("CARAMELO_APP"),
		"application these secrets belong to (default: the checkout you are in)")
	cmd.PersistentFlags().StringVar(&s.f.env, "env", os.Getenv("CARAMELO_ENV"),
		"environment (default: the positional argument, or the worktree you are in)")

	cmd.PersistentPreRunE = s.preRun
	asGroup(cmd)
	cmd.AddCommand(
		s.setCmd(),
		s.listCmd(),
		s.removeCmd(),
		s.exportCmd(),

		s.a.secretsBundleCmd(),
	)
	return cmd
}

func (s *secretsCmd) preRun(cmd *cobra.Command, args []string) error {
	if err := s.a.setProgress(); err != nil {
		return err
	}
	if s.a.service != nil {
		return nil
	}
	if err := s.resolveApp(cmd); err != nil {
		return err
	}
	if cmd.Name() == "set" {
		if err := s.moveValuesToStdin(cmd, args); err != nil {
			return err
		}
	}
	if err := s.injectEnv(cmd, args); err != nil {
		return err
	}
	return s.a.forwardIfCommander(cmd)
}

func (s *secretsCmd) injectEnv(cmd *cobra.Command, args []string) error {
	if s.f.machine || s.f.appScope || cmd.Flags().Changed("env") {
		return nil
	}
	for _, arg := range args {
		if env.ValidSlug(arg) {
			return nil
		}
	}
	name := strings.TrimSpace(s.f.env)
	if name == "" {
		name = envFromCheckout(cmd.Context(), ".")
	}
	if name == "" {
		return nil
	}
	if err := env.ValidateName("env", name); err != nil {
		return &usageError{err}
	}
	s.f.env = name
	s.a.args = injectFlag(s.a.args, "--env", name)
	return nil
}

func (s *secretsCmd) resolveApp(cmd *cobra.Command) error {
	if s.f.machine {
		return nil
	}
	name := strings.TrimSpace(s.f.app)
	if name == "" {
		name = appFromCheckout(cmd.Context(), ".")
	}
	if name == "" {
		return &usageError{errors.New(
			"cannot tell which app these secrets belong to: run this inside the app's checkout, " +
				"name it with --app NAME (or CARAMELO_APP), or use --machine-scope for a secret every app shares")}
	}
	if err := env.ValidateName("app", name); err != nil {
		return &usageError{err}
	}
	s.f.app = name
	if !cmd.Flags().Changed("app") {
		s.a.args = injectFlag(s.a.args, "--app", name)
	}
	return nil
}

func (s *secretsCmd) moveValuesToStdin(cmd *cobra.Command, args []string) error {
	values, rest, err := s.values(cmd.Context(), args)
	if err != nil {
		return err
	}

	for _, name := range rest {
		if env.ValidSlug(name) || vault.ValidateName(name) != nil {
			continue
		}
		if _, ok := values[name]; ok {
			continue
		}
		value, aerr := s.a.secretValue(cmd.Context(), name)
		if aerr != nil {
			return aerr
		}
		if verr := vault.ValidateValue(name, value); verr != nil {
			return &usageError{verr}
		}
		values[name] = value
	}
	if len(values) == 0 {
		return &usageError{errors.New(
			"nothing to set: pass NAME=VALUE pairs, or --from-file FILE with KEY=value lines")}
	}
	forwarded := stripSecretValues(s.a.args)
	if !s.f.stdin {
		forwarded = injectSwitch(forwarded, "--stdin")
	}
	s.a.args = forwarded
	s.a.stdin = strings.NewReader(renderSecretsJSON(values))
	return nil
}

func (s *secretsCmd) values(ctx context.Context, args []string) (map[string]string, []string, error) {
	values := map[string]string{}
	if s.f.fromFile != "" {
		from, err := readEnvFile(ctx, s.f.fromFile)
		if err != nil {
			return nil, nil, err
		}
		for k, v := range from {
			values[k] = v
		}
	}
	if s.f.stdin {
		from, err := parseSecrets(stdinFrom(ctx), "standard input")
		if err != nil {
			return nil, nil, err
		}
		for k, v := range from {
			values[k] = v
		}
	}
	rest := make([]string, 0, len(args))
	for _, arg := range args {
		name, value, ok := strings.Cut(arg, "=")
		if !ok {
			rest = append(rest, arg)
			continue
		}
		if err := vault.ValidateName(name); err != nil {
			return nil, nil, &usageError{err}
		}
		if err := vault.ValidateValue(name, value); err != nil {
			return nil, nil, &usageError{err}
		}
		values[name] = value
	}
	return values, rest, nil
}

func (s *secretsCmd) scopeFor(cmd *cobra.Command, rest []string) (vault.Scope, string, string, error) {
	switch {
	case s.f.machine && s.f.appScope:
		return "", "", "", &usageError{errors.New("--machine-scope and --app-scope are two different scopes: pass one")}
	case s.f.machine:
		if len(rest) > 0 {
			return "", "", "", &usageError{fmt.Errorf(
				"--machine-scope is the scope every app shares, so %q has no place in it", rest[0])}
		}
		return vault.ScopeMachine, "", "", nil
	case s.f.appScope:
		if len(rest) > 0 {
			return "", "", "", &usageError{fmt.Errorf(
				"--app-scope is the scope every environment of %s shares, so %q has no place in it",
				s.f.app, rest[0])}
		}
		if err := s.requireApp(); err != nil {
			return "", "", "", err
		}
		return vault.ScopeApp, s.f.app, "", nil
	}
	name, err := s.resolveEnv(cmd, rest)
	if err != nil {
		return "", "", "", err
	}
	if err := s.requireApp(); err != nil {
		return "", "", "", err
	}
	return vault.ScopeEnv, s.f.app, name, nil
}

func (s *secretsCmd) resolveEnv(cmd *cobra.Command, rest []string) (string, error) {
	for _, arg := range rest {
		if env.ValidSlug(arg) {
			return arg, nil
		}
	}
	if name := strings.TrimSpace(s.f.env); name != "" {
		if err := env.ValidateName("env", name); err != nil {
			return "", &usageError{err}
		}
		return name, nil
	}
	if s.a.service == nil {
		if name := envFromCheckout(cmd.Context(), "."); name != "" {
			return name, nil
		}
	}
	return "", &usageError{fmt.Errorf(
		"cannot tell which environment: name it (caramelo secrets %s ENV …), pass --env NAME, "+
			"set CARAMELO_ENV, or choose a wider scope with --app-scope or --machine-scope", cmd.Name())}
}

func (s *secretsCmd) requireApp() error {
	if strings.TrimSpace(s.f.app) == "" {
		return &usageError{errors.New("no app: pass --app NAME (or run the command inside the app's checkout)")}
	}
	return env.ValidateName("app", s.f.app)
}

func (s *secretsCmd) service() api.Service { return s.a.service }

func (s *secretsCmd) setCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set [ENV] [NAME=VALUE ...]",
		Short: "Write secrets at one scope",
		Long: `set writes one or more secrets. With an ENV they belong to that
environment; with --app-scope to every environment of the app; with --machine-scope to
every app on the machine.

--from-file reads KEY=value lines, so a .env.production goes in with one
command. A value that is already there is replaced and its version goes up, which
secrets list shows.

Setting a secret does not roll anything out: the next caramelo deploy (or
caramelo up, in a development environment) carries it, because a changed secret
is part of the definition. The exception is a dependency that was already
initialised with a different password — a database reads it once — and that is
said here rather than pretended away.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			values, rest, err := s.values(cmd.Context(), args)
			if err != nil {
				return err
			}
			if len(values) == 0 {
				return &usageError{errors.New(
					"nothing to set: pass NAME=VALUE pairs, --from-file FILE, or --stdin with KEY=value lines")}
			}
			scope, app, name, err := s.scopeFor(cmd, rest)
			if err != nil {
				return err
			}
			res, err := s.service().VaultSet(cmd.Context(), api.VaultSetRequest{
				Scope: scope, App: app, Env: name, Values: values,
			})
			if err != nil {
				return err
			}
			return s.a.printer().Result(res, func(w io.Writer) error {
				return writeVaultResult(w, res)
			})
		},
	}
	cmd.Flags().BoolVar(&s.f.machine, "machine-scope", false, "the machine's scope: every app")
	cmd.Flags().BoolVar(&s.f.appScope, "app-scope", false, "the app's scope: every environment of it")
	cmd.Flags().StringVar(&s.f.fromFile, "from-file", "", "read KEY=value lines from this file")
	cmd.Flags().BoolVar(&s.f.stdin, "stdin", false,
		"read the secrets from standard input: KEY=value lines, or one JSON object of names to values")
	return cmd
}

func (s *secretsCmd) listCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list [ENV]",
		Aliases: []string{"ls"},
		Short:   "Names, scopes and versions — never values",
		Long: `list prints every secret that applies, widest scope first, so the layers
read top to bottom and a name that is overridden appears above the value that
wins. With an ENV it also says which scope each resolved value comes from.

It never prints a value, so its output is safe to paste anywhere.`,
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {

			req := api.VaultListRequest{App: s.f.app}
			switch {
			case s.f.machine && s.f.appScope:
				return &usageError{errors.New(
					"--machine-scope and --app-scope are two different scopes: pass one")}
			case s.f.machine:

				req.Scope, req.App = vault.ScopeMachine, ""
			case s.f.appScope:
				if err := s.requireApp(); err != nil {
					return err
				}
				req.Scope = vault.ScopeApp
			default:
				if err := s.requireApp(); err != nil {
					return err
				}
				if resolved, err := s.resolveEnv(cmd, args); err == nil {
					req.Env = resolved
				}
			}
			res, err := s.service().VaultList(cmd.Context(), req)
			if err != nil {
				return err
			}
			return s.a.printer().Result(res, func(w io.Writer) error {
				return writeVaultResult(w, res)
			})
		},
	}
	cmd.Flags().BoolVar(&s.f.machine, "machine-scope", false, "only the machine's scope")
	cmd.Flags().BoolVar(&s.f.appScope, "app-scope", false, "only the app's scope")
	return cmd
}

func (s *secretsCmd) removeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "rm [ENV] NAME [NAME ...]",
		Aliases: []string{"remove"},
		Short:   "Forget secrets at one scope",
		Long: `rm forgets one or more secrets at the scope given. It does not touch the
same name at another scope: removing an environment's override leaves the app's
value, which the environment then resolves.

A name nobody set is an error, because that is almost always a typo.`,
		Args: minArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			names, rest := splitSecretNames(args)
			if len(names) == 0 {
				return &usageError{errors.New(
					"no secret named: a secret's name is upper-case (DB_PASSWORD), " +
						"an environment's is a lowercase slug")}
			}
			scope, app, name, err := s.scopeFor(cmd, rest)
			if err != nil {
				return err
			}
			res, err := s.service().VaultRemove(cmd.Context(), api.VaultRemoveRequest{
				Scope: scope, App: app, Env: name, Names: names,
			})
			if err != nil {
				return err
			}
			return s.a.printer().Result(res, func(w io.Writer) error {
				return writeVaultResult(w, res)
			})
		},
	}
	cmd.Flags().BoolVar(&s.f.machine, "machine-scope", false, "the machine's scope")
	cmd.Flags().BoolVar(&s.f.appScope, "app-scope", false, "the app's scope")
	return cmd
}

func (s *secretsCmd) exportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export ENV",
		Short: "An environment's resolved secrets, redacted unless --reveal",
		Long: `export prints the environment a container is given: every secret the
machine, the app and the environment define, merged with the narrowest winning.

Every value is <secret> unless --reveal is passed. --reveal is recorded as an
event naming you, because reading production's secrets is exactly the thing an
audit trail exists for.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := s.resolveEnv(cmd, args)
			if err != nil {
				return err
			}
			if err := s.requireApp(); err != nil {
				return err
			}
			res, err := s.service().VaultExport(cmd.Context(), api.VaultExportRequest{
				App: s.f.app, Env: name, Reveal: s.f.reveal, Format: s.f.format,
			})
			if err != nil {
				return err
			}
			return s.a.printer().Result(res, func(w io.Writer) error {
				return writeVaultExport(w, res)
			})
		},
	}
	cmd.Flags().BoolVar(&s.f.reveal, "reveal", false, "print the real values (recorded as an event)")
	cmd.Flags().StringVar(&s.f.format, "format", "env", "env (KEY=value lines) or json")
	return cmd
}

func splitSecretNames(args []string) (names, rest []string) {
	for _, arg := range args {
		if vault.ValidateName(arg) == nil {
			names = append(names, arg)
			continue
		}
		rest = append(rest, arg)
	}
	return names, rest
}

func stripSecretValues(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		tok := args[i]
		switch {
		case tok == "--from-file":
			i++
		case strings.HasPrefix(tok, "--from-file="):
		case !strings.HasPrefix(tok, "-") && strings.Contains(tok, "="):
		default:
			out = append(out, tok)
		}
	}
	return out
}

func readEnvFile(ctx context.Context, path string) (map[string]string, error) {
	if path == "-" {
		return parseSecrets(stdinFrom(ctx), "standard input")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read secrets from %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return parseSecrets(f, path)
}

func parseSecrets(r io.Reader, what string) (map[string]string, error) {
	br := bufio.NewReader(r)
	if err := skipSpace(br); err != nil {
		return nil, fmt.Errorf("read secrets from %s: %w", what, err)
	}
	if b, err := br.Peek(1); err == nil && b[0] == '{' {
		var values map[string]string
		if err := json.NewDecoder(br).Decode(&values); err != nil {
			return nil, fmt.Errorf("read secrets from %s: %w", what, err)
		}
		for name, value := range values {
			if err := vault.ValidateName(name); err != nil {
				return nil, fmt.Errorf("%s: %w", what, err)
			}
			if err := vault.ValidateValue(name, value); err != nil {
				return nil, fmt.Errorf("%s: %w", what, err)
			}
		}
		return values, nil
	}
	return parseEnvFile(br, what)
}

func skipSpace(br *bufio.Reader) error {
	for {
		b, err := br.Peek(1)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch b[0] {
		case ' ', '\t', '\n', '\r':
			if _, err := br.ReadByte(); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func parseEnvFile(r io.Reader, what string) (map[string]string, error) {
	values := map[string]string{}
	sc := bufio.NewScanner(r)

	sc.Buffer(make([]byte, 0, 64*1024), vault.MaxValueLen+vault.MaxNameLen+2)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		name, value, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: not a KEY=value line", what, line)
		}
		name = strings.TrimSpace(name)
		if err := vault.ValidateName(name); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", what, line, err)
		}
		value = unquote(strings.TrimSpace(value))
		if err := vault.ValidateValue(name, value); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", what, line, err)
		}
		values[name] = value
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read secrets from %s: %w", what, err)
	}
	return values, nil
}

func unquote(s string) string {
	if len(s) < 2 {
		return s
	}
	if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

func renderSecretsJSON(values map[string]string) string {
	b, err := json.Marshal(values)
	if err != nil {

		panic("cli: marshalling secrets: " + err.Error())
	}
	return string(b) + "\n"
}

func renderEnvFile(values map[string]string) string {
	names := make([]string, 0, len(values))
	for k := range values {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		fmt.Fprintf(&b, "%s=%s\n", k, values[k])
	}
	return b.String()
}
