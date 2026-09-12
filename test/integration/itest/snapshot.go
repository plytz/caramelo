//go:build integration

package itest

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func Reset(m *Machine, state string) error {
	if m.Kind != KindMachine {
		return fmt.Errorf("reset %s: only a machine has states", m.Alias)
	}
	if state != StateClean && state != StateProvisioned {
		return fmt.Errorf("reset %s: unknown state %q", m.Alias, state)
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.Budget().Reset)
	defer cancel()
	if err := m.drv.reset(ctx, state); err != nil {
		return err
	}
	if err := installLabKeys(ctx, m); err != nil {
		return fmt.Errorf("reset %s to %q: %w", m.Alias, state, err)
	}
	return nil
}

func EnsureState(m *Machine, state string) error {
	if m.Kind != KindMachine {
		return fmt.Errorf("ensure %s: only a machine has states", m.Alias)
	}
	if state != StateClean && state != StateProvisioned {
		return fmt.Errorf("ensure %s: unknown state %q", m.Alias, state)
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.Budget().Reset)
	defer cancel()
	if err := m.drv.ensure(ctx, state); err != nil {
		return err
	}
	if err := installLabKeys(ctx, m); err != nil {
		return fmt.Errorf("ensure %s is %q: %w", m.Alias, state, err)
	}
	return nil
}

func MustReset(t testing.TB, m *Machine, state string) {
	t.Helper()
	start := time.Now()
	if err := Reset(m, state); err != nil {
		t.Fatalf("itest: %v", err)
	}
	t.Logf("itest: reset %s to %s in %s", m.Alias, state, time.Since(start).Round(time.Millisecond))
}

func Restart(m *Machine) error {
	if m.Kind != KindMachine {
		return fmt.Errorf("restart %s: only a machine can be power cycled", m.Alias)
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.Budget().Boot+time.Minute)
	defer cancel()
	return m.drv.restart(ctx)
}

func MustRestart(t testing.TB, m *Machine) {
	t.Helper()
	start := time.Now()
	if err := Restart(m); err != nil {
		t.Fatalf("itest: %v", err)
	}
	t.Logf("itest: restarted %s in %s", m.Alias, time.Since(start).Round(time.Millisecond))
}
