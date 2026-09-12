package vpnclient

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/state"
)

func TestWGQuickRendersAUsableConfiguration(t *testing.T) {
	kp, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	rec := testRecord("worker1")
	conf, err := WGQuick(rec, kp.Private)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[Interface]",
		"PrivateKey = " + kp.Private,
		"Address = 10.86.0.2/32",
		"DNS = 10.86.0.1, internal",
		"MTU = 1280",
		"[Peer]",
		"PublicKey = " + rec.MachineKey,
		"Endpoint = 192.168.56.11:4021",

		"AllowedIPs = 10.86.0.0/16",

		"PersistentKeepalive = 25",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("the configuration is missing %q:\n%s", want, conf)
		}
	}

	if strings.Index(conf, "[Interface]") > strings.Index(conf, "[Peer]") {
		t.Error("the sections are in the wrong order")
	}
	if _, err := WGQuick(Record{}, kp.Private); err == nil {
		t.Error("a configuration was rendered for a machine that was never joined")
	}
}

func TestConfigCommandRendersThePrivateKey(t *testing.T) {
	keys, records := stores(t)
	kp, _, err := keys.Ensure("worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := records.Save(testRecord("worker1")); err != nil {
		t.Fatal(err)
	}
	client, err := NewWith(Options{Control: &fakeControl{}, Keys: keys, Records: records})
	if err != nil {
		t.Fatal(err)
	}
	conf, err := client.Config(context.Background(), "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conf, kp.Private) {
		t.Fatal("the wg-quick file has no private key in it")
	}
}

func TestStateJSONHasNoPrivateKey(t *testing.T) {
	keys, records := stores(t)
	kp, _, err := keys.Ensure("worker1")
	if err != nil {
		t.Fatal(err)
	}
	rec := testRecord("worker1")
	rec.PublicKey = kp.Public
	if err := records.Save(rec); err != nil {
		t.Fatal(err)
	}
	client, err := NewWith(Options{Control: &fakeControl{}, Keys: keys,
		Records: records, Installer: notInstalled{}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.Status(context.Background(), "worker1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), kp.Private) {
		t.Fatalf("the private key is in --json output:\n%s", b)
	}
	if !strings.Contains(string(b), kp.Public) {
		t.Fatalf("the public key is missing from --json output:\n%s", b)
	}
}

func TestStatusOfAMachineNeverJoined(t *testing.T) {
	keys, records := stores(t)
	client, err := NewWith(Options{Control: &fakeControl{}, Keys: keys,
		Records: records, Installer: notInstalled{}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.Status(context.Background(), "stranger")
	if err != nil {
		t.Fatalf("status of an unjoined machine: %v", err)
	}
	if st.Mode != ModeOff {
		t.Fatalf("mode = %q, want %q", st.Mode, ModeOff)
	}
}

func TestDialerWithoutAKey(t *testing.T) {
	keys, records := stores(t)
	client, err := NewWith(Options{Control: &fakeControl{}, Keys: keys,
		Records: records, Installer: notInstalled{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Dialer(context.Background(), "stranger")
	if err == nil || !strings.Contains(err.Error(), ErrNoKey.Error()) {
		t.Fatalf("err = %v, want ErrNoKey", err)
	}
}

func TestUpDiagnosesSilence(t *testing.T) {
	keys, records := stores(t)
	timeout := verifyTimeout
	verifyTimeout = time.Second
	t.Cleanup(func() { verifyTimeout = timeout })
	const machine = "silent"
	if _, _, err := keys.Ensure(machine); err != nil {
		t.Fatal(err)
	}

	stranger, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	peerIP := netip.MustParseAddr("10.86.0.2")
	boxIP := netip.MustParseAddr("10.86.0.1")
	b := startBox(t, stranger.Public, peerIP, boxIP)

	status, _ := json.Marshal(api.Status{Hostname: "worker1", VPN: &api.VPNStatus{
		Enabled: true, PublicKey: b.key.Public, Listen: "0.0.0.0:4021",
		Endpoint: b.endpoint, Subnet: "10.86.0.0/16", Address: boxIP.String(),
	}})
	peer, _ := json.Marshal(state.Peer{Name: "p", IP: peerIP.String()})
	control := &fakeControl{answers: map[string]string{
		"status --json": string(status),
		"peer add":      string(peer),
	}}
	client, err := NewWith(Options{Control: control, Keys: keys, Records: records,
		Installer: notInstalled{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.close(machine) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err = client.Up(ctx, UpRequest{Machine: machine, PeerName: "p"})
	if err == nil {
		t.Fatal("vpn up succeeded against a machine that does not know this key")
	}
	if !strings.Contains(err.Error(), "no handshake") || !strings.Contains(err.Error(), b.endpoint) {
		t.Errorf("err = %v, want it to name the missing handshake and the endpoint", err)
	}
	if d := time.Since(start); d > verifyTimeout+10*time.Second {
		t.Errorf("vpn up took %s; it must never wait unbounded", d)
	}
}

type notInstalled struct{}

var _ Installer = notInstalled{}

func (notInstalled) Install(context.Context, InstallOptions) error { return ErrUnsupported }
func (notInstalled) Uninstall(context.Context) error               { return nil }
func (notInstalled) Installed(context.Context) (bool, error)       { return false, nil }
