package vpnclient

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vpn"
)

func stores(t *testing.T) (*FileKeyStore, *FileRecordStore) {
	t.Helper()
	dir := t.TempDir()
	return &FileKeyStore{Dir: dir}, &FileRecordStore{Dir: dir}
}

func TestUpJoinsTheNetwork(t *testing.T) {
	keys, records := stores(t)
	const machine = "e2e-up"
	kp, created, err := keys.Ensure(machine)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("the first Ensure did not create a key")
	}
	peerIP := netip.MustParseAddr("10.86.0.2")
	boxIP := netip.MustParseAddr("10.86.0.1")
	b := startBox(t, kp.Public, peerIP, boxIP)

	status, _ := json.Marshal(api.Status{
		Hostname: "worker1",
		VPN: &api.VPNStatus{
			Enabled:   true,
			PublicKey: b.key.Public,
			Listen:    "0.0.0.0:4021",
			Endpoint:  b.endpoint,
			Subnet:    "10.86.0.0/16",
			Address:   boxIP.String(),
			APIListen: "both",
		},
	})
	peer, _ := json.Marshal(state.Peer{Name: "test-peer", IP: peerIP.String(), PublicKey: kp.Public})
	control := &fakeControl{answers: map[string]string{
		"status --json": string(status),
		"peer add":      string(peer),
	}}

	client, err := NewWith(Options{Control: control, Keys: keys, Records: records})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.close(machine) })

	st, err := client.Up(context.Background(), UpRequest{Machine: machine, PeerName: "test-peer"})
	if err != nil {
		t.Fatalf("vpn up: %v", err)
	}
	if st.Mode != ModeUserspace {
		t.Errorf("mode = %q, want %q", st.Mode, ModeUserspace)
	}
	if st.IP != peerIP {
		t.Errorf("address = %s, want %s", st.IP, peerIP)
	}
	if st.LastHandshake.IsZero() {
		t.Error("vpn up returned without a handshake; it must prove the tunnel carries packets")
	}
	if st.Resolver != "10.86.0.1:53" {
		t.Errorf("resolver = %q, want 10.86.0.1:53", st.Resolver)
	}

	joined := strings.Join(control.calls, "\n")
	if !strings.Contains(joined, "peer add test-peer "+kp.Public+" --json") {
		t.Errorf("peer add was not called with the public key; calls were:\n%s", joined)
	}
	if strings.Contains(joined, kp.Private) {
		t.Fatal("the private key was sent to the machine")
	}

	rec, err := records.Load(machine)
	if err != nil {
		t.Fatal(err)
	}
	if rec.MachineKey != b.key.Public || rec.Endpoint != b.endpoint {
		t.Errorf("record = %+v, want the machine's key and endpoint", rec)
	}
	again, err := client.Status(context.Background(), machine)
	if err != nil {
		t.Fatal(err)
	}
	if again.PeerName != "test-peer" || again.LastHandshake.IsZero() {
		t.Errorf("status = %+v, want the joined peer with its handshake", again)
	}

	control.calls = nil
	if _, err := client.Up(context.Background(), UpRequest{Machine: machine}); err != nil {
		t.Fatalf("vpn up a second time, with no name: %v", err)
	}
	joined = strings.Join(control.calls, "\n")
	if !strings.Contains(joined, "peer add test-peer ") {
		t.Errorf("the second up did not reuse the name it joined under; calls were:\n%s", joined)
	}

	control.calls = nil
	if _, err := client.Up(context.Background(), UpRequest{Machine: machine, Identity: "renamed"}); err != nil {
		t.Fatalf("vpn up after the commander was given a name: %v", err)
	}
	joined = strings.Join(control.calls, "\n")
	if !strings.Contains(joined, "peer add test-peer ") {
		t.Errorf("a commander renamed in its config must stay the peer it already is; calls were:\n%s", joined)
	}
}

