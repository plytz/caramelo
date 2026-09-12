package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/env"
)

func (e *envCmd) setSecretsBeforeCreate(cmd *cobra.Command, args []string) error {
	path := strings.TrimSpace(e.create.secretsFrom)
	if path == "" {
		return nil
	}
	if len(args) == 0 {

		return nil
	}
	name := args[0]
	if err := env.ValidateName("env", name); err != nil {
		return &usageError{err}
	}
	values, err := readEnvFile(cmd.Context(), path)
	if err != nil {
		return err
	}
	if len(values) == 0 {
		return &usageError{fmt.Errorf("%s has no KEY=value lines: nothing to put in the vault", path)}
	}
	e.a.args = stripFlagWithValue(e.a.args, "--secrets-from")

	sub := &app{
		stdout:  io.Discard,
		stderr:  e.a.stderr,
		machine: e.a.machine,
		args: []string{"secrets", "set", name,
			"--app", e.app, "--stdin", "--json"},
		stdin: strings.NewReader(renderSecretsJSON(values)),
	}
	e.a.printer().Infof("vault: %d secret(s) from %s, at the scope of %s/%s",
		len(values), path, e.app, name)
	code, err := forward(cmd.Context(), sub)
	if err != nil {
		return fmt.Errorf("write the secrets from %s before creating %s: %w", path, name, err)
	}
	if code != ExitOK {
		return errors.New("the secrets from " + path + " could not be written, so " + name +
			" was not created: its dependencies would have taken the wrong password")
	}
	return nil
}

func stripFlagWithValue(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == flag:
			i++
		case strings.HasPrefix(args[i], flag+"="):
		default:
			out = append(out, args[i])
		}
	}
	return out
}
