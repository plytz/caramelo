package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/api"
	cenv "github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/git"
)

func init() {
	register(func(a *app) *cobra.Command { return a.pushCheckCmd() })
	register(func(a *app) *cobra.Command { return a.imagesCmd() })
}

func (a *app) machineAnnounceCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "announce",
		Short:  "Tell the hub what this machine is and what it holds (machines only)",
		Hidden: true,
		Args:   exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			var ann fleet.Announcement
			if err := readJSON(a.in(cmd.Context()), &ann); err != nil {
				return fmt.Errorf("machine announce: %w", err)
			}
			res, err := a.service.MachineAnnounce(cmd.Context(), ann)
			if err != nil {
				return fmt.Errorf("machine announce: %w", err)
			}
			return json.NewEncoder(a.stdout).Encode(res)
		},
	}
}

func (a *app) machineRedeemCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "redeem",
		Short:  "Redeem a join ticket (the hub's half of a join)",
		Hidden: true,
		Args:   exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			var req api.RedeemRequest
			if err := readJSON(a.in(cmd.Context()), &req); err != nil {
				return fmt.Errorf("machine redeem: %w", err)
			}
			res, err := a.service.MachineRedeem(cmd.Context(), req)
			if err != nil {
				return err
			}
			return json.NewEncoder(a.stdout).Encode(res)
		},
	}
}

func (a *app) machineRemovedCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "removed",
		Short:  "Stop obeying the hub that has removed this machine (machines only)",
		Hidden: true,
		Args:   exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.service.MachineRemoved(cmd.Context()); err != nil {
				return err
			}
			return a.printer().Result(map[string]any{"left": true}, func(w io.Writer) error {
				_, err := fmt.Fprintln(w, "this machine is one of one again")
				return err
			})
		},
	}
}

func (a *app) secretsBundleCmd() *cobra.Command {
	var app, envName, reason string
	cmd := &cobra.Command{
		Use:    "bundle",
		Short:  "Fetch an environment's resolved secrets (machines only)",
		Hidden: true,
		Args:   exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := a.service.VaultBundle(cmd.Context(), api.BundleRequest{
				App: app, Env: envName, Reason: reason,
			})
			if err != nil {
				return err
			}
			return json.NewEncoder(a.stdout).Encode(b)
		},
	}
	f := cmd.Flags()
	f.StringVar(&app, "app", "", "the application")
	f.StringVar(&envName, "env", "", "the environment")
	f.StringVar(&reason, "reason", "", "what the secrets are for, for the hub's audit row")
	return cmd
}

func (a *app) imagesCmd() *cobra.Command {
	cmd := clientCmd(&cobra.Command{
		Use:    "images",
		Short:  "Move a release's images between machines (machines only)",
		Hidden: true,
	})
	cmd.AddCommand(
		a.imagesTransferCmd("send", true),
		a.imagesTransferCmd("receive", false),
		a.imagesOfCmd(),
		a.imagesRecordCmd(),
	)
	return cmd
}

func (a *app) imagesOfCmd() *cobra.Command {
	var app, tree string
	cmd := &cobra.Command{
		Use:    "of",
		Short:  "The release of one tree, and who holds its images",
		Hidden: true,
		Args:   exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			rel, err := a.service.ReleaseOfTree(cmd.Context(), app, tree)
			if err != nil {
				return err
			}
			return json.NewEncoder(a.stdout).Encode(rel)
		},
	}
	f := cmd.Flags()
	f.StringVar(&app, "app", "", "the application")
	f.StringVar(&tree, "tree", "", "the release's tree hash, which is its name")
	return cmd
}

func (a *app) imagesRecordCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "record",
		Short:  "Tell the hub what this machine holds (machines only)",
		Hidden: true,
		Args:   exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			var req api.ReleaseRecordRequest
			if err := readJSON(a.in(cmd.Context()), &req); err != nil {
				return fmt.Errorf("images record: %w", err)
			}
			return a.service.ReleaseRecord(cmd.Context(), req)
		},
	}
}

