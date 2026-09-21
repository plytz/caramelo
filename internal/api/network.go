package api

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vpn"
)

type NetworkStore interface {
	state.VPNStore

	Envs(ctx context.Context, app string) ([]state.EnvRecord, error)
}

type Network struct {
	Store NetworkStore

	Device vpn.Device

	Alloc vpn.Allocator

	Subnet  netip.Prefix
	KeyPath string

	Listen    string
	APIListen string

	Hostname string

	Endpoint string

	Disabled string

	Publish func(ctx context.Context) error

	Now func() time.Time
}

func NewNetwork(store NetworkStore, device vpn.Device, cfg serverconfig.Config) *Network {
	n := &Network{
		Store:     store,
		Device:    device,
		KeyPath:   cfg.VPNKeyPath(),
		Listen:    cfg.VPNListen,
		APIListen: cfg.APIListen,
	}
	subnet, err := vpn.Subnet(cfg.VPNSubnet)
	if err != nil {
		n.Device, n.Disabled = nil, err.Error()
		return n
	}
	n.Subnet = subnet
	n.Alloc = vpn.NewAllocator(subnet, store)
	if device == nil {
		n.Disabled = "this machine's WireGuard device is not running"
	}
	return n
}

const addrAttempts = 32

var ErrNoNetwork = errors.New("this machine has no private network")

func (n *Network) AddPeer(ctx context.Context, name, publicKey string) (*state.Peer, error) {
	if err := n.available(); err != nil {
		return nil, err
	}
	if err := env.ValidateName("peer", name); err != nil {
		return nil, err
	}
	key, err := vpn.ParseKey(publicKey)
	switch {
	case err != nil:
		return nil, fmt.Errorf("peer %q: %w", name, err)
	case key.IsZero():
		return nil, fmt.Errorf("peer %q: the public key is empty", name)
	}
	canonical := key.Base64()

	existing, err := n.Store.Peer(ctx, name)
	switch {
	case err == nil:
		return n.rotate(ctx, existing, canonical)
	case !errors.Is(err, state.ErrNotFound):
		return nil, fmt.Errorf("read peer %q: %w", name, err)
	}
	return n.admit(ctx, name, canonical)
}

func (n *Network) rotate(ctx context.Context, p *state.Peer, publicKey string) (*state.Peer, error) {
	if p.PublicKey == publicKey {

		if err := n.install(ctx, *p); err != nil {
			return nil, err
		}
		return p, nil
	}

	rotated := *p
	rotated.PublicKey = publicKey
	if err := n.Store.SetPeerKey(ctx, p.Name, publicKey); err != nil {
		if errors.Is(err, state.ErrExists) {
			return nil, fmt.Errorf("peer %q: that public key is already registered under another name", p.Name)
		}
		return nil, fmt.Errorf("rotate the key of peer %q: %w", p.Name, err)
	}
	if err := n.install(ctx, rotated); err != nil {

		_ = n.Store.SetPeerKey(ctx, p.Name, p.PublicKey)
		return nil, err
	}
	return &rotated, nil
}

func (n *Network) admit(ctx context.Context, name, publicKey string) (*state.Peer, error) {
	for attempt := 0; attempt < addrAttempts; attempt++ {
		ip, err := n.Alloc.AllocatePeer(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("allocate an address for peer %q: %w", name, err)
		}
		p := state.Peer{
			Name:      name,
			PublicKey: publicKey,
			IP:        ip.String(),

			AddedBy:   env.IdentityFrom(ctx),
			CreatedAt: n.now(),
		}
		err = n.Store.AddPeer(ctx, p)
		switch {
		case err == nil:
			if err := n.install(ctx, p); err != nil {

				_ = n.Store.RemovePeer(ctx, name)
				return nil, err
			}
			return &p, nil
		case errors.Is(err, state.ErrExists):

			if _, err := n.Store.Peer(ctx, name); err == nil {
				return nil, fmt.Errorf("peer %q was added by someone else at the same moment; run the command again", name)
			}
			if who, err := n.peerWithKey(ctx, publicKey); err == nil && who != "" {
				return nil, fmt.Errorf("peer %q: that public key is already registered as %q", name, who)
			}
			continue
		default:
			return nil, fmt.Errorf("record peer %q: %w", name, err)
		}
	}
	return nil, fmt.Errorf("record peer %q: %d addresses in a row were taken by another add", name, addrAttempts)
}

func (n *Network) peerWithKey(ctx context.Context, publicKey string) (string, error) {
	peers, err := n.Store.Peers(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range peers {
		if p.PublicKey == publicKey {
			return p.Name, nil
		}
	}
	return "", nil
}

func (n *Network) install(ctx context.Context, p state.Peer) error {
	dp, err := toDevicePeer(p)
	if err != nil {
		return err
	}
	if err := n.Device.AddPeer(ctx, dp); err != nil {
		return fmt.Errorf("admit peer %q to the network: %w", p.Name, err)
	}
	return nil
}

func (n *Network) Peers(ctx context.Context) ([]state.Peer, error) {
	if err := n.available(); err != nil {
		return nil, err
	}
	rows, err := n.Store.Peers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list peers: %w", err)
	}
	live, err := n.Device.Peers(ctx)
	if err != nil {

		return rows, nil
	}
	seen := make(map[string]time.Time, len(live))
	for _, p := range live {
		seen[p.Name] = p.LastHandshake
	}
	for i := range rows {
		at, ok := seen[rows[i].Name]
		if !ok || at.IsZero() || at.Equal(rows[i].LastHandshake) {
			continue
		}
		rows[i].LastHandshake = at

		_ = n.Store.SetPeerHandshake(ctx, rows[i].Name, at)
	}
	return rows, nil
}

