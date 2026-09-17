package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/machine"
	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/cli/ui"
)

func init() {
	register(func(a *app) *cobra.Command {
		cmd := commanderCmd(&cobra.Command{
			Use:   "machine",
			Short: "Inspect this machine, and the fleet it is part of",
			Long: `The machine record describes the box: CPU, memory, disks, OS, network,
cgroups and Docker. It is gauged at setup and at every caramelod start.

The group is also the fleet's (M8): add, token and join are how a box becomes a
member of a fleet; list, show and remove are what the fleet says about itself.
A machine that has joined nothing is a fleet of one.`,
		})
		asGroup(cmd)
		cmd.AddCommand(
			a.machineShowCmd(),

			a.machineAddCmd(),
			a.machineTokenCmd(),
			a.machineJoinCmd(),
			a.machineLeaveCmd(),
			a.machineListCmd(),
			a.machineRemoveCmd(),

			a.machineAnnounceCmd(),
			a.machineRedeemCmd(),
			a.machineRemovedCmd(),
		)
		return cmd
	})
}

func (a *app) machineShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show [NAME]",
		Short: "Print the machine record, or one machine of the fleet",
		Long: `show prints this machine's record: what the box is, how big it is, where
its data lives and what Docker is doing.

With a NAME it prints that machine of the fleet instead — its architecture,
subnet, gauge, environments and last handshake — which is the same question
asked about somebody else's box.`,
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {

				d, err := a.service.MachineInfo(cmd.Context(), args[0])
				if err != nil {
					return fmt.Errorf("machine show %s: %w", args[0], err)
				}
				if a.json {
					return a.indentedJSON(d)
				}
				return a.printer().Result(d, func(w io.Writer) error {
					return writeMachineDetail(w, d)
				})
			}
			rec, err := a.service.Machine(cmd.Context())
			if err != nil {
				return fmt.Errorf("machine show: %w", err)
			}
			if a.json {
				return a.indentedJSON(rec)
			}
			return a.printer().Result(rec, func(w io.Writer) error {
				return writeMachine(w, rec)
			})
		},
	}
}

func (a *app) indentedJSON(v any) error {
	enc := json.NewEncoder(a.stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}
	return nil
}

func writeMachine(w io.Writer, r *machine.Record) error { return machineView(r).Write(w) }

func machineView(r *machine.Record) *ui.View {
	if r == nil {
		return ui.NewView().Text("no machine record: the box has not been gauged yet")
	}
	head := strOr(r.Hostname, "(unknown host)")
	if r.MachineID != "" {
		head += "  " + shortID(r.MachineID)
	}
	if !r.GaugedAt.IsZero() {
		head += "  gauged " + r.GaugedAt.UTC().Format(time.RFC3339)
	}
	f := ui.NewFields(head)
	row := func(label, format string, args ...any) { f.Add(label, format, args...) }

	row("os", "%s", describeOS(r.OS))
	cpu := fmt.Sprintf("%d vCPU", r.CPU.Count)
	if r.CPU.Model != "" {
		cpu += "  " + r.CPU.Model
	}
	row("cpu", "%s", cpu)
	row("memory", "%s", describeMemory(r.Memory))
	row("data dir", "%s", describeDataDir(r.DataDir))
	if len(r.Disks) > 0 {
		var disks []string
		for _, d := range r.Disks {
			kind := "ssd"
			if d.Rotational {
				kind = "hdd"
			}
			disks = append(disks, fmt.Sprintf("%s %s %s", d.Name, fmtBytesIEC(d.SizeBytes), kind))
		}
		row("disks", "%s", strings.Join(disks, ", "))
	}
	row("network", "%s", describeNetwork(r.Network))
	if r.Cgroup.Version != 0 {
		cg := fmt.Sprintf("v%d", r.Cgroup.Version)
		if len(r.Cgroup.Controllers) > 0 {
			cg += "  " + strings.Join(r.Cgroup.Controllers, " ")
		}
		row("cgroup", "%s", cg)
	}
	row("docker", "%s", describeDocker(r.Docker))
	row("reserved", "%s, %.2f CPU", fmtBytesIEC(r.Reserved.MemoryBytes), r.Reserved.CPU)
	row("caramelo", "%s", describeCaramelo(r.Caramelo))
	row("paths", "config %s, state %s, data %s", strOr(r.Dirs.Config, "-"), strOr(r.Dirs.State, "-"), strOr(r.Dirs.Data, "-"))
	for _, warn := range r.Docker.Warnings {
		f.Note("  ! %s", warn)
	}
	return ui.NewView().Fields(f)
}

