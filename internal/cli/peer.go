package cli

import (
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/state"

	"github.com/plytz/caramelo/internal/cli/ui"
)

func init() {
	register(func(a *app) *cobra.Command {
		cmd := &cobra.Command{
			Use:   "peer",
			Short: "Manage the identities allowed on the machine's network",
			Long: `A peer is an identity — a laptop, an agent, a CI runner — admitted to
the machine's private network by its WireGuard public key. The machine never
answers a packet from a key it does not know, and every command that arrives
through the tunnel is logged under the peer's name.

'caramelo vpn up' adds this computer as a peer; these commands are for adding
the others.`,
		}
		asGroup(cmd)
		cmd.AddCommand(a.peerAddCmd(), a.peerListCmd(), a.peerRemoveCmd())
		return clientCmd(cmd)
	})
}

func (a *app) peerAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add NAME PUBLIC_KEY",
		Short: "Admit an identity by its WireGuard public key",
		Long: `Allocates an address for the peer, records it and installs it on the
running device. Adding the same name with the same key again changes nothing;
adding it with a different key rotates that peer's key and keeps its address.`,
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			peer, err := a.service.AddPeer(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			return a.printer().Result(peer, func(w io.Writer) error {
				return peerAddedView(peer).Write(w)
			})
		},
	}
}

func (a *app) peerListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the identities on the machine's network",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			peers, err := a.service.Peers(cmd.Context())
			if err != nil {
				return err
			}
			if peers == nil {
				peers = []state.Peer{}
			}
			return a.printer().Result(peers, func(w io.Writer) error {
				return renderPeers(w, peers)
			})
		},
	}
}

func renderPeers(w io.Writer, peers []state.Peer) error { return peersView(peers).Write(w) }

func peersView(peers []state.Peer) *ui.View {
	if len(peers) == 0 {
		return ui.NewView().Text("no peers; add this computer with 'caramelo vpn up'")
	}
	t := ui.NewTable("NAME", "ADDRESS", "PUBLIC KEY", "ADDED BY", "LAST HANDSHAKE")
	for _, p := range peers {
		t.Row(p.Name, p.IP, shortKey(p.PublicKey), strOr(p.AddedBy, "-"), sinceOrNever(p.LastHandshake))
	}
	return ui.NewView().Table(t)
}

func shortKey(key string) string {
	const shown = 12
	if len(key) <= shown {
		return key
	}
	return key[:shown] + "…"
}

func sinceOrNever(at time.Time) string {
	if at.IsZero() {
		return "never"
	}
	d := time.Since(at)
	if d < 0 {
		d = 0
	}
	return fmtUptime(d) + " ago"
}

func (a *app) peerRemoveCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:     "remove NAME",
		Aliases: []string{"rm"},
		Short:   "Revoke an identity by name",
		Long: `After this the machine stops answering that peer entirely: its next
request gets no reply at all, not a refusal, because WireGuard never answers an
unknown key.

Revoking the identity this session itself arrived as is refused without
--force, because it would take away the way in and this command's own answer
with it: the session stops rather than failing. Revoke it from another peer,
or on the machine itself, where the local socket is the way in.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			if err := a.service.RemovePeer(cmd.Context(), name, force); err != nil {
				return err
			}
			out := removed{Name: name, Removed: true}
			return a.printer().Result(out, func(w io.Writer) error {
				return removedView("peer", out).Write(w)
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"revoke it even when it is this session's own identity (the answer may never arrive)")
	return cmd
}

func peerAddedView(p *state.Peer) *ui.View {
	if p == nil {
		return ui.NewView().Text("no peer")
	}
	return ui.NewView().Text("added peer %s on %s", p.Name, p.IP)
}
