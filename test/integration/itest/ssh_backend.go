//go:build integration && e2e

package itest

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/test/e2e/inventory"
)

const TargetSSH = "ssh"

func init() {
	inventoryDriver = bindInventory
	sshBudgets = SSHBudgets
}

func SSHBudgets() Budgets {
	factor := 1.5
	if v := strings.TrimSpace(os.Getenv(FactorEnv)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			factor = f
		}
	}
	b := Budgets{
		Factor:     factor,
		Boot:       5 * time.Minute,
		Setup:      20 * time.Minute,
		Reset:      6 * time.Minute,
		Suite:      60 * time.Minute,
		PerMachine: 10 * time.Minute,
	}
	b.Boot = b.For(b.Boot)
	b.Setup = b.For(b.Setup)
	b.Reset = b.For(b.Reset)
	b.Suite = b.For(b.Suite)
	return b
}

func bindInventory(t testing.TB, l *Lab, want []*Machine) ([]*Machine, error) {
	inv, err := inventory.Load(l.inventoryPath)
	if err != nil {
		return nil, err
	}
	l.inventoryCount = inv.Count()
	l.budget = SSHBudgets()
	hook := ResetHook()

	var bound []*Machine
	for i, m := range want {
		if i >= len(inv.Machines) {
			break
		}
		entry := inv.Machines[i]
		if _, err := newSSHDriver(m, entry, l.inventoryPath, hook); err != nil {
			return bound, err
		}
		m.Name = entry.Name
		m.Hostname = entry.Name
		l.logf("itest: %s plays %s from %s", entry.Name, m.Alias, l.inventoryPath)
		bound = append(bound, m)
	}
	if len(bound) == 0 {
		return nil, fmt.Errorf("the inventory %s holds no machine: the end-to-end tier needs at least one", l.inventoryPath)
	}
	return bound, nil
}

func ResetHook() []string {
	return strings.Fields(strings.TrimSpace(os.Getenv(ResetEnv)))
}
