package sshapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"runtime"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/git"
	"github.com/plytz/caramelo/internal/machine"
	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vault"
	"github.com/plytz/caramelo/internal/vpn"
)

func (d *Daemon) fleetOn(ctx context.Context) bool {
	if d.leftFleet(ctx) {

		return false
	}
	if d.Config.IsMember() || strings.TrimSpace(d.Config.Fleet.Role) != "" {
		return true
	}
	rows, err := d.Store.FleetMachines(ctx)
	return err == nil && len(rows) > 0
}

func (d *Daemon) configureFleet(ctx context.Context, logf func(string, ...any)) {
	if !d.fleetOn(ctx) {
		return
	}
	self, role := d.fleetName(), fleet.Role(d.Config.FleetRole())
	d.setKnownMachines(storeMachines{d.Store})
	st := storeFleetState{d.Store}
	if d.EnvManager != nil {
		m := d.EnvManager
		w := m.FleetWiringOf()
		w.Machine, w.Role, w.Private = self, role, d.Config.Fleet.Private
		w.Fleet = st
		w.Arch = runtime.GOARCH

		w.Gauge = func(ctx context.Context) *machine.Record {
			rec, err := d.Store.Machine(ctx)
			if err != nil || rec == nil {
				return nil
			}
			if s, err := machine.Resample(); err == nil {
				rec.Apply(s)
			}
			return rec
		}
		if m.Builder != nil {
			w.Supply = &release.Supply{
				Images:  d.transfer(),
				Builder: m.Builder,
				Puller:  &machinePuller{d: d},
				Sources: func(ctx context.Context) ([]release.Source, error) { return d.imageSources(ctx) },
			}
			if d.Config.IsMember() {

				w.Supply.Lookup = d.releaseOfTree
				w.Supply.Report = d.reportRelease
				w.Supply.Adopt = d.adoptRelease
			}
			if w.Supply.Images == nil {

				w.Supply = nil
				logf("fleet: this machine's container runtime cannot move images; releases are always built here")
			}
		}
		if d.Config.IsMember() {
			w.Mirror = d.mirror
			w.OpenIngress = d.openIngressFor
		}
		m.SetFleetWiring(w)
	}
	if role.IsHub() {
		d.setResolver(&api.FleetResolver{Self: self, Dir: fleetDirectory{d.Store}, Now: d.now})
	}
	if d.Config.IsMember() {
		d.configureMemberVault(logf)
	}
	logf("fleet: this machine is %s, role %s%s", self, role, privateSuffix(d.Config.Fleet.Private))
}

func privateSuffix(private bool) string {
	if private {
		return ", private"
	}
	return ""
}

func (d *Daemon) configureMemberVault(logf func(string, ...any)) {
	hub := d.Config.Fleet.Hub.Name
	remote := &vault.Remote{Hub: hub, Machine: d.fleetName(), Fetch: &hubFetcher{d: d}}
	d.Vault = remote
	if d.EnvManager != nil {
		d.EnvManager.Secrets = remote
	}
	logf("vault: this machine is a member of %s and keeps no secrets of its own", hub)
}

func (d *Daemon) startFleetNetwork(ctx context.Context, logf func(string, ...any)) *fleetFeeds {
	if !d.fleetOn(ctx) || d.dev() == nil {
		return nil
	}
	d.setForwarder(&api.TunnelForwarder{Dialer: d.dev(), Machine: d.fleetName()})
	if d.EnvManager != nil {
		d.EnvManager.UpdateFleetWiring(func(w *env.FleetWiring) {
			w.RemoteWorktree = d.remoteWorktree
			w.Announce = d.announcer()
		})
	}
	if d.Config.IsMember() {
		if err := d.recordOwnFleet(ctx); err != nil {
			logf("fleet: %v", err)
		}
	}
	if !d.Config.IsMember() {

		if _, err := d.ensureHubRow(ctx); err != nil {
			logf("fleet: %v", err)
		}
	}
	if err := d.syncMachinePeers(ctx); err != nil {
		logf("fleet: %v", err)
	}
	if err := d.openIngress(ctx); err != nil {
		logf("fleet: %v", err)
	}
	if err := d.syncForwards(ctx); err != nil {
		logf("fleet: %v", err)
	}
	if err := d.syncFleetNames(ctx); err != nil {
		logf("fleet: %v", err)
	}
	if d.EnvManager != nil {
		if err := d.EnvManager.AnnounceAll(ctx); removedByHub(err) {
			d.leaveFleet(ctx, d.Config.Fleet.Hub.Name, logf)
			return nil
		} else if err != nil {
			logf("fleet: %v", err)
		}
	}
	go d.heartbeat(ctx, logf)
	feeds := d.followMembers(ctx)
	if feeds != nil {
		logf("fleet: following %s", plural(len(feeds.f.Machines()), "member's feed"))
	}
	d.setFeeds(feeds)
	return feeds
}

