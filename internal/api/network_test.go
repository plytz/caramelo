package api

import (
	"context"
	"errors"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vpn"
)

type fakeStore struct {
	mu        sync.Mutex
	peers     []state.Peer
	envs      []state.EnvRecord
	vpn       *state.VPN
	addErr    error
	keyErr    error
	setVPNErr error

	onAdd func()
}

func (s *fakeStore) Peers(ctx context.Context) ([]state.Peer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]state.Peer(nil), s.peers...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *fakeStore) Peer(ctx context.Context, name string) (*state.Peer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.peers {
		if s.peers[i].Name == name {
			p := s.peers[i]
			return &p, nil
		}
	}
	return nil, state.ErrNotFound
}

func (s *fakeStore) AddPeer(ctx context.Context, p state.Peer) error {
	if s.onAdd != nil {
		add := s.onAdd
		s.onAdd = nil
		add()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.addErr; err != nil {
		s.addErr = nil
		return err
	}
	for _, e := range s.peers {
		if e.Name == p.Name || e.PublicKey == p.PublicKey || e.IP == p.IP {
			return state.ErrExists
		}
	}
	s.peers = append(s.peers, p)
	return nil
}

func (s *fakeStore) SetPeerKey(ctx context.Context, name, publicKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.keyErr; err != nil {
		s.keyErr = nil
		return err
	}
	for _, e := range s.peers {
		if e.PublicKey == publicKey && e.Name != name {
			return state.ErrExists
		}
	}
	for i := range s.peers {
		if s.peers[i].Name == name {
			s.peers[i].PublicKey = publicKey
			return nil
		}
	}
	return state.ErrNotFound
}

func (s *fakeStore) RemovePeer(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.peers {
		if s.peers[i].Name == name {
			s.peers = append(s.peers[:i], s.peers[i+1:]...)
			return nil
		}
	}
	return state.ErrNotFound
}

func (s *fakeStore) SetPeerHandshake(ctx context.Context, name string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.peers {
		if s.peers[i].Name == name {
			s.peers[i].LastHandshake = at
		}
	}
	return nil
}

func (s *fakeStore) EnvVPNIP(ctx context.Context, envID int64) (string, error) {
	return "", state.ErrNotFound
}

func (s *fakeStore) SetEnvVPNIP(ctx context.Context, envID int64, ip string) error {
	return state.ErrNotFound
}

func (s *fakeStore) TakenVPNIPs(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, p := range s.peers {
		out = append(out, p.IP)
	}
	for _, e := range s.envs {
		if e.VPNIP != "" {
			out = append(out, e.VPNIP)
		}
	}
	return out, nil
}

func (s *fakeStore) VPN(ctx context.Context) (*state.VPN, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vpn == nil {
		return nil, state.ErrNotFound
	}
	v := *s.vpn
	return &v, nil
}

func (s *fakeStore) SetVPN(ctx context.Context, v state.VPN) error {
	if s.setVPNErr != nil {
		return s.setVPNErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vpn = &v
	return nil
}

func (s *fakeStore) Envs(ctx context.Context, app string) ([]state.EnvRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]state.EnvRecord(nil), s.envs...), nil
}

type fakeDevice struct {
	vpn.Device

	mu      sync.Mutex
	peers   map[string]vpn.Peer
	up      bool
	addErr  error
	rmErr   error
	upErr   error
	status  vpn.Status
	stErr   error
	removed []string
	addrs   []vpn.Address
}

func newDevice() *fakeDevice {
	return &fakeDevice{
		peers: map[string]vpn.Peer{},
		status: vpn.Status{
			Up:        true,
			PublicKey: "bWFjaGluZS1rZXktYmFzZTY0LXBhZGRpbmctaGVyZS0xMjM0NTY=",
			Listen:    "0.0.0.0:4021",
			Subnet:    netip.MustParsePrefix("10.86.0.0/16"),
			IP:        netip.MustParseAddr("10.86.0.1"),
			Resolver:  "10.86.0.1:53",
			Routes:    3,
		},
	}
}

func (d *fakeDevice) AddPeer(ctx context.Context, p vpn.Peer) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.addErr; err != nil {
		d.addErr = nil
		return err
	}
	d.peers[p.Name] = p
	return nil
}

