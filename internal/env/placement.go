package env

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/fleet"
)

const HubName = "hub"

var depMemory = map[string]int64{
	"postgres":  64 << 20,
	"mysql":     384 << 20,
	"mariadb":   384 << 20,
	"mongo":     256 << 20,
	"redis":     32 << 20,
	"valkey":    32 << 20,
	"rabbitmq":  128 << 20,
	"memcached": 64 << 20,
}

const DefaultDepMemory int64 = 64 << 20

func DemandOf(app, name string, cfg *config.App) fleet.Demand {
	d := fleet.Demand{App: app, Env: name}
	if cfg == nil {
		return d
	}
	eff := cfg.ForEnv(name)
	for _, s := range eff.Services {
		if s.Resources == nil || s.Resources.Memory <= 0 {
			continue
		}
		d.MemoryBytes += s.Resources.Memory * int64(s.Replicas())
		d.CPU += s.Resources.CPU * float64(s.Replicas())
	}
	for _, dep := range eff.Deps {
		d.MemoryBytes += DepMemory(dep.Image)
	}
	return d
}

func DepMemory(image string) int64 {
	if m, ok := depMemory[config.ImageName(image)]; ok {
		return m
	}
	return DefaultDepMemory
}

func (m *Manager) PlaceEnv(ctx context.Context, req CreateRequest, cfg *config.App) (fleet.Decision, error) {

	if m.fw().Fleet == nil {
		pin, _, err := m.pinOf(req, cfg, "")
		if err != nil {
			return fleet.Decision{}, err
		}
		if pin != "" && !m.isSelf(pin) {
			return fleet.Decision{}, fmt.Errorf("this machine is not part of a fleet, so env %q cannot be created on %q: "+
				"join it to a hub with `caramelo machine join`, or leave the machine out", req.Name, pin)
		}
		return fleet.Decision{Machine: m.fw().Machine, Why: "this machine"}, nil
	}

	machines, err := m.fw().Fleet.Machines(ctx)
	if err != nil {
		return fleet.Decision{}, fmt.Errorf("read the fleet's machines: %w", err)
	}

	hubName := hubOf(machines)
	pin, byFile, err := m.pinOf(req, cfg, hubName)
	if err != nil {
		return fleet.Decision{}, err
	}
	if pin == HubName {
		if hubName == "" {
			return fleet.Decision{}, fmt.Errorf("env %q asks for the hub, and this fleet has no machine with that role", req.Name)
		}
		pin = hubName
	}
	if pin != "" {
		if err := m.knows(machines, pin, req.Name); err != nil {
			return fleet.Decision{}, err
		}
	}
	placement := config.PlacementAuto
	if cfg != nil {
		placement = cfg.PlacementOf()
	}
	if pin == "" && placement == config.PlacementHub && hubName != "" {
		pin, byFile = hubName, true
	}

	if len(machines) <= 1 {
		if pin != "" && !m.isSelf(pin) {
			return fleet.Decision{}, fmt.Errorf("env %q asks for machine %q, and this fleet has only %s",
				req.Name, pin, m.selfName())
		}
		return fleet.Decision{Machine: m.fw().Machine, Why: "the fleet's only machine"}, nil
	}

	candidates, err := m.candidates(ctx, machines)
	if err != nil {
		return fleet.Decision{}, err
	}
	choose := m.Place
	if choose == nil {
		choose = fleet.Choose
	}
	dec, err := choose(candidates, DemandOf(req.App, req.Name, cfg), fleet.Preference{
		Pin:          pin,
		PinnedByFile: byFile,
		Placement:    placement,
	}, m.now())
	if err != nil {
		return dec, fmt.Errorf("place env %q of app %q: %w", req.Name, req.App, err)
	}
	if dec.Machine == "" {
		return dec, fmt.Errorf("place env %q of app %q: placement named no machine", req.Name, req.App)
	}
	return dec, nil
}

func (m *Manager) pinOf(req CreateRequest, cfg *config.App, hubName string) (pin string, byFile bool, err error) {
	var fromFile string
	if cfg != nil {
		fromFile = cfg.MachineOf(req.Name)
	}
	flag := strings.TrimSpace(req.On)
	switch {
	case flag == "" && fromFile == "":
		return "", false, nil
	case flag == "":
		return fromFile, true, nil
	case fromFile == "", sameMachine(flag, fromFile, hubName):
		return flag, false, nil
	}
	return "", false, fmt.Errorf("env %q is pinned to machine %q by envs.%s.machine in %s, "+
		"so --on %s is refused: change the file, or leave --on out",
		req.Name, fromFile, req.Name, config.FileName, flag)
}

func sameMachine(a, b, hub string) bool {
	resolve := func(n string) string {
		if n == HubName && hub != "" {
			return hub
		}
		return n
	}
	return resolve(a) == resolve(b)
}

func (m *Manager) knows(machines []fleet.Machine, pin, name string) error {
	names := make([]string, 0, len(machines))
	for _, mm := range machines {
		if mm.Name == pin {
			return nil
		}
		names = append(names, mm.Name)
	}
	sort.Strings(names)
	return fmt.Errorf("env %q asks for machine %q, which is not in this fleet (it has %s)",
		name, pin, strings.Join(names, ", "))
}

func (m *Manager) candidates(ctx context.Context, machines []fleet.Machine) ([]fleet.Candidate, error) {
	counts := map[string]int{}
	entries, err := m.fw().Fleet.DirectoryEntries(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("read the environment directory: %w", err)
	}
	for _, e := range entries {
		counts[e.Machine]++
	}
	out := make([]fleet.Candidate, 0, len(machines))
	for _, mm := range machines {
		if mm.Envs == 0 {
			mm.Envs = counts[mm.Name]
		}
		out = append(out, fleet.Candidate{Machine: mm, Gauge: mm.Gauge})
	}
	return out, nil
}

func (m *Manager) isSelf(name string) bool {
	return name != "" && (name == m.fw().Machine || (name == HubName && m.fw().Role.IsHub()))
}

func (m *Manager) selfName() string {
	if m.fw().Machine == "" {
		return "this machine"
	}
	return m.fw().Machine
}

func (m *Manager) acceptPlacement(req CreateRequest) error {
	pin := strings.TrimSpace(req.On)
	if pin == "" || m.isSelf(pin) {
		return nil
	}
	return fmt.Errorf("env %q was asked for machine %q and this is %s: "+
		"ask the fleet's hub, which is what places and forwards", req.Name, pin, m.selfName())
}

func hubOf(machines []fleet.Machine) string {
	for _, mm := range machines {
		if mm.Role == fleet.RoleHub {
			return mm.Name
		}
	}
	return ""
}
