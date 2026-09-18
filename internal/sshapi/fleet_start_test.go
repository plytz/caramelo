package sshapi

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/vpn"
)

type machinePeerDevice struct {
	vpn.Device
	mu    sync.Mutex
	peers map[string]vpn.MachinePeer
}

func newMachinePeerDevice() *machinePeerDevice {
	return &machinePeerDevice{peers: map[string]vpn.MachinePeer{}}
}

func (d *machinePeerDevice) Status(context.Context) (vpn.Status, error) {
	return vpn.Status{PublicKey: "m1-key"}, nil
}

func (d *machinePeerDevice) MachinePeers(context.Context) ([]vpn.MachinePeer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]vpn.MachinePeer, 0, len(d.peers))
	for _, p := range d.peers {
		out = append(out, p)
	}
	return out, nil
}

func (d *machinePeerDevice) AddMachinePeer(_ context.Context, p vpn.MachinePeer) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.peers[p.Name] = p
	return nil
}

func (d *machinePeerDevice) RemoveMachinePeer(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.peers, name)
	return nil
}

func (d *machinePeerDevice) peer(name string) (vpn.MachinePeer, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, ok := d.peers[name]
	return p, ok
}

func aMemberOfAFleetNamedApart(t *testing.T) (*Daemon, *machinePeerDevice) {
	t.Helper()
	priv, err := vpn.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := priv.Public()
	if err != nil {
		t.Fatal(err)
	}
	cfg := serverconfig.Default()
	cfg.Name, cfg.Role, cfg.Hub = "m1", serverconfig.RoleMember, serverconfig.Hub{}
	cfg.VPNSubnet = "10.87.0.0/16"
	cfg.Member = serverconfig.Member{
		Fleet: "home", Subnet: "10.87.0.0/16",
		Hub: serverconfig.MemberHub{
			Name: "box", Endpoint: "hub.example.com:4021",
			Address: "10.86.0.1", PublicKey: pub.Base64(),
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the member fixture does not validate: %v", err)
	}
	dev := newMachinePeerDevice()
	d := &Daemon{
		Store: fleetStore(t), Device: dev, Config: cfg,
		Now: func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	return d, dev
}

func TestAMemberRecordsItsHubUnderTheHubsOwnNameNotTheFleets(t *testing.T) {
	ctx := context.Background()
	d, _ := aMemberOfAFleetNamedApart(t)

	if err := d.recordOwnFleet(ctx); err != nil {
		t.Fatalf("recordOwnFleet: %v", err)
	}
	row, err := d.Store.FleetMachine(ctx, "box")
	if err != nil {
		t.Fatalf("the hub's row is not under the name the hub answers to: %v", err)
	}
	if row.Role != "hub" || row.PublicKey != d.Config.Member.Hub.PublicKey || row.Endpoint != "hub.example.com:4021" {
		t.Errorf("the hub's row is %+v, want the hub's role, key and endpoint", row)
	}
	if _, err := d.Store.FleetMachine(ctx, "home"); err == nil {
		t.Error("the member recorded a machine called home, which is the fleet and not a machine")
	}
	loc, err := d.hubLocation()
	if err != nil {
		t.Fatalf("hubLocation: %v", err)
	}
	if loc.Machine != "box" {
		t.Errorf("a call to the hub is labelled %q, want box: the fleet has that name, the machine this one", loc.Machine)
	}
}

func TestAMemberPeersWithItsHubUnderTheHubsOwnName(t *testing.T) {
	ctx := context.Background()
	d, dev := aMemberOfAFleetNamedApart(t)

	if err := d.syncMachinePeers(ctx); err != nil {
		t.Fatalf("syncMachinePeers: %v", err)
	}
	p, ok := dev.peer("box")
	if !ok {
		t.Fatalf("no peer named box: %v", dev.peers)
	}
	if p.PublicKey != d.Config.Member.Hub.PublicKey || p.Endpoint != "hub.example.com:4021" {
		t.Errorf("the hub peer is %+v, want the hub's key and endpoint", p)
	}
	if !p.Holds(netip.MustParseAddr("10.86.0.1")) {
		t.Errorf("the hub peer holds %v, want the fleet's range", p.AllowedIPs)
	}
	if _, ok := dev.peer("home"); ok {
		t.Error("the member peered with a machine called home, which is the fleet and not a machine")
	}
}

func TestALeftFleetIsRememberedByTheFleetsNameAndDropsTheHubsPeer(t *testing.T) {
	ctx := context.Background()
	d, dev := aMemberOfAFleetNamedApart(t)
	if err := d.syncMachinePeers(ctx); err != nil {
		t.Fatalf("syncMachinePeers: %v", err)
	}

	d.leaveFleet(ctx, d.Config.FleetName(), func(string, ...any) {})

	if got, err := d.Store.Setting(ctx, leftFleetSetting); err != nil || got != "home" {
		t.Errorf("the setting says %q (%v), want the fleet home", got, err)
	}
	if !d.leftFleet(ctx) {
		t.Error("a member that was removed does not read itself as removed")
	}
	if _, ok := dev.peer("box"); ok {
		t.Error("the hub's peer survived the removal")
	}
}
