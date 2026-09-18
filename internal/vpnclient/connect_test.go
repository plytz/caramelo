package vpnclient

import (
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/vpn"
)

func TestPipeCarriesTheAnswerAfterAHalfClose(t *testing.T) {

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		req, err := io.ReadAll(c)
		if err != nil {
			return
		}
		_, _ = c.Write([]byte("answer to " + string(req)))
	}()

	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = local.Close() }()
	go func() {
		a, err := local.Accept()
		if err != nil {
			return
		}
		defer func() { _ = a.Close() }()
		b, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return
		}
		defer func() { _ = b.Close() }()
		pipe(a, b)
	}()

	c, err := net.Dial("tcp", local.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("a request")); err != nil {
		t.Fatal(err)
	}
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("half-close: %v", err)
	}
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := "answer to a request"; string(got) != want {
		t.Fatalf("got %q, want %q: the local listener closed before the answer came back", got, want)
	}
}

func TestTargetsFromURLsUseTheEnvironmentsOwnPorts(t *testing.T) {
	got := targetsFromURLs([]env.URL{
		{Name: "web", Kind: env.KindService, Port: 20000, Protocol: "tcp",
			InternalHost: "web.feat-x.shop.internal", InternalPort: 20000},
		{Name: "echo", Kind: env.KindService, Port: 20003, Protocol: "udp",
			InternalHost: "echo.feat-x.shop.internal", InternalPort: 9999},
		{Name: "db", Kind: env.KindDep, Port: 20001, Protocol: "tcp",
			InternalHost: "db.feat-x.shop.internal", InternalPort: 5432},
	})
	if len(got) != 3 {
		t.Fatalf("targets = %+v", got)
	}

	if got[0].Name != "db" || got[1].Name != "echo" || got[2].Name != "web" {
		t.Errorf("targets are not in name order: %+v", got)
	}
	byName := map[string]Target{}
	for _, tg := range got {
		byName[tg.Name] = tg
	}
	if tg := byName["db"]; tg.Port != 5432 || tg.Kind != "dep" || tg.Protocols[0] != vpn.TCP {
		t.Errorf("db = %+v, want the dependency on 5432/tcp", tg)
	}
	if tg := byName["echo"]; tg.Port != 9999 || tg.Kind != "service" || tg.Protocols[0] != vpn.UDP {
		t.Errorf("echo = %+v, want the service on 9999/udp", tg)
	}
	if tg := byName["web"]; tg.Port != 20000 {
		t.Errorf("web = %+v, want its block port, which is also its own", tg)
	}
}

func TestTargetsFromURLsSkipWhatHasNoAddress(t *testing.T) {
	got := targetsFromURLs([]env.URL{
		{Name: "web", Kind: env.KindService, Port: 20000, Protocol: "tcp"},
		{Name: "db", Kind: env.KindDep, Port: 20001, Protocol: "tcp"},
	})
	if len(got) != 0 {
		t.Errorf("targets = %+v, want none", got)
	}
}

func TestSilenceErrorSaysWhatToCheck(t *testing.T) {
	rec := Record{Machine: "box", Endpoint: "192.168.56.11:4021"}
	ap := netip.MustParseAddrPort("10.86.0.1:4022")

	never := silenceError(rec, ap, time.Time{}, true).Error()
	for _, want := range []string{"no handshake with box", "192.168.56.11:4021", "peer list",
		"firewall", "security group", "caramelo hub probe box"} {
		if !strings.Contains(never, want) {
			t.Errorf("a device that never handshook says %q, want it to mention %q", never, want)
		}
	}

	revoked := silenceError(rec, ap, time.Now().Add(-90*time.Second), true).Error()
	for _, want := range []string{"handshake", "1m30s ago", "no longer be admitted", "peer list", "firewall"} {
		if !strings.Contains(revoked, want) {
			t.Errorf("a revoked peer says %q, want it to mention %q", revoked, want)
		}
	}
	if strings.Contains(revoked, "no handshake with") {
		t.Errorf("a peer that did handshake is reported as one that never did: %q", revoked)
	}

	unknown := silenceError(rec, ap, time.Time{}, false).Error()
	if strings.Contains(unknown, "handshake") {
		t.Errorf("a device with no handshake to read invented one: %q", unknown)
	}
	if !strings.Contains(unknown, "nothing answered within") {
		t.Errorf("unknown = %q, want it to say nothing answered", unknown)
	}
	for _, want := range []string{"firewall", "security group", "caramelo hub probe box"} {
		if !strings.Contains(unknown, want) {
			t.Errorf("unknown = %q, want it to mention %q", unknown, want)
		}
	}
}
