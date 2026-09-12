package env

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vpn"
)

func Endpoints(rec *state.EnvRecord, cfg *config.App) ([]URL, error) {
	return EndpointsAt(rec, cfg, nil)
}

func EndpointsAt(rec *state.EnvRecord, cfg *config.App, front map[string]int) ([]URL, error) {
	if rec == nil || cfg == nil {
		return nil, nil
	}
	l, err := layoutOfRecord(rec, cfg)
	if err != nil {
		return nil, err
	}
	for name, port := range front {
		if _, ok := l.services[name]; ok && port > 0 {
			l.services[name] = port
		}
	}
	ip, err := netip.ParseAddr(rec.VPNIP)
	if err != nil {
		ip = netip.Addr{}
	}
	out := make([]URL, 0, len(cfg.Services)+len(cfg.Deps))
	for _, s := range cfg.Services {
		port, ok := l.services[s.Name]
		if !ok {
			continue
		}
		protocol := string(s.Protocol)
		if protocol == "" {
			protocol = string(config.ProtocolTCP)
		}
		u := URL{
			Name:     s.Name,
			Kind:     KindService,
			URL:      serviceURL(port, protocol),
			Host:     DepHost,
			Port:     port,
			Protocol: protocol,
		}
		fillInternal(&u, rec, ip, l.containerPorts[s.Name])
		out = append(out, u)
	}
	for _, d := range cfg.Deps {
		port := l.deps[d.Name]
		u := URL{
			Name:     d.Name,
			Kind:     KindDep,
			URL:      fmt.Sprintf("tcp://%s:%d", DepHost, port),
			Host:     DepHost,
			Port:     port,
			Protocol: string(config.ProtocolTCP),
		}

		internal := d.Port
		if internal <= 0 {
			internal = port
		}
		fillInternal(&u, rec, ip, internal)
		out = append(out, u)
	}
	return out, nil
}

func fillInternal(u *URL, rec *state.EnvRecord, ip netip.Addr, port int) {
	if !ip.IsValid() || port <= 0 {
		return
	}
	scheme := "tcp"
	switch {
	case u.Protocol == string(config.ProtocolUDP):
		scheme = "udp"
	case u.Kind == KindService:
		scheme = "http"
	}
	u.InternalHost = vpn.ServiceHost(rec.App, rec.Name, u.Name)
	u.InternalPort = port
	u.InternalAddress = net.JoinHostPort(ip.String(), strconv.Itoa(port))
	u.InternalURL = scheme + "://" + net.JoinHostPort(u.InternalHost, strconv.Itoa(port))
}

func layoutOfRecord(rec *state.EnvRecord, cfg *config.App) (portLayout, error) {
	plans := make([]svcPlan, 0, len(cfg.Services))
	for _, s := range cfg.Services {
		plans = append(plans, svcPlan{name: s.Name, port: servicePort(s), portless: s.Port == config.PortNone})
	}
	return layout(rec.PortBase, rec.PortCount, cfg.Deps, plans)
}

func FindEndpoint(endpoints []URL, name string) (URL, bool) {
	for _, e := range endpoints {
		if e.Name == name {
			return e, true
		}
	}
	return URL{}, false
}

func EnvURL(endpoints []URL) (URL, bool) {
	for _, e := range endpoints {
		if e.Kind == KindService {
			return e, true
		}
	}
	return URL{}, false
}
