package vpn

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T, b byte) Key {
	t.Helper()
	var k Key
	for i := range k {
		k[i] = b
	}
	return k
}

func TestIPCConfigRendersTheWholeDevice(t *testing.T) {
	priv := testKey(t, 0x11)
	port := 4021
	cfg := ipcConfig{
		PrivateKey:   &priv,
		ListenPort:   &port,
		ReplacePeers: true,
		Peers: []ipcPeer{
			peerSection(testKey(t, 0x22), netip.MustParseAddr("10.86.0.2")),
			peerSection(testKey(t, 0x33), netip.MustParseAddr("10.86.0.3")),
		},
	}
	want := strings.Join([]string{
		"private_key=" + priv.Hex(),
		"listen_port=4021",
		"replace_peers=true",
		"public_key=" + testKey(t, 0x22).Hex(),
		"replace_allowed_ips=true",
		"allowed_ip=10.86.0.2/32",
		"public_key=" + testKey(t, 0x33).Hex(),
		"replace_allowed_ips=true",
		"allowed_ip=10.86.0.3/32",
		"",
	}, "\n")
	if got := cfg.String(); got != want {
		t.Fatalf("ipc document:\n%s\nwant:\n%s", got, want)
	}
}

func TestPeerSectionAllowsExactlyThePeersOwnAddress(t *testing.T) {

	p := peerSection(testKey(t, 0x44), netip.MustParseAddr("10.86.0.7"))
	if len(p.AllowedIPs) != 1 || p.AllowedIPs[0].String() != "10.86.0.7/32" {
		t.Fatalf("allowed ips = %v, want exactly 10.86.0.7/32", p.AllowedIPs)
	}
	if !p.ReplaceAllowedIPs {
		t.Error("replace_allowed_ips is not set, so a re-added peer would accumulate addresses")
	}
}

func TestIPCConfigRemoveSectionSaysNothingElse(t *testing.T) {
	key := testKey(t, 0x55)
	cfg := ipcConfig{Peers: []ipcPeer{{
		PublicKey:  key,
		Remove:     true,
		AllowedIPs: []netip.Prefix{netip.MustParsePrefix("10.86.0.2/32")},
	}}}
	want := "public_key=" + key.Hex() + "\nremove=true\n"
	if got := cfg.String(); got != want {
		t.Fatalf("ipc document = %q, want %q", got, want)
	}
}

func TestIPCConfigOmitsWhatIsNotSet(t *testing.T) {
	cfg := ipcConfig{Peers: []ipcPeer{{
		PublicKey: testKey(t, 0x66),
		Endpoint:  "192.168.56.11:4021",
		Keepalive: 25,
	}}}
	got := cfg.String()
	for _, absent := range []string{"private_key=", "listen_port=", "replace_peers="} {
		if strings.Contains(got, absent) {
			t.Errorf("document contains %q, which was not set:\n%s", absent, got)
		}
	}
	for _, present := range []string{"endpoint=192.168.56.11:4021", "persistent_keepalive_interval=25"} {
		if !strings.Contains(got, present) {
			t.Errorf("document is missing %q:\n%s", present, got)
		}
	}
}

func TestParseIPCStatus(t *testing.T) {
	a, b := testKey(t, 0x77), testKey(t, 0x88)
	doc := strings.Join([]string{
		"private_key=" + testKey(t, 0x11).Hex(),
		"listen_port=41234",
		"fwmark=0",
		"public_key=" + a.Hex(),
		"endpoint=192.168.56.1:51820",
		"last_handshake_time_sec=1757000000",
		"last_handshake_time_nsec=500",
		"tx_bytes=148",
		"rx_bytes=92",
		"public_key=" + b.Hex(),
		"last_handshake_time_sec=0",
		"last_handshake_time_nsec=0",
		"tx_bytes=0",
		"rx_bytes=0",
		"errno=0",
		"",
	}, "\n")
	st, err := parseIPCStatus(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("parseIPCStatus: %v", err)
	}
	if st.ListenPort != 41234 {
		t.Errorf("listen port = %d, want 41234", st.ListenPort)
	}
	if len(st.Peers) != 2 {
		t.Fatalf("peers = %d, want 2", len(st.Peers))
	}
	pa := st.Peers[a.Hex()]
	if want := time.Unix(1757000000, 500); !pa.LastHandshake.Equal(want) {
		t.Errorf("last handshake = %v, want %v", pa.LastHandshake, want)
	}
	if pa.RxBytes != 92 || pa.TxBytes != 148 {
		t.Errorf("rx/tx = %d/%d, want 92/148", pa.RxBytes, pa.TxBytes)
	}
	if pa.Endpoint != "192.168.56.1:51820" {
		t.Errorf("endpoint = %q", pa.Endpoint)
	}

	if pb := st.Peers[b.Hex()]; !pb.LastHandshake.IsZero() {
		t.Errorf("last handshake of an unseen peer = %v, want the zero time", pb.LastHandshake)
	}

	if pb := st.Peers[b.Hex()]; pb.Endpoint != "" {
		t.Errorf("endpoint leaked between peer sections: %q", pb.Endpoint)
	}
}

func TestParseIPCStatusReportsErrno(t *testing.T) {
	if _, err := parseIPCStatus(strings.NewReader("errno=98\n")); err == nil {
		t.Fatal("a non-zero errno was not reported")
	}
}
