package sshapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/runtime/docker"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vault"
	"github.com/plytz/caramelo/internal/vpn"
)

func (d *Daemon) dev() vpn.Device { return d.Device }

func (d *Daemon) logf(format string, args ...any) {
	if d == nil || d.Log == nil {
		return
	}
	fmt.Fprintf(d.Log, "%s caramelod: fleet: "+format+"\n",
		append([]any{time.Now().UTC().Format(time.RFC3339)}, args...)...)
}

func plural(n int, what string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", what)
	}
	return fmt.Sprintf("%d %ss", n, what)
}

func (d *Daemon) Machines(ctx context.Context) ([]fleet.Machine, error) {
	rows, err := storeFleetState{d.Store}.Machines(ctx)
	if err != nil {
		return nil, err
	}
	if len(rows) > 0 {
		return d.withHandshakes(ctx, rows), nil
	}
	self, err := d.selfMachine(ctx)
	if err != nil {
		return nil, err
	}
	return []fleet.Machine{self}, nil
}

func (d *Daemon) withHandshakes(ctx context.Context, rows []fleet.Machine) []fleet.Machine {
	dev := d.dev()
	if dev == nil {
		return rows
	}
	peers, err := dev.MachinePeers(ctx)
	if err != nil {
		return rows
	}
	seen := make(map[string]time.Time, len(peers))
	from := make(map[string]string, len(peers))
	for _, p := range peers {
		if p.LastHandshake.IsZero() {
			continue
		}
		seen[p.PublicKey] = p.LastHandshake
		from[p.PublicKey] = p.SeenAt
	}
	out := fleet.Seen(rows, seen)
	for i, m := range out {
		if m.Name == d.fleetName() || m.PublicKey == "" {
			continue
		}
		hs, ok := seen[m.PublicKey]
		if !ok || !hs.After(rows[i].LastSeen) {
			continue
		}
		endpoint := from[m.PublicKey]
		if endpoint == m.Endpoint {

			endpoint = ""
		} else if endpoint != "" {
			out[i].Endpoint = endpoint
		}
		if err := seenAt(ctx, d.Store, m.Name, endpoint, hs); err != nil {
			d.logf("%v", err)
		}
	}
	return out
}

func (d *Daemon) selfMachine(ctx context.Context) (fleet.Machine, error) {
	m := fleet.Machine{
		Name: d.fleetName(),
		Role: fleet.Role(d.Config.FleetRole()),
		Arch: runtime.GOARCH, OS: runtime.GOOS,
		Private: d.Config.Fleet.Private,
	}

	if d.leftFleet(ctx) {
		m.Role = fleet.RoleHub
	}
	if p, err := d.Config.VPNSubnetPrefix(); err == nil {
		m.Subnet = p
	}
	if dev := d.dev(); dev != nil {
		if st, err := dev.Status(ctx); err == nil {
			m.PublicKey = st.PublicKey
		}
	}
	if rec, err := d.Store.Machine(ctx); err == nil && rec != nil {
		m.Gauge = rec
		if rec.OS.Arch != "" {
			m.Arch = rec.OS.Arch
		}
	}

	m.Envs = 0
	if dir, err := d.Store.Directory(ctx, state.DirectoryFilter{Machine: m.Name}); err == nil {
		m.Envs = len(dir)
	}
	if m.Envs == 0 {
		if envs, err := d.Store.Envs(ctx, ""); err == nil {
			m.Envs = len(envs)
		}
	}
	return m, nil
}

func (d *Daemon) fleetName() string {
	if n := strings.TrimSpace(d.Config.Fleet.Name); n != "" {
		return n
	}
	host := d.Hostname
	if host == "" {
		host, _ = os.Hostname()
	}
	return machineSlug(host)
}

func machineSlug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "hub"
	}
	return out
}

