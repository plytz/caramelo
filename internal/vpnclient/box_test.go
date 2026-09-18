package vpnclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"github.com/plytz/caramelo/internal/vpn"
)

type box struct {
	t        *testing.T
	key      KeyPair
	endpoint string
	tnet     *netstack.Net
	dev      *device.Device
	addrs    []netip.Addr
	zone     map[string]netip.Addr
}

func startBox(t *testing.T, peerPublic string, peerIP netip.Addr, addrs ...netip.Addr) *box {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	tdev, tnet, err := netstack.CreateNetTUN(addrs, nil, vpn.MTU)
	if err != nil {
		t.Fatal(err)
	}
	dev := device.NewDevice(tdev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, "box "))
	priv, err := KeyHex(key.Private)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := KeyHex(peerPublic)
	if err != nil {
		t.Fatal(err)
	}
	cfg := "private_key=" + priv + "\n" +
		"listen_port=0\n" +
		"public_key=" + pub + "\n" +
		"allowed_ip=" + peerIP.String() + "/32\n"
	if err := dev.IpcSet(cfg); err != nil {
		t.Fatal(err)
	}
	if err := dev.Up(); err != nil {
		t.Fatal(err)
	}
	b := &box{t: t, key: key, tnet: tnet, dev: dev, addrs: addrs, zone: map[string]netip.Addr{}}
	b.endpoint = "127.0.0.1:" + strconv.Itoa(b.listenPort())
	t.Cleanup(func() { dev.Close() })
	return b
}

func (b *box) listenPort() int {
	var sb strings.Builder
	if err := b.dev.IpcGetOperation(&sb); err != nil {
		b.t.Fatal(err)
	}
	for _, line := range strings.Split(sb.String(), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok && key == "listen_port" {
			port, err := strconv.Atoi(value)
			if err != nil {
				b.t.Fatalf("listen_port=%q: %v", value, err)
			}
			return port
		}
	}
	b.t.Fatal("the device reported no listen port")
	return 0
}

func (b *box) serveEcho(ap netip.AddrPort, proto vpn.Protocol, prefix string) {
	b.t.Helper()
	switch proto {
	case vpn.TCP:
		ln, err := b.tnet.ListenTCPAddrPort(ap)
		if err != nil {
			b.t.Fatal(err)
		}
		b.t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer func() { _ = c.Close() }()
					buf := make([]byte, 4096)
					for {
						n, err := c.Read(buf)
						if n > 0 {
							if _, werr := c.Write([]byte(prefix + string(buf[:n]))); werr != nil {
								return
							}
						}
						if err != nil {
							return
						}
					}
				}()
			}
		}()
	case vpn.UDP:
		pc, err := b.tnet.ListenUDPAddrPort(ap)
		if err != nil {
			b.t.Fatal(err)
		}
		b.t.Cleanup(func() { _ = pc.Close() })
		go func() {
			buf := make([]byte, 4096)
			for {
				n, from, err := pc.ReadFrom(buf)
				if err != nil {
					return
				}
				if _, err := pc.WriteTo([]byte(prefix+string(buf[:n])), from); err != nil {
					return
				}
			}
		}()
	}
}

func (b *box) serveRequestResponse(ap netip.AddrPort, prefix string) {
	b.t.Helper()
	ln, err := b.tnet.ListenTCPAddrPort(ap)
	if err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				req, err := io.ReadAll(c)
				if err != nil {
					return
				}
				_, _ = c.Write([]byte(prefix + string(req)))
			}()
		}
	}()
}

func (b *box) serveDNS(at netip.Addr) {
	b.t.Helper()
	pc, err := b.tnet.ListenUDPAddrPort(netip.AddrPortFrom(at, vpn.ResolverPort))
	if err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			resp, err := b.answer(buf[:n])
			if err != nil {
				continue
			}
			if _, err := pc.WriteTo(resp, from); err != nil {
				return
			}
		}
	}()
}

func (b *box) answer(query []byte) ([]byte, error) {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil {
		return nil, err
	}
	q, err := p.Question()
	if err != nil {
		return nil, err
	}
	name := vpn.Normalize(q.Name.String())
	ip, ok := b.zone[name]
	rcode := dnsmessage.RCodeSuccess
	if !ok || q.Type != dnsmessage.TypeA {
		rcode = dnsmessage.RCodeNameError
	}
	bl := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: h.ID, Response: true, Authoritative: true, RCode: rcode,
	})
	if err := bl.StartQuestions(); err != nil {
		return nil, err
	}
	if err := bl.Question(q); err != nil {
		return nil, err
	}
	if ok && rcode == dnsmessage.RCodeSuccess {
		if err := bl.StartAnswers(); err != nil {
			return nil, err
		}
		err := bl.AResource(dnsmessage.ResourceHeader{
			Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60,
		}, dnsmessage.AResource{A: ip.As4()})
		if err != nil {
			return nil, err
		}
	}
	return bl.Finish()
}

func (b *box) record(machine string, peerIP netip.Addr) Record {
	return Record{
		Fleet:       machine,
		MachineName: machine,
		Endpoint:    b.endpoint,
		MachineKey:  b.key.Public,
		Subnet:      netip.MustParsePrefix("10.86.0.0/16"),
		MachineIP:   b.addrs[0],
		PeerName:    "test-peer",
		IP:          peerIP,
		PublicKey:   "",
		APIPort:     defaultAPIPort,
	}
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}

func dialLocal(t *testing.T, network, address string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout(network, address, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s %s: %v", network, address, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	return c
}

func roundTrip(t *testing.T, c net.Conn, msg string) string {
	t.Helper()
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(buf[:n])
}

type fakeControl struct {
	answers map[string]string
	codes   map[string]int
	calls   []string
	err     error
}

var _ Control = (*fakeControl)(nil)

func (f *fakeControl) Run(ctx context.Context, machine string, argv []string, stdout, stderr io.Writer) (int, error) {
	key := strings.Join(argv, " ")
	f.calls = append(f.calls, machine+": "+key)
	if f.err != nil {
		return 1, f.err
	}
	for prefix, answer := range f.answers {
		if strings.HasPrefix(key, prefix) {
			fmt.Fprint(stdout, answer)
			return f.codes[prefix], nil
		}
	}
	fmt.Fprintf(stderr, "unexpected command %q\n", key)
	return 1, nil
}

func TestStatusOfAnUnjoinedMachineStillHasAKey(t *testing.T) {
	dir := t.TempDir()
	keys := &FileKeyStore{Dir: dir}
	c, err := NewWith(Options{
		Control: ControlFunc(func(context.Context, string, []string, io.Writer, io.Writer) (int, error) {
			return 1, errors.New("unreachable")
		}),
		Keys:      keys,
		Records:   &FileRecordStore{Dir: dir},
		Installer: notInstalled{},
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := c.Status(context.Background(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != ModeOff {
		t.Errorf("mode = %q, want off", st.Mode)
	}
	if st.PublicKey == "" {
		t.Fatal("no public key: an unjoined commander cannot be admitted by a peer")
	}
	kp, err := keys.Load("box")
	if err != nil {
		t.Fatal(err)
	}
	if kp.Public != st.PublicKey {
		t.Errorf("status reported %q, the key store holds %q", st.PublicKey, kp.Public)
	}

	again, err := c.Status(context.Background(), "box")
	if err != nil || again.PublicKey != st.PublicKey {
		t.Errorf("a second status reported %q (err %v), want the same key", again.PublicKey, err)
	}
}
