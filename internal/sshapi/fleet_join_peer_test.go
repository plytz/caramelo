package sshapi

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/fleet"

	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vpn"
)

type peerDevice struct {
	vpn.Device
	mu       sync.Mutex
	peers    map[string]vpn.Peer
	machines map[string]bool
}

func newPeerDevice(names ...string) *peerDevice {
	d := &peerDevice{peers: map[string]vpn.Peer{}}
	for i, n := range names {
		d.peers[n] = vpn.Peer{Name: n, IP: netip.AddrFrom4([4]byte{10, 86, 0, byte(i + 2)})}
	}
	return d
}

func (d *peerDevice) Status(context.Context) (vpn.Status, error) {
	return vpn.Status{PublicKey: "self"}, nil
}

func (d *peerDevice) Peers(context.Context) ([]vpn.Peer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]vpn.Peer, 0, len(d.peers))
	for _, p := range d.peers {
		out = append(out, p)
	}
	return out, nil
}

func (d *peerDevice) RemovePeer(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.peers, name)
	return nil
}

func (d *peerDevice) RemoveMachinePeer(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.peers, name)
	delete(d.machines, name)
	return nil
}

func (d *peerDevice) RemoveForward(context.Context, string) error { return nil }

func (d *peerDevice) MachinePeers(context.Context) ([]vpn.MachinePeer, error) { return nil, nil }

func (d *peerDevice) hasMachine(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.machines[name]
}

func (d *peerDevice) AddMachinePeer(context.Context, vpn.MachinePeer) error { return nil }

func (d *peerDevice) Forwards(context.Context) ([]vpn.Forward, error) { return nil, nil }

func (d *peerDevice) AddForward(_ context.Context, f vpn.Forward) (vpn.Forward, error) { return f, nil }

func (d *peerDevice) SetFleetNames(context.Context, []vpn.Address) error { return nil }

func (d *peerDevice) has(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.peers[name]
	return ok
}

