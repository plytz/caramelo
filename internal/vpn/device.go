package vpn

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"sync"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"

	"github.com/plytz/caramelo/internal/vpn/netstack"
)

func New(opts Options) (Device, error) { return newDevice(opts) }

func newDevice(opts Options) (*wgDevice, error) {
	if !opts.Subnet.IsValid() {
		return nil, fmt.Errorf("vpn: no subnet")
	}
	subnet, err := Subnet(opts.Subnet.String())
	if err != nil {
		return nil, err
	}
	listen := opts.Listen
	if listen == "" {
		listen = DefaultListen
	}
	host, port, err := splitListen(listen)
	if err != nil {
		return nil, err
	}
	mtu := opts.MTU
	if mtu <= 0 {
		mtu = MTU
	}
	return &wgDevice{
		opts:       opts,
		subnet:     subnet,
		machineIP:  MachineIP(subnet),
		listenHost: host,
		listenPort: port,
		mtu:        mtu,
		peers:      map[string]Peer{},
		machines:   map[string]MachinePeer{},
		addrs:      map[netip.Addr]Address{},
		fleet:      map[netip.Addr]Address{},
		relays:     map[relayKey]*relayEntry{},
		forwards:   map[string]*forwardEntry{},
		resolver:   NewResolver(),
		dial:       netDial,
	}, nil
}