func (d *Daemon) MachineInfo(ctx context.Context, name string) (*api.MachineDetail, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("machine show: which machine")
	}
	machines, err := d.Machines(ctx)
	if err != nil {
		return nil, err
	}
	m, ok := fleet.Find(machines, name)
	if !ok {
		return nil, fmt.Errorf("machine show %s: this fleet has no such machine (`caramelo machine list`)", name)
	}
	det := &api.MachineDetail{Machine: m, Gauge: m.Gauge, Unreachable: !m.Reachable(d.now())}
	if dir, err := d.Store.Directory(ctx, state.DirectoryFilter{Machine: name}); err == nil {
		for _, r := range dir {
			det.Envs = append(det.Envs, directoryEntryOf(r))
		}
	}
	if len(det.Envs) == 0 && m.Name == d.fleetName() {

		if envs, err := d.Store.Envs(ctx, ""); err == nil {
			for i := range envs {
				det.Envs = append(det.Envs, fleet.DirectoryEntry{
					App: envs[i].App, Env: envs[i].Name, Machine: m.Name,
					Address: envs[i].VPNIP, Owner: envs[i].Owner, Mode: envs[i].Mode,
					Via: envs[i].Via, UpdatedAt: envs[i].UpdatedAt,
				})
			}
		}
	}
	det.Machine.Envs = len(det.Envs)
	return det, nil
}

func (d *Daemon) MachineToken(ctx context.Context, req api.MachineTokenRequest) (*api.MachineTokenResult, error) {
	if d.Config.IsMember() {
		return nil, fmt.Errorf("machine token: this machine is a member of %s; ask for a token there",
			d.Config.Fleet.Hub.Name)
	}
	dev := d.dev()
	if dev == nil {
		return nil, errors.New("machine token: this machine's tunnel is not up, so nothing could dial it")
	}
	hub, err := d.ensureHubRow(ctx)
	if err != nil {
		return nil, err
	}
	secret, err := fleet.NewSecret()
	if err != nil {
		return nil, err
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = fleet.TokenTTL
	}
	sess, _ := SessionFrom(ctx)
	tok, err := fleet.NewToken(secret, sess.Identity, d.now(), ttl)
	if err != nil {
		return nil, err
	}

	peerAddr, err := d.reserveJoinAddress(ctx)
	if err != nil {
		return nil, err
	}
	boot, err := vpn.KeyFromSecret(secret).Public()
	if err != nil {
		return nil, fmt.Errorf("machine token: derive the ticket's key: %w", err)
	}
	name := joinPeerName(tok.Hash)
	if err := dev.AddPeer(ctx, vpn.Peer{
		Name: name, PublicKey: boot.Base64(), IP: peerAddr, AddedBy: "machine token", CreatedAt: d.now(),
	}); err != nil {
		return nil, fmt.Errorf("machine token: admit the joining machine: %w", err)
	}
	if err := d.Store.AddJoinToken(ctx, state.JoinToken{
		Hash: tok.Hash, CreatedBy: tok.CreatedBy, CreatedAt: tok.CreatedAt, ExpiresAt: tok.ExpiresAt,
	}); err != nil {
		_ = dev.RemovePeer(ctx, name)
		return nil, fmt.Errorf("machine token: record the token: %w", err)
	}
	d.holdJoinPeer(name, tok.ExpiresAt.Add(joinExpiryGrace))
	ticket := fleet.Ticket{
		Hub: hub.Name, Endpoint: d.hubEndpoint(), PublicKey: hub.PublicKey,
		Address: hub.Address().String(), Peer: peerAddr.String(),
		Range: fleet.FleetRange, Secret: secret,
	}
	blob, err := ticket.Encode()
	if err != nil {
		return nil, err
	}
	return &api.MachineTokenResult{
		Token: blob, Hub: hub.Name, Endpoint: ticket.Endpoint,
		PublicKey: hub.PublicKey, ExpiresAt: tok.ExpiresAt,
	}, nil
}

func joinPeerName(hash string) string {
	if len(hash) > 12 {
		hash = hash[:12]
	}
	return joinPeerPrefix + hash
}

const joinPeerPrefix = "join-"

const joinExpiryGrace = 2 * time.Minute

