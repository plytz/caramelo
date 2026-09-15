//go:build integration

package itest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	KindMachine   = "machine"
	KindCommander = "commander"
)

const DefaultTimeout = 5 * time.Minute

type Options struct {
	Suite      string
	State      string
	Roles      []string
	Commanders []string
	Ports      []Port
}

type Lab struct {
	Suite   string
	ID      string
	Network string
	State   string

	t          testing.TB
	budget     Budgets
	machines   []*Machine
	commanders []*Machine
	byRole     map[string]*Machine
	byAlias    map[string]*Machine
	ports      []Port

	unbound        []string
	inventoryCount int
	inventoryPath  string
}

func New(t testing.TB, o Options) *Lab {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	suite := sanitize(o.Suite)
	if suite == "" {
		suite = defaultSuiteName()
	}
	state := o.State
	if state == "" {
		state = StateClean
	}
	if state != StateClean && state != StateProvisioned {
		t.Fatalf("itest: unknown state %q", state)
	}
	roles := o.Roles
	if len(roles) == 0 && len(o.Commanders) == 0 {
		roles = []string{RoleHub}
	}

	l := &Lab{
		Suite:         suite,
		ID:            shortID(),
		State:         state,
		t:             t,
		budget:        Budget(),
		byRole:        map[string]*Machine{},
		byAlias:       map[string]*Machine{},
		ports:         append(append([]Port(nil), DefaultPorts...), o.Ports...),
		inventoryPath: strings.TrimSpace(os.Getenv(InventoryEnv)),
	}
	if l.inventoryPath != "" && inventoryDriver == nil {
		t.Fatalf("itest: %s=%s is set but this build has no ssh backend: build with -tags integration,e2e",
			InventoryEnv, l.inventoryPath)
	}

	counts := map[string]int{}
	var wanted, commanders []*Machine
	for _, role := range roles {
		wanted = append(wanted, l.newMachine(role, counts, KindMachine, state))
	}
	for _, role := range o.Commanders {
		commanders = append(commanders, l.newMachine(role, counts, KindCommander, ""))
	}

	var overSSH, inDocker []*Machine
	for _, m := range wanted {
		if l.inventoryPath != "" && bindsToInventory(m) {
			overSSH = append(overSSH, m)
			continue
		}
		inDocker = append(inDocker, m)
	}
	containers := append(append([]*Machine(nil), inDocker...), commanders...)

	if len(containers) > 0 {
		if err := RequireDocker(ctx); err != nil {
			t.Fatalf("itest: %v", err)
		}
		l.Network = "caramelo-itest-" + suite + "-" + l.ID
		if err := createNetwork(ctx, l.Network); err != nil {
			t.Fatalf("itest: %v", err)
		}
	}
	t.Cleanup(l.cleanup)

	bound := overSSH
	if len(overSSH) > 0 {
		var err error
		bound, err = inventoryDriver(t, l, overSSH)
		if err != nil {
			t.Fatalf("itest: %v", err)
		}
	}
	boundSSH := map[*Machine]bool{}
	for _, m := range bound {
		boundSSH[m] = true
	}
	for _, m := range overSSH {
		if !boundSSH[m] {
			l.unbound = append(l.unbound, m.Alias)
		}
	}
	for _, m := range containers {
		newDockerDriver(m)
	}

	for _, m := range wanted {
		if m.drv == nil {
			continue
		}
		l.register(m)
		if err := l.startMachine(m); err != nil {
			t.Fatalf("itest: %v", err)
		}
	}
	for _, m := range commanders {
		l.register(m)
		if err := l.startMachine(m); err != nil {
			t.Fatalf("itest: %v", err)
		}
	}
	return l
}

func (l *Lab) newMachine(role string, counts map[string]int, kind, state string) *Machine {
	alias := role
	if n := counts[role]; n > 0 {
		alias = role + "-" + strconv.Itoa(n)
	}
	counts[role]++

	m := &Machine{
		Role:     role,
		Alias:    alias,
		Kind:     kind,
		State:    state,
		Hostname: alias,
		Name:     l.Suite + "-" + alias + "-" + l.ID,
		lab:      l,
	}
	if kind == KindMachine {
		m.Volume = "caramelo-itest-" + l.Suite + "-" + alias + "-" + l.ID
		if role == RoleCommander {
			m.State = StateClean
		}
		if m.State == StateProvisioned {
			m.Hostname = ProvisionedHostname
		}
	}
	return m
}

func (l *Lab) register(m *Machine) {
	if m.Kind == KindMachine {
		l.machines = append(l.machines, m)
	} else {
		l.commanders = append(l.commanders, m)
	}
	if _, ok := l.byRole[m.Role]; !ok {
		l.byRole[m.Role] = m
	}
	l.byAlias[m.Alias] = m
}

