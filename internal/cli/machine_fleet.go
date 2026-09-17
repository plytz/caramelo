package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/serverconfig"
)

func writeMachines(w io.Writer, ms []fleet.Machine) error {
	return machinesView(ms, time.Now()).Write(w)
}

func writeMachineDetail(w io.Writer, d *api.MachineDetail) error {
	return machineDetailView(d, time.Now()).Write(w)
}

func writeMachineToken(w io.Writer, r *api.MachineTokenResult) error {
	return machineTokenView(r).Write(w)
}

func (a *app) machineAddCmd() *cobra.Command {
	var name, binary, release, acmeEmail, acmeCA, tls string
	var edge, private bool
	cmd := localCmd(&cobra.Command{
		Use:   "add TARGET",
		Short: "Set a box up and join it to this fleet",
		Long: `add turns a box into a Caramelo machine and joins it to the hub in one
command: it ships a binary for the target's own architecture over ssh, runs
'caramelo server setup' there as root, takes a one-time join token from the hub
and redeems it, and returns once the machine answers in 'caramelo machine list'.

TARGET is an ssh destination the commander can reach (you@box.example.com).
The box itself never needs an inbound port: from the join onwards it dials the
hub and keeps the tunnel alive, which is why a box behind a home router joins
exactly like a box in a rack.

Adding a box that is already a member re-runs setup and changes nothing else.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkDoorFlags(edge, private); err != nil {
				return err
			}
			if err := checkBinarySource(cmd.Flags(), args[0], binary, release); err != nil {
				return err
			}
			if binary != "" {
				if _, err := os.Stat(binary); err != nil {
					return &usageError{fmt.Errorf("--binary names a local file: %w", err)}
				}
			}
			return a.runMachineAdd(cmd, args[0], machineAddSpec{
				Name: name, Binary: binary, Release: release, Edge: edge, Private: private,
				ACMEEmail: acmeEmail, ACMECA: acmeCA, TLS: tls,
			})
		},
	})
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "what to call the machine in the fleet (default: the box's hostname)")
	f.BoolVar(&edge, "edge", false, "install the public edge on it (ports 80 and 443)")
	f.BoolVar(&private, "private", false,
		"a member with no public listener at all: everything it hosts is served through the hub")
	f.StringVar(&binary, "binary", "",
		"executable to ship instead of this one, for a target of another architecture")
	f.StringVar(&release, "release", "",
		"ship this release of caramelo to the target instead of this binary: a tag such as v0.0.1, downloaded from GitHub for the target's platform")

	f.StringVar(&acmeEmail, "acme-email", "", "address to register with the certificate authority")
	f.StringVar(&acmeCA, "acme-ca", "", "ACME directory URL (default: Let's Encrypt production)")
	f.StringVar(&tls, "tls", "", "where the member's certificates come from: "+strings.Join(serverconfig.TLSValues, ", "))
	return cmd
}

type machineAddSpec struct {
	Name      string
	Binary    string
	Release   string
	Edge      bool
	Private   bool
	ACMEEmail string
	ACMECA    string
	TLS       string
}

func (a *app) machineTokenCmd() *cobra.Command {
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Print a one-time token a machine can join with",
		Long: `token asks the hub for a join token and prints it once, with the endpoint
and public key a machine needs in order to peer with the hub.

It is printed once because only its hash is stored: nothing can ever print it
again, and a copy of the machine's state database is not a way into the fleet.
It is good for one machine and it expires.

Use it when the commander cannot ssh to the box that is joining. On the box:

    sudo caramelo machine join <hub endpoint> --token <token>`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			if ttl < 0 {
				return &usageError{errors.New("--ttl is how long the token lives: it cannot be negative")}
			}
			res, err := a.service.MachineToken(cmd.Context(), api.MachineTokenRequest{TTL: ttl})
			if err != nil {
				return fmt.Errorf("machine token: %w", err)
			}
			return a.printer().Result(res, func(w io.Writer) error {
				return writeMachineToken(w, res)
			})
		},
	}
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "how long the token is good for (default 1h, at most 24h)")
	return cmd
}

func (a *app) machineJoinCmd() *cobra.Command {
	var token, name, configDir string
	var private bool
	cmd := localCmd(&cobra.Command{
		Use:   "join HUB",
		Short: "Join this machine to a hub (run on the machine, as root)",
		Long: `join makes this box a member of a fleet: it writes the hub into
/etc/caramelo/config.yaml, peers with it, announces what this machine is and
what it holds, and from then on dials out and keeps the tunnel alive.

HUB is the hub's endpoint — a hostname, optionally with the UDP port. Run it on
the machine that is joining, as root, with a token from 'caramelo machine token'
on the hub.

A member keeps serving everything it already holds while the hub is down. What
it cannot do without the hub is start a definition it has not started before,
because the secrets for it live there.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := ticketFrom(cmd.Context(), token)
			if err != nil {
				return err
			}
			return a.runMachineJoin(cmd.Context(), configDir, args[0], t, name, private)
		},
	})
	f := cmd.Flags()
	f.StringVar(&token, "token", "", "the one-time token from `caramelo machine token` on the hub, or - to read it from standard input")
	f.StringVar(&name, "name", "", "what to call this machine in the fleet (default: its hostname)")
	f.BoolVar(&private, "private", false, "join with no public listener: the hub is this machine's only door")
	f.StringVar(&configDir, "config-dir", serverconfig.DefaultConfigDir, "directory holding config.yaml")
	return cmd
}