func (d *Daemon) hubEndpoint() string {
	host := ""
	if rec, err := d.Store.Machine(context.Background()); err == nil && rec != nil {
		host = rec.Network.PrimaryIP
	}
	if host == "" {
		host = d.Hostname
	}
	if _, port, err := net.SplitHostPort(d.Config.VPNListen); err == nil && port != "" {
		return net.JoinHostPort(host, port)
	}
	return net.JoinHostPort(host, "4021")
}

func (d *Daemon) reserveJoinAddress(ctx context.Context) (netip.Addr, error) {
	dev := d.dev()
	taken := map[netip.Addr]bool{}
	if peers, err := dev.Peers(ctx); err == nil {
		for _, p := range peers {
			taken[p.IP] = true
		}
	}
	if addrs, err := dev.Addresses(ctx); err == nil {
		for _, a := range addrs {
			taken[a.IP] = true
		}
	}
	subnet, err := d.Config.VPNSubnetPrefix()
	if err != nil {
		return netip.Addr{}, fmt.Errorf("machine token: read this machine's subnet: %w", err)
	}

	first, last := vpn.PeerRange(subnet)
	for ip := first; ip.Compare(last) <= 0; ip = ip.Next() {
		if !taken[ip] {
			return ip, nil
		}
	}
	return netip.Addr{}, errors.New("machine token: this machine has no free peer address to lend a joining machine")
}

func (d *Daemon) ensureHubRow(ctx context.Context) (fleet.Machine, error) {
	self, err := d.selfMachine(ctx)
	if err != nil {
		return fleet.Machine{}, err
	}
	self.Role = fleet.RoleHub
	self.Subnet = fleet.Hub()
	if self.PublicKey == "" {
		return fleet.Machine{}, errors.New(
			"this machine has no WireGuard key, so nothing could peer with it: is the tunnel up?")
	}
	if existing, err := d.Store.FleetMachine(ctx, self.Name); err == nil && existing != nil {
		self.JoinedAt = existing.JoinedAt
	}
	if self.JoinedAt.IsZero() {
		self.JoinedAt = d.now()
	}
	self.LastSeen = d.now()
	if err := d.Store.PutFleetMachine(ctx, machineRowOf(self)); err != nil {
		return fleet.Machine{}, fmt.Errorf("record this machine as the hub: %w", err)
	}
	return self, nil
}

func (d *Daemon) MachineRedeem(ctx context.Context, req api.RedeemRequest) (*api.RedeemResult, error) {
	if d.Config.IsMember() {
		return nil, fmt.Errorf("a join: this machine is a member of %s and not a hub", d.Config.Fleet.Hub.Name)
	}
	dev := d.dev()
	if dev == nil {
		return nil, errors.New("a join: this machine's tunnel is not up")
	}
	name := machineSlug(req.Name)
	if strings.TrimSpace(req.Name) == "" {
		return nil, errors.New("a join: the joining machine did not say what to call it")
	}
	if strings.TrimSpace(req.PublicKey) == "" {
		return nil, fmt.Errorf("a join: %s sent no public key, and a machine is its key", name)
	}
	hash := fleet.Hash(req.Secret)
	row, err := d.Store.JoinToken(ctx, hash)
	if errors.Is(err, state.ErrNotFound) {
		return nil, errors.New("a join: that is not a token of this hub's")
	}
	if err != nil {
		return nil, fmt.Errorf("a join: read the token: %w", err)
	}
	tok := fleet.Token{
		Hash: row.Hash, CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt,
		ExpiresAt: row.ExpiresAt, UsedBy: row.UsedBy, UsedAt: row.UsedAt,
	}
	if err := tok.Check(d.now()); err != nil {

		if !tok.Used() || tok.UsedBy != name {
			return nil, fmt.Errorf("a join: %w", err)
		}
	}
	hub, err := d.ensureHubRow(ctx)
	if err != nil {
		return nil, err
	}
	machines, err := storeFleetState{d.Store}.Machines(ctx)
	if err != nil {
		return nil, err
	}
	member, changed, err := d.admitMember(ctx, machines, name, req)
	if err != nil {
		return nil, err
	}
	if err := dev.AddMachinePeer(ctx, vpn.MemberPeer(member.Name, member.PublicKey, member.Subnet)); err != nil {
		return nil, fmt.Errorf("a join: peer with %s: %w", member.Name, err)
	}
	if err := d.Store.RedeemJoinToken(ctx, hash, member.Name, d.now()); err != nil &&
		!errors.Is(err, state.ErrExists) {
		return nil, fmt.Errorf("a join: mark the token used: %w", err)
	}

	d.retireJoinPeer(member.Name, joinPeerName(hash))
	d.fleetChanged(ctx)
	return &api.RedeemResult{Machine: member, Hub: hub, Endpoint: d.hubEndpoint(), Changed: changed}, nil
}