func (l *Lab) startMachine(m *Machine) error {
	ctx, cancel := context.WithTimeout(context.Background(), imageBuildTimeout+l.budget.Boot)
	defer cancel()
	if err := m.drv.start(ctx); err != nil {
		return err
	}
	return installLabKeys(ctx, m)
}

func (l *Lab) cleanup() {
	if Keep() && l.t.Failed() {
		l.t.Logf("itest: %s=1 and the test failed: keeping network %s and containers %s",
			KeepEnv, l.Network, strings.Join(l.names(), ", "))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, m := range l.all() {
		if err := m.drv.remove(ctx); err != nil {
			l.t.Logf("itest: remove %s: %v", m.Name, err)
		}
	}
	if l.Network != "" {
		if err := removeNetwork(ctx, l.Network); err != nil {
			l.t.Logf("itest: remove network %s: %v", l.Network, err)
		}
	}
}

func (l *Lab) all() []*Machine {
	return append(append([]*Machine(nil), l.machines...), l.commanders...)
}

func (l *Lab) names() []string {
	var out []string
	for _, m := range l.all() {
		out = append(out, m.Name)
	}
	return out
}

func (l *Lab) Machine(role string) *Machine {
	l.t.Helper()
	if m, ok := l.byRole[role]; ok {
		return m
	}
	if m, ok := l.byAlias[role]; ok {
		return m
	}
	l.t.Fatalf("itest: no machine plays the %q role in suite %s (bound: %s%s)",
		role, l.Suite, strings.Join(l.roles(), ", "), l.unboundNote())
	return nil
}

func (l *Lab) unboundNote() string {
	if len(l.unbound) == 0 {
		return ""
	}
	return "; unbound: " + strings.Join(l.unbound, ", ")
}

func (l *Lab) Commander(role string) *Machine {
	l.t.Helper()
	for _, m := range l.commanders {
		if m.Role == role {
			return m
		}
	}
	l.t.Fatalf("itest: no commander plays the %q role in suite %s", role, l.Suite)
	return nil
}

func (l *Lab) roles() []string {
	var out []string
	for _, m := range l.all() {
		out = append(out, m.Alias)
	}
	return out
}

func (l *Lab) Machines() []*Machine { return append([]*Machine(nil), l.machines...) }

func (l *Lab) Commanders() []*Machine { return append([]*Machine(nil), l.commanders...) }

func (l *Lab) Members() []*Machine {
	var out []*Machine
	for _, m := range l.machines {
		if m.Role == RoleMember {
			out = append(out, m)
		}
	}
	return out
}

func (l *Lab) Budget() Budgets { return l.budget }

func (l *Lab) logf(format string, args ...any) {
	if l.t != nil {
		l.t.Helper()
		l.t.Logf(format, args...)
	}
}

func (l *Lab) count(role string) int {
	n := 0
	for _, m := range l.all() {
		if m.Role == role {
			n++
		}
	}
	return n
}

func MissingRoles(l *Lab, roles ...string) string {
	want := map[string]int{}
	var order []string
	for _, role := range roles {
		if want[role] == 0 {
			order = append(order, role)
		}
		want[role]++
	}
	short := false
	var wantParts, gotParts []string
	for _, role := range order {
		got := l.count(role)
		if got < want[role] {
			short = true
		}
		wantParts = append(wantParts, plural(want[role], role))
		gotParts = append(gotParts, plural(got, role))
	}
	if !short {
		return ""
	}
	note := ""
	if l.inventoryCount > 0 {
		note = fmt.Sprintf(" (the inventory holds %s)", plural(l.inventoryCount, "machine"))
	}
	return fmt.Sprintf("this case needs %s; suite %s has %s%s",
		strings.Join(wantParts, " + "), l.Suite, strings.Join(gotParts, " + "), note)
}

func NeedRoles(t testing.TB, l *Lab, roles ...string) {
	t.Helper()
	if why := MissingRoles(l, roles...); why != "" {
		t.Skipf("itest: %s", why)
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

func sanitize(s string) string {
	s = unsafeName.ReplaceAllString(strings.TrimSpace(s), "-")
	return strings.Trim(s, "-._")
}

func defaultSuiteName() string {
	base := filepath.Base(os.Args[0])
	base = strings.TrimSuffix(base, ".test")
	if s := sanitize(base); s != "" {
		return s
	}
	return "itest"
}

func shortID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano()%1e8, 16)
	}
	return hex.EncodeToString(b[:])
}
