package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/serverconfig"
)

func TestServerStatusExplainsAClosedPublicPort(t *testing.T) {
	var b bytes.Buffer
	st := hubStatus{
		Hostname: "box",
		Port: portStatus{
			Port: 4022, Listening: false,
			APIListen: serverconfig.APIListenVPN,
			VPNPort:   4021, VPNListening: true,
			VPNMode: serverconfig.VPNModeUserspace,
		},
	}
	if err := writeHubStatus(&b, st); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"port 4022", "api_listen vpn", "answers inside the tunnel", "udp 4021", "vpn_mode userspace", "listening"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
}

func TestServerStatusSaysListeningPlainly(t *testing.T) {
	var b bytes.Buffer
	st := hubStatus{Port: portStatus{Port: 4022, Listening: true, APIListen: serverconfig.APIListenBoth, VPNPort: 4021, VPNListening: true}}
	if err := writeHubStatus(&b, st); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "api_listen") {
		t.Errorf("a listening port needs no explanation:\n%s", b.String())
	}
}

func TestServerStatusOmitsTheTunnelWhenThereIsNone(t *testing.T) {
	var b bytes.Buffer
	if err := writeHubStatus(&b, hubStatus{Port: portStatus{Port: 4022, Listening: true}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "udp") {
		t.Errorf("a machine with no network reported one:\n%s", b.String())
	}
}

func TestServerStatusSaysHowMuchSwapThereIsAndWhoMadeIt(t *testing.T) {
	tests := []struct {
		name string
		swap swapStatus
		want string
	}{
		{
			name: "caramelo made it",
			swap: swapStatus{TotalBytes: 4 << 30, Managed: true, Backend: serverconfig.SwapFile, SizeBytes: 4 << 30},
			want: "4.0 GiB (caramelo)",
		},
		{
			name: "the machine came with it",
			swap: swapStatus{TotalBytes: 2 << 30, Backend: serverconfig.SwapFile, SizeBytes: 4 << 30},
			want: "2.0 GiB",
		},
		{
			name: "asked for none",
			swap: swapStatus{Backend: serverconfig.SwapOff},
			want: "none (swap: off)",
		},
		{
			name: "asked for some and has none yet",
			swap: swapStatus{Backend: serverconfig.SwapFile, SizeBytes: 4 << 30},
			want: "none (4.0 GiB configured)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			if err := writeHubStatus(&b, hubStatus{Swap: tc.swap}); err != nil {
				t.Fatal(err)
			}
			line := ""
			for _, l := range strings.Split(b.String(), "\n") {
				if strings.HasPrefix(l, "swap") {
					line = l
				}
			}
			if line == "" {
				t.Fatalf("no swap row at all:\n%s", b.String())
			}
			if !strings.Contains(line, tc.want) {
				t.Errorf("swap row = %q, want it to say %q", line, tc.want)
			}
		})
	}
}

func TestServerStatusCarriesTheConfiguredVPNMode(t *testing.T) {
	dir := t.TempDir()
	cfg := serverconfig.Default()
	cfg.Name, cfg.Hub.Fleet = "box", "home"
	if err := serverconfig.Save(dir, cfg, 0o640); err != nil {
		t.Fatal(err)
	}

	st := hubStatusOf(context.Background(), stubRunner{}, dir)
	if st.Port.VPNMode != serverconfig.VPNModeUserspace {
		t.Errorf("vpn_mode = %q, want the configured %q", st.Port.VPNMode, serverconfig.VPNModeUserspace)
	}
	var b bytes.Buffer
	if err := writeHubStatus(&b, st); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "vpn_mode userspace") {
		t.Errorf("the udp row does not name the tunnel this machine runs:\n%s", b.String())
	}
	doc, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), `"vpn_mode":"userspace"`) {
		t.Errorf("--json does not carry vpn_mode:\n%s", doc)
	}
}

func TestVPNListenPort(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"0.0.0.0:4021", 4021},
		{"[::]:4021", 4021},
		{" 0.0.0.0:9999 ", 9999},
		{"", 0},
		{"0.0.0.0", 0},
		{"0.0.0.0:nope", 0},
	} {
		if got := vpnListenPort(c.in); got != c.want {
			t.Errorf("vpnListenPort(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestUDPBound(t *testing.T) {
	conn, err := net.ListenPacket("udp", "0.0.0.0:0")
	if err != nil {
		t.Skipf("no udp socket available: %v", err)
	}
	_, portStr, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	if !udpBound(port) {
		t.Errorf("udp %d is held by this test and was reported free", port)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if udpBound(port) {
		t.Errorf("udp %d was released and is still reported held", port)
	}
}
