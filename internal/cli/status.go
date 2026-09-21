package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/fleet"
	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/cli/ui"
)

func init() {
	register(func(a *app) *cobra.Command {
		cmd := commanderCmd(&cobra.Command{
			Use:   "status",
			Short: "Show whether caramelod and Docker are up, and how we reached them",
			Long: `status answers the first question anyone asks: is the machine working, and
which machine am I actually talking to? It reports the daemon version and
uptime, the transport this command used (local socket or SSH), the Docker
daemon, the SSH listener and host key, and where everything lives on disk.`,
			Args: exactArgs(0),
			RunE: func(cmd *cobra.Command, args []string) error {
				st, err := a.service.Status(cmd.Context())
				if err != nil {
					return fmt.Errorf("status: %w", err)
				}
				res := &statusResult{Status: st, Fleet: fleetOf(cmd.Context(), a.service)}
				return a.printer().Result(res, func(w io.Writer) error {
					return writeStatusResult(w, res)
				})
			},
		})
		return available(cmd, onCommander.or(onServer))
	})
}

type statusResult struct {
	*api.Status

	Fleet []fleet.Machine `json:"fleet,omitempty"`
}

func fleetOf(ctx context.Context, svc api.Service) []fleet.Machine {
	ms, err := svc.Machines(ctx)
	switch {
	case errors.Is(err, api.ErrNotImplemented):
		return nil
	case err != nil:
		return []fleet.Machine{{Name: "(unknown: " + err.Error() + ")"}}
	}
	return ms
}

func writeStatus(w io.Writer, st *api.Status) error {
	return statusView(st, nil, time.Now()).Write(w)
}

func writeStatusResult(w io.Writer, r *statusResult) error {
	return statusResultView(r, time.Now()).Write(w)
}

func statusResultView(r *statusResult, now time.Time) *ui.View {
	if r == nil {
		return ui.NewView().Text("no status")
	}
	v := statusView(r.Status, r.Fleet, now)
	if len(r.Fleet) > 1 {
		v.Section(machinesTable(r.Fleet, now))
	}
	return v
}

func statusView(st *api.Status, ms []fleet.Machine, now time.Time) *ui.View {
	if st == nil {
		return ui.NewView().Text("no status")
	}
	head := fmt.Sprintf("caramelod %s on %s", strOr(st.Version, "unknown"),
		strOr(strOr(st.Machine, st.Hostname), "(unknown host)"))
	if st.Transport != "" {
		head += fmt.Sprintf(" (via %s)", st.Transport)
	}
	f := ui.NewFields(head)
	row := func(label, format string, args ...any) { f.Add(label, format, args...) }

	if st.Uptime > 0 || !st.StartedAt.IsZero() {
		up := fmtUptime(st.Uptime)
		if !st.StartedAt.IsZero() {
			up += fmt.Sprintf("  (since %s)", st.StartedAt.UTC().Format(time.RFC3339))
		}
		row("uptime", "%s", up)
	}
	row("docker", "%s", describeDockerStatus(st.Docker))
	if st.VPN != nil {
		row("tunnel", "%s", describeVPNStatus(st.VPN))
		if st.VPN.Enabled {
			row("peers", "%s", fmt.Sprintf("%d, %d environment(s) addressed, %d relayed port(s)",
				st.VPN.Peers, st.VPN.Envs, st.VPN.Routes))
			if st.VPN.PublicKey != "" {
				row("public key", "%s", st.VPN.PublicKey)
			}
		}
	}
	if st.SSHPort != 0 {
		row("ssh port", "%s", describeAPIListen(st))
	}
	if st.HostKeyFingerprint != "" {
		row("host key", "%s", st.HostKeyFingerprint)
	}
	if st.Identity != "" {
		row("identity", "%s", st.Identity)
	}

	if len(ms) > 1 {
		row("fleet", "%s", describeFleet(ms, strOr(st.Machine, st.Hostname), now))
	}
	if p := st.Production; p != nil {
		row("environments", "%s", describeEnvCounts(*p))
		row("vault", "%s", describeVault(*p))
		if line := describeFeed(*p); line != "" {
			row("feed", "%s", line)
		}
	}
	row("config", "%s", strOr(st.Paths.Config, "-"))
	row("state", "%s", strOr(st.Paths.State, "-"))
	row("data", "%s", strOr(st.Paths.Data, "-"))
	row("socket", "%s", strOr(st.Paths.Socket, "-"))
	return ui.NewView().Fields(f)
}

func describeEnvCounts(p api.ProductionStatus) string {
	s := fmt.Sprintf("%d, %d in release mode", p.Envs, p.Release)
	if p.Deploying > 0 {
		s += fmt.Sprintf(", %d deploy(s) in progress", p.Deploying)
	}
	if p.Held > 0 {
		s += fmt.Sprintf(", %d replica(s) held", p.Held)
	}
	if p.Unhealthy > 0 {
		s += fmt.Sprintf(", %d unhealthy", p.Unhealthy)
	}
	return s
}

func describeVault(p api.ProductionStatus) string {
	if !p.Vault {
		return "none: run `caramelo fleet setup` again to create the vault key"
	}
	return fmt.Sprintf("%d secret(s)", p.Secrets)
}

func describeFeed(p api.ProductionStatus) string {
	switch {
	case p.FeedDropped > 0:
		return fmt.Sprintf("%d watching, %d event(s) dropped to a reader that fell behind",
			p.Watching, p.FeedDropped)
	case p.Watching > 0:
		return fmt.Sprintf("%d watching", p.Watching)
	}
	return ""
}

func describeVPNStatus(v *api.VPNStatus) string {
	if !v.Enabled {
		s := "not running"
		if v.Error != "" {
			s += ": " + v.Error
		}
		return s
	}
	s := fmt.Sprintf("%s on %s (udp)", strOr(v.Address, "?"), strOr(v.Listen, "?"))
	if v.Subnet != "" {
		s += ", subnet " + v.Subnet
	}
	if v.Resolver != "" {
		s += ", resolver " + v.Resolver
	}
	return s
}

func describeAPIListen(st *api.Status) string {
	where := ""
	if st.VPN != nil {
		switch st.VPN.APIListen {
		case "vpn":
			where = " (inside the tunnel only)"
		case "both":
			where = " (public and inside the tunnel)"
		case "public":

			where = " (public only)"
		}
	}
	return fmt.Sprintf("%d%s", st.SSHPort, where)
}

func describeDockerStatus(d api.DockerStatus) string {
	if !d.Running {
		s := "not running"
		if d.Error != "" {
			s += ": " + d.Error
		}
		return s
	}
	mode := "rootful"
	if d.Rootless {
		mode = "rootless"
	}
	s := "running, " + mode
	if d.ServerVersion != "" {
		s += ", " + d.ServerVersion
	}
	return s
}

func fmtUptime(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)
	days := int(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	h := int(d / time.Hour)
	d -= time.Duration(h) * time.Hour
	m := int(d / time.Minute)
	s := int((d - time.Duration(m)*time.Minute) / time.Second)
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, h, m)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