func (d *Daemon) heartbeat(ctx context.Context, logf func(string, ...any)) {
	t := time.NewTicker(fleet.HeartbeatInterval)
	defer t.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if d.EnvManager == nil {
			return
		}
		if !d.Config.IsMember() {

			if _, err := d.ensureHubRow(ctx); err != nil {
				d.logf("%v", err)
			}

			d.expireJoinPeers(ctx)
			continue
		}
		err := d.EnvManager.AnnounceAll(ctx)
		if removedByHub(err) {

			d.leaveFleet(ctx, d.Config.Fleet.Hub.Name, logf)
			return
		}
		if err != nil {

			if failures == 0 {
				logf("fleet: %v", err)
			}
			failures++
			continue
		}
		if failures > 0 {
			logf("fleet: the hub %s is answering again (%d announcement(s) missed)",
				d.Config.Fleet.Hub.Name, failures)
			failures = 0
		}
	}
}

type fleetFeeds struct {
	f   *progress.Fleet
	ctx context.Context
}

func (f *fleetFeeds) Close() {
	if f != nil && f.f != nil {
		f.f.Close()
	}
}

func (d *Daemon) followMembers(ctx context.Context) *fleetFeeds {
	if d.Feed == nil || !fleet.Role(d.Config.FleetRole()).IsHub() {
		return nil
	}
	if d.fleetFeedsNow() != nil {

		d.fleetFeedsNow().f.Set(ctx, d.memberNames(ctx))
		return d.fleetFeedsNow()
	}
	f := progress.NewFleet(&memberFeeds{d: d}, d.Feed)
	feeds := &fleetFeeds{f: f, ctx: ctx}
	f.Set(ctx, d.memberNames(ctx))
	return feeds
}

func (d *Daemon) memberNames(ctx context.Context) []string {
	rows, err := d.Store.FleetMachines(ctx)
	if err != nil {
		return nil
	}
	self := d.fleetName()
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Name != self {
			out = append(out, r.Name)
		}
	}
	return out
}

func (d *Daemon) fleetChanged(ctx context.Context) {
	d.fleetBegan(ctx)
	if err := d.syncMachinePeers(ctx); err != nil {
		d.logf("%v", err)
	}
	if err := d.syncForwards(ctx); err != nil {
		d.logf("%v", err)
	}
	if err := d.syncFleetNames(ctx); err != nil {
		d.logf("%v", err)
	}
	if d.fleetFeedsNow() != nil {
		d.fleetFeedsNow().f.Set(d.fleetFeedsNow().ctx, d.memberNames(ctx))
	}
	if d.EnvManager != nil {
		if err := d.EnvManager.PushRoutes(ctx); err != nil {
			d.logf("push the route table after the fleet changed: %v", err)
		}
	}
}