func (n *Network) RemovePeer(ctx context.Context, name string) error {
	if err := n.available(); err != nil {
		return err
	}
	if _, err := n.Store.Peer(ctx, name); err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return fmt.Errorf("no such peer %q", name)
		}
		return fmt.Errorf("read peer %q: %w", name, err)
	}
	if err := n.Device.RemovePeer(ctx, name); err != nil {
		return fmt.Errorf("revoke peer %q on the network: %w", name, err)
	}
	if err := n.Store.RemovePeer(ctx, name); err != nil && !errors.Is(err, state.ErrNotFound) {
		return fmt.Errorf("forget peer %q: %w", name, err)
	}
	return nil
}

func (n *Network) VPNStatus(ctx context.Context) (*VPNStatus, error) {
	st := &VPNStatus{APIListen: n.APIListen, Endpoint: n.Endpoint}
	if n.Subnet.IsValid() {
		st.Subnet = n.Subnet.String()
		st.Address = vpn.MachineIP(n.Subnet).String()
	}
	if n.Device == nil {
		st.Error = n.disabledReason()
		return st, nil
	}
	ds, err := n.Device.Status(ctx)
	if err != nil {
		st.Error = err.Error()
		return st, nil
	}
	st.Enabled = ds.Up
	st.PublicKey = ds.PublicKey
	st.Listen = strOr(ds.Listen, n.Listen)
	st.Resolver = ds.Resolver
	st.Routes = ds.Routes
	if ds.Subnet.IsValid() {
		st.Subnet = ds.Subnet.String()
	}
	if ds.IP.IsValid() {
		st.Address = ds.IP.String()
	}

	st.Reach = st.Subnet
	if ds.Relay && ds.Machines > 0 && ds.Range.IsValid() {
		st.Reach = ds.Range.String()
	}
	if !ds.Up {
		st.Error = strOr(n.Disabled, "the device is not listening")
	}
	if peers, err := n.Store.Peers(ctx); err == nil {
		st.Peers = len(peers)
	}
	if envs, err := n.Store.Envs(ctx, ""); err == nil {
		for _, e := range envs {
			if e.VPNIP != "" {
				st.Envs++
			}
		}
	}
	return st, nil
}

func (n *Network) Start(ctx context.Context) error {
	if n.Device == nil {
		return ErrNoNetwork
	}
	if err := n.recordDevice(ctx); err != nil {
		return err
	}
	if err := n.installMachineAddress(ctx); err != nil {
		return err
	}
	peers, err := n.Store.Peers(ctx)
	if err != nil {
		return fmt.Errorf("list peers: %w", err)
	}
	for _, p := range peers {
		if err := n.install(ctx, p); err != nil {
			return err
		}
	}
	if err := n.Device.Up(ctx); err != nil {
		return fmt.Errorf("start the machine's network: %w", err)
	}
	if n.Publish == nil {
		return nil
	}
	if err := n.Publish(ctx); err != nil {
		return fmt.Errorf("publish the environments on the network: %w", err)
	}
	return nil
}

func (n *Network) installMachineAddress(ctx context.Context) error {
	if !n.Subnet.IsValid() {
		return nil
	}
	a := vpn.Address{IP: vpn.MachineIP(n.Subnet), Kind: vpn.KindMachine, Owner: n.Hostname}
	if n.Hostname != "" {
		a.Names = []string{vpn.MachineHost(n.Hostname)}
	}
	if err := n.Device.AddAddress(ctx, a); err != nil {
		return fmt.Errorf("name the machine's own address: %w", err)
	}
	return nil
}

func (n *Network) Stop(ctx context.Context) error {
	if n.Device == nil {
		return nil
	}
	return n.Device.Down(ctx)
}

func (n *Network) recordDevice(ctx context.Context) error {
	if n.KeyPath == "" || !n.Subnet.IsValid() {
		return nil
	}
	err := n.Store.SetVPN(ctx, state.VPN{
		PrivateKeyPath: n.KeyPath,
		Subnet:         n.Subnet.String(),
		Listen:         strOr(n.Listen, vpn.DefaultListen),
		UpdatedAt:      n.now(),
	})
	if err != nil {
		return fmt.Errorf("record the machine's network: %w", err)
	}
	return nil
}

func (n *Network) available() error {
	if n == nil || n.Device == nil || n.Store == nil || n.Alloc == nil {
		return fmt.Errorf("%w: %s", ErrNoNetwork, n.disabledReason())
	}
	return nil
}

func (n *Network) disabledReason() string {
	if n != nil && n.Disabled != "" {
		return n.Disabled
	}
	return "re-run 'caramelo fleet setup' on it to add one"
}

func (n *Network) now() time.Time {
	if n == nil || n.Now == nil {
		return time.Now().UTC()
	}
	return n.Now()
}

func toDevicePeer(p state.Peer) (vpn.Peer, error) {
	ip, err := netip.ParseAddr(strings.TrimSpace(p.IP))
	if err != nil {
		return vpn.Peer{}, fmt.Errorf("peer %q: address %q: %w", p.Name, p.IP, err)
	}
	return vpn.Peer{
		Name:          p.Name,
		PublicKey:     p.PublicKey,
		IP:            ip,
		AddedBy:       p.AddedBy,
		CreatedAt:     p.CreatedAt,
		LastHandshake: p.LastHandshake,
	}, nil
}

func strOr(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
