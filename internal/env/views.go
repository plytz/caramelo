package env

import (
	"context"
	"fmt"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/state"
)

type portLayout struct {
	base int

	count int

	deps     map[string]int
	services map[string]int

	containerPorts map[string]int

	order map[string]int

	spare     int
	published int
}

func layout(base, count int, deps []config.Dep, services []svcPlan) (portLayout, error) {
	l := portLayout{
		base:           base,
		count:          count,
		deps:           make(map[string]int, len(deps)),
		services:       make(map[string]int, len(services)),
		containerPorts: make(map[string]int, len(services)),
		order:          make(map[string]int, len(services)),
	}
	for i, d := range deps {
		l.deps[d.Name] = depPort(base, i)
	}

	next, after := 0, base+1+len(deps)
	for _, s := range services {
		if s.portless {
			continue
		}
		port := base
		if next > 0 {
			port = after + next - 1
		}
		l.order[s.name] = next
		next++
		if port >= base+count {
			return portLayout{}, fmt.Errorf("%d dependencies and %d published services do not fit in a block of %d ports",
				len(deps), next, count)
		}
		l.services[s.name] = port
		l.containerPorts[s.name] = port
		if s.port > 0 {
			l.containerPorts[s.name] = s.port
		}
	}
	if len(deps) > count-1 {
		return portLayout{}, fmt.Errorf("%d dependencies do not fit in a block of %d ports", len(deps), count)
	}
	l.published = next
	l.spare = after + max(next-1, 0)
	return l, nil
}

func (l portLayout) replicaPort(service string, replica int) (int, error) {
	primary, ok := l.services[service]
	if !ok {
		return 0, fmt.Errorf("service %q publishes no port, so it has no replica ports", service)
	}
	if replica <= 1 {
		return primary, nil
	}
	port := l.spare + (replica-2)*l.published + l.order[service]
	if port >= l.base+l.count {
		return 0, fmt.Errorf("replica %d of service %q would need port %d, past the end of the env's block %d-%d: "+
			"ask for fewer replicas, or for fewer dependencies",
			replica, service, port, l.base, l.base+l.count-1)
	}
	return port, nil
}

func (l portLayout) fits(service string, want int) error {
	_, err := l.replicaPort(service, want)
	return err
}

func (l portLayout) views(rec *state.EnvRecord, deps []config.Dep) config.Views {
	host := config.ExpandContext{
		App: rec.App, Env: rec.Name, Port: l.base, View: config.ViewHost,
		Deps:     make(map[string]config.DepAddr, len(deps)),
		Services: make(map[string]config.DepAddr, len(l.services)),
	}
	network := config.ExpandContext{
		App: rec.App, Env: rec.Name, Port: l.base, View: config.ViewNetwork,
		Deps:     make(map[string]config.DepAddr, len(deps)),
		Services: make(map[string]config.DepAddr, len(l.services)),
	}
	for _, d := range deps {
		host.Deps[d.Name] = config.DepAddr{Host: DepHost, Port: l.deps[d.Name]}
		network.Deps[d.Name] = config.DepAddr{Host: d.Name, Port: d.Port}
	}
	for name, port := range l.services {
		host.Services[name] = config.DepAddr{Host: DepHost, Port: port}
		network.Services[name] = config.DepAddr{Host: name, Port: l.containerPorts[name]}
	}
	return config.Views{Host: host, Network: network}
}

func (m *Manager) Views(ctx context.Context, app, name string) (config.Views, error) {
	rec, err := m.env(ctx, app, name)
	if err != nil {
		return config.Views{}, err
	}
	cfg, err := decodeConfig(rec)
	if err != nil {
		return config.Views{}, err
	}
	l, err := m.layoutOf(rec, cfg)
	if err != nil {
		return config.Views{}, err
	}
	return l.views(rec, cfg.Deps), nil
}

func (m *Manager) layoutOf(rec *state.EnvRecord, cfg *config.App) (portLayout, error) {
	plans := make([]svcPlan, 0, len(cfg.Services))
	for _, s := range cfg.Services {
		plans = append(plans, svcPlan{name: s.Name, port: servicePort(s), portless: s.Port == config.PortNone})
	}
	return layout(rec.PortBase, rec.PortCount, cfg.Deps, plans)
}

func servicePort(s config.Service) int {
	if s.Port == config.PortNone || s.Port < 0 {
		return 0
	}
	return s.Port
}