func (d *Daemon) recordOwnFleet(ctx context.Context) error {
	self, err := d.selfMachine(ctx)
	if err != nil {
		return err
	}
	self.Role = fleet.RoleMember
	if p, err := d.Config.FleetSubnetPrefix(); err == nil {
		self.Subnet = p
	}
	if self.PublicKey == "" {
		return errors.New("this machine has no WireGuard key yet; its own row is not recorded")
	}
	if existing, err := d.Store.FleetMachine(ctx, self.Name); err == nil && existing != nil {
		self.JoinedAt = existing.JoinedAt
	}
	if self.JoinedAt.IsZero() {
		self.JoinedAt = d.now()
	}
	self.LastSeen = d.now()
	if err := d.Store.PutFleetMachine(ctx, machineRowOf(self)); err != nil {
		return fmt.Errorf("record this machine's own row: %w", err)
	}
	h := d.Config.Fleet.Hub
	hub := fleet.Machine{
		Name: h.Name, Role: fleet.RoleHub, PublicKey: h.PublicKey,
		Endpoint: h.Endpoint, JoinedAt: self.JoinedAt, LastSeen: d.now(),
	}
	if p, err := d.Config.HubSubnetPrefix(); err == nil {
		hub.Subnet = p
	}
	if err := hub.Validate(); err != nil {
		return fmt.Errorf("record the hub's row: %w", err)
	}
	if err := d.Store.PutFleetMachine(ctx, machineRowOf(hub)); err != nil {
		return fmt.Errorf("record the hub's row: %w", err)
	}
	return nil
}

func (d *Daemon) fleetBegan(ctx context.Context) {
	d.fleetBeginMu.Lock()
	defer d.fleetBeginMu.Unlock()
	if d.resolver() != nil || d.Config.IsMember() || !d.fleetOn(ctx) {
		return
	}
	bg := d.Background
	if bg == nil {
		bg = context.WithoutCancel(ctx)
	}
	d.configureFleet(bg, d.logf)
	if feeds := d.startFleetNetwork(bg, d.logf); feeds != nil {
		d.setFeeds(feeds)
	}
}

func (d *Daemon) syncMachinePeers(ctx context.Context) error {
	dev := d.dev()
	if dev == nil {
		return nil
	}
	want := map[string]vpn.MachinePeer{}
	if d.Config.IsMember() {
		h := d.Config.Fleet.Hub
		addr, err := d.Config.HubAddress()
		if err != nil {
			return fmt.Errorf("peer with the hub %s: %w", h.Name, err)
		}
		rng, err := d.Config.FleetRangePrefix()
		if err != nil {
			return fmt.Errorf("peer with the hub %s: %w", h.Name, err)
		}
		want[h.Name] = vpn.HubPeer(h.Name, h.PublicKey, h.Endpoint, addr, rng)
	} else {
		rows, err := d.Store.FleetMachines(ctx)
		if err != nil {
			return fmt.Errorf("read the fleet's machines: %w", err)
		}
		self := d.fleetName()
		for _, r := range rows {
			if r.Name == self || r.Role == string(fleet.RoleHub) {
				continue
			}
			p, err := netip.ParsePrefix(r.Subnet)
			if err != nil {
				continue
			}
			want[r.Name] = vpn.MemberPeer(r.Name, r.PublicKey, p)
		}
	}
	have, err := dev.MachinePeers(ctx)
	if err != nil {
		return fmt.Errorf("read the machine peers: %w", err)
	}
	for _, p := range have {
		if _, ok := want[p.Name]; !ok {
			if err := dev.RemoveMachinePeer(ctx, p.Name); err != nil {
				return fmt.Errorf("drop the machine peer %s: %w", p.Name, err)
			}
		}
	}
	for name, p := range want {
		if err := dev.AddMachinePeer(ctx, p); err != nil {
			return fmt.Errorf("peer with %s: %w", name, err)
		}
	}
	return nil
}

func (d *Daemon) openIngress(ctx context.Context) error {
	if d.EnvManager == nil {
		return nil
	}
	return d.openIngressFor(ctx, d.EnvManager.IngressWanted(ctx))
}

func (d *Daemon) openIngressFor(ctx context.Context, ing *edge.Ingress) error {
	dev := d.dev()
	if dev == nil || !d.Config.IsMember() {
		return nil
	}
	me, err := d.Config.FleetSubnetPrefix()
	if err != nil {
		return fmt.Errorf("open the private ingress: %w", err)
	}
	if ing == nil || !ing.Enabled {

		if err := dev.SetAddressRoutes(ctx, vpn.MachineIP(me), nil); err != nil {
			return fmt.Errorf("close the private ingress: %w", err)
		}
		return nil
	}
	port := ing.Port
	if port == 0 {
		port = edge.IngressPort
	}
	subnet, err := d.Config.HubSubnetPrefix()
	if err != nil {
		return fmt.Errorf("open the private ingress: %w", err)
	}
	route := vpn.IngressRoute(vpn.MachineIP(me), port, subnet)
	if err := dev.SetAddressRoutes(ctx, route.IP, []vpn.Route{route}); err != nil {
		return fmt.Errorf("open the private ingress at %s: %w", route.IP, err)
	}
	return nil
}