func (d *fakeDevice) RemovePeer(ctx context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.rmErr != nil {
		return d.rmErr
	}
	d.removed = append(d.removed, name)
	delete(d.peers, name)
	return nil
}

func (d *fakeDevice) AddAddress(ctx context.Context, a vpn.Address) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addrs = append(d.addrs, a)
	return nil
}

func (d *fakeDevice) Peers(ctx context.Context) ([]vpn.Peer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]vpn.Peer, 0, len(d.peers))
	for _, p := range d.peers {
		out = append(out, p)
	}
	return out, nil
}

func (d *fakeDevice) Up(ctx context.Context) error {
	if d.upErr != nil {
		return d.upErr
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.up = true
	return nil
}

func (d *fakeDevice) Down(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.up = false
	return nil
}

func (d *fakeDevice) Status(ctx context.Context) (vpn.Status, error) {
	if d.stErr != nil {
		return vpn.Status{}, d.stErr
	}
	return d.status, nil
}

func (d *fakeDevice) peer(name string) (vpn.Peer, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, ok := d.peers[name]
	return p, ok
}

func newNetwork(t *testing.T) (*Network, *fakeStore, *fakeDevice) {
	t.Helper()
	store := &fakeStore{}
	dev := newDevice()
	subnet := netip.MustParsePrefix(vpn.DefaultSubnet)
	n := &Network{
		Store:     store,
		Device:    dev,
		Alloc:     vpn.NewAllocator(subnet, store),
		Subnet:    subnet,
		KeyPath:   "/var/lib/caramelo/vpn/private.key",
		Listen:    vpn.DefaultListen,
		APIListen: "both",
		Now:       func() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) },
		Hostname:  "box",
	}
	return n, store, dev
}

func key(t *testing.T, seed byte) string {
	t.Helper()
	var k vpn.Key
	for i := range k {
		k[i] = seed + byte(i)
	}
	return k.Base64()
}

func TestAddPeerAllocatesAnAddressAndInstallsIt(t *testing.T) {
	n, store, dev := newNetwork(t)
	ctx := env.WithIdentity(context.Background(), "laptop")

	p, err := n.AddPeer(ctx, "agent-7", key(t, 1))
	if err != nil {
		t.Fatalf("AddPeer: %v", err)
	}

	if p.IP != "10.86.0.2" {
		t.Errorf("address = %q, want 10.86.0.2", p.IP)
	}
	if p.AddedBy != "laptop" {
		t.Errorf("added_by = %q, want the caller's identity", p.AddedBy)
	}
	if got, ok := dev.peer("agent-7"); !ok || got.IP.String() != "10.86.0.2" {
		t.Errorf("the peer was not installed on the device: %+v, %v", got, ok)
	}
	if rows, _ := store.Peers(ctx); len(rows) != 1 {
		t.Errorf("peer rows = %+v", rows)
	}

	q, err := n.AddPeer(ctx, "ci", key(t, 9))
	if err != nil || q.IP != "10.86.0.3" {
		t.Fatalf("second peer: %+v, %v", q, err)
	}
}

func TestAddPeerIsIdempotentAndRotatesAKey(t *testing.T) {
	n, _, dev := newNetwork(t)
	ctx := context.Background()

	first, err := n.AddPeer(ctx, "agent-7", key(t, 1))
	if err != nil {
		t.Fatal(err)
	}

	again, err := n.AddPeer(ctx, "agent-7", key(t, 1))
	if err != nil {
		t.Fatalf("re-adding the same peer: %v", err)
	}
	if again.IP != first.IP || again.PublicKey != first.PublicKey {
		t.Errorf("re-adding changed the peer: %+v, was %+v", again, first)
	}

	rotated, err := n.AddPeer(ctx, "agent-7", key(t, 33))
	if err != nil {
		t.Fatalf("rotating a key: %v", err)
	}
	if rotated.IP != first.IP {
		t.Errorf("the address moved on a rotation: %s, was %s", rotated.IP, first.IP)
	}
	if rotated.PublicKey != key(t, 33) {
		t.Errorf("the key was not rotated: %s", rotated.PublicKey)
	}
	if got, _ := dev.peer("agent-7"); got.PublicKey != key(t, 33) {
		t.Errorf("the device still holds the old key: %s", got.PublicKey)
	}
}

