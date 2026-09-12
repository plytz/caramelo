package vpn

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
)

func (d *wgDevice) AddMachinePeer(ctx context.Context, m MachinePeer) error {
	key, err := checkMachinePeer(m, d.opts.FleetRange)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.machineDoesNotCollideLocked(m); err != nil {
		return err
	}
	old, existed := d.machines[m.Name]
	if existed && old.PublicKey != m.PublicKey {

		if oldKey, err := ParseKey(old.PublicKey); err == nil {
			if err := d.ipcSetLocked(ipcConfig{Peers: []ipcPeer{{PublicKey: oldKey, Remove: true}}}); err != nil {
				return err
			}
		}
	}
	if err := d.ipcSetLocked(ipcConfig{Peers: []ipcPeer{machineSection(key, m)}}); err != nil {
		return err
	}
	d.machines[m.Name] = m
	d.logf("machine %s admitted for %s", m.Name, prefixList(m.AllowedIPs))
	return nil
}

func (d *wgDevice) machineDoesNotCollideLocked(m MachinePeer) error {
	for _, p := range m.AllowedIPs {
		if p.Bits() <= d.opts.FleetRange.Bits() {
			continue
		}
		for name, other := range d.machines {
			if name == m.Name {
				continue
			}
			for _, q := range other.AllowedIPs {
				if q.Bits() <= d.opts.FleetRange.Bits() || !q.Overlaps(p) {
					continue
				}
				return fmt.Errorf("vpn: machine %s: %s overlaps %s, which is machine %s's", m.Name, p, q, name)
			}
		}
		if p.Overlaps(d.subnet) {
			return fmt.Errorf("vpn: machine %s: %s overlaps %s, which is this machine's own range", m.Name, p, d.subnet)
		}
	}
	return nil
}

func (d *wgDevice) RemoveMachinePeer(ctx context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	m, ok := d.machines[name]
	if !ok {
		return nil
	}
	if key, err := ParseKey(m.PublicKey); err == nil {
		if err := d.ipcSetLocked(ipcConfig{Peers: []ipcPeer{{PublicKey: key, Remove: true}}}); err != nil {
			return err
		}
	}
	delete(d.machines, name)
	d.logf("machine %s dropped", name)
	return nil
}

