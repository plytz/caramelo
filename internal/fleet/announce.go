package fleet

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"
)

func Seen(machines []Machine, handshakes map[string]time.Time) []Machine {
	out := make([]Machine, len(machines))
	copy(out, machines)
	for i, m := range out {
		hs, ok := handshakes[m.PublicKey]
		if !ok || hs.IsZero() || !hs.After(m.LastSeen) {
			continue
		}
		out[i].LastSeen = hs
	}
	return out
}

func Unreachable(machines []Machine, now time.Time) []Machine {
	var out []Machine
	for _, m := range machines {
		if !m.Reachable(now) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func Find(machines []Machine, name string) (Machine, bool) {
	name = strings.TrimSpace(name)
	for _, m := range machines {
		if m.Name == name {
			return m, true
		}
	}
	return Machine{}, false
}

func HubOf(machines []Machine) (Machine, bool) {
	for _, m := range machines {
		if m.Role.IsHub() {
			return m, true
		}
	}
	return Machine{}, false
}

func Reconcile(existing []DirectoryEntry, a Announcement) (upsert, remove []DirectoryEntry, err error) {
	if err := a.Validate(); err != nil {
		return nil, nil, err
	}
	keep := make(map[[2]string]bool, len(a.Envs))
	for _, e := range a.Envs {
		e.Machine = a.Machine
		if e.UpdatedAt.IsZero() {
			e.UpdatedAt = a.At
		}
		keep[[2]string{e.App, e.Env}] = true
		upsert = append(upsert, e)
	}
	if !a.Full {
		return upsert, nil, nil
	}
	for _, e := range existing {
		if e.Machine != a.Machine || keep[[2]string{e.App, e.Env}] {
			continue
		}
		remove = append(remove, e)
	}
	sortEntries(upsert)
	sortEntries(remove)
	return upsert, remove, nil
}

func (a Announcement) Validate() error {
	if strings.TrimSpace(a.Machine) == "" {
		return fmt.Errorf("fleet: an announcement must say which machine it is from")
	}
	seen := make(map[[2]string]bool, len(a.Envs))
	for _, e := range a.Envs {
		switch {
		case strings.TrimSpace(e.App) == "" || strings.TrimSpace(e.Env) == "":
			return fmt.Errorf("fleet: %s announced an environment with no name", a.Machine)
		case e.Machine != "" && e.Machine != a.Machine:
			return fmt.Errorf("fleet: %s announced %s/%s as belonging to %s",
				a.Machine, e.App, e.Env, e.Machine)
		case seen[[2]string{e.App, e.Env}]:
			return fmt.Errorf("fleet: %s announced %s/%s twice", a.Machine, e.App, e.Env)
		}
		if e.Address != "" {
			if _, err := netip.ParseAddr(e.Address); err != nil {
				return fmt.Errorf("fleet: %s announced %s/%s at %q: %w", a.Machine, e.App, e.Env, e.Address, err)
			}
		}
		seen[[2]string{e.App, e.Env}] = true
	}
	return nil
}

func (m Machine) Validate() error {
	switch {
	case strings.TrimSpace(m.Name) == "":
		return fmt.Errorf("fleet: a machine needs a name")
	case strings.TrimSpace(m.PublicKey) == "":
		return fmt.Errorf("fleet: machine %s: a machine is its key, and this one has none", m.Name)
	case !m.Subnet.IsValid():
		return fmt.Errorf("fleet: machine %s: no subnet", m.Name)
	case !Contains(m.Subnet):
		return fmt.Errorf("fleet: machine %s: subnet %s is not a /%d of the fleet's range %s",
			m.Name, m.Subnet, SubnetBits, FleetRange)
	}
	if _, err := ParseRole(string(m.Role)); err != nil {
		return fmt.Errorf("fleet: machine %s: %w", m.Name, err)
	}
	if m.Private && m.Role.IsHub() {
		return fmt.Errorf("fleet: machine %s: a hub is the fleet's door and cannot be private", m.Name)
	}
	return nil
}

func sortEntries(es []DirectoryEntry) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].App != es[j].App {
			return es[i].App < es[j].App
		}
		return es[i].Env < es[j].Env
	})
}
