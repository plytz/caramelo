package api

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/plytz/caramelo/internal/fleet"
)

const APIPort = 4022

type Directory interface {
	Locate(ctx context.Context, app, env string) (fleet.DirectoryEntry, bool, error)

	Machine(ctx context.Context, name string) (fleet.Machine, bool, error)
}

type FleetResolver struct {
	Self string

	Dir Directory

	Now func() time.Time
}

var _ Resolver = (*FleetResolver)(nil)

func (r *FleetResolver) ResolveEnv(ctx context.Context, app, name string) (Location, error) {
	if r == nil || r.Dir == nil {
		return Location{Local: true, Reachable: true}, nil
	}
	entry, ok, err := r.Dir.Locate(ctx, app, name)
	if err != nil {
		return Location{}, fmt.Errorf("look up env %s/%s in the fleet's directory: %w", app, name, err)
	}
	if !ok || entry.Machine == "" || entry.Machine == r.Self {
		return Location{Machine: r.Self, Local: true, Reachable: true}, nil
	}
	return r.ResolveMachine(ctx, entry.Machine)
}

func (r *FleetResolver) ResolveMachine(ctx context.Context, name string) (Location, error) {
	if r == nil || r.Dir == nil {
		if name != "" && name != r.self() {
			return Location{}, fmt.Errorf("this machine is not part of a fleet, so it knows no machine %q", name)
		}
		return Location{Local: true, Reachable: true}, nil
	}
	if name == "" || name == r.Self {
		return Location{Machine: r.Self, Local: true, Reachable: true}, nil
	}
	m, ok, err := r.Dir.Machine(ctx, name)
	if err != nil {
		return Location{}, fmt.Errorf("look up machine %q: %w", name, err)
	}
	if !ok {
		return Location{}, fmt.Errorf("no machine %q in this fleet: `caramelo member list` says which there are", name)
	}
	if m.Name == r.Self {
		return Location{Machine: r.Self, Local: true, Reachable: true}, nil
	}
	loc := Location{
		Machine:   m.Name,
		Reachable: m.Reachable(r.now()),
		LastSeen:  m.LastSeen,
	}
	if addr := m.Address(); addr.IsValid() {
		loc.Address = addr.String() + ":" + strconv.Itoa(APIPort)
	}
	return loc, nil
}

func (r *FleetResolver) now() time.Time {
	if r != nil && r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *FleetResolver) self() string {
	if r == nil {
		return ""
	}
	return r.Self
}

func Unreachable(loc Location) error {
	if loc.LastSeen.IsZero() {
		return fmt.Errorf("machine %q has never been heard from, so it cannot be sent work: "+
			"`caramelo member list` shows what the fleet knows", loc.Machine)
	}
	return fmt.Errorf("machine %q is unreachable (last seen %s ago): "+
		"it may be rebooting; `caramelo member list` shows what the fleet knows",
		loc.Machine, time.Since(loc.LastSeen).Round(time.Second))
}

func NoAddress(loc Location) error {
	return fmt.Errorf("machine %q has no address on the fleet's network yet, so nothing can be sent to it: "+
		"it is still joining, or its join did not finish", loc.Machine)
}
