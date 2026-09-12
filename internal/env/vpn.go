package env

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vpn"
)

type Network interface {
	AddAddress(ctx context.Context, a vpn.Address) error

	RemoveAddress(ctx context.Context, ip netip.Addr) error

	SetAddressRoutes(ctx context.Context, ip netip.Addr, routes []vpn.Route) error

	Addresses(ctx context.Context) ([]vpn.Address, error)
	Routes(ctx context.Context) ([]vpn.Route, error)
}

const addrAttempts = allocAttempts

func (m *Manager) allocAddress(ctx context.Context) (netip.Addr, error) {
	if m.VPNAlloc == nil {
		return netip.Addr{}, nil
	}
	ip, err := m.VPNAlloc.AllocateEnv(ctx, 0)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("allocate an address on the machine's network: %w", err)
	}
	return ip, nil
}

func (m *Manager) publish(ctx context.Context, rec *state.EnvRecord, cfg *config.App, progress io.Writer) {
	if m.Net == nil || rec.VPNIP == "" {
		return
	}
	ip, err := netip.ParseAddr(rec.VPNIP)
	if err != nil {
		progressf(progress, "warning", "network", "env %q has an unusable address %q: %v", rec.Name, rec.VPNIP, err)
		return
	}
	addr := Address(rec, cfg)
	if err := m.Net.AddAddress(ctx, addr); err != nil {
		progressf(progress, "warning", "network", "%s is not reachable on %s: %v", rec.Name, ip, err)
		return
	}
	routes := RoutesAt(rec, cfg, m.frontPorts(ctx, rec, cfg))
	if err := m.Net.SetAddressRoutes(ctx, ip, routes); err != nil {
		progressf(progress, "warning", "network", "the ports of %s are not relayed: %v", rec.Name, err)
		return
	}
	progressf(progress, "changed", "network", "%s on %s (%s)", vpn.EnvHost(rec.App, rec.Name), ip, plural(len(routes), "port"))
}

func (m *Manager) unpublish(ctx context.Context, rec *state.EnvRecord, progress io.Writer) error {
	if m.Net == nil || rec == nil || rec.VPNIP == "" {
		return nil
	}
	ip, err := netip.ParseAddr(rec.VPNIP)
	if err != nil {

		return nil
	}
	if err := m.Net.RemoveAddress(ctx, ip); err != nil {
		return fmt.Errorf("remove address %s of env %q: %w", ip, rec.Name, err)
	}
	progressf(progress, "changed", "network", "%s released", ip)
	return nil
}

func (m *Manager) releaseAddress(ctx context.Context, rec *state.EnvRecord) error {
	if m.VPNAlloc == nil || rec == nil || rec.VPNIP == "" {
		return nil
	}
	ip, err := netip.ParseAddr(rec.VPNIP)
	if err != nil {
		return nil
	}
	if err := m.VPNAlloc.Release(ctx, ip); err != nil {
		return fmt.Errorf("release the address of env %q: %w", rec.Name, err)
	}
	return nil
}

func Address(rec *state.EnvRecord, cfg *config.App) vpn.Address {
	ip, _ := netip.ParseAddr(rec.VPNIP)
	return vpn.Address{
		IP:    ip,
		Kind:  vpn.KindEnv,
		Owner: rec.App + "/" + rec.Name,
		Names: vpn.EnvHosts(rec.App, rec.Name, hostNames(cfg)),
	}
}

func Routes(rec *state.EnvRecord, cfg *config.App) []vpn.Route {
	return RoutesAt(rec, cfg, nil)
}

func RoutesAt(rec *state.EnvRecord, cfg *config.App, front map[string]int) []vpn.Route {
	ip, err := netip.ParseAddr(rec.VPNIP)
	if err != nil {
		return nil
	}
	endpoints, err := EndpointsAt(rec, cfg, front)
	if err != nil {
		return nil
	}
	type key struct {
		port  int
		proto vpn.Protocol
	}
	seen := make(map[key]bool, len(endpoints))
	routes := make([]vpn.Route, 0, len(endpoints))
	for _, e := range endpoints {
		if e.InternalPort <= 0 || e.Port <= 0 {
			continue
		}
		proto := vpn.TCP
		if e.Protocol == string(config.ProtocolUDP) {
			proto = vpn.UDP
		}
		k := key{e.InternalPort, proto}
		if seen[k] {
			continue
		}
		seen[k] = true
		routes = append(routes, vpn.Route{
			IP:       ip,
			Port:     e.InternalPort,
			Protocol: proto,
			Target:   net.JoinHostPort(DepHost, strconv.Itoa(e.Port)),
			App:      rec.App,
			Env:      rec.Name,
			Name:     e.Name,
		})
	}
	return routes
}