func (d *Daemon) syncForwards(ctx context.Context) error {
	dev := d.dev()
	if dev == nil || d.Config.IsMember() {
		return nil
	}
	machines, err := storeFleetState{d.Store}.Machines(ctx)
	if err != nil {
		return fmt.Errorf("read the fleet's machines: %w", err)
	}
	self := d.fleetName()
	want := map[string]vpn.Forward{}
	for _, m := range machines {
		if m.Name == self || m.Role.IsHub() {
			continue
		}
		addr := m.Address()
		port, ok := env.ViaRelayPortFor(m)
		if !addr.IsValid() || !ok {
			continue
		}
		f := vpn.IngressForward(m.Name, addr)
		f.Port = port
		want[f.Name] = f
	}
	have, err := dev.Forwards(ctx)
	if err != nil {
		return fmt.Errorf("read the forwards: %w", err)
	}
	for _, f := range have {
		if _, ok := want[f.Name]; !ok {
			if err := dev.RemoveForward(ctx, f.Name); err != nil {
				return fmt.Errorf("close the forward %s: %w", f.Name, err)
			}
		}
	}
	for name, f := range want {
		if _, err := dev.AddForward(ctx, f); err != nil {
			return fmt.Errorf("open the door to %s: %w", name, err)
		}
	}
	return nil
}

func (d *Daemon) imageSources(ctx context.Context) ([]release.Source, error) {
	machines, err := d.knownMachines(ctx)
	if err != nil {
		return nil, err
	}
	self, now := d.fleetName(), d.now()
	member, hub := d.Config.IsMember(), d.Config.Fleet.Hub.Name
	out := make([]release.Source, 0, len(machines))
	for _, m := range machines {
		if m.Name == self {
			continue
		}
		reachable := m.Reachable(now)
		if member && m.Name != hub {
			reachable = false
		}
		out = append(out, release.Source{Machine: m.Name, Reachable: reachable})
	}
	return out, nil
}

func (d *Daemon) hubLocation() (api.Location, error) {
	if !d.Config.IsMember() {
		return api.Location{}, errors.New("this machine is not a member of any fleet")
	}
	at, err := d.Config.HubAPIAddrPort()
	if err != nil {
		return api.Location{}, fmt.Errorf("find the hub %s: %w", d.Config.Fleet.Hub.Name, err)
	}
	return api.Location{
		Machine: d.Config.Fleet.Hub.Name, Address: at.String(), Reachable: true,
	}, nil
}

func (d *Daemon) machineLocation(ctx context.Context, name string) (api.Location, error) {
	if d.resolver() != nil {
		return d.resolver().ResolveMachine(ctx, name)
	}
	machines, err := d.knownMachines(ctx)
	if err != nil {
		return api.Location{}, err
	}
	m, ok := fleet.Find(machines, name)
	if !ok {
		return api.Location{}, fmt.Errorf("machine %q: this fleet has no such machine", name)
	}
	if m.Name == d.fleetName() {
		return api.Location{Machine: m.Name, Local: true, Reachable: true}, nil
	}
	addr := m.Address()
	loc := api.Location{Machine: m.Name, Reachable: m.Reachable(d.now()), LastSeen: m.LastSeen}
	if addr.IsValid() {
		loc.Address = netip.AddrPortFrom(addr, uint16(d.Config.SSHPort)).String()
	}
	return loc, nil
}