func (d *wgDevice) MachinePeers(ctx context.Context) ([]MachinePeer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var live map[string]ipcPeerStatus
	if d.dev != nil {
		st, err := d.ipcStatusLocked()
		if err != nil {
			return nil, err
		}
		live = st.Peers
	}
	out := make([]MachinePeer, 0, len(d.machines))
	for _, m := range d.machines {
		if live != nil {
			if key, err := ParseKey(m.PublicKey); err == nil {
				if s, ok := live[key.Hex()]; ok {
					m.LastHandshake, m.SeenAt = s.LastHandshake, s.Endpoint
				}
			}
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (d *wgDevice) MachineAt(ctx context.Context, ip netip.Addr) (MachinePeer, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var (
		best MachinePeer
		bits = -1
	)
	for _, m := range d.machines {
		for _, p := range m.AllowedIPs {
			if p.Contains(ip) && p.Bits() > bits {
				best, bits = m, p.Bits()
			}
		}
	}
	return best, bits >= 0
}

type forwardEntry struct {
	forward Forward
	closer  net.Listener
}

func (d *wgDevice) AddForward(ctx context.Context, f Forward) (Forward, error) {
	if err := checkForward(f); err != nil {
		return Forward{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if e, ok := d.forwards[f.Name]; ok {
		if e.forward.To == f.To {
			return e.forward, nil
		}

		if e.closer != nil {
			e.closer.Close()
		}
		delete(d.forwards, f.Name)
	}
	if !d.up() {
		return Forward{}, fmt.Errorf("vpn: forward %s: the device is not up", f.Name)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(loopback, fmt.Sprint(f.Port)))
	if err != nil {
		return Forward{}, fmt.Errorf("vpn: forward %s to %s: %w", f.Name, f.To, err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		ln.Close()
		return Forward{}, fmt.Errorf("vpn: forward %s: listener is %T, not TCP", f.Name, ln.Addr())
	}
	f.Port = addr.Port
	go forwardTCP(ln, d.stk, f, d.logf)
	d.forwards[f.Name] = &forwardEntry{forward: f, closer: ln}
	d.logf("forward %s: %s -> %s in the tunnel", f.Name, f.Addr(), f.To)
	return f, nil
}

func (d *wgDevice) RemoveForward(ctx context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.forwards[name]
	if !ok {
		return nil
	}
	if e.closer != nil {
		e.closer.Close()
	}
	delete(d.forwards, name)
	d.logf("forward %s closed", name)
	return nil
}

func (d *wgDevice) Forwards(ctx context.Context) ([]Forward, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Forward, 0, len(d.forwards))
	for _, e := range d.forwards {
		out = append(out, e.forward)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

const loopback = "127.0.0.1"

func checkForward(f Forward) error {
	switch {
	case strings.TrimSpace(f.Name) == "":
		return fmt.Errorf("vpn: forward: no name")
	case !f.To.IsValid() || !f.To.Addr().Is4():
		return fmt.Errorf("vpn: forward %s: %q is not an address inside the tunnel", f.Name, f.To)
	case f.To.Port() == 0:
		return fmt.Errorf("vpn: forward %s: no port at %s", f.Name, f.To.Addr())
	case f.Port < 0 || f.Port > 65535:
		return fmt.Errorf("vpn: forward %s: port %d is out of range", f.Name, f.Port)
	}
	return nil
}

func forwardTCP(ln net.Listener, stk stack, f Forward, logf func(string, ...any)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
			up, err := stk.DialTCP(ctx, f.To)
			cancel()
			if err != nil {
				logf("forward %s -> %s: %v", f.Name, f.To, err)
				return
			}
			defer up.Close()
			splice(c, up)
		}()
	}
}

func (d *wgDevice) closeForwardsLocked() {
	for name, e := range d.forwards {
		if e.closer != nil {
			e.closer.Close()
		}
		d.forwards[name] = &forwardEntry{forward: forwardWithoutPort(e.forward)}
	}
}

func forwardWithoutPort(f Forward) Forward {
	f.Port = 0
	return f
}

func (d *wgDevice) startForwardsLocked() error {
	for name, e := range d.forwards {
		if e.closer != nil {
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(loopback, fmt.Sprint(e.forward.Port)))
		if err != nil {
			return fmt.Errorf("vpn: forward %s to %s: %w", name, e.forward.To, err)
		}
		f := e.forward
		if addr, ok := ln.Addr().(*net.TCPAddr); ok {
			f.Port = addr.Port
		}
		go forwardTCP(ln, d.stk, f, d.logf)
		d.forwards[name] = &forwardEntry{forward: f, closer: ln}
	}
	return nil
}

func (d *wgDevice) machinePeerSectionsLocked() []ipcPeer {
	names := make([]string, 0, len(d.machines))
	for name := range d.machines {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ipcPeer, 0, len(names))
	for _, name := range names {
		m := d.machines[name]
		key, err := ParseKey(m.PublicKey)
		if err != nil {
			continue
		}
		out = append(out, machineSection(key, m))
	}
	return out
}

func (d *wgDevice) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	stk := d.stk
	d.mu.Unlock()
	if stk == nil {
		return nil, fmt.Errorf("vpn: dial %s %s: the tunnel is not up on this machine", network, address)
	}
	at, err := netip.ParseAddrPort(address)
	if err != nil {
		return nil, fmt.Errorf("vpn: dial %s %q: %w", network, address, err)
	}
	switch network {
	case "tcp", "tcp4":
		return stk.DialTCP(ctx, at)
	case "udp", "udp4":
		return stk.DialUDP(at)
	default:
		return nil, fmt.Errorf("vpn: dial %s %s: the tunnel carries tcp and udp only", network, address)
	}
}

func (d *wgDevice) SetFleetNames(ctx context.Context, addrs []Address) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	next := make(map[netip.Addr]Address, len(addrs))
	for _, a := range addrs {
		if !a.IP.IsValid() || len(a.Names) == 0 {
			continue
		}
		for _, n := range a.Names {
			if !IsInternal(n) {
				return fmt.Errorf("vpn: fleet name %q is outside %s", n, Suffix)
			}
		}
		a.Kind = KindEnv
		next[a.IP] = a
	}
	d.fleet = next
	if !d.up() {
		return nil
	}
	return d.syncZoneLocked()
}