func TestUpRefusesAMachineWithNoNetwork(t *testing.T) {
	keys, records := stores(t)
	status, _ := json.Marshal(api.Status{Hostname: "old"})
	control := &fakeControl{answers: map[string]string{"status --json": string(status)}}
	client, err := NewWith(Options{Control: control, Keys: keys, Records: records})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Up(context.Background(), UpRequest{Machine: "old"})
	if err == nil || !strings.Contains(err.Error(), "no private network") {
		t.Fatalf("err = %v, want it to say the machine has no network", err)
	}
	if strings.Contains(err.Error(), "peer add") {
		t.Error("the peer was registered on a machine with no network")
	}
}

func TestDialerReachesAnEnvAndResolvesNames(t *testing.T) {
	keys, records := stores(t)
	const machine = "e2e-dial"
	kp, _, err := keys.Ensure(machine)
	if err != nil {
		t.Fatal(err)
	}
	peerIP := netip.MustParseAddr("10.86.0.2")
	boxIP := netip.MustParseAddr("10.86.0.1")
	envIP := netip.MustParseAddr("10.86.1.4")
	b := startBox(t, kp.Public, peerIP, boxIP, envIP)
	b.zone[vpn.EnvHost("shop", "feat-x")] = envIP
	b.zone[vpn.ServiceHost("shop", "feat-x", "db")] = envIP
	b.serveDNS(boxIP)

	b.serveEcho(netip.AddrPortFrom(envIP, 5432), vpn.TCP, "db-tcp:")
	b.serveEcho(netip.AddrPortFrom(envIP, 5533), vpn.UDP, "db-udp:")

	if err := records.Save(b.record(machine, peerIP)); err != nil {
		t.Fatal(err)
	}
	client, err := NewWith(Options{Control: &fakeControl{}, Keys: keys, Records: records})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.close(machine) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dialer, err := client.Dialer(ctx, machine)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dialer.Close() }()

	addrs, err := dialer.Resolve(ctx, vpn.EnvHost("shop", "feat-x"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(addrs) != 1 || addrs[0] != envIP {
		t.Fatalf("resolve = %v, want [%s]", addrs, envIP)
	}

	if _, err := dialer.Resolve(ctx, "nope.shop.internal"); !isNXDOMAIN(err) {
		t.Errorf("resolving an unknown name: %v, want NXDOMAIN", err)
	}

	if _, err := dialer.Resolve(ctx, "example.com"); !isNXDOMAIN(err) {
		t.Errorf("resolving a public name: %v, want NXDOMAIN", err)
	}

	c, err := dialer.DialContext(ctx, "tcp", vpn.ServiceHost("shop", "feat-x", "db")+":5432")
	if err != nil {
		t.Fatalf("dial db by name: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if got := roundTrip(t, c, "ping"); got != "db-tcp:ping" {
		t.Errorf("tcp round trip = %q, want %q", got, "db-tcp:ping")
	}

	u, err := dialer.DialContext(ctx, "udp", netip.AddrPortFrom(envIP, 5533).String())
	if err != nil {
		t.Fatalf("dial the udp service: %v", err)
	}
	defer func() { _ = u.Close() }()
	_ = u.SetDeadline(time.Now().Add(10 * time.Second))
	if got := roundTrip(t, u, "ping"); got != "db-udp:ping" {
		t.Errorf("udp round trip = %q, want %q", got, "db-udp:ping")
	}
}

func isNXDOMAIN(err error) bool {
	return err != nil && strings.Contains(err.Error(), vpn.ErrNXDOMAIN.Error())
}

func TestConnectPublishesLocalPorts(t *testing.T) {
	keys, records := stores(t)
	const machine = "e2e-connect"
	kp, _, err := keys.Ensure(machine)
	if err != nil {
		t.Fatal(err)
	}
	peerIP := netip.MustParseAddr("10.86.0.2")
	boxIP := netip.MustParseAddr("10.86.0.1")
	envIP := netip.MustParseAddr("10.86.1.7")
	b := startBox(t, kp.Public, peerIP, boxIP, envIP)
	b.zone[vpn.EnvHost("shop", "feat-x")] = envIP
	b.serveDNS(boxIP)
	b.serveEcho(netip.AddrPortFrom(envIP, 5432), vpn.TCP, "db:")
	b.serveEcho(netip.AddrPortFrom(envIP, 7777), vpn.UDP, "echo:")
	b.serveRequestResponse(netip.AddrPortFrom(envIP, 9418), "git:")

	if err := records.Save(b.record(machine, peerIP)); err != nil {
		t.Fatal(err)
	}
	client, err := NewWith(Options{Control: &fakeControl{}, Keys: keys, Records: records})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.close(machine) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dialer, err := client.Dialer(ctx, machine)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dialer.Close() }()

	connector, err := NewConnector(ConnectorOptions{
		Dialer: dialer,
		Lookup: fixedLookup{
			{Name: "db", Kind: "dep", Port: 5432, Protocols: []vpn.Protocol{vpn.TCP}},
			{Name: "echo", Kind: "service", Port: 7777, Protocols: []vpn.Protocol{vpn.UDP}},
			{Name: "git", Kind: "service", Port: 9418, Protocols: []vpn.Protocol{vpn.TCP}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connector.Close() }()

	listeners, err := connector.Open(ctx, ConnectRequest{Machine: machine, App: "shop", Env: "feat-x"})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if len(listeners) != 3 {
		t.Fatalf("got %d listeners, want 3: %+v", len(listeners), listeners)
	}
	byName := map[string]Listener{}
	for _, l := range listeners {
		byName[l.Name] = l
		if PortOf(l) == 0 {
			t.Errorf("%s got no local port: %+v", l.Name, l)
		}
		if !strings.HasPrefix(l.Local, "127.0.0.1:") {
			t.Errorf("%s listens on %q, want a loopback address", l.Name, l.Local)
		}
	}
	if got, want := byName["db"].Remote, "10.86.1.7:5432"; got != want {
		t.Errorf("db goes to %q, want %q", got, want)
	}
	if got, want := byName["db"].Host, "db.feat-x.shop.internal"; got != want {
		t.Errorf("db is labelled %q, want %q", got, want)
	}

	if got := roundTrip(t, dialLocal(t, "tcp", byName["db"].Local), "select 1"); got != "db:select 1" {
		t.Errorf("through the local tcp port: %q", got)
	}
	if got := roundTrip(t, dialLocal(t, "udp", byName["echo"].Local), "hello"); got != "echo:hello" {
		t.Errorf("through the local udp port: %q", got)
	}

	{
		c := dialLocal(t, "tcp", byName["git"].Local)
		defer func() { _ = c.Close() }()
		if err := c.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write([]byte("want refs")); err != nil {
			t.Fatal(err)
		}
		if err := c.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatalf("half-close: %v", err)
		}
		got, err := io.ReadAll(c)
		if err != nil {
			t.Fatalf("reading the answer to a half-closed request: %v", err)
		}
		if want := "git:want refs"; string(got) != want {
			t.Errorf("through the local tcp port after a half-close: %q, want %q", got, want)
		}
	}

	local := byName["db"].Local
	if err := connector.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the local port to close", func() bool {
		c, err := dialLocalOnce(local)
		if err != nil {
			return true
		}
		_ = c.Close()
		return false
	})
}

func TestConnectOnlyNamesUnknown(t *testing.T) {
	connector, err := NewConnector(ConnectorOptions{
		Dialer: stubDialer{},
		Lookup: fixedLookup{{Name: "db", Kind: "dep", Port: 5432}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = connector.Open(context.Background(), ConnectRequest{
		App: "shop", Env: "feat-x", Only: []string{"cache"},
	})
	if err == nil || !strings.Contains(err.Error(), "cache") || !strings.Contains(err.Error(), "db") {
		t.Fatalf("err = %v, want it to name the unknown one and list what there is", err)
	}
}