func splitListen(listen string) (host string, port int, err error) {
	h, p, err := net.SplitHostPort(listen)
	if err != nil {
		return "", 0, fmt.Errorf("vpn: listen %q is not host:port: %w", listen, err)
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 0 || n > 65535 {
		return "", 0, fmt.Errorf("vpn: listen %q: %q is not a port", listen, p)
	}
	return h, n, nil
}

type wgDevice struct {
	opts       Options
	subnet     netip.Prefix
	machineIP  netip.Addr
	listenHost string
	listenPort int
	mtu        int

	dial dialFunc

	mu       sync.Mutex
	pub      Key
	dev      *device.Device
	stk      stack
	dnsConn  net.PacketConn
	peers    map[string]Peer
	machines map[string]MachinePeer
	addrs    map[netip.Addr]Address
	fleet    map[netip.Addr]Address
	relays   map[relayKey]*relayEntry
	forwards map[string]*forwardEntry
	resolver *ZoneResolver
}

var _ Device = (*wgDevice)(nil)

func (d *wgDevice) logf(format string, args ...any) {
	if d.opts.Log == nil {
		return
	}
	fmt.Fprintf(d.opts.Log, "vpn: "+format+"\n", args...)
}

func (d *wgDevice) up() bool { return d.stk != nil }

func (d *wgDevice) Up(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.up() {
		return nil
	}

	priv, err := ReadPrivateKey(d.opts.PrivateKeyPath)
	if err != nil {
		return err
	}
	pub, err := priv.Public()
	if err != nil {
		return err
	}

	tdev, tnet, err := netstack.CreateNetTUN([]netip.Addr{d.machineIP}, nil, d.mtu)
	if err != nil {
		return fmt.Errorf("vpn: create the network stack: %w", err)
	}

	dev := device.NewDevice(tdev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "vpn "))
	if err := dev.IpcSet(d.ipcConfig(&priv).String()); err != nil {
		dev.Close()
		return d.ipcError("configure the device", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return fmt.Errorf("vpn: bring the device up: %w", err)
	}

	d.pub, d.dev = pub, dev
	d.stk = netStack{net: tnet}

	if d.opts.Relay {
		if err := tnet.SetForwarding(true); err != nil {
			dev.Close()
			d.stk = nil
			return err
		}
	}

	if d.listenHost != "" && d.listenHost != "0.0.0.0" && d.listenHost != "::" {
		d.logf("listen address %s: only the port is used, the UDP socket always binds every interface",
			d.opts.Listen)
	}

	if err := d.startLocked(); err != nil {
		d.downLocked()
		return err
	}
	d.logf("up on udp %d, %d peers, %d addresses, %d routes",
		d.listenPort, len(d.peers), len(d.addrs), len(d.relays))
	return nil
}

func (d *wgDevice) startLocked() error {
	for _, a := range d.addrs {
		if a.IP == d.machineIP {
			continue
		}
		if err := d.stk.AddAddress(a.IP); err != nil {
			return err
		}
	}
	if err := d.syncZoneLocked(); err != nil {
		return err
	}

	if d.opts.Forward.IsValid() {
		d.resolver.SetForwarder(NewForwarder(d.stk, d.opts.Forward))
	} else {
		d.resolver.SetForwarder(nil)
	}
	pc, err := d.stk.ListenUDP(netip.AddrPortFrom(d.machineIP, ResolverPort))
	if err != nil {
		return fmt.Errorf("vpn: start the resolver: %w", err)
	}
	d.dnsConn = pc
	go serveDNS(pc, d.resolver, d.logf)

	for key, e := range d.relays {
		started, err := startRelay(d.stk, d.dial, e.Route(), d.logf)
		if err != nil {
			return err
		}
		d.relays[key] = started
	}
	return d.startForwardsLocked()
}

func (d *wgDevice) Down(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.up() {
		return nil
	}
	d.downLocked()
	d.logf("down")
	return nil
}

func (d *wgDevice) downLocked() {
	for key, e := range d.relays {
		if e.closer != nil {
			e.closer.Close()
		}
		d.relays[key] = &relayEntry{route: e.Route()}
	}
	d.closeForwardsLocked()
	if d.dnsConn != nil {
		d.dnsConn.Close()
		d.dnsConn = nil
	}
	if d.dev != nil {

		d.dev.Close()
		d.dev = nil
	}
	d.stk = nil
}

func (d *wgDevice) Status(ctx context.Context) (Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	st := Status{
		Up:       d.up(),
		Subnet:   d.subnet,
		IP:       d.machineIP,
		Listen:   d.opts.Listen,
		Peers:    len(d.peers),
		Routes:   len(d.relays),
		Machines: len(d.machines),
		Forwards: len(d.forwards),
		Range:    d.opts.FleetRange,
		Relay:    d.opts.Relay,
	}
	if d.dev != nil {
		st.PublicKey = d.pub.Base64()
		st.Resolver = netip.AddrPortFrom(d.machineIP, ResolverPort).String()
		ipc, err := d.ipcStatusLocked()
		if err != nil {
			return st, err
		}

		st.Listen = net.JoinHostPort(d.listenHost, strconv.Itoa(ipc.ListenPort))
	} else if k, err := ReadPrivateKey(d.opts.PrivateKeyPath); err == nil {

		if pub, err := k.Public(); err == nil {
			st.PublicKey = pub.Base64()
		}
	}
	return st, nil
}

func (d *wgDevice) AddPeer(ctx context.Context, p Peer) error {
	key, err := checkPeer(p, d.subnet)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	old, existed := d.peers[p.Name]
	if existed && old.PublicKey == p.PublicKey && old.IP == p.IP {
		d.peers[p.Name] = p
		return nil
	}
	if existed && old.PublicKey != p.PublicKey {

		oldKey, err := ParseKey(old.PublicKey)
		if err == nil {
			if err := d.ipcSetLocked(ipcConfig{Peers: []ipcPeer{{PublicKey: oldKey, Remove: true}}}); err != nil {
				return err
			}
		}
	}
	if err := d.ipcSetLocked(ipcConfig{Peers: []ipcPeer{peerSection(key, p.IP)}}); err != nil {
		return err
	}
	d.peers[p.Name] = p
	d.logf("peer %s admitted at %s", p.Name, p.IP)
	return nil
}

func (d *wgDevice) RemovePeer(ctx context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, ok := d.peers[name]
	if !ok {
		return nil
	}
	if key, err := ParseKey(p.PublicKey); err == nil {
		if err := d.ipcSetLocked(ipcConfig{Peers: []ipcPeer{{PublicKey: key, Remove: true}}}); err != nil {
			return err
		}
	}
	delete(d.peers, name)
	d.logf("peer %s revoked", name)
	return nil
}

func (d *wgDevice) Peers(ctx context.Context) ([]Peer, error) {
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
	out := make([]Peer, 0, len(d.peers))
	for _, p := range d.peers {
		if live != nil {
			if key, err := ParseKey(p.PublicKey); err == nil {
				if s, ok := live[key.Hex()]; ok {
					p.LastHandshake = s.LastHandshake
				}
			}
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func checkPeer(p Peer, subnet netip.Prefix) (Key, error) {
	if p.Name == "" {
		return Key{}, fmt.Errorf("vpn: peer: no name")
	}
	key, err := ParseKey(p.PublicKey)
	if err != nil {
		return Key{}, fmt.Errorf("vpn: peer %s: %w", p.Name, err)
	}
	if key.IsZero() {
		return Key{}, fmt.Errorf("vpn: peer %s: the public key is empty", p.Name)
	}
	if !p.IP.IsValid() {
		return Key{}, fmt.Errorf("vpn: peer %s: no address", p.Name)
	}
	if !subnet.Contains(p.IP) {
		return Key{}, fmt.Errorf("vpn: peer %s: address %s is outside %s", p.Name, p.IP, subnet)
	}
	return key, nil
}

func peerSection(key Key, ip netip.Addr) ipcPeer {
	return ipcPeer{
		PublicKey:         key,
		ReplaceAllowedIPs: true,
		AllowedIPs:        []netip.Prefix{netip.PrefixFrom(ip, ip.BitLen())},
	}
}

func (d *wgDevice) PeerAt(ctx context.Context, ip netip.Addr) (Peer, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range d.peers {
		if p.IP == ip {
			return p, true
		}
	}
	return Peer{}, false
}

func (d *wgDevice) Listen(ctx context.Context, at netip.AddrPort) (net.Listener, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.up() {
		return nil, fmt.Errorf("vpn: listen %s: the device is not up", at)
	}
	if _, ok := d.addrs[at.Addr()]; !ok && at.Addr() != d.machineIP {
		return nil, fmt.Errorf("vpn: listen %s: %s is not an address on this machine's network", at, at.Addr())
	}
	return d.stk.ListenTCP(at)
}

func (d *wgDevice) SetAddresses(ctx context.Context, addrs []Address) error {
	next := make(map[netip.Addr]Address, len(addrs))
	for _, a := range addrs {
		if err := checkAddress(a, d.subnet); err != nil {
			return err
		}
		next[a.IP] = a
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	for ip := range d.addrs {
		if _, keep := next[ip]; keep {
			continue
		}

		if ip == d.machineIP {
			continue
		}
		if err := d.removeAddressLocked(ip); err != nil {
			return err
		}
	}
	for ip, a := range next {
		if _, had := d.addrs[ip]; !had {
			if err := d.addAddressLocked(a); err != nil {
				return err
			}
			continue
		}
		d.addrs[ip] = a
	}
	return d.syncZoneLocked()
}

func (d *wgDevice) AddAddress(ctx context.Context, a Address) error {
	if err := checkAddress(a, d.subnet); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.addAddressLocked(a); err != nil {
		return err
	}
	return d.syncZoneLocked()
}

func (d *wgDevice) RemoveAddress(ctx context.Context, ip netip.Addr) error {
	if ip == d.machineIP {
		return fmt.Errorf("vpn: %s is the machine's own address and cannot be removed", ip)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.removeAddressLocked(ip); err != nil {
		return err
	}
	return d.syncZoneLocked()
}

func (d *wgDevice) Addresses(ctx context.Context) ([]Address, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Address, 0, len(d.addrs))
	for _, a := range d.addrs {
		out = append(out, a)
	}
	sortAddresses(out)
	return out, nil
}

func (d *wgDevice) addAddressLocked(a Address) error {
	if _, had := d.addrs[a.IP]; !had && d.up() && a.IP != d.machineIP {
		if err := d.stk.AddAddress(a.IP); err != nil {
			return err
		}
	}
	d.addrs[a.IP] = a
	return nil
}

func (d *wgDevice) removeAddressLocked(ip netip.Addr) error {
	for key, e := range d.relays {
		if key.IP != ip {
			continue
		}
		if e.closer != nil {
			e.closer.Close()
		}
		delete(d.relays, key)
	}
	if d.up() && ip != d.machineIP {
		if err := d.stk.RemoveAddress(ip); err != nil {
			return err
		}
	}
	delete(d.addrs, ip)
	return nil
}

func (d *wgDevice) syncZoneLocked() error {
	addrs := make([]Address, 0, len(d.addrs)+len(d.fleet))
	for _, a := range d.addrs {
		addrs = append(addrs, a)
	}

	for _, a := range d.fleet {
		addrs = append(addrs, a)
	}
	return d.resolver.SetZone(context.Background(), addrs)
}

func checkAddress(a Address, subnet netip.Prefix) error {
	if !a.IP.IsValid() {
		return fmt.Errorf("vpn: address for %q: not an address", a.Owner)
	}
	if !subnet.Contains(a.IP) {
		return fmt.Errorf("vpn: address %s is outside %s", a.IP, subnet)
	}
	for _, n := range a.Names {
		if !IsInternal(n) {
			return fmt.Errorf("vpn: address %s: name %q is outside %s", a.IP, n, Suffix)
		}
	}
	return nil
}

func (d *wgDevice) SetRoutes(ctx context.Context, routes []Route) error {
	return d.setRoutes(routes, func(relayKey) bool { return true })
}

func (d *wgDevice) SetAddressRoutes(ctx context.Context, ip netip.Addr, routes []Route) error {
	for _, r := range routes {
		if r.IP != ip {
			return fmt.Errorf("vpn: route %s:%d does not belong to address %s", r.IP, r.Port, ip)
		}
	}
	return d.setRoutes(routes, func(k relayKey) bool { return k.IP == ip })
}

func (d *wgDevice) setRoutes(routes []Route, scope func(relayKey) bool) error {
	next := make(map[relayKey]Route, len(routes))
	for _, r := range routes {
		if err := checkRoute(r); err != nil {
			return err
		}
		k := keyOf(r)
		if prev, dup := next[k]; dup && prev.Target != r.Target {
			return fmt.Errorf("vpn: route %s is given twice, for %s and %s", k, prev.Target, r.Target)
		}
		next[k] = r
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	for k := range next {
		if _, ok := d.addrs[k.IP]; !ok {
			return fmt.Errorf("vpn: route %s: %s is not an address on this machine's network", k, k.IP)
		}
	}

	for k, e := range d.relays {
		if !scope(k) {
			continue
		}
		r, keep := next[k]
		if keep {
			e.setRoute(r)
			continue
		}
		if e.closer != nil {
			e.closer.Close()
		}
		delete(d.relays, k)
	}

	for k, r := range next {
		if _, have := d.relays[k]; have {
			continue
		}
		if !d.up() {
			d.relays[k] = &relayEntry{route: r}
			continue
		}
		e, err := openRelay(d.stk, d.dial, r, d.logf)
		if err != nil {
			return err
		}
		d.relays[k] = e
	}
	return nil
}

func (d *wgDevice) Routes(ctx context.Context) ([]Route, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Route, 0, len(d.relays))
	for _, e := range d.relays {
		out = append(out, e.Route())
	}
	sort.Slice(out, func(i, j int) bool {
		if c := out[i].IP.Compare(out[j].IP); c != 0 {
			return c < 0
		}
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Protocol < out[j].Protocol
	})
	return out, nil
}

func (d *wgDevice) ipcConfig(priv *Key) ipcConfig {
	port := d.listenPort
	cfg := ipcConfig{PrivateKey: priv, ListenPort: &port, ReplacePeers: true}
	names := make([]string, 0, len(d.peers))
	for name := range d.peers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := d.peers[name]
		key, err := ParseKey(p.PublicKey)
		if err != nil {
			continue
		}
		cfg.Peers = append(cfg.Peers, peerSection(key, p.IP))
	}

	cfg.Peers = append(cfg.Peers, d.machinePeerSectionsLocked()...)
	return cfg
}

func (d *wgDevice) ipcSetLocked(cfg ipcConfig) error {
	if d.dev == nil {
		return nil
	}
	if err := d.dev.IpcSet(cfg.String()); err != nil {
		return d.ipcError("configure the device", err)
	}
	return nil
}

func (d *wgDevice) ipcStatusLocked() (ipcStatus, error) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	if err := d.dev.IpcGetOperation(w); err != nil {
		return ipcStatus{}, d.ipcError("read the device state", err)
	}
	if err := w.Flush(); err != nil {
		return ipcStatus{}, fmt.Errorf("vpn: read the device state: %w", err)
	}
	return parseIPCStatus(&buf)
}

func (d *wgDevice) ipcError(what string, err error) error {
	var ipcErr *device.IPCError
	if e, ok := err.(*device.IPCError); ok {
		ipcErr = e
	}
	if ipcErr != nil && ipcErr.ErrorCode() == -int64(unixEADDRINUSE) {
		return fmt.Errorf("vpn: %s: udp port %d is already in use; another caramelod or a WireGuard interface has it: %w",
			what, d.listenPort, err)
	}
	return fmt.Errorf("vpn: %s: %w", what, err)
}

const unixEADDRINUSE = 98