func (a *app) imagesTransferCmd(verb string, send bool) *cobra.Command {
	var release int64
	var arch string
	var refs []string
	cmd := &cobra.Command{
		Use:    verb,
		Short:  "Stream a release's images " + map[bool]string{true: "out", false: "in"}[send],
		Hidden: true,
		Args:   exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := api.ImageTransferRequest{Release: release, Refs: refs, Arch: arch, Send: send}
			var out io.Writer = a.stdout
			if !send {
				req.Archive = a.in(cmd.Context())
				out = io.Discard
			}
			res, err := a.service.ImageTransfer(cmd.Context(), req, out)
			if err != nil {
				return err
			}
			if send {

				return nil
			}
			return json.NewEncoder(a.stdout).Encode(res)
		},
	}
	f := cmd.Flags()
	f.Int64Var(&release, "release", 0, "the release the images belong to")
	f.StringArrayVar(&refs, "ref", nil, "an image reference to move (repeatable)")
	f.StringVar(&arch, "arch", "", "the architecture both ends must agree on")
	return cmd
}

func (e *envCmd) worktreeStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "worktree-status NAME",
		Short:  "Say whether an environment's checkout has uncommitted work",
		Hidden: true,
		Args:   exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := e.service().EnvWorktreeStatus(cmd.Context(), e.app, args[0])
			if err != nil {
				return err
			}
			return json.NewEncoder(e.a.stdout).Encode(st)
		},
	}
}

func (e *envCmd) syncCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "sync NAME",
		Short:  "Fetch this environment's branch from the hub and check it out",
		Hidden: true,
		Args:   exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ev, err := e.service().EnvSync(cmd.Context(), e.app, args[0])
			if err != nil {
				return err
			}
			return json.NewEncoder(e.a.stdout).Encode(ev)
		},
	}
}

func (a *app) pushCheckCmd() *cobra.Command {
	var repo string
	cmd := clientCmd(&cobra.Command{
		Use:    "push-check",
		Short:  "Ask whether a push may land (run by the pre-receive hook)",
		Hidden: true,
		Args:   exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(repo) == "" {
				return &usageError{errors.New("push-check: --repo is the bare repository the push is landing in")}
			}

			abs, err := filepath.Abs(repo)
			if err != nil {
				return fmt.Errorf("push-check: %s: %w", repo, err)
			}
			app := cenv.AppOfRepo(abs)
			if app == "" {
				return nil
			}
			branches, err := branchesOfRefs(a.in(cmd.Context()))
			if err != nil {
				return fmt.Errorf("push-check: %w", err)
			}
			if len(branches) == 0 {
				return nil
			}
			if err := a.service.GitPrecheck(cmd.Context(), app, branches); err != nil {

				fmt.Fprintf(a.stderr, "caramelo: %v\n", err)
				return &exitError{git.PushRefusedCode}
			}
			return nil
		},
	})
	cmd.Flags().StringVar(&repo, "repo", "", "the bare repository the push is landing in")
	return cmd
}

func branchesOfRefs(r io.Reader) ([]string, error) {
	if r == nil {
		return nil, nil
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read the refs being pushed: %w", err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		newRev, ref := fields[1], fields[2]
		if !strings.HasPrefix(ref, "refs/heads/") || isZeroRev(newRev) {
			continue
		}
		out = append(out, strings.TrimPrefix(ref, "refs/heads/"))
	}
	return out, nil
}

func isZeroRev(s string) bool { return strings.Trim(s, "0") == "" }

func readJSON(r io.Reader, v any) error {
	if r == nil {
		return errors.New("nothing on standard input")
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read the request: %w", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return errors.New("nothing on standard input")
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("read the request: %w", err)
	}
	return nil
}

func (a *app) in(ctx context.Context) io.Reader {
	if a.stdin != nil {
		return a.stdin
	}
	return stdinFrom(ctx)
}