func TestAddPeerRefusesAKeyThatIsAlreadyRegistered(t *testing.T) {
	n, _, _ := newNetwork(t)
	ctx := context.Background()
	if _, err := n.AddPeer(ctx, "agent-7", key(t, 1)); err != nil {
		t.Fatal(err)
	}
	_, err := n.AddPeer(ctx, "laptop", key(t, 1))
	if err == nil || !strings.Contains(err.Error(), "agent-7") {
		t.Fatalf("err = %v, want it to name the peer already holding the key", err)
	}
}

func TestAddPeerRefusesBadNamesAndKeys(t *testing.T) {
	n, _, _ := newNetwork(t)
	ctx := context.Background()
	for _, tc := range []struct{ name, key string }{
		{"", key(t, 1)},
		{"Agent 7", key(t, 1)},
		{"agent-7", ""},
		{"agent-7", "not-a-key"},

		{"agent-7", strings.Repeat("A", 43) + "="},
	} {
		if _, err := n.AddPeer(ctx, tc.name, tc.key); err == nil {
			t.Errorf("AddPeer(%q, %q) was accepted", tc.name, tc.key)
		}
	}
}

func TestAddPeerRollsBackTheRowWhenTheDeviceRefuses(t *testing.T) {
	n, store, dev := newNetwork(t)
	ctx := context.Background()
	dev.addErr = errors.New("device is down")

	if _, err := n.AddPeer(ctx, "agent-7", key(t, 1)); err == nil {
		t.Fatal("AddPeer succeeded although the device refused")
	}

	rows, _ := store.Peers(ctx)
	if len(rows) != 0 {
		t.Errorf("peer rows after a failed add: %+v", rows)
	}
}

func TestAddPeerRetriesWhenTheAddressWasTakenBetweenTheTwoSteps(t *testing.T) {
	n, store, _ := newNetwork(t)
	ctx := context.Background()

	store.addErr = state.ErrExists
	store.peers = append(store.peers, state.Peer{Name: "other", PublicKey: key(t, 50), IP: "10.86.0.2"})

	p, err := n.AddPeer(ctx, "agent-7", key(t, 1))
	if err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	if p.IP != "10.86.0.3" {
		t.Errorf("address = %s, want the next free one", p.IP)
	}
}

func TestPeersCarryTheLiveHandshake(t *testing.T) {
	n, store, dev := newNetwork(t)
	ctx := context.Background()
	if _, err := n.AddPeer(ctx, "agent-7", key(t, 1)); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 8, 13, 0, 0, 0, time.UTC)
	p, _ := dev.peer("agent-7")
	p.LastHandshake = at
	dev.peers["agent-7"] = p

	got, err := n.Peers(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("Peers = %+v, %v", got, err)
	}
	if !got[0].LastHandshake.Equal(at) {
		t.Errorf("last handshake = %v, want %v", got[0].LastHandshake, at)
	}

	rows, _ := store.Peers(ctx)
	if !rows[0].LastHandshake.Equal(at) {
		t.Errorf("the handshake was not recorded: %v", rows[0].LastHandshake)
	}
}

func TestRemovePeerRevokesBeforeForgetting(t *testing.T) {
	n, store, dev := newNetwork(t)
	ctx := context.Background()
	if _, err := n.AddPeer(ctx, "agent-7", key(t, 1)); err != nil {
		t.Fatal(err)
	}
	if err := n.RemovePeer(ctx, "agent-7"); err != nil {
		t.Fatalf("RemovePeer: %v", err)
	}
	if _, ok := dev.peer("agent-7"); ok {
		t.Error("the peer is still on the device")
	}
	if rows, _ := store.Peers(ctx); len(rows) != 0 {
		t.Errorf("peer rows = %+v", rows)
	}

	p, err := n.AddPeer(ctx, "laptop", key(t, 70))
	if err != nil || p.IP != "10.86.0.2" {
		t.Errorf("the address was not freed: %+v, %v", p, err)
	}

	if err := n.RemovePeer(ctx, "nobody"); err == nil {
		t.Error("removing an unknown peer succeeded")
	}
}

