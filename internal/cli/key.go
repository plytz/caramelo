package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/state"

	"github.com/plytz/caramelo/internal/cli/ui"
)

func init() {
	register(func(a *app) *cobra.Command {
		cmd := &cobra.Command{
			Use:   "key",
			Short: "Manage the SSH keys allowed to use the API",
			Long: `Keys are the only way in: caramelod re-reads its authorized_keys on
every connection, and the key's name is the identity logged with every command.`,
		}
		asGroup(cmd)
		cmd.AddCommand(a.keyAddCmd(), a.keyListCmd(), a.keyRemoveCmd())
		return commanderCmd(cmd)
	})
}

type stdinKey struct{}

func withStdin(ctx context.Context, r io.Reader) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, stdinKey{}, r)
}

func stdinFrom(ctx context.Context) io.Reader {
	if r, ok := ctx.Value(stdinKey{}).(io.Reader); ok && r != nil {
		return r
	}
	return os.Stdin
}

func (a *app) keyAddCmd() *cobra.Command {
	var name, options, file string
	cmd := &cobra.Command{
		Use:   "add --name NAME [--file KEY.pub]",
		Short: "Authorize a public key, read from a file or standard input",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(name) == "" {
				return &usageError{fmt.Errorf(
					"--name is required here: it is the identity this key is logged under, and only a " +
						"commander has a name of its own to fall back on")}
			}
			line, err := readKeyLine(cmd.Context(), file)
			if err != nil {
				return err
			}
			key, err := a.service.AddKey(cmd.Context(), name, options, line)
			if err != nil {
				return err
			}
			return a.printer().Result(key, func(w io.Writer) error {
				return keyAddedView(key).Write(w)
			})
		},
	}
	cmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if err := a.beforeRun(cmd); err != nil {
			return err
		}
		if a.service == nil && strings.TrimSpace(name) == "" {
			if own := commanderPeerName(""); own != "" {
				name = own
				a.args = injectFlag(a.args, "--name", own)
			}
		}
		return a.forwardIfCommander(cmd)
	}
	cmd.Flags().StringVar(&name, "name", "",
		"identity for this key (unique, no spaces; default: the commander's own name)")
	cmd.Flags().StringVar(&options, "options", "", "authorized_keys options, e.g. caramelo-role=admin")
	cmd.Flags().StringVarP(&file, "file", "f", "", "read the key from this file instead of standard input")
	return available(cmd, onCommander.or(onHub))
}

func readKeyLine(ctx context.Context, file string) (string, error) {
	var (
		b   []byte
		err error
	)
	if file != "" {
		b, err = os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read key file: %w", err)
		}
	} else {
		b, err = io.ReadAll(io.LimitReader(stdinFrom(ctx), 1<<16))
		if err != nil {
			return "", fmt.Errorf("read key from standard input: %w", err)
		}
	}
	line := strings.TrimSpace(string(b))
	if line == "" {
		if file != "" {
			return "", fmt.Errorf("%s is empty", file)
		}
		return "", fmt.Errorf("no key on standard input; pass --file or pipe one in")
	}
	if strings.ContainsAny(line, "\n\r") {
		return "", fmt.Errorf("expected exactly one public key, got several lines")
	}
	return line, nil
}

func (a *app) keyListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the authorized keys",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			keys, err := a.service.Keys(cmd.Context())
			if err != nil {
				return err
			}
			if keys == nil {
				keys = []state.Key{}
			}
			return a.printer().Result(keys, func(w io.Writer) error {
				return renderKeys(w, keys)
			})
		},
	}
	return available(cmd, onCommander.or(onHub))
}

func renderKeys(w io.Writer, keys []state.Key) error { return keysView(keys).Write(w) }

func keysView(keys []state.Key) *ui.View {
	if len(keys) == 0 {
		return ui.NewView().Text("no keys authorized")
	}
	t := ui.NewTable("NAME", "TYPE", "FINGERPRINT", "ADDED", "OPTIONS")
	for _, k := range keys {
		added := "-"
		if !k.AddedAt.IsZero() {
			added = k.AddedAt.Local().Format(time.RFC3339)
		}
		t.Row(k.Name, k.Type, k.Fingerprint, added, strOrDash(k.Options))
	}
	return ui.NewView().Table(t)
}

func (a *app) keyRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "remove NAME",
		Aliases: []string{"rm"},
		Short:   "Revoke a key by name",
		Args:    exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := a.service.RemoveKey(cmd.Context(), name); err != nil {
				return err
			}
			out := removed{Name: name, Removed: true}
			return a.printer().Result(out, func(w io.Writer) error {
				return removedView("key", out).Write(w)
			})
		},
	}
	return available(cmd, onCommander.or(onHub))
}

func keyAddedView(k *state.Key) *ui.View {
	if k == nil {
		return ui.NewView().Text("no key")
	}
	return ui.NewView().Text("added key %s (%s %s)", k.Name, k.Type, k.Fingerprint)
}

type removed struct {
	Name    string `json:"name"`
	Removed bool   `json:"removed"`
}

func removedView(kind string, r removed) *ui.View {
	return ui.NewView().Text("removed %s %s", kind, r.Name)
}
