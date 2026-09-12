package vpnclient

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/plytz/caramelo/internal/vpn"
)

func udpResolver(t *testing.T, answer func(name string, id uint16) []byte) dnsDialer {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			h, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			resp := answer(vpn.Normalize(q.Name.String()), h.ID)
			if resp == nil {
				continue
			}
			if _, err := pc.WriteTo(resp, from); err != nil {
				return
			}
		}
	}()
	addr := pc.LocalAddr().String()
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", addr)
	}
}

func buildAnswer(t *testing.T, id uint16, name string, ip netip.Addr, rcode dnsmessage.RCode) []byte {
	t.Helper()
	n, err := dnsmessage.NewName(name + ".")
	if err != nil {
		t.Fatal(err)
	}
	q := dnsmessage.Question{Name: n, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, Response: true, Authoritative: true, RCode: rcode})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(q); err != nil {
		t.Fatal(err)
	}
	if rcode == dnsmessage.RCodeSuccess {
		if err := b.StartAnswers(); err != nil {
			t.Fatal(err)
		}
		err := b.AResource(dnsmessage.ResourceHeader{
			Name: n, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60,
		}, dnsmessage.AResource{A: ip.As4()})
		if err != nil {
			t.Fatal(err)
		}
	}
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestLookupAnswersAndNXDOMAIN(t *testing.T) {
	want := netip.MustParseAddr("10.86.1.4")
	dial := udpResolver(t, func(name string, id uint16) []byte {
		if name == "feat-x.shop.internal" {
			return buildAnswer(t, id, name, want, dnsmessage.RCodeSuccess)
		}
		return buildAnswer(t, id, name, netip.Addr{}, dnsmessage.RCodeNameError)
	})
	got, err := lookupA(context.Background(), dial, "feat-x.shop.internal")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("lookup = %v, want [%s]", got, want)
	}

	if got, err := lookupA(context.Background(), dial, "FEAT-X.Shop.Internal."); err != nil || got[0] != want {
		t.Fatalf("lookup of a fully-qualified name = %v, %v", got, err)
	}
	_, err = lookupA(context.Background(), dial, "nope.shop.internal")
	if !isNXDOMAIN(err) {
		t.Fatalf("err = %v, want NXDOMAIN", err)
	}
}

func TestLookupRefusesNamesOutsideTheDomain(t *testing.T) {
	asked := false
	dial := udpResolver(t, func(name string, id uint16) []byte {
		asked = true
		return buildAnswer(t, id, name, netip.MustParseAddr("1.2.3.4"), dnsmessage.RCodeSuccess)
	})
	if _, err := lookupA(context.Background(), dial, "example.com"); !isNXDOMAIN(err) {
		t.Fatalf("err = %v, want NXDOMAIN", err)
	}
	if asked {
		t.Fatal("a query for a public name left this computer")
	}
}

func TestLookupTimesOutOnSilence(t *testing.T) {
	shortenDNS(t)
	dial := udpResolver(t, func(string, uint16) []byte { return nil })
	start := time.Now()
	_, err := lookupA(context.Background(), dial, "feat-x.shop.internal")
	if err == nil {
		t.Fatal("a resolver that never answers produced no error")
	}
	if isNXDOMAIN(err) {
		t.Fatalf("silence was reported as NXDOMAIN: %v", err)
	}
	if d := time.Since(start); d > (dnsTimeout*time.Duration(dnsAttempts) + 2*time.Second) {
		t.Fatalf("the lookup took %s; it must be bounded", d)
	}
}

func TestLookupRetries(t *testing.T) {
	shortenDNS(t)
	var attempts int
	want := netip.MustParseAddr("10.86.1.9")
	dial := udpResolver(t, func(name string, id uint16) []byte {
		attempts++
		if attempts == 1 {
			return nil
		}
		return buildAnswer(t, id, name, want, dnsmessage.RCodeSuccess)
	})
	got, err := lookupA(context.Background(), dial, "feat-x.shop.internal")
	if err != nil {
		t.Fatalf("the lookup gave up after one lost datagram: %v", err)
	}
	if got[0] != want {
		t.Fatalf("lookup = %v, want [%s]", got, want)
	}
}

func TestParseAIgnoresOtherRecordTypes(t *testing.T) {
	name, err := dnsmessage.NewName("feat-x.shop.internal.")
	if err != nil {
		t.Fatal(err)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, Response: true, Authoritative: true})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(dnsmessage.Question{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}); err != nil {
		t.Fatal(err)
	}
	if err := b.StartAnswers(); err != nil {
		t.Fatal(err)
	}
	hdr := dnsmessage.ResourceHeader{Name: name, Class: dnsmessage.ClassINET, TTL: 60}
	txt := hdr
	txt.Type = dnsmessage.TypeTXT
	if err := b.TXTResource(txt, dnsmessage.TXTResource{TXT: []string{"hello"}}); err != nil {
		t.Fatal(err)
	}
	a := hdr
	a.Type = dnsmessage.TypeA
	if err := b.AResource(a, dnsmessage.AResource{A: [4]byte{10, 86, 1, 4}}); err != nil {
		t.Fatal(err)
	}
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseA(msg, 7, name)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].String() != "10.86.1.4" {
		t.Fatalf("parseA = %v, want [10.86.1.4]", got)
	}

	if _, err := parseA(msg, 8, name); err == nil || !strings.Contains(err.Error(), "different query") {
		t.Fatalf("err = %v, want a complaint about the query id", err)
	}
}

func shortenDNS(t *testing.T) {
	t.Helper()
	timeout, attempts := dnsTimeout, dnsAttempts
	dnsTimeout, dnsAttempts = 150*time.Millisecond, 2
	t.Cleanup(func() { dnsTimeout, dnsAttempts = timeout, attempts })
}
