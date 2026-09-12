package cli

import (
	"bytes"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/serverconfig"
)

func TestServerStatusExplainsAClosedPublicPort(t *testing.T) {
	var b bytes.Buffer
	st := serverStatus{
		Hostname: "box",
		Port: portStatus{
			Port: 4022, Listening: false,
			APIListen: serverconfig.APIListenVPN,
			VPNPort:   4021, VPNListening: true,
		},
	}
	if err := writeServerStatus(&b, st); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"port 4022", "api_listen vpn", "answers inside the tunnel", "udp 4021", "listening"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
}

func TestServerStatusSaysListeningPlainly(t *testing.T) {
	var b bytes.Buffer
	st := serverStatus{Port: portStatus{Port: 4022, Listening: true, APIListen: serverconfig.APIListenBoth, VPNPort: 4021, VPNListening: true}}
	if err := writeServerStatus(&b, st); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "api_listen") {
		t.Errorf("a listening port needs no explanation:\n%s", b.String())
	}
}

func TestServerStatusOmitsTheTunnelWhenThereIsNone(t *testing.T) {
	var b bytes.Buffer
	if err := writeServerStatus(&b, serverStatus{Port: portStatus{Port: 4022, Listening: true}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "udp") {
		t.Errorf("a machine with no network reported one:\n%s", b.String())
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