func describeOS(o machine.OS) string {
	parts := []string{strings.TrimSpace(strOr(o.ID, "unknown") + " " + o.VersionID)}
	if o.Codename != "" {
		parts[0] += " (" + o.Codename + ")"
	}
	if o.Kernel != "" {
		parts = append(parts, "kernel "+o.Kernel)
	}
	if o.Arch != "" {
		parts = append(parts, o.Arch)
	}
	if o.Virt != "" && o.Virt != "none" {
		parts = append(parts, o.Virt)
	}
	return strings.Join(parts, ", ")
}

func describeMemory(m machine.Memory) string {
	s := fmt.Sprintf("%s total, %s available", fmtBytesIEC(m.TotalBytes), fmtBytesIEC(m.AvailableBytes))
	if m.SwapTotalBytes > 0 {
		s += ", " + fmtBytesIEC(m.SwapTotalBytes) + " swap"
		if m.SwapManaged {
			s += " (caramelo)"
		}
	} else {
		s += ", no swap"
	}
	return s
}

func describeDataDir(m machine.Mount) string {
	s := strOr(m.Path, "-")
	if m.SizeBytes > 0 {
		s += fmt.Sprintf("  %s free of %s", fmtBytesIEC(m.AvailBytes), fmtBytesIEC(m.SizeBytes))
	}
	if m.FSType != "" || m.Source != "" {
		s += fmt.Sprintf("  (%s on %s)", strOr(m.FSType, "?"), strOr(m.Source, "?"))
	}
	if m.OwnMountPoint {
		s += "  own mount point"
	}
	return s
}

func describeNetwork(n machine.Network) string {
	if n.PrimaryIP == "" && len(n.Addresses) == 0 {
		return "-"
	}
	s := n.PrimaryIP
	if n.PrimaryIface != "" {
		s += " on " + n.PrimaryIface
	}
	var others []string
	for _, a := range n.Addresses {
		if a != n.PrimaryIP {
			others = append(others, a)
		}
	}
	if len(others) > 0 {
		s += "  also " + strings.Join(others, " ")
	}
	return strings.TrimSpace(s)
}

func describeDocker(d machine.Docker) string {
	if !d.Installed {
		return "not installed"
	}
	if d.ServerVersion == "" {
		return "installed, daemon not reachable"
	}
	mode := "rootful"
	if d.Rootless {
		mode = "rootless"
	}
	s := fmt.Sprintf("%s %s", d.ServerVersion, mode)
	if d.StorageDriver != "" {
		s += ", " + d.StorageDriver
	}
	if d.NetDriver != "" || d.PortDriver != "" {
		s += fmt.Sprintf(", %s/%s", strOr(d.NetDriver, "?"), strOr(d.PortDriver, "?"))
	}
	if d.DataRoot != "" {
		s += ", data " + d.DataRoot
	}
	return s
}

func describeCaramelo(c machine.Caramelo) string {
	s := strOr(c.Version, "unknown")
	if c.User != "" {
		s += fmt.Sprintf(", user %s(%d)", c.User, c.UID)
	}
	if c.SSHPort != 0 {
		s += fmt.Sprintf(", ssh port %d", c.SSHPort)
	}
	if c.HostKeyFingerprint != "" {
		s += ", " + c.HostKeyFingerprint
	}
	return s
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func strOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func fmtBytesIEC(n int64) string {
	if n < 0 {
		return "-"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
