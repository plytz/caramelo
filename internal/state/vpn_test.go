package state

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMigration4Schema(t *testing.T) {
	s, _ := tempDB(t)
	db := s.(*store).db
	ctx := context.Background()

	now := time.Now().UTC().Format(timeFormat)
	insert := `INSERT INTO peers (name, public_key, ip, added_by, created_at, last_handshake)
	           VALUES (?, ?, ?, '', ?, '')`
	if _, err := db.ExecContext(ctx, insert, "laptop", "key-a", "10.86.0.2", now); err != nil {
		t.Fatalf("insert peer: %v", err)
	}
	for _, tc := range []struct {
		what            string
		name, key, addr string
	}{
		{"name", "laptop", "key-b", "10.86.0.3"},
		{"public key", "agent-7", "key-a", "10.86.0.3"},
		{"address", "agent-7", "key-b", "10.86.0.2"},
	} {
		if _, err := db.ExecContext(ctx, insert, tc.name, tc.key, tc.addr, now); err == nil {
			t.Errorf("a duplicate %s was accepted", tc.what)
		}
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO apps (name, repo_path, created_at) VALUES ('shop', '/repo', ?)`, now); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	newEnv := func(name string, base int, ip string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO envs
			(app, name, branch, worktree, port_base, port_count, status, created_at, updated_at, vpn_ip)
			VALUES ('shop', ?, ?, '/wt', ?, 16, 'ready', ?, ?, ?)`, name, name, base, now, now, ip)
		return err
	}
	if err := newEnv("feat-x", 20000, "10.86.1.1"); err != nil {
		t.Fatalf("insert env: %v", err)
	}
	if err := newEnv("feat-y", 20016, "10.86.1.1"); err == nil {
		t.Error("two envs were allowed to hold the same address")
	}
	if err := newEnv("feat-y", 20016, ""); err != nil {
		t.Fatalf("env without an address: %v", err)
	}
	if err := newEnv("feat-z", 20032, ""); err != nil {
		t.Fatalf("a second env without an address must be allowed: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO vpn (id, private_key, subnet, listen, updated_at) VALUES (1, ?, ?, ?, ?)`,
		"/var/lib/caramelo/vpn/private.key", "10.86.0.0/16", "0.0.0.0:4021", now); err != nil {
		t.Fatalf("insert vpn row: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO vpn (id, private_key, subnet, listen, updated_at) VALUES (2, '', '', '', ?)`, now); err == nil {
		t.Error("a second vpn row was accepted")
	}
}

func TestPeers(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)

	if got, err := s.Peers(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty store: %v, %v", got, err)
	}
	if _, err := s.Peer(ctx, "laptop"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	created := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	for _, p := range []Peer{
		{Name: "laptop", PublicKey: "key-b", IP: "10.86.0.3", AddedBy: "setup", CreatedAt: created},
		{Name: "agent-7", PublicKey: "key-a", IP: "10.86.0.2", CreatedAt: created},
	} {
		if err := s.AddPeer(ctx, p); err != nil {
			t.Fatalf("AddPeer(%s): %v", p.Name, err)
		}
	}
	got, err := s.Peers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "agent-7" || got[1].Name != "laptop" {
		t.Fatalf("peers are not sorted by name: %+v", got)
	}
	if !got[0].LastHandshake.IsZero() {
		t.Errorf("a peer that has never been seen has a handshake time: %v", got[0].LastHandshake)
	}

	one, err := s.Peer(ctx, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if one.PublicKey != "key-b" || one.IP != "10.86.0.3" || one.AddedBy != "setup" || !one.CreatedAt.Equal(created) {
		t.Errorf("peer round trip: %+v", one)
	}

	for _, tc := range []struct {
		what string
		p    Peer
	}{
		{"name", Peer{Name: "laptop", PublicKey: "key-c", IP: "10.86.0.4"}},
		{"public key", Peer{Name: "other", PublicKey: "key-a", IP: "10.86.0.4"}},
		{"address", Peer{Name: "other", PublicKey: "key-c", IP: "10.86.0.2"}},
	} {
		if err := s.AddPeer(ctx, tc.p); !errors.Is(err, ErrExists) {
			t.Errorf("duplicate %s: err = %v, want ErrExists", tc.what, err)
		}
	}

	at := time.Date(2026, 9, 8, 13, 30, 0, 0, time.UTC)
	if err := s.SetPeerHandshake(ctx, "laptop", at); err != nil {
		t.Fatalf("SetPeerHandshake: %v", err)
	}
	if err := s.SetPeerHandshake(ctx, "gone", at); err != nil {
		t.Errorf("SetPeerHandshake on an unknown peer: %v", err)
	}
	one, err = s.Peer(ctx, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !one.LastHandshake.Equal(at) {
		t.Errorf("last handshake = %v, want %v", one.LastHandshake, at)
	}

	if err := s.SetPeerKey(ctx, "laptop", "key-rotated"); err != nil {
		t.Fatalf("SetPeerKey: %v", err)
	}
	one, err = s.Peer(ctx, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if one.PublicKey != "key-rotated" || one.IP != "10.86.0.3" {
		t.Errorf("after a rotation: %+v, want the new key and the same address", one)
	}
	if err := s.SetPeerKey(ctx, "gone", "key-x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rotating an unknown peer: err = %v, want ErrNotFound", err)
	}
	if err := s.SetPeerKey(ctx, "laptop", "key-a"); !errors.Is(err, ErrExists) {
		t.Errorf("rotating onto another peer's key: err = %v, want ErrExists", err)
	}

	if err := s.RemovePeer(ctx, "laptop"); err != nil {
		t.Fatalf("RemovePeer: %v", err)
	}
	if err := s.RemovePeer(ctx, "laptop"); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing a peer twice: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Peer(ctx, "laptop"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the peer is still there: %v", err)
	}
}

func TestAddPeerRefusesIncompleteRows(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	for _, p := range []Peer{
		{PublicKey: "k", IP: "10.86.0.2"},
		{Name: "laptop", IP: "10.86.0.2"},
		{Name: "laptop", PublicKey: "k"},
	} {
		if err := s.AddPeer(ctx, p); err == nil {
			t.Errorf("AddPeer(%+v) was accepted", p)
		}
	}
}

func TestEnvVPNIP(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	if err := s.AddApp(ctx, sampleApp("shop")); err != nil {
		t.Fatal(err)
	}

	withIP := sampleEnv("shop", "feat-x", 20000)
	withIP.VPNIP = "10.86.1.1"
	x, err := s.CreateEnv(ctx, withIP)
	if err != nil {
		t.Fatalf("CreateEnv: %v", err)
	}
	y, err := s.CreateEnv(ctx, sampleEnv("shop", "feat-y", 20016))
	if err != nil {
		t.Fatalf("CreateEnv: %v", err)
	}

	if got, err := s.EnvVPNIP(ctx, x.ID); err != nil || got != "10.86.1.1" {
		t.Fatalf("EnvVPNIP = %q, %v", got, err)
	}
	if got, err := s.EnvVPNIP(ctx, y.ID); err != nil || got != "" {
		t.Fatalf("an env without an address: %q, %v", got, err)
	}
	if _, err := s.EnvVPNIP(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("EnvVPNIP of an unknown env: %v", err)
	}

	rec, err := s.Env(ctx, "shop", "feat-x")
	if err != nil || rec.VPNIP != "10.86.1.1" {
		t.Fatalf("env row: %+v, %v", rec, err)
	}

	if err := s.SetEnvVPNIP(ctx, y.ID, "10.86.1.1"); !errors.Is(err, ErrExists) {
		t.Errorf("two envs on one address: err = %v, want ErrExists", err)
	}
	if err := s.SetEnvVPNIP(ctx, y.ID, "10.86.1.2"); err != nil {
		t.Fatalf("SetEnvVPNIP: %v", err)
	}
	if err := s.SetEnvVPNIP(ctx, 9999, "10.86.1.3"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetEnvVPNIP on an unknown env: %v", err)
	}

	if err := s.SetEnvVPNIP(ctx, x.ID, ""); err != nil {
		t.Fatalf("free an address: %v", err)
	}
	if err := s.SetEnvVPNIP(ctx, y.ID, ""); err != nil {
		t.Fatalf("free a second address: %v", err)
	}
	if err := s.SetEnvVPNIP(ctx, x.ID, "10.86.1.2"); err != nil {
		t.Fatalf("the freed address must be available again: %v", err)
	}

	rec, err = s.Env(ctx, "shop", "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	rec.Status = EnvReady
	rec.VPNIP = ""
	if err := s.UpdateEnv(ctx, *rec); err != nil {
		t.Fatalf("UpdateEnv: %v", err)
	}
	if got, err := s.EnvVPNIP(ctx, x.ID); err != nil || got != "10.86.1.2" {
		t.Errorf("UpdateEnv changed the address: %q, %v", got, err)
	}
}

func TestTakenVPNIPs(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	if got, err := s.TakenVPNIPs(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty store: %v, %v", got, err)
	}
	if err := s.AddApp(ctx, sampleApp("shop")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPeer(ctx, Peer{Name: "laptop", PublicKey: "key-a", IP: "10.86.0.2"}); err != nil {
		t.Fatal(err)
	}
	env := sampleEnv("shop", "feat-x", 20000)
	env.VPNIP = "10.86.1.1"
	if _, err := s.CreateEnv(ctx, env); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateEnv(ctx, sampleEnv("shop", "feat-y", 20016)); err != nil {
		t.Fatal(err)
	}
	got, err := s.TakenVPNIPs(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 || got[0] != "10.86.0.2" || got[1] != "10.86.1.1" {
		t.Fatalf("taken addresses = %v", got)
	}
}

func TestVPNRow(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	if _, err := s.VPN(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a machine with no network: err = %v, want ErrNotFound", err)
	}
	v := VPN{
		PrivateKeyPath: "/var/lib/caramelo/vpn/private.key",
		Subnet:         "10.86.0.0/16",
		Listen:         "0.0.0.0:4021",
		UpdatedAt:      time.Date(2026, 9, 8, 14, 0, 0, 0, time.UTC),
	}
	if err := s.SetVPN(ctx, v); err != nil {
		t.Fatalf("SetVPN: %v", err)
	}
	got, err := s.VPN(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if *got != v {
		t.Errorf("vpn row round trip: %+v, want %+v", got, v)
	}

	v.Listen = "0.0.0.0:4123"
	if err := s.SetVPN(ctx, v); err != nil {
		t.Fatalf("SetVPN again: %v", err)
	}
	got, err = s.VPN(ctx)
	if err != nil || got.Listen != "0.0.0.0:4123" {
		t.Fatalf("vpn row = %+v, %v", got, err)
	}

	for _, bad := range []VPN{
		{Subnet: "10.86.0.0/16", Listen: "0.0.0.0:4021"},
		{PrivateKeyPath: "/k", Listen: "0.0.0.0:4021"},
		{PrivateKeyPath: "/k", Subnet: "10.86.0.0/16"},
	} {
		if err := s.SetVPN(ctx, bad); err == nil {
			t.Errorf("SetVPN(%+v) was accepted", bad)
		}
	}
}