func (d *Daemon) admitMember(ctx context.Context, machines []fleet.Machine, name string, req api.RedeemRequest) (fleet.Machine, bool, error) {
	m := fleet.Machine{
		Name: name, Role: fleet.RoleMember, PublicKey: strings.TrimSpace(req.PublicKey),
		Arch: docker.NormaliseArch(req.Arch), OS: req.OS, Private: req.Private,
		Endpoint: req.Endpoint, JoinedAt: d.now(), LastSeen: d.now(),
	}
	changed := true
	if existing, ok := fleet.Find(machines, name); ok {
		if existing.PublicKey != m.PublicKey {
			return fleet.Machine{}, false, fmt.Errorf(
				"a join: this fleet already has a machine called %s with another key; "+
					"remove it (`caramelo machine remove %s`) or join under another name", name, name)
		}
		m.Subnet, m.JoinedAt = existing.Subnet, existing.JoinedAt
		changed = existing.Private != m.Private || existing.Arch != m.Arch
	} else {
		subnet, err := fleet.AllocateSubnet(fleet.TakenSubnets(machines))
		if err != nil {
			return fleet.Machine{}, false, fmt.Errorf("a join: %w", err)
		}
		m.Subnet = subnet
	}
	if err := m.Validate(); err != nil {
		return fleet.Machine{}, false, err
	}
	if err := d.Store.PutFleetMachine(ctx, machineRowOf(m)); err != nil {
		return fleet.Machine{}, false, fmt.Errorf("a join: record %s: %w", name, err)
	}
	return m, changed, nil
}

func (d *Daemon) MachineRemoved(ctx context.Context) error {
	peer, err := d.requireMachinePeer(ctx, "machine removed", "machine remove")
	if err != nil {
		return err
	}
	hub := d.Config.Fleet.Hub.Name
	if !d.Config.IsMember() || peer.Name != hub {
		return fmt.Errorf("machine removed: %q is not this machine's hub", peer.Name)
	}

	d.leaveFleetIn(ctx, hub, leaveReplyGrace, d.logf)
	return nil
}

const leaveReplyGrace = 2 * time.Second

const tellRemovedTimeout = 20 * time.Second

func (d *Daemon) tellMemberRemoved(ctx context.Context, loc api.Location, name string) {
	if d.forwarder() == nil {
		return
	}
	if loc.Machine == "" || loc.Local || !loc.Reachable {
		d.logf("machine remove %s: it could not be told; it will find out when its next announcement is refused", name)
		return
	}

	ctx, cancel := context.WithTimeout(ctx, tellRemovedTimeout)
	defer cancel()
	argv := []string{"machine", "removed", "--json"}
	if _, err := d.forwarder().Forward(ctx, loc, argv, nil, io.Discard, io.Discard); err != nil {
		d.logf("machine remove %s: telling it did not land (%v); "+
			"it will find out when its next announcement is refused", name, err)
		return
	}
	d.logf("machine remove %s: told it, and it is a machine of one again", name)
}

