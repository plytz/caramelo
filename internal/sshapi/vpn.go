package sshapi

import (
	"context"
	"fmt"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vpn"
)

func (d *Daemon) SetDevice(dev vpn.Device) {
	d.netMu.Lock()
	defer d.netMu.Unlock()
	d.Device = dev
	d.net = d.newNetwork(dev)
}

func (d *Daemon) network() *api.Network {
	d.netMu.Lock()
	defer d.netMu.Unlock()
	if d.net == nil {
		d.net = d.newNetwork(d.Device)
	}
	return d.net
}

func (d *Daemon) newNetwork(dev vpn.Device) *api.Network {
	n := api.NewNetwork(d.Store, dev, d.Config)
	n.Hostname = d.hostname()

	if d.EnvManager != nil {
		n.Publish = d.EnvManager.Republish
	}
	return n
}

func (d *Daemon) AddPeer(ctx context.Context, name, publicKey string) (*state.Peer, error) {
	return d.network().AddPeer(ctx, name, publicKey)
}

func (d *Daemon) Peers(ctx context.Context) ([]state.Peer, error) {
	return d.network().Peers(ctx)
}

func (d *Daemon) RemovePeer(ctx context.Context, name string, force bool) error {
	if !force {
		if sess, ok := SessionFrom(ctx); ok && sess.Identity != "" && sess.Identity == name {
			return fmt.Errorf("%q is the identity this session arrived as: revoking it takes away "+
				"the way in, and this command's own answer with it. Revoke it from another peer, "+
				"from the machine itself, or pass --force", name)
		}
	}
	return d.network().RemovePeer(ctx, name)
}

func (d *Daemon) VPNStatus(ctx context.Context) (*api.VPNStatus, error) {
	return d.network().VPNStatus(ctx)
}