func (d *Daemon) knownMachines(ctx context.Context) ([]fleet.Machine, error) {
	if !d.Config.IsMember() {
		return storeFleetState{d.Store}.Machines(ctx)
	}
	loc, err := d.hubLocation()
	if err != nil {
		return nil, err
	}
	var out []fleet.Machine
	if err := d.callJSON(ctx, loc, []string{"machine", "list", "--json"}, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (d *Daemon) callMachine(ctx context.Context, loc api.Location, argv []string, stdin io.Reader, stdout io.Writer) error {
	if d.forwarder() == nil {
		return fmt.Errorf("machine %q: this machine's tunnel is not up", loc.Machine)
	}
	var stderr strings.Builder
	code, err := d.forwarder().Forward(ctx, loc, argv, stdin, stdout, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = fmt.Sprintf("exit %d", code)
		}
		return fmt.Errorf("%s on machine %s: %s", argv[0]+" "+argv[1], loc.Machine, detail)
	}
	return nil
}

func (d *Daemon) callJSON(ctx context.Context, loc api.Location, argv []string, req, out any) error {
	var stdin io.Reader
	if req != nil {
		b, err := json.Marshal(req)
		if err != nil {
			return fmt.Errorf("encode the request for machine %s: %w", loc.Machine, err)
		}
		stdin = strings.NewReader(string(b))
	}
	var buf strings.Builder
	if err := d.callMachine(ctx, loc, argv, stdin, &buf); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(buf.String()), out); err != nil {
		return fmt.Errorf("read machine %s's answer to %s: %w", loc.Machine, argv[1], err)
	}
	return nil
}

func (d *Daemon) announcer() env.Announcer {
	if !d.Config.IsMember() {
		st := storeFleetState{d.Store}
		return env.AnnouncerFunc(func(ctx context.Context, a fleet.Announcement) error {
			_, _, err := env.ApplyAnnouncement(ctx, st, a, d.now())
			return err
		})
	}
	return env.AnnouncerFunc(func(ctx context.Context, a fleet.Announcement) error {
		loc, err := d.hubLocation()
		if err != nil {
			return err
		}
		var res api.AnnounceResult
		return d.callJSON(ctx, loc, []string{"machine", "announce", "--json"}, a, &res)
	})
}