func (a *app) machineLeaveCmd() *cobra.Command {
	var configDir string
	var force bool
	cmd := localCmd(&cobra.Command{
		Use:   "leave",
		Short: "Leave the fleet this machine joined (run on the machine, as root)",
		Long: `leave takes the fleet block out of this machine's configuration and
restarts caramelod, which comes back a machine of one: its environments, its
ports, its addresses and its edge exactly as they were, obeying nobody.

It is the mirror of 'caramelo machine join' and it is run in the same place, on
the machine itself, as root. Removing a machine from the hub's side is
'caramelo machine remove NAME' there; a member finds out at its next
announcement and stops obeying at once, and this is what tidies the file.

The subnet stays: this machine's environments hold addresses in it.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runMachineLeave(cmd.Context(), configDir, force)
		},
	})
	f := cmd.Flags()
	f.StringVar(&configDir, "config-dir", serverconfig.DefaultConfigDir, "directory holding config.yaml")
	f.BoolVar(&force, "force", false, "do not warn that the hub may still hold this machine")
	return cmd
}

func (a *app) machineListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the machines of the fleet",
		Long: `list shows every machine the hub knows: its role, architecture, subnet,
how many environments it holds, how much room it has left, and when it was last
heard from.

A machine that has not been heard from within the keepalive window reads
'unreachable'. On a machine that is not part of a fleet it is one row, its own.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			ms, err := a.service.Machines(cmd.Context())
			if err != nil {
				return fmt.Errorf("machine list: %w", err)
			}
			if ms == nil {
				ms = []fleet.Machine{}
			}
			return a.printer().Result(ms, func(w io.Writer) error {
				return writeMachines(w, ms)
			})
		},
	}
}

func (a *app) machineRemoveCmd() *cobra.Command {
	var force, yes bool
	cmd := &cobra.Command{
		Use:     "remove NAME",
		Aliases: []string{"rm"},
		Short:   "Remove a machine from the fleet",
		Long: `remove drops a machine's peer, frees its subnet and forgets its rows.

It is refused while the machine holds environments, and names them: a machine
removed out from under a running environment would leave that environment with
nothing to destroy it. --force destroys them first.

The machine itself keeps working; it goes back to being a Caramelo machine of
one.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			if !yes {
				return &usageError{fmt.Errorf(
					"removing machine %s needs confirmation: re-run with --yes", name)}
			}
			if err := a.service.MachineRemove(cmd.Context(),
				api.MachineRemoveRequest{Name: name, Force: force}, a.progressWriter()); err != nil {
				return fmt.Errorf("machine remove %s: %w", name, err)
			}
			out := removed{Name: name, Removed: true}
			return a.printer().Result(out, func(w io.Writer) error {
				return removedView("machine", out).Write(w)
			})
		},
	}

	cmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if err := a.setProgress(); err != nil {
			return err
		}
		if a.service == nil {
			if err := a.confirmMachineRemove(cmd, args, force, &yes); err != nil {
				return err
			}
		}
		return a.forwardIfCommander(cmd)
	}
	f := cmd.Flags()
	f.BoolVar(&force, "force", false, "destroy the environments the machine holds instead of refusing")
	f.BoolVar(&yes, "yes", false, "do not ask for confirmation")
	return cmd
}

func (a *app) confirmMachineRemove(cmd *cobra.Command, args []string, force bool, yes *bool) error {
	if *yes {
		return nil
	}
	name := "this machine"
	if len(args) > 0 {
		name = args[0]
	}
	needs := &usageError{fmt.Errorf("removing machine %s needs confirmation: re-run with --yes", name)}
	plan := fmt.Sprintf("Remove machine %s from the fleet: its peer, its subnet and its rows.\n", name)
	ctx := cmd.Context()
	var proceed bool
	var err error
	if force {
		plan = fmt.Sprintf(
			"Remove machine %s from the fleet AND destroy every environment it holds.\n", name)
		proceed, err = a.confirmName(ctx, needs, plan, fmt.Sprintf("Type %q to confirm: ", name), name)
	} else {
		proceed, err = a.confirmWith(ctx, needs, plan)
	}
	switch {
	case err != nil:
		return err
	case !proceed:
		return errors.New("cancelled")
	}
	*yes = true
	a.args = injectSwitch(a.args, "--yes")
	return nil
}

func checkDoorFlags(edge, private bool) error {
	if edge && private {
		return &usageError{errors.New(
			"--edge and --private are opposites: --private is a machine with no public listener at all, " +
				"and everything it hosts is served through the hub")}
	}
	return nil
}

const TokenFromStdin = "-"

func ticketFrom(ctx context.Context, token string) (string, error) {
	if strings.TrimSpace(token) == TokenFromStdin {
		b, err := io.ReadAll(io.LimitReader(stdinFrom(ctx), ticketLimit))
		if err != nil {
			return "", fmt.Errorf("read the join ticket from standard input: %w", err)
		}
		token = string(b)
	}
	if token = strings.TrimSpace(token); token == "" {
		return "", &usageError{errors.New(
			"joining needs a token from `caramelo machine token` on the hub: --token TOKEN (or --token - on standard input)")}
	}
	return token, nil
}

const ticketLimit = 64 << 10