func TestAnExpiredTicketsBootstrapPeerIsRevoked(t *testing.T) {
	store := fleetStore(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	dev := newPeerDevice("join-aaaa", "join-bbbb", "laptop")
	d := &Daemon{Store: store, Device: dev, Now: func() time.Time { return now }}

	if err := store.AddJoinToken(ctx, state.JoinToken{
		Hash: "aaaa", CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("AddJoinToken: %v", err)
	}
	d.holdJoinPeer("join-aaaa", now.Add(-time.Hour))
	d.holdJoinPeer("join-bbbb", now.Add(time.Hour))

	d.expireJoinPeers(ctx)

	if dev.has("join-aaaa") {
		t.Error("the peer of an expired ticket is still admitted")
	}
	if !dev.has("join-bbbb") {
		t.Error("the peer of a live ticket was revoked")
	}
	if !dev.has("laptop") {
		t.Error("an ordinary peer was revoked")
	}
	if _, err := store.JoinToken(ctx, "aaaa"); err == nil {
		t.Error("the expired token row is still there")
	}

	d.retireJoinPeer("m1", "join-bbbb")
	d.holdJoinPeer("join-bbbb", now.Add(-time.Hour))
	d.expireJoinPeers(ctx)
	if !dev.has("join-bbbb") {
		t.Error("a bootstrap peer in flight was revoked out from under the join")
	}
}

func TestATicketThatJustExpiredCanStillBeToldSo(t *testing.T) {
	store := fleetStore(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	clock := now
	dev := newPeerDevice("join-cccc")
	d := &Daemon{Store: store, Device: dev, Now: func() time.Time { return clock }}

	expired := now.Add(-30 * time.Second)
	if err := store.AddJoinToken(ctx, state.JoinToken{
		Hash: "cccc", CreatedAt: now.Add(-time.Minute), ExpiresAt: expired,
	}); err != nil {
		t.Fatalf("AddJoinToken: %v", err)
	}
	d.holdJoinPeer("join-cccc", expired.Add(joinExpiryGrace))

	d.expireJoinPeers(ctx)
	if !dev.has("join-cccc") {
		t.Error("the peer went on the stroke of expiry; a join a second late meets silence")
	}
	row, err := store.JoinToken(ctx, "cccc")
	if err != nil {
		t.Fatalf("the row went with it, so the refusal cannot say \"expired\": %v", err)
	}
	tok := fleet.Token{Hash: row.Hash, ExpiresAt: row.ExpiresAt}
	if err := tok.Check(clock); !errors.Is(err, fleet.ErrTokenExpired) {
		t.Fatalf("the row judges the token %v, want %v", err, fleet.ErrTokenExpired)
	}

	clock = expired.Add(joinExpiryGrace + time.Second)
	d.expireJoinPeers(ctx)
	if dev.has("join-cccc") {
		t.Error("the peer of a ticket past its grace is still admitted")
	}
	if _, err := store.JoinToken(ctx, "cccc"); err == nil {
		t.Error("the row of a ticket past its grace is still there")
	}
}

func TestAMachineThatNamesNoHubForgetsItWasRemoved(t *testing.T) {
	store := fleetStore(t)
	ctx := context.Background()
	if err := store.SetSetting(ctx, leftFleetSetting, "hub1"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	memberCfg := serverconfig.Config{
		Fleet: serverconfig.Fleet{Role: "member", Name: "m1",
			Hub: serverconfig.FleetHub{Name: "hub1", PublicKey: "k", Endpoint: "hub:4021", Address: "10.86.0.1"}},
	}
	member := &Daemon{Store: store, Config: memberCfg}
	if !member.leftFleet(ctx) {
		t.Fatal("a member whose hub removed it should still know so")
	}

	left := &Daemon{Store: store, Config: serverconfig.Config{}}
	if left.leftFleet(ctx) {
		t.Fatal("a machine that names no hub cannot have been removed from one")
	}
	got, err := store.Setting(ctx, leftFleetSetting)
	if err != nil {
		t.Fatalf("Setting: %v", err)
	}
	if got != "" {
		t.Fatalf("fleet.left = %q, want it forgotten", got)
	}

	rejoined := &Daemon{Store: store, Config: memberCfg}
	if rejoined.leftFleet(ctx) {
		t.Fatal("a machine that joined again is still recorded as removed")
	}
}

func TestAMemberIsToldBeforeItsPeerIsDropped(t *testing.T) {
	store := fleetStore(t)
	ctx := context.Background()
	dev := newPeerDevice()
	fwd := &fakeForwarder{}
	d := &Daemon{
		Store: store, Device: dev, Forwarder: fwd,
		Resolver: &fakeResolver{machine: map[string]api.Location{"m1": remote("m1")}},
		Config: serverconfig.Config{Fleet: serverconfig.Fleet{
			Role: "hub", Name: "hub1", Range: "10.80.0.0/12"}},
		Now: func() time.Time { return time.Unix(1, 0) },
	}

	if err := d.MachineRemove(ctx, api.MachineRemoveRequest{Name: "m1", Force: true},
		io.Discard); err != nil {
		t.Fatalf("MachineRemove: %v", err)
	}
	if got := strings.Join(fwd.argv, " "); got != "machine removed --json" {
		t.Fatalf("argv = %q, want the member told over the peer it is about to lose", got)
	}
	if _, err := store.FleetMachine(ctx, "m1"); err == nil {
		t.Error("the machine's row is still there")
	}
}

func TestAToldMemberKeepsThePeerLongEnoughToAnswer(t *testing.T) {
	store := fleetStore(t)
	ctx := context.Background()
	dev := newPeerDevice()
	dev.machines = map[string]bool{"hub1": true}
	member := &Daemon{
		Store: store, Device: dev, KnownMachines: machines("hub1"),
		Config: serverconfig.Config{Fleet: serverconfig.Fleet{
			Role: "member", Name: "m1",
			Hub: serverconfig.FleetHub{Name: "hub1", PublicKey: "k", Endpoint: "hub:4021", Address: "10.86.0.1"}}},
	}
	told := WithSession(ctx, api.Session{Transport: "tunnel", Identity: "hub1", Peer: "hub1"})
	if err := member.MachineRemoved(told); err != nil {
		t.Fatalf("MachineRemoved: %v", err)
	}
	if !dev.hasMachine("hub1") {
		t.Error("the peer the answer travels through was dropped inside the call")
	}

	if got, _ := store.Setting(ctx, leftFleetSetting); got != "hub1" {
		t.Errorf("fleet.left = %q, want the hub recorded before anything else", got)
	}

	deadline := time.Now().Add(10 * time.Second)
	for dev.hasMachine("hub1") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if dev.hasMachine("hub1") {
		t.Error("the peer was kept for good; the grace is a moment, not a reprieve")
	}
}

func TestARemovedMemberListsItselfAndNobodyElse(t *testing.T) {
	store := fleetStore(t)
	ctx := context.Background()
	for _, m := range []state.MachineRow{
		{Name: "hub1", Role: "hub", PublicKey: "hk", Subnet: "10.86.0.0/16"},
		{Name: "m1", Role: "member", PublicKey: "mk", Subnet: "10.80.0.0/16"},
	} {
		if err := store.PutFleetMachine(ctx, m); err != nil {
			t.Fatalf("write the machine %s: %v", m.Name, err)
		}
	}
	dev := newPeerDevice()
	dev.machines = map[string]bool{"hub1": true}
	member := &Daemon{
		Store: store, Device: dev, KnownMachines: machines("hub1"),
		Config: serverconfig.Config{Fleet: serverconfig.Fleet{
			Role: "member", Name: "m1", Subnet: "10.80.0.0/16",
			Hub: serverconfig.FleetHub{Name: "hub1", PublicKey: "k", Endpoint: "hub:4021", Address: "10.86.0.1"}}},
	}
	told := WithSession(ctx, api.Session{Transport: "tunnel", Identity: "hub1", Peer: "hub1"})
	if err := member.MachineRemoved(told); err != nil {
		t.Fatalf("MachineRemoved: %v", err)
	}

	rows, err := member.Machines(ctx)
	if err != nil {
		t.Fatalf("machine list on a removed member: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("machine list = %d rows, want only its own: %+v", len(rows), rows)
	}
	if rows[0].Name != "m1" {
		t.Errorf("the one row is %q, want this machine", rows[0].Name)
	}
	if !rows[0].Role.IsHub() {
		t.Errorf("a removed machine still answers as %q, want a hub of one", rows[0].Role)
	}
}

func TestOnlyAMachinesOwnHubMayRetireIt(t *testing.T) {
	member := &Daemon{
		Store:         fleetStore(t),
		KnownMachines: machines("hub1", "m2"),
		Config: serverconfig.Config{Fleet: serverconfig.Fleet{
			Role: "member", Name: "m1",
			Hub: serverconfig.FleetHub{Name: "hub1", PublicKey: "k", Endpoint: "hub:4021", Address: "10.86.0.1"}}},
	}

	other := WithSession(context.Background(), api.Session{Transport: "tunnel", Identity: "m2", Peer: "m2"})
	if err := member.MachineRemoved(other); err == nil ||
		!strings.Contains(err.Error(), "not this machine's hub") {
		t.Fatalf("error = %v, want a refusal naming the hub", err)
	}

	commander := WithSession(context.Background(), api.Session{Transport: "tunnel", Identity: "commander"})
	if err := member.MachineRemoved(commander); err == nil ||
		!strings.Contains(err.Error(), "machine remove") {
		t.Fatalf("error = %v, want the command a person runs instead", err)
	}
}

func TestTheFleetArrivesWhileCommandsAreReadingIt(t *testing.T) {
	store := fleetStore(t)
	ctx := context.Background()
	m := &env.Manager{Store: store}
	d := &Daemon{Store: store, EnvManager: m, Background: ctx,
		Now: func() time.Time { return time.Unix(1, 0) }}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 500; n++ {

				_ = d.resolver()
				_ = d.forwarder()
				_ = d.machineLookup()
				w := m.FleetWiringOf()
				_, _ = w.Machine, w.Fleet
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.fleetBegan(ctx)
		}()
	}
	wg.Wait()

	if d.resolver() == nil {
		t.Fatal("the fleet began and left no resolver")
	}
	if got := m.FleetWiringOf().Fleet; got == nil {
		t.Fatal("the environment manager was not given the directory")
	}
}

type handshakeDevice struct {
	vpn.Device
	peers []vpn.MachinePeer
}

func (d *handshakeDevice) MachinePeers(context.Context) ([]vpn.MachinePeer, error) {
	return d.peers, nil
}

func TestAHandshakeIsMatchedByKeyAndMovesTheEndpoint(t *testing.T) {
	store := fleetStore(t)
	ctx := context.Background()
	if err := store.SetMachineSeen(ctx, "m1", "192.168.0.74:4021", time.Unix(1_000, 0)); err != nil {
		t.Fatalf("SetMachineSeen: %v", err)
	}
	shook := time.Unix(2_000, 0)
	d := &Daemon{Store: store, Device: &handshakeDevice{peers: []vpn.MachinePeer{{
		Name: "m1", PublicKey: "m1-key", LastHandshake: shook, SeenAt: "192.168.0.91:4021",
	}}}}

	got, err := d.Machines(ctx)
	if err != nil {
		t.Fatalf("Machines: %v", err)
	}
	m, ok := fleet.Find(got, "m1")
	if !ok {
		t.Fatalf("machines = %+v, want m1", got)
	}
	if !m.LastSeen.Equal(shook) {
		t.Errorf("last_seen = %v, want the device's handshake %v", m.LastSeen, shook)
	}
	if m.Endpoint != "192.168.0.91:4021" {
		t.Errorf("endpoint = %q, want the address the handshake came from", m.Endpoint)
	}

	row, err := store.FleetMachine(ctx, "m1")
	if err != nil {
		t.Fatalf("FleetMachine: %v", err)
	}
	if row.Endpoint != "192.168.0.91:4021" || !row.LastSeen.Equal(shook) {
		t.Errorf("row = %+v, want the moved lease recorded", row)
	}
}

func TestCreatingANameAnotherMachineHoldsGoesThere(t *testing.T) {
	fwd := &fakeForwarder{}
	m := (&env.Manager{}).WithFleet(env.FleetWiring{Machine: "hub"})
	d := &Daemon{
		EnvManager: m, Forwarder: fwd,
		Resolver: &fakeResolver{env: map[string]api.Location{"shop/feat-x": remote("m1")}},
	}
	ctx, _ := fleetSession(t, "env", "create", "feat-x", "--app", "shop", "--json")

	req := env.CreateRequest{App: "shop", Name: "feat-x"}
	err := d.placeAndForward(ctx, m, &req, io.Discard)
	var fe *api.ForwardedError
	if !errors.As(err, &fe) || fe.Machine != "m1" {
		t.Fatalf("error = %v, want it answered by m1", err)
	}
	if got := strings.Join(fwd.argv, " "); !strings.Contains(got, "--on m1") {
		t.Fatalf("argv = %q, want the create sent to the machine that holds the name", got)
	}
}

func TestCreatingANameAnotherMachineHoldsSomewhereElseIsRefused(t *testing.T) {
	m := (&env.Manager{}).WithFleet(env.FleetWiring{Machine: "hub"})
	d := &Daemon{
		EnvManager: m, Forwarder: &fakeForwarder{},
		Resolver: &fakeResolver{env: map[string]api.Location{"shop/feat-x": remote("m1")}},
	}
	ctx, _ := fleetSession(t, "env", "create", "feat-x", "--app", "shop", "--on", "m2", "--json")

	req := env.CreateRequest{App: "shop", Name: "feat-x", On: "m2"}
	err := d.placeAndForward(ctx, m, &req, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "already exists on machine m1") {
		t.Fatalf("error = %v, want a refusal naming the machine that holds it", err)
	}
}

type doorDevice struct {
	vpn.Device
	mu   sync.Mutex
	open map[string]bool
	adds int
}

func (d *doorDevice) AddForward(_ context.Context, f vpn.Forward) (vpn.Forward, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.open == nil {
		d.open = map[string]bool{}
	}
	d.open[f.Name] = true
	d.adds++
	f.Port = 39000
	return f, nil
}

func (d *doorDevice) RemoveForward(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.open, name)
	return nil
}

func (d *doorDevice) isOpen(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.open[name]
}

func TestTheDoorToTheHubIsShutWhenTheFetchIsDone(t *testing.T) {
	dev := &doorDevice{}
	d := &Daemon{Device: dev, Config: serverconfig.Config{
		SSHPort: 4022,
		Fleet: serverconfig.Fleet{
			Role: "member", Name: "m1",
			Hub: serverconfig.FleetHub{Name: "hub1", PublicKey: "k", Endpoint: "hub:4021", Address: "10.86.0.1"},
		},
	}}

	var inside bool
	if err := d.withHubAPI(context.Background(), func(addr string) error {
		inside = dev.isOpen(hubAPIForwardName)
		if addr == "" {
			t.Error("the door opened onto no address")
		}
		return nil
	}); err != nil {
		t.Fatalf("withHubAPI: %v", err)
	}
	if !inside {
		t.Error("the door was not open while the fetch was running")
	}
	if dev.isOpen(hubAPIForwardName) {
		t.Error("the door to the hub is still open after the fetch returned")
	}

	started, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = d.withHubAPI(context.Background(), func(string) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	if err := d.withHubAPI(context.Background(), func(string) error { return nil }); err != nil {
		t.Fatalf("the second fetch: %v", err)
	}
	if !dev.isOpen(hubAPIForwardName) {
		t.Error("the second fetch closed the door the first was still using")
	}
	close(release)
	wg.Wait()
	if dev.isOpen(hubAPIForwardName) {
		t.Error("the door stayed open after the last fetch returned")
	}
}

func TestForcedRemovalDoesNotDependOnReachingTheMachine(t *testing.T) {
	store := fleetStore(t)
	ctx := context.Background()
	if err := store.PutFleetMachine(ctx, state.MachineRow{
		Name: "m2", Role: "member", PublicKey: "k2", Subnet: "10.81.0.0/16",
	}); err != nil {
		t.Fatalf("write the machine: %v", err)
	}
	if err := store.PutDirectoryEntry(ctx, state.DirectoryRow{
		App: "shop", Env: "feat-p", Machine: "m2", Address: "10.81.0.1",
		Owner: "commander", Mode: "dev", UpdatedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatalf("write the directory row: %v", err)
	}
	d := &Daemon{
		Store:    store,
		Device:   newPeerDevice("m2"),
		Resolver: &fakeResolver{machine: map[string]api.Location{"m2": remote("m2")}},
		Forwarder: &fakeForwarder{
			err: errors.New("dial tcp 10.81.0.1:4022 in the tunnel: context deadline exceeded"),
		},
		Config: serverconfig.Config{Fleet: serverconfig.Fleet{Role: "hub", Name: "hub1"}},
	}

	if err := d.MachineRemove(ctx, api.MachineRemoveRequest{Name: "m2", Force: true}, io.Discard); err != nil {
		t.Fatalf("machine remove --force of a machine that will not answer: %v", err)
	}
	if _, err := store.FleetMachine(ctx, "m2"); err == nil {
		t.Error("the machine is still in the fleet")
	}
	if rows, err := store.Directory(ctx, state.DirectoryFilter{Machine: "m2"}); err != nil || len(rows) != 0 {
		t.Errorf("the directory still holds %d row(s) on the machine that left (%v)", len(rows), err)
	}
}

func TestARemovalForgetsTheMachineBeforeItsEnvironments(t *testing.T) {
	store := fleetStore(t)
	ctx := context.Background()
	if err := store.PutFleetMachine(ctx, state.MachineRow{
		Name: "m2", Role: "member", PublicKey: "k2", Subnet: "10.81.0.0/16",
	}); err != nil {
		t.Fatalf("write the machine: %v", err)
	}
	if err := store.PutDirectoryEntry(ctx, state.DirectoryRow{
		App: "shop", Env: "feat-p", Machine: "m2", Address: "10.81.0.1",
		Owner: "commander", Mode: "dev", UpdatedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatalf("write the directory row: %v", err)
	}
	d := &Daemon{
		Store:    store,
		Device:   newPeerDevice("m2"),
		Resolver: &fakeResolver{machine: map[string]api.Location{"m2": remote("m2")}},
		Config:   serverconfig.Config{Fleet: serverconfig.Fleet{Role: "hub", Name: "hub1"}},
	}
	d.KnownMachines = storeMachines{store}

	announced := make(chan error, 1)
	d.Forwarder = forwarderFunc(func(ctx context.Context, _ api.Location, _ []string,
		_ io.Reader, _, _ io.Writer) (int, error) {
		_, err := d.requireMachinePeer(WithSession(ctx, api.Session{
			Transport: "tunnel", Identity: "m2", Peer: "m2",
		}), "an announcement", "machine list")
		select {
		case announced <- err:
		default:
		}
		return 0, nil
	})

	if err := d.MachineRemove(ctx, api.MachineRemoveRequest{Name: "m2", Force: true}, io.Discard); err != nil {
		t.Fatalf("machine remove --force: %v", err)
	}
	select {
	case err := <-announced:
		if err == nil {
			t.Error("an announcement made in the middle of a removal was accepted")
		}
	default:
		t.Fatal("the removal never reached the machine, so the race was not exercised")
	}
	rows, err := store.Directory(ctx, state.DirectoryFilter{Machine: "m2"})
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("the directory still holds %d row(s) on the machine that left", len(rows))
	}
}

type forwarderFunc func(ctx context.Context, loc api.Location, argv []string,
	stdin io.Reader, stdout, stderr io.Writer) (int, error)

func (f forwarderFunc) Forward(ctx context.Context, loc api.Location, argv []string,
	stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	return f(ctx, loc, argv, stdin, stdout, stderr)
}
