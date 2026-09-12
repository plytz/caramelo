package cli

import (
	"fmt"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/fleet"

	"github.com/plytz/caramelo/internal/cli/ui"
)

func machinesView(ms []fleet.Machine, now time.Time) *ui.View {
	if len(ms) == 0 {
		return ui.NewView().Text("no machines")
	}
	t := machinesTable(ms, now)
	for _, m := range ms {
		if m.Private {

			t.Note("* private: no public listener at all; the hub is its door")
			break
		}
	}
	return ui.NewView().Table(t)
}

func machinesTable(ms []fleet.Machine, now time.Time) *ui.Table {
	if len(ms) == 0 {
		return nil
	}
	t := ui.NewTable("NAME", "ROLE", "ARCH", "SUBNET", "ENVS", "FREE", "SEEN")
	for _, m := range ms {
		t.Row(m.Name, machineRole(m), strOrDash(m.Arch), machineSubnet(m),
			fmt.Sprint(m.Envs), machineFree(m), describeSeen(m, now))
	}
	return t
}

func machineRole(m fleet.Machine) string {
	role := m.Role.String()
	if m.Private {
		role += "*"
	}
	return role
}

func machineSubnet(m fleet.Machine) string {
	if !m.Subnet.IsValid() {
		return "-"
	}
	return m.Subnet.String()
}

func machineFree(m fleet.Machine) string {
	if m.Gauge == nil {
		return "-"
	}
	return fmtBytesIEC(m.Gauge.Memory.AvailableBytes)
}

func describeSeen(m fleet.Machine, now time.Time) string {
	if m.Role.IsHub() {
		return "-"
	}
	if m.LastSeen.IsZero() {
		return "never"
	}
	age := now.Sub(m.LastSeen).Round(time.Second)
	if age < 0 {
		age = 0
	}
	if !m.Reachable(now) {
		return fmt.Sprintf("unreachable (%s ago)", age)
	}
	return fmt.Sprintf("%s ago", age)
}

func machineDetailView(d *api.MachineDetail, now time.Time) *ui.View {
	if d == nil {
		return ui.NewView().Text("no machine")
	}
	m := d.Machine
	head := fmt.Sprintf("%s  %s", strOr(m.Name, "(unnamed)"), machineRole(m))
	if m.Arch != "" {
		head += "  " + m.Arch
		if m.OS != "" {
			head += "/" + m.OS
		}
	}
	f := ui.NewFields(head)
	f.Add("subnet", "%s", machineSubnet(m))
	if addr := m.Address(); addr.IsValid() {
		f.Add("address", "%s", addr.String())
	}
	if m.Endpoint != "" {

		f.Add("last endpoint", "%s", m.Endpoint)
	}
	if m.Private {
		f.Add("door", "%s", "private: no public listener; the hub serves its names")
	}
	if !m.JoinedAt.IsZero() {
		f.Add("joined", "%s", m.JoinedAt.Local().Format(time.RFC3339))
	}
	f.Add("last seen", "%s", describeSeen(m, now))
	if g := d.Gauge; g != nil {
		f.Add("memory", "%s free of %s", fmtBytesIEC(g.Memory.AvailableBytes), fmtBytesIEC(g.Memory.TotalBytes))
		f.Add("cpu", "%d vCPU", g.CPU.Count)
		f.Add("os", "%s", describeOS(g.OS))
	}
	f.Add("environments", "%d", len(d.Envs))
	if m.PublicKey != "" {
		f.Add("public key", "%s", m.PublicKey)
	}
	v := ui.NewView().Fields(f)
	if d.Unreachable {
		v.Text("  ! unreachable: nothing above has been confirmed since the last handshake")
	}
	return v.Section(directoryTable(d.Envs))
}

func directoryTable(entries []fleet.DirectoryEntry) *ui.Table {
	if len(entries) == 0 {
		return nil
	}
	t := ui.NewTable("ENV", "APP", "MODE", "ADDRESS", "OWNER", "VIA")
	for _, e := range entries {
		t.Row(e.Env, e.App, strOrDash(e.Mode), strOrDash(e.Address), strOrDash(e.Owner), strOrDash(e.Via))
	}
	return t
}

func machineTokenView(r *api.MachineTokenResult) *ui.View {
	if r == nil {
		return ui.NewView().Text("no token")
	}
	v := ui.NewView().
		Text("On the machine that is joining, as root:").
		Blank().
		Text("  sudo caramelo machine join %s --token %s", strOr(r.Endpoint, r.Hub), r.Token).
		Blank()
	f := ui.NewFields("")
	f.Add("hub", "%s", strOrDash(r.Hub))
	if r.PublicKey != "" {
		f.Add("public key", "%s", r.PublicKey)
	}
	if !r.ExpiresAt.IsZero() {
		f.Add("expires", "%s", r.ExpiresAt.Local().Format(time.RFC3339))
	}
	f.Note("good for one machine, stored only as a hash: this is the only time it can be printed")
	return v.Fields(f)
}

func machineAddedView(r *api.MachineAddResult, now time.Time) *ui.View {
	if r == nil {
		return ui.NewView().Text("nothing was added")
	}
	v := ui.NewView()
	if r.Joined {
		v.Text("%s joined the fleet", r.Machine.Name)
	} else {
		v.Text("%s was already a member: setup ran again and nothing else changed", r.Machine.Name)
	}
	return v.Blank().Table(machinesTable([]fleet.Machine{r.Machine}, now))
}

func machineJoinedView(r *api.MachineJoinResult) *ui.View {
	if r == nil {
		return ui.NewView().Text("nothing joined")
	}
	v := ui.NewView()
	if r.Changed {
		v.Text("%s is now a member of %s", r.Machine.Name, r.Hub.Name)
	} else {
		v.Text("%s was already a member of %s; nothing changed", r.Machine.Name, r.Hub.Name)
	}
	v.Blank()
	f := ui.NewFields("")
	f.Add("subnet", "%s", machineSubnet(r.Machine))
	if addr := r.Machine.Address(); addr.IsValid() {
		f.Add("address", "%s", addr.String())
	}
	if r.Hub.Endpoint != "" {
		f.Add("hub", "%s", r.Hub.Endpoint)
	}
	if r.Machine.Private {
		f.Add("door", "%s", "private: no public listener; the hub serves its names")
	}
	f.Note("this machine dials the hub and keeps the tunnel alive; nothing ever dials it")
	return v.Fields(f)
}

func describeFleet(ms []fleet.Machine, self string, now time.Time) string {
	if len(ms) <= 1 {
		return "a fleet of one"
	}
	role := fleet.RoleHub
	members, unreachable := 0, 0
	for _, m := range ms {
		if m.Name == self {
			role = m.Role
		}
		if !m.Role.IsHub() {
			members++
		}
		if !m.Reachable(now) {
			unreachable++
		}
	}
	s := fmt.Sprintf("%s of %d machines, %d member(s)", role, len(ms), members)
	if unreachable > 0 {
		s += fmt.Sprintf(", %d unreachable", unreachable)
	}
	return s
}