func TestRemovePeerKeepsTheRowWhenTheDeviceRefuses(t *testing.T) {
	n, store, dev := newNetwork(t)
	ctx := context.Background()
	if _, err := n.AddPeer(ctx, "agent-7", key(t, 1)); err != nil {
		t.Fatal(err)
	}
	dev.rmErr = errors.New("device is down")
	if err := n.RemovePeer(ctx, "agent-7"); err == nil {
		t.Fatal("RemovePeer succeeded although the device refused")
	}

	if rows, _ := store.Peers(ctx); len(rows) != 1 {
		t.Errorf("peer rows = %+v", rows)
	}
}

func TestVPNStatus(t *testing.T) {
	n, store, _ := newNetwork(t)
	ctx := context.Background()
	if _, err := n.AddPeer(ctx, "agent-7", key(t, 1)); err != nil {
		t.Fatal(err)
	}
	store.envs = []state.EnvRecord{{Name: "feat-x", VPNIP: "10.86.1.1"}, {Name: "feat-y"}}

	st, err := n.VPNStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || st.Address != "10.86.0.1" || st.Subnet != "10.86.0.0/16" {
		t.Errorf("status = %+v", st)
	}
	if st.Resolver != "10.86.0.1:53" || st.Listen != "0.0.0.0:4021" || st.APIListen != "both" {
		t.Errorf("status = %+v", st)
	}
	if st.Peers != 1 || st.Envs != 1 || st.Routes != 3 {
		t.Errorf("counts = %d peers, %d envs, %d routes", st.Peers, st.Envs, st.Routes)
	}
	if st.PublicKey == "" {
		t.Error("status carries no public key: a peer cannot configure itself without it")
	}
}

func TestVPNStatusOnAMachineWithoutANetwork(t *testing.T) {
	n := &Network{Store: &fakeStore{}, Disabled: "setup predates M5"}
	st, err := n.VPNStatus(context.Background())
	if err != nil {
		t.Fatalf("VPNStatus must answer even with no device: %v", err)
	}
	if st.Enabled {
		t.Error("a machine with no device says its network is enabled")
	}
	if !strings.Contains(st.Error, "predates") {
		t.Errorf("error = %q, want it to say why", st.Error)
	}

	if _, err := n.AddPeer(context.Background(), "agent-7", key(t, 1)); !errors.Is(err, ErrNoNetwork) {
		t.Errorf("AddPeer = %v, want ErrNoNetwork", err)
	}
	if _, err := n.Peers(context.Background()); !errors.Is(err, ErrNoNetwork) {
		t.Errorf("Peers = %v, want ErrNoNetwork", err)
	}
	if err := n.RemovePeer(context.Background(), "agent-7"); !errors.Is(err, ErrNoNetwork) {
		t.Errorf("RemovePeer = %v, want ErrNoNetwork", err)
	}
}

func TestStartRebuildsTheTablesFromTheRows(t *testing.T) {
	n, store, dev := newNetwork(t)
	ctx := context.Background()
	store.peers = []state.Peer{
		{Name: "agent-7", PublicKey: key(t, 1), IP: "10.86.0.2"},
		{Name: "laptop", PublicKey: key(t, 40), IP: "10.86.0.3"},
	}
	published := false
	n.Publish = func(context.Context) error { published = true; return nil }

	if err := n.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, name := range []string{"agent-7", "laptop"} {
		if _, ok := dev.peer(name); !ok {
			t.Errorf("peer %q was not installed at start", name)
		}
	}
	if !dev.up {
		t.Error("the device was not brought up")
	}
	if !published {
		t.Error("the environments were not published")
	}

	v, err := store.VPN(ctx)
	if err != nil {
		t.Fatalf("vpn row: %v", err)
	}
	if v.PrivateKeyPath != "/var/lib/caramelo/vpn/private.key" || v.Subnet != "10.86.0.0/16" {
		t.Errorf("vpn row = %+v", v)
	}

	var named bool
	for _, a := range dev.addrs {
		if a.IP == netip.MustParseAddr("10.86.0.1") && a.Kind == vpn.KindMachine &&
			len(a.Names) == 1 && a.Names[0] == "box.internal" {
			named = true
		}
	}
	if !named {
		t.Errorf("the machine's address was not named at start: %+v", dev.addrs)
	}
}

func TestStartRefusesAPeerRowItCannotInstall(t *testing.T) {
	n, store, _ := newNetwork(t)
	store.peers = []state.Peer{{Name: "agent-7", PublicKey: key(t, 1), IP: "not-an-address"}}
	if err := n.Start(context.Background()); err == nil {
		t.Fatal("Start accepted a peer row with an unusable address")
	}
}