func (d *Daemon) remoteWorktree(ctx context.Context, mach, app, name string) (*env.WorktreeStatus, error) {
	loc, err := d.machineLocation(ctx, mach)
	if err != nil {
		return nil, err
	}
	if loc.Local {
		return d.EnvManager.WorktreeStatusOf(ctx, app, name)
	}
	var st env.WorktreeStatus
	argv := []string{"env", "worktree-status", name, "--app", app, "--json"}
	if err := d.callJSON(ctx, loc, argv, nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

type hubFetcher struct{ d *Daemon }

func (h *hubFetcher) Bundle(ctx context.Context, req vault.BundleRequest) (*vault.Bundle, error) {
	loc, err := h.d.hubLocation()
	if err != nil {
		return nil, err
	}
	argv := []string{"secrets", "bundle", "--app", req.App, "--json"}
	if req.Env != "" {
		argv = append(argv, "--env", req.Env)
	}
	if req.Reason != "" {
		argv = append(argv, "--reason", req.Reason)
	}
	var b vault.Bundle
	if err := h.d.callJSON(ctx, loc, argv, nil, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

type machinePuller struct{ d *Daemon }

func (p *machinePuller) PullImages(ctx context.Context, from string, req release.TransferRequest) (io.ReadCloser, error) {
	loc, err := p.d.machineLocation(ctx, from)
	if err != nil {
		return nil, err
	}
	if loc.Local {
		return nil, fmt.Errorf("copy release %d from %s: that machine is this one", req.Release, from)
	}
	argv := []string{"images", "send", "--release", fmt.Sprint(req.Release), "--arch", req.Arch}
	for _, ref := range req.Refs {
		argv = append(argv, "--ref", ref)
	}
	pr, pw := io.Pipe()
	go func() {
		err := p.d.callMachine(ctx, loc, argv, nil, pw)
		_ = pw.CloseWithError(err)
	}()
	return pr, nil
}

type memberFeeds struct{ d *Daemon }

func (m *memberFeeds) Follow(ctx context.Context, mach string) (io.ReadCloser, error) {
	loc, err := m.d.machineLocation(ctx, mach)
	if err != nil {
		return nil, err
	}
	if loc.Local {
		return nil, fmt.Errorf("follow %s: that machine is this one", mach)
	}
	pr, pw := io.Pipe()
	go func() {
		err := m.d.callMachine(ctx, loc, []string{"events", "--follow", "--json"}, nil, pw)
		_ = pw.CloseWithError(err)
	}()
	return pr, nil
}

const hubAPIForwardName = "hub/api"

func (d *Daemon) withHubAPI(ctx context.Context, fn func(addr string) error) error {
	dev := d.dev()
	if dev == nil {
		return errors.New("this machine's tunnel is not up, so the hub cannot be reached")
	}
	at, err := d.Config.HubAPIAddrPort()
	if err != nil {
		return fmt.Errorf("find the hub %s: %w", d.Config.Fleet.Hub.Name, err)
	}
	d.hubDoorMu.Lock()
	f, err := dev.AddForward(ctx, vpn.Forward{
		Name: hubAPIForwardName, Machine: d.Config.Fleet.Hub.Name, To: at,
	})
	if err != nil {
		d.hubDoorMu.Unlock()
		return fmt.Errorf("open the door to the hub %s: %w", d.Config.Fleet.Hub.Name, err)
	}
	d.hubDoorUsers++
	d.hubDoorMu.Unlock()

	defer func() {
		d.hubDoorMu.Lock()
		defer d.hubDoorMu.Unlock()
		if d.hubDoorUsers--; d.hubDoorUsers > 0 {
			return
		}

		if err := dev.RemoveForward(context.WithoutCancel(ctx), hubAPIForwardName); err != nil {
			d.logf("close the door to the hub %s: %v", d.Config.Fleet.Hub.Name, err)
		}
	}()
	return fn(f.Addr())
}

func (d *Daemon) mirror(ctx context.Context, app, branch string) error {
	m, err := d.envs()
	if err != nil {
		return err
	}
	mirror, ok := m.Git.(git.Mirrorer)
	if !ok {
		return errors.New("this machine's git driver cannot follow a mirror")
	}
	if c, ok := m.Git.(*git.CLI); ok && len(c.Env) == 0 {

		c.Env = []string{"GIT_SSH_COMMAND=" + git.TunnelSSHCommand}
	}
	repo := env.RepoPath(d.Config.DataDir, app)
	if err := d.ensureApp(ctx, app, repo); err != nil {
		return err
	}
	return d.withHubAPI(ctx, func(addr string) error {
		if _, err := mirror.EnsureMirror(ctx, repo, git.MirrorURL(addr, app)); err != nil {
			return fmt.Errorf("point %s at the hub: %w", repo, err)
		}
		if branch != "" {
			if err := mirror.FetchBranch(ctx, repo, branch); err == nil {
				return nil
			}

		}
		if err := mirror.FetchMirror(ctx, repo); err != nil {
			return fmt.Errorf("fetch %s from the hub: %w", app, err)
		}
		return nil
	})
}

const leftFleetSetting = "fleet.left"

func (d *Daemon) leaveFleet(ctx context.Context, hub string, logf func(string, ...any)) {
	d.leaveFleetIn(ctx, hub, 0, logf)
}

func (d *Daemon) leaveFleetIn(ctx context.Context, hub string, after time.Duration, logf func(string, ...any)) {
	if err := d.Store.SetSetting(ctx, leftFleetSetting, hub); err != nil {
		logf("fleet: record that %s removed this machine: %v", hub, err)
	}

	d.forgetFleetMachines(ctx, logf)
	drop := func() {
		dev := d.dev()
		if dev == nil {
			return
		}

		if err := dev.RemoveMachinePeer(context.WithoutCancel(ctx), hub); err != nil {
			logf("fleet: drop the peer %s: %v", hub, err)
		}
	}
	if after > 0 {
		time.AfterFunc(after, drop)
	} else {
		drop()
	}
	if d.EnvManager != nil {

		d.EnvManager.UpdateFleetWiring(func(w *env.FleetWiring) {
			w.Announce = nil
			w.Role = fleet.RoleHub
		})
	}
	logf("fleet: %s has removed this machine; it is a machine of one again. "+
		"Run `sudo caramelo machine leave` on it to take the fleet block out of %s",
		hub, serverconfig.Path(d.ConfigDir))
}

func (d *Daemon) forgetFleetMachines(ctx context.Context, logf func(string, ...any)) {
	rows, err := d.Store.FleetMachines(ctx)
	if err != nil {
		logf("fleet: read this machine's fleet rows to forget them: %v", err)
		return
	}
	for _, r := range rows {
		if err := d.Store.DeleteFleetMachine(ctx, r.Name); err != nil {
			logf("fleet: forget the machine %s: %v", r.Name, err)
		}
	}
}

func removedByHub(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "is not a machine of this fleet")
}

func (d *Daemon) leftFleet(ctx context.Context) bool {
	if !d.Config.IsMember() {
		d.forgetLeftFleet(ctx)
		return false
	}
	got, err := d.Store.Setting(ctx, leftFleetSetting)
	return err == nil && got == d.Config.Fleet.Hub.Name
}

func (d *Daemon) forgetLeftFleet(ctx context.Context) {
	got, err := d.Store.Setting(ctx, leftFleetSetting)
	if err != nil || got == "" {
		return
	}
	if err := d.Store.SetSetting(ctx, leftFleetSetting, ""); err != nil {
		d.logf("fleet: forget that %s had removed this machine: %v", got, err)
		return
	}
	d.logf("fleet: this machine no longer names a hub; forgot that %s had removed it", got)
}

func (d *Daemon) releaseOfTree(ctx context.Context, app, tree string) (*release.Release, error) {
	loc, err := d.hubLocation()
	if err != nil {
		return nil, err
	}
	var rel release.Release
	argv := []string{"images", "of", "--app", app, "--tree", tree, "--json"}
	if err := d.callJSON(ctx, loc, argv, nil, &rel); err != nil {
		if strings.Contains(err.Error(), "no release of tree") {
			return nil, nil
		}
		return nil, err
	}
	if rel.Tree == "" {
		return nil, nil
	}
	return &rel, nil
}

func (d *Daemon) reportRelease(ctx context.Context, rel *release.Release, images release.Images) error {
	if rel == nil {
		return nil
	}
	loc, err := d.hubLocation()
	if err != nil {
		return err
	}
	req := api.ReleaseRecordRequest{Release: *rel, Images: images}
	return d.callJSON(ctx, loc, []string{"images", "record", "--json"}, req, nil)
}

func (d *Daemon) adoptRelease(ctx context.Context, rel *release.Release) (*release.Release, error) {
	if rel == nil {
		return nil, nil
	}
	row, err := d.Store.ReleaseByTree(ctx, rel.App, rel.Tree)
	switch {
	case errors.Is(err, state.ErrNotFound):
		if row, err = d.Store.AddRelease(ctx, releaseRowOf(*rel)); err != nil {
			return nil, fmt.Errorf("record release %s of app %q here: %w", rel.Tree, rel.App, err)
		}
	case err != nil:
		return nil, fmt.Errorf("read the release of tree %s of app %q: %w", rel.Tree, rel.App, err)
	}
	return release.FromRecord(row)
}

func (d *Daemon) syncFleetNames(ctx context.Context) error {
	dev := d.dev()
	if dev == nil || d.Config.IsMember() {
		return nil
	}
	rows, err := d.Store.Directory(ctx, state.DirectoryFilter{})
	if err != nil {
		return fmt.Errorf("read the directory for the fleet's names: %w", err)
	}
	self := d.fleetName()
	out := make([]vpn.Address, 0, len(rows))
	for _, r := range rows {
		if r.Machine == "" || r.Machine == self || r.Address == "" {
			continue
		}
		ip, err := netip.ParseAddr(r.Address)
		if err != nil {
			continue
		}
		out = append(out, vpn.Address{
			IP: ip, Kind: vpn.KindEnv, Owner: r.App + "/" + r.Env,
			Names: []string{vpn.EnvHost(r.App, r.Env)},
		})
	}
	if err := dev.SetFleetNames(ctx, out); err != nil {
		return fmt.Errorf("publish the fleet's names: %w", err)
	}
	return nil
}