func (d *Daemon) MachineRemove(ctx context.Context, req api.MachineRemoveRequest, out io.Writer) error {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return errors.New("machine remove: which machine")
	}
	if name == d.fleetName() {
		return fmt.Errorf("machine remove %s: that is this machine, and a hub cannot remove itself", name)
	}
	if _, err := d.Store.FleetMachine(ctx, name); errors.Is(err, state.ErrNotFound) {

		return nil
	} else if err != nil {
		return fmt.Errorf("machine remove %s: read the machine: %w", name, err)
	}
	dir, err := d.Store.Directory(ctx, state.DirectoryFilter{Machine: name})
	if err != nil {
		return fmt.Errorf("machine remove %s: read what it holds: %w", name, err)
	}
	if len(dir) > 0 && !req.Force {
		held := make([]string, 0, len(dir))
		for _, r := range dir {
			held = append(held, r.App+"/"+r.Env)
		}
		sort.Strings(held)
		return fmt.Errorf("machine remove %s: it holds %s (%s); "+
			"destroy them first, or pass --force to destroy them with it",
			name, plural(len(held), "environment"), strings.Join(held, ", "))
	}

	where := d.machineAt(ctx, name)

	if err := d.Store.DeleteFleetMachine(ctx, name); err != nil {
		return fmt.Errorf("machine remove %s: %w", name, err)
	}
	for _, r := range dir {
		d.destroyOnMember(ctx, where, name, r.App, r.Env, out)
		if err := d.Store.DeleteDirectoryEntry(ctx, r.App, r.Env); err != nil {
			return fmt.Errorf("machine remove %s: forget %s/%s: %w", name, r.App, r.Env, err)
		}
	}

	d.tellMemberRemoved(ctx, where, name)
	if dev := d.dev(); dev != nil {
		if err := dev.RemoveMachinePeer(ctx, name); err != nil {
			return fmt.Errorf("machine remove %s: drop the peer: %w", name, err)
		}
		if err := dev.RemoveForward(ctx, vpn.IngressForward(name, netip.Addr{}).Name); err != nil {
			d.logf("machine remove %s: close the ingress forward: %v", name, err)
		}
	}
	if err := d.Store.DeleteMachineImages(ctx, name); err != nil {
		d.logf("machine remove %s: forget its images: %v", name, err)
	}
	d.fleetChanged(ctx)
	return nil
}

func (d *Daemon) machineAt(ctx context.Context, name string) api.Location {
	if d.resolver() == nil {
		return api.Location{}
	}
	loc, err := d.resolver().ResolveMachine(ctx, name)
	if err != nil {
		return api.Location{}
	}
	return loc
}

func (d *Daemon) destroyOnMember(ctx context.Context, loc api.Location, mach, app, name string, out io.Writer) {
	if d.forwarder() == nil {
		return
	}
	if loc.Machine == "" || loc.Local || !loc.Reachable {

		d.logf("machine remove %s: %s/%s could not be destroyed there; forgetting it", mach, app, name)
		return
	}

	argv := []string{"env", "destroy", name, "--app", app, "--yes", "--force", "--json"}
	if _, err := d.forwarder().Forward(ctx, loc, argv, nil, out, out); err != nil {

		d.logf("machine remove %s: %s/%s could not be destroyed there (%v); forgetting it", mach, app, name, err)
	}
}

func (d *Daemon) holdsEnv(ctx context.Context, peer, app, name string) error {
	if peer == "" || app == "" || name == "" || d.Store == nil {
		return nil
	}

	entry, ok, err := fleetDirectory{d.Store}.Locate(ctx, app, name)
	if err != nil {
		return fmt.Errorf("a secrets bundle: %w", err)
	}
	if !ok || entry.Machine == "" || entry.Machine == peer {
		return nil
	}
	return fmt.Errorf("a secrets bundle: %s/%s runs on machine %s, and %s is asking; "+
		"a machine is only answered about the environments it holds", app, name, entry.Machine, peer)
}

