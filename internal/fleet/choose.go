package fleet

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/config"
)

func Choose(candidates []Candidate, d Demand, pref Preference, now time.Time) (Decision, error) {
	considered := make([]Consideration, 0, len(candidates))
	byName := make(map[string]Consideration, len(candidates))
	for _, c := range candidates {
		con := consider(c, d, now)
		considered = append(considered, con)
		byName[con.Machine] = con
	}

	if pin := strings.TrimSpace(pref.Pin); pin != "" {
		return choosePinned(pin, pref, considered, byName, d)
	}
	if pref.Placement == config.PlacementHub {

		hub := hubName(candidates)
		if hub == "" {
			return Decision{Considered: rank(considered)}, fmt.Errorf(
				"place %s/%s: caramelo.yaml says `placement: %s` and no hub is in the fleet: %w",
				d.App, d.Env, config.PlacementHub, ErrNoMachine)
		}
		p := pref
		p.Pin, p.PinnedByFile = hub, true
		return choosePinned(hub, p, considered, byName, d)
	}

	ranked := rank(considered)
	for _, con := range ranked {
		if !con.Fits {
			continue
		}
		return Decision{Machine: con.Machine, Why: why(con), Considered: ranked}, nil
	}
	return Decision{Considered: ranked}, fmt.Errorf("place %s/%s: %s: %w",
		d.App, d.Env, refusal(ranked), ErrNoMachine)
}

func choosePinned(pin string, pref Preference, considered []Consideration, byName map[string]Consideration, d Demand) (Decision, error) {
	ranked := rank(considered)
	con, ok := byName[pin]
	switch {
	case !ok:
		return Decision{Considered: ranked}, fmt.Errorf("place %s/%s: %s: %w",
			d.App, d.Env, unknownMachine(pin, pref, considered), ErrNoMachine)
	case !con.Fits:
		return Decision{Considered: ranked}, fmt.Errorf("place %s/%s: %s: %s: %w",
			d.App, d.Env, pinnedBy(pin, pref), con.Reason, ErrNoMachine)
	}
	return Decision{Machine: pin, Why: pinnedBy(pin, pref) + ": " + why(con), Considered: ranked}, nil
}

func consider(c Candidate, d Demand, now time.Time) Consideration {
	con := Consideration{Machine: c.Machine.Name, Envs: c.Machine.Envs}
	free, gauged := c.Free()
	con.FreeBytes = free

	switch {
	case con.Machine == "":
		con.Reason = "a machine with no name is not a machine"
	case !c.Machine.Reachable(now):
		con.Reason = unreachableReason(c.Machine, now)
	case d.Arch != "" && c.Machine.Arch != "" && c.Machine.Arch != d.Arch:
		con.Reason = fmt.Sprintf("it is %s and this environment needs %s", c.Machine.Arch, d.Arch)
	case d.Arch != "" && c.Machine.Arch == "":
		con.Reason = fmt.Sprintf("it has not said which architecture it is, and this environment needs %s", d.Arch)
	case !gauged:
		con.Reason = "it has never said how big it is, so it cannot be shown to fit"
	case d.MemoryBytes > 0 && free < d.MemoryBytes:
		con.Reason = fmt.Sprintf("%s free, %s asked", humanBytes(free), humanBytes(d.MemoryBytes))
	case d.MemoryBytes == 0 && free == 0:

		con.Reason = "it has no memory free at all"
	default:
		con.Fits = true
	}
	return con
}

func unreachableReason(m Machine, now time.Time) string {
	if m.LastSeen.IsZero() {
		return "it has never been heard from"
	}
	return fmt.Sprintf("it was last seen %s ago", now.Sub(m.LastSeen).Round(time.Second))
}

func rank(cons []Consideration) []Consideration {
	out := append([]Consideration(nil), cons...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Fits != b.Fits {
			return a.Fits
		}
		if a.FreeBytes != b.FreeBytes {
			return a.FreeBytes > b.FreeBytes
		}
		if a.Envs != b.Envs {
			return a.Envs < b.Envs
		}
		return a.Machine < b.Machine
	})
	return out
}

func why(con Consideration) string {
	envs := "1 env"
	if con.Envs != 1 {
		envs = fmt.Sprintf("%d envs", con.Envs)
	}
	return fmt.Sprintf("%s free, %s", humanBytes(con.FreeBytes), envs)
}

func pinnedBy(pin string, pref Preference) string {
	if pref.PinnedByFile {
		return fmt.Sprintf("caramelo.yaml pins this environment to %s", pin)
	}
	return fmt.Sprintf("--on %s", pin)
}

func unknownMachine(pin string, pref Preference, cons []Consideration) string {
	names := make([]string, 0, len(cons))
	for _, c := range cons {
		if c.Machine != "" {
			names = append(names, c.Machine)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Sprintf("%s: there is no such machine, and the fleet is empty", pinnedBy(pin, pref))
	}
	return fmt.Sprintf("%s: there is no such machine; the fleet is %s",
		pinnedBy(pin, pref), strings.Join(names, ", "))
}

func refusal(ranked []Consideration) string {
	if len(ranked) == 0 {
		return "there is no machine in the fleet"
	}
	parts := make([]string, 0, len(ranked))
	for _, c := range ranked {
		parts = append(parts, fmt.Sprintf("%s: %s", c.Machine, c.Reason))
	}
	return "no machine can hold it (" + strings.Join(parts, "; ") + ")"
}

func hubName(candidates []Candidate) string {
	for _, c := range candidates {
		if c.Machine.Role.IsHub() {
			return c.Machine.Name
		}
	}
	return ""
}

func humanBytes(n int64) string {
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