func hostNames(cfg *config.App) []string {
	if cfg == nil {
		return nil
	}
	names := make([]string, 0, len(cfg.Services)+len(cfg.Deps))
	for _, s := range cfg.Services {
		names = append(names, s.Name)
	}
	for _, d := range cfg.Deps {
		names = append(names, d.Name)
	}
	return names
}

func (m *Manager) Republish(ctx context.Context) error {
	if m.Net == nil {
		return nil
	}
	recs, err := m.Store.Envs(ctx, "")
	if err != nil {
		return fmt.Errorf("list envs: %w", err)
	}
	var problems []error
	for i := range recs {
		rec := &recs[i]
		if rec.VPNIP == "" {
			continue
		}
		cfg, err := decodeConfig(rec)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		ip, err := netip.ParseAddr(rec.VPNIP)
		if err != nil {
			problems = append(problems, fmt.Errorf("env %s/%s: address %q: %w", rec.App, rec.Name, rec.VPNIP, err))
			continue
		}
		if err := m.Net.AddAddress(ctx, Address(rec, cfg)); err != nil {
			problems = append(problems, err)
			continue
		}
		if err := m.Net.SetAddressRoutes(ctx, ip, RoutesAt(rec, cfg, m.frontPorts(ctx, rec, cfg))); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

func (m *Manager) republish(ctx context.Context, rec *state.EnvRecord, cfg *config.App, progress io.Writer) {
	if m.Net == nil || rec == nil || rec.VPNIP == "" {
		return
	}
	ip, err := netip.ParseAddr(rec.VPNIP)
	if err != nil {
		return
	}
	wantAddr, wantRoutes := Address(rec, cfg), RoutesAt(rec, cfg, m.frontPorts(ctx, rec, cfg))
	haveAddr, haveRoutes, err := m.published(ctx, ip)
	if err != nil {
		progressf(progress, "warning", "network", "read the device's tables: %v", err)
		return
	}
	if sameNames(haveAddr, wantAddr) && sameRoutes(haveRoutes, wantRoutes) {
		return
	}
	if err := m.Net.AddAddress(ctx, wantAddr); err != nil {
		progressf(progress, "warning", "network", "%s is not reachable on %s: %v", rec.Name, ip, err)
		return
	}
	if err := m.Net.SetAddressRoutes(ctx, ip, wantRoutes); err != nil {
		progressf(progress, "warning", "network", "the ports of %s are not relayed: %v", rec.Name, err)
		return
	}
	progressf(progress, "changed", "network", "%s on %s (%s)",
		vpn.EnvHost(rec.App, rec.Name), ip, plural(len(wantRoutes), "port"))
}

func (m *Manager) published(ctx context.Context, ip netip.Addr) (vpn.Address, []vpn.Route, error) {
	addrs, err := m.Net.Addresses(ctx)
	if err != nil {
		return vpn.Address{}, nil, err
	}
	var addr vpn.Address
	for _, a := range addrs {
		if a.IP == ip {
			addr = a
			break
		}
	}
	all, err := m.Net.Routes(ctx)
	if err != nil {
		return vpn.Address{}, nil, err
	}
	var routes []vpn.Route
	for _, r := range all {
		if r.IP == ip {
			routes = append(routes, r)
		}
	}
	return addr, routes, nil
}

func sameNames(a, b vpn.Address) bool {
	if a.IP != b.IP || len(a.Names) != len(b.Names) {
		return false
	}
	for i := range a.Names {
		if a.Names[i] != b.Names[i] {
			return false
		}
	}
	return true
}

func sameRoutes(a, b []vpn.Route) bool {
	if len(a) != len(b) {
		return false
	}
	type key struct {
		port   int
		proto  vpn.Protocol
		target string
	}
	have := make(map[key]int, len(a))
	for _, r := range a {
		have[key{r.Port, r.Protocol, r.Target}]++
	}
	for _, r := range b {
		k := key{r.Port, r.Protocol, r.Target}
		if have[k] == 0 {
			return false
		}
		have[k]--
	}
	return true
}