func (d *Daemon) VaultBundle(ctx context.Context, req api.BundleRequest) (*vault.Bundle, error) {
	peer, err := d.requireMachinePeer(ctx, "a secrets bundle", "secrets list")
	if err != nil {
		return nil, err
	}
	if err := d.holdsEnv(ctx, peer.Name, req.App, req.Env); err != nil {
		return nil, err
	}
	if d.Vault == nil {
		return nil, errors.New("a secrets bundle: this machine has no vault")
	}
	h := &vault.Hub{
		Store: d.Vault, Machine: d.fleetName(), Now: d.now,
		Asker: func(context.Context) string { return peer.Name },
		Audit: d.auditFetch,
	}
	return h.Bundle(ctx, vault.BundleRequest{
		App: req.App, Env: req.Env, Machine: peer.Name, Reason: req.Reason,
	})
}

func (d *Daemon) auditFetch(ctx context.Context, f vault.Fetch) {
	m, err := d.envs()
	if err != nil {
		return
	}
	detail := fmt.Sprintf("%s fetched %s for %s/%s", f.Machine, plural(len(f.Names), "secret"), f.App, f.Env)
	if f.Reason != "" {
		detail += " (" + f.Reason + ")"
	}
	if f.Fingerprint != "" {
		detail += ", fingerprint " + f.Fingerprint
	}
	m.RecordFetch(context.WithoutCancel(ctx), f.App, f.Env, f.Machine, detail)
}

func (d *Daemon) ImageTransfer(ctx context.Context, req api.ImageTransferRequest, out io.Writer) (*api.ImageTransferResult, error) {
	if _, err := d.requireMachinePeer(ctx, "an image transfer", "deploy"); err != nil {
		return nil, err
	}
	t := d.transfer()
	if t == nil {
		return nil, errors.New("an image transfer: this machine has no container runtime that can move images")
	}
	tr := release.TransferRequest{Release: req.Release, Refs: req.Refs, Arch: req.Arch}
	var res *release.TransferResult
	var err error
	if req.Send {
		res, err = t.Send(ctx, tr, out)
	} else {
		res, err = t.Receive(ctx, tr, req.Archive)
	}
	if err != nil {
		return nil, err
	}
	return &api.ImageTransferResult{Images: res.Images, Bytes: res.Bytes, Duration: res.Duration}, nil
}

func (d *Daemon) transfer() *release.Transfer {
	m, err := d.envs()
	if err != nil || m.Driver == nil {
		return nil
	}
	mover, ok := m.Driver.(release.Mover)
	if !ok {
		return nil
	}
	return &release.Transfer{
		Mover: mover, Store: releaseImages{d.Store},
		Machine: d.fleetName(), Arch: runtime.GOARCH, Now: d.now,
	}
}

func (d *Daemon) EnvWorktreeStatus(ctx context.Context, app, name string) (*env.WorktreeStatus, error) {
	m, err := d.envs()
	if err != nil {
		return nil, err
	}
	return m.WorktreeStatusOf(d.withIdentity(ctx), app, name)
}

func (d *Daemon) EnvSync(ctx context.Context, app, name string) (*env.Env, error) {
	m, err := d.envs()
	if err != nil {
		return nil, err
	}
	return m.SyncBranch(d.withIdentity(ctx), app, name)
}

func (d *Daemon) GitPrecheck(ctx context.Context, app string, branches []string) error {
	m, err := d.envs()
	if err != nil {
		return err
	}
	return m.CheckPush(d.withIdentity(ctx), app, branches)
}

func (d *Daemon) recordAnnouncement(ctx context.Context, peer fleet.Machine, a fleet.Announcement) {
	if d.Store == nil {

		return
	}
	if a.Gauge != nil {
		if b, err := json.Marshal(a.Gauge); err == nil {
			if err := d.Store.SetMachineGauge(ctx, peer.Name, string(b)); err != nil {
				d.logf("record %s's gauge: %v", peer.Name, err)
			}
		}
	}
	arch := docker.NormaliseArch(a.Arch)
	if arch == "" && a.Gauge != nil {
		arch = a.Gauge.OS.Arch
	}
	if arch != "" && arch != peer.Arch {
		row := machineRowOf(peer)
		row.Arch, row.OS = arch, a.OS
		if err := d.Store.PutFleetMachine(ctx, row); err != nil {
			d.logf("record %s's architecture: %v", peer.Name, err)
		}
	}

	d.dropJoinPeer(ctx, peer.Name)
	at := a.At
	if at.IsZero() {
		at = d.now()
	}
	if err := seenAt(ctx, d.Store, peer.Name, "", at); err != nil {
		d.logf("%v", err)
	}
}

