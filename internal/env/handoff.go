package env

import (
	"context"
	"fmt"
	"strings"
)

func (m *Manager) Handoff(ctx context.Context, app, name, to string) (*Env, error) {
	if err := ValidateName("app", app); err != nil {
		return nil, err
	}
	if err := ValidateName("env", name); err != nil {
		return nil, err
	}
	to = strings.TrimSpace(to)
	if to == "" {
		return nil, fmt.Errorf("hand env %q over: name the peer it goes to with --to", name)
	}
	if m.fw().Fleet == nil {
		return nil, fmt.Errorf("hand env %q over: this machine keeps no owner for its environments "+
			"(it was set up before M8, or it has no fleet state)", name)
	}
	defer m.lockEnv(app, name)()

	rec, err := m.env(ctx, app, name)
	if err != nil {
		return nil, err
	}
	row := m.fleetRow(ctx, rec.ID)
	from := row.Owner
	if from == to {
		e, err := recordToEnv(rec)
		if err != nil {
			return nil, err
		}
		m.stampFleet(e, row)
		return e, nil
	}
	row.Owner = to
	if err := m.setFleetRow(ctx, rec.ID, row); err != nil {
		return nil, err
	}
	m.event(ctx, rec.ID, "handoff", "changed", ownerLine(from, to))

	m.announce(ctx, rec)

	e, err := recordToEnv(rec)
	if err != nil {
		return nil, err
	}
	m.stampFleet(e, row)
	return e, nil
}

func ownerLine(from, to string) string {
	if from == "" {
		return "owner " + to
	}
	return from + " → " + to
}

func (m *Manager) Owned(ctx context.Context, app, owner string) ([]Env, error) {
	all, err := m.List(ctx, app)
	if err != nil {
		return nil, err
	}
	out := make([]Env, 0, len(all))
	for _, e := range all {
		if e.Owner == owner {
			out = append(out, e)
		}
	}
	return out, nil
}