func TestNewNetworkFromTheConfig(t *testing.T) {
	cfg := serverconfig.Default()
	store := &fakeStore{}
	n := NewNetwork(store, newDevice(), cfg)
	if n.Subnet.String() != vpn.DefaultSubnet || n.Alloc == nil {
		t.Fatalf("network = %+v", n)
	}
	if n.KeyPath != "/var/lib/caramelo/vpn/private.key" || n.Listen != vpn.DefaultListen {
		t.Errorf("network = %+v", n)
	}

	if _, err := n.AddPeer(context.Background(), "agent-7", key(t, 1)); err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
}

func TestNewNetworkWithoutADevice(t *testing.T) {
	n := NewNetwork(&fakeStore{}, nil, serverconfig.Default())
	if _, err := n.AddPeer(context.Background(), "agent-7", key(t, 1)); !errors.Is(err, ErrNoNetwork) {
		t.Errorf("AddPeer = %v, want ErrNoNetwork", err)
	}
	st, err := n.VPNStatus(context.Background())
	if err != nil || st.Enabled {
		t.Fatalf("status = %+v, %v", st, err)
	}

	if st.Subnet != vpn.DefaultSubnet || st.Address != "10.86.0.1" {
		t.Errorf("status = %+v", st)
	}
}

func TestNewNetworkWithAnUnusableSubnet(t *testing.T) {
	cfg := serverconfig.Default()
	cfg.VPNSubnet = "10.86.0.0/30"
	n := NewNetwork(&fakeStore{}, newDevice(), cfg)
	if _, err := n.AddPeer(context.Background(), "agent-7", key(t, 1)); !errors.Is(err, ErrNoNetwork) {
		t.Errorf("AddPeer = %v, want ErrNoNetwork", err)
	}
	st, _ := n.VPNStatus(context.Background())
	if !strings.Contains(st.Error, "too small") {
		t.Errorf("status error = %q", st.Error)
	}
}

func TestAddPeerReportsANameAddedAtTheSameMoment(t *testing.T) {
	n, store, _ := newNetwork(t)
	ctx := context.Background()

	store.onAdd = func() {
		store.mu.Lock()
		defer store.mu.Unlock()
		store.peers = append(store.peers, state.Peer{Name: "agent-7", PublicKey: key(t, 60), IP: "10.86.0.9"})
	}

	_, err := n.AddPeer(ctx, "agent-7", key(t, 1))
	if err == nil || !strings.Contains(err.Error(), "same moment") {
		t.Fatalf("err = %v, want it to name the race", err)
	}
}

func TestRotateRestoresTheOldRowWhenTheWriteFails(t *testing.T) {
	n, store, _ := newNetwork(t)
	ctx := context.Background()
	first, err := n.AddPeer(ctx, "agent-7", key(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	store.keyErr = errors.New("disk is full")
	if _, err := n.AddPeer(ctx, "agent-7", key(t, 80)); err == nil {
		t.Fatal("the rotation succeeded although the write failed")
	}
	got, err := store.Peer(ctx, "agent-7")
	if err != nil {
		t.Fatalf("the peer was lost: %v", err)
	}
	if got.PublicKey != first.PublicKey || got.IP != first.IP {
		t.Errorf("peer = %+v, want it unchanged (%+v)", got, first)
	}
}

func TestRotateRestoresTheOldKeyWhenTheDeviceRefuses(t *testing.T) {
	n, store, dev := newNetwork(t)
	ctx := context.Background()
	first, err := n.AddPeer(ctx, "agent-7", key(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	dev.addErr = errors.New("device is down")
	if _, err := n.AddPeer(ctx, "agent-7", key(t, 80)); err == nil {
		t.Fatal("the rotation succeeded although the device refused it")
	}
	got, err := store.Peer(ctx, "agent-7")
	if err != nil {
		t.Fatalf("the peer was lost: %v", err)
	}
	if got.PublicKey != first.PublicKey {
		t.Errorf("peer key = %s, want the old one (%s): the device still admits it",
			got.PublicKey, first.PublicKey)
	}
	if got.IP != first.IP {
		t.Errorf("peer address = %s, want %s", got.IP, first.IP)
	}
}