func (d *Daemon) envFromDirectory(ctx context.Context, app, name string) (*api.EnvDetail, bool, error) {
	if d.resolver() == nil {
		return nil, false, nil
	}
	loc, err := d.resolver().ResolveEnv(ctx, app, name)
	if err != nil || loc.Local || loc.Reachable {
		return nil, false, err
	}
	return d.envInDirectory(ctx, app, name)
}

func (d *Daemon) envInDirectory(ctx context.Context, app, name string) (*api.EnvDetail, bool, error) {
	row, err := d.Store.DirectoryEntry(ctx, app, name)
	if errors.Is(err, state.ErrNotFound) || (err == nil && row == nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("look %s/%s up in the directory: %w", app, name, err)
	}
	e := directoryEntryOf(*row)
	return &api.EnvDetail{
		Env: env.Env{
			App: e.App, Name: e.Env, Branch: e.Env, VPNIP: e.Address,
			Mode: env.Mode(e.Mode), Machine: e.Machine, Owner: e.Owner,
			Via: config.Via(e.Via), UpdatedAt: e.UpdatedAt,
		},
		Machine:     e.Machine,
		Unreachable: true,
	}, true, nil
}

func (d *Daemon) ReleaseOfTree(ctx context.Context, app, tree string) (*release.Release, error) {
	row, err := d.Store.ReleaseByTree(ctx, app, tree)
	if errors.Is(err, state.ErrNotFound) {
		return nil, fmt.Errorf("app %q has no release of tree %s: %w", app, tree, state.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("read the release of tree %s of app %q: %w", tree, app, err)
	}
	rel, err := release.FromRecord(row)
	if err != nil {
		return nil, err
	}
	if err := release.Attach(ctx, releaseImages{d.Store}, rel); err != nil {
		return nil, err
	}
	return rel, nil
}

func (d *Daemon) ReleaseRecord(ctx context.Context, req api.ReleaseRecordRequest) error {
	if _, err := d.requireMachinePeer(ctx, "a release record", "deploy"); err != nil {
		return err
	}
	rel := req.Release
	if strings.TrimSpace(rel.App) == "" || strings.TrimSpace(rel.Tree) == "" {
		return errors.New("a release record: it names no app or no tree")
	}
	row, err := d.Store.ReleaseByTree(ctx, rel.App, rel.Tree)
	switch {
	case errors.Is(err, state.ErrNotFound):
		if row, err = d.Store.AddRelease(ctx, releaseRowOf(rel)); err != nil && !errors.Is(err, state.ErrExists) {
			return fmt.Errorf("record release %s of app %q: %w", rel.Tree, rel.App, err)
		}
		if errors.Is(err, state.ErrExists) {

			if row, err = d.Store.ReleaseByTree(ctx, rel.App, rel.Tree); err != nil {
				return fmt.Errorf("read release %s of app %q back: %w", rel.Tree, rel.App, err)
			}
		}
	case err != nil:
		return fmt.Errorf("read the release of tree %s of app %q: %w", rel.Tree, rel.App, err)
	}
	store := releaseImages{d.Store}
	for _, im := range req.Images {
		im.ReleaseID = row.ID
		if err := store.PutImage(ctx, im); err != nil {
			return err
		}
	}
	return nil
}

func releaseRowOf(rel release.Release) state.Release {
	row := state.Release{
		App: rel.App, Commit: rel.Commit, Tree: rel.Tree, Ref: rel.Ref,
		BuiltBy: rel.BuiltBy, BuiltAt: rel.BuiltAt, Machine: rel.Machine,
	}
	if len(rel.Images) > 0 {
		if b, err := json.Marshal(rel.Images); err == nil {
			row.ImagesJSON = string(b)
		}
	}
	if rel.Config != nil {
		if b, err := json.Marshal(rel.Config); err == nil {
			row.ConfigJSON = string(b)
		}
	}
	return row
}

func (d *Daemon) retireJoinPeer(machine, peer string) {
	d.joinMu.Lock()
	defer d.joinMu.Unlock()
	if d.joinPeers == nil {
		d.joinPeers = map[string]string{}
	}
	d.joinPeers[machine] = peer
}

func (d *Daemon) dropJoinPeer(ctx context.Context, machine string) {
	d.joinMu.Lock()
	peer := d.joinPeers[machine]
	delete(d.joinPeers, machine)
	delete(d.joinTickets, peer)
	d.joinMu.Unlock()
	if peer == "" {
		return
	}
	dev := d.dev()
	if dev == nil {
		return
	}
	if err := dev.RemovePeer(ctx, peer); err != nil {
		d.logf("drop the bootstrap peer of %s: %v", machine, err)
	}
}

func (d *Daemon) holdJoinPeer(peer string, until time.Time) {
	d.joinMu.Lock()
	if d.joinTickets == nil {
		d.joinTickets = map[string]time.Time{}
	}
	d.joinTickets[peer] = until
	d.joinMu.Unlock()

	if d.Background == nil {
		return
	}
	bg := d.Background
	if wait := until.Sub(d.now()); wait > 0 {
		time.AfterFunc(wait, func() { d.expireJoinPeers(bg) })
	}
}

func (d *Daemon) expireJoinPeers(ctx context.Context) {
	now := d.now()
	if d.Store != nil {

		if n, err := d.Store.DeleteExpiredJoinTokens(ctx, now.Add(-joinExpiryGrace)); err != nil {
			d.logf("forget the expired join tokens: %v", err)
		} else if n > 0 {
			d.logf("forgot %s", plural(n, "expired join token"))
		}
	}
	dev := d.dev()
	if dev == nil {
		return
	}
	d.joinMu.Lock()
	inFlight := make(map[string]bool, len(d.joinPeers))
	for _, name := range d.joinPeers {
		inFlight[name] = true
	}
	var stale []string
	for peer, until := range d.joinTickets {
		if inFlight[peer] || until.IsZero() || now.Before(until) {
			continue
		}
		stale = append(stale, peer)
		delete(d.joinTickets, peer)
	}
	d.joinMu.Unlock()
	sort.Strings(stale)
	for _, peer := range stale {
		if err := dev.RemovePeer(ctx, peer); err != nil {
			d.logf("drop the expired bootstrap peer %s: %v", peer, err)
			continue
		}
		d.logf("dropped the expired bootstrap peer %s", peer)
	}
}

func (d *Daemon) sweepJoinPeers(ctx context.Context) {
	dev := d.dev()
	if dev == nil {
		return
	}
	peers, err := dev.Peers(ctx)
	if err != nil {
		return
	}
	d.joinMu.Lock()
	inFlight := make(map[string]bool, len(d.joinPeers))
	for _, name := range d.joinPeers {
		inFlight[name] = true
	}
	for peer := range d.joinTickets {
		if !inFlight[peer] {
			delete(d.joinTickets, peer)
		}
	}
	d.joinMu.Unlock()
	for _, p := range peers {
		if !strings.HasPrefix(p.Name, joinPeerPrefix) || inFlight[p.Name] {
			continue
		}
		if err := dev.RemovePeer(ctx, p.Name); err != nil {
			d.logf("drop the stale bootstrap peer %s: %v", p.Name, err)
			continue
		}
		d.logf("dropped the stale bootstrap peer %s", p.Name)
	}
}

type daemonMachines struct{ d *Daemon }

func (m daemonMachines) MachineNamed(ctx context.Context, name string) (fleet.Machine, bool) {
	if m.d == nil || m.d.machineLookup() == nil {
		return fleet.Machine{}, false
	}
	return m.d.machineLookup().MachineNamed(ctx, name)
}
