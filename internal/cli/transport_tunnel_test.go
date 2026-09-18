package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/plytz/caramelo/internal/testutil"
	"net"
	"strings"
	"sync"
	"testing"

	"charm.land/ssh"

	"github.com/plytz/caramelo/internal/remote"
)

func installTunnel(t *testing.T, addr string, err error) *tunnelProbe {
	t.Helper()
	p := &tunnelProbe{addr: addr, err: err}
	remote.TunnelDialer = func(_ context.Context, _ string, target remote.Target) (remote.Dialer, error) {
		p.mu.Lock()
		p.asked = append(p.asked, target)
		p.mu.Unlock()
		if p.err != nil {
			return nil, p.err
		}
		return p, nil
	}
	t.Cleanup(func() { remote.TunnelDialer = nil })
	return p
}

type tunnelProbe struct {
	addr string
	err  error

	mu      sync.Mutex
	asked   []remote.Target
	dialled []string
}

func (p *tunnelProbe) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	p.mu.Lock()
	p.dialled = append(p.dialled, network+" "+address)
	p.mu.Unlock()
	var d net.Dialer
	return d.DialContext(ctx, network, p.addr)
}

func (p *tunnelProbe) MachineAddr() string { return "10.86.0.1" }

func tunnelDaemon(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &ssh.Server{Handler: func(sess ssh.Session) {
		fmt.Fprintf(sess, "argv=%s\n", strings.Join(sess.Command(), "|"))
		_ = sess.Exit(0)
	}}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func TestResolveTransportPrefersTheTunnel(t *testing.T) {
	noCommanderConfig(t)
	useSystemConfigDir(t, t.TempDir())
	probe := installTunnel(t, "127.0.0.1:1", nil)

	got, err := resolveTransport(context.Background(), &app{machine: "alex@box:2222"})
	if err != nil {
		t.Fatal(err)
	}
	if got.kind != kindTunnel {
		t.Fatalf("kind = %q, want %q", got.kind, kindTunnel)
	}
	if got.dialer == nil {
		t.Error("the tunnel transport carries no dialer")
	}
	if len(probe.asked) != 1 || probe.asked[0].Host != "box" || probe.asked[0].Port != 2222 {
		t.Errorf("the tunnel was asked for %v, want the target", probe.asked)
	}
	if !strings.Contains(got.String(), "tunnel") {
		t.Errorf("String() = %q, want it to name the tunnel", got.String())
	}
}

func TestResolveTransportFallsBackToSSH(t *testing.T) {
	noCommanderConfig(t)
	useSystemConfigDir(t, t.TempDir())
	installTunnel(t, "", remote.ErrNoTunnel)

	got, err := resolveTransport(context.Background(), &app{machine: "box"})
	if err != nil {
		t.Fatal(err)
	}
	if got.kind != kindSSH {
		t.Fatalf("kind = %q, want %q", got.kind, kindSSH)
	}
}

func TestResolveTransportReportsABrokenTunnel(t *testing.T) {
	noCommanderConfig(t)
	useSystemConfigDir(t, t.TempDir())
	boom := errors.New("no handshake with box after 10s")
	installTunnel(t, "", boom)

	_, err := resolveTransport(context.Background(), &app{machine: "box"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the tunnel's own error", err)
	}
}

func TestResolveTransportSocketIgnoresTheTunnel(t *testing.T) {
	noCommanderConfig(t)
	runDir := testutil.ShortDir(t)
	useSystemConfigDir(t, writeServerConfig(t, runDir))
	makeSocket(t, runDir, false)
	probe := installTunnel(t, "127.0.0.1:1", nil)

	got, err := resolveTransport(context.Background(), &app{})
	if err != nil {
		t.Fatal(err)
	}
	if got.kind != kindSocket {
		t.Fatalf("kind = %q, want %q", got.kind, kindSocket)
	}
	if len(probe.asked) != 0 {
		t.Errorf("the socket asked for a tunnel: %v", probe.asked)
	}
}

func TestForwardThroughTheTunnel(t *testing.T) {
	noCommanderConfig(t)
	useSystemConfigDir(t, t.TempDir())
	probe := installTunnel(t, tunnelDaemon(t), nil)

	var stdout, stderr strings.Builder
	a := &app{args: []string{"env", "list"}, machine: "box", stdout: &stdout, stderr: &stderr}
	code, err := forwardImpl(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if code != ExitOK {
		t.Errorf("exit %d, want %d", code, ExitOK)
	}
	if got := stdout.String(); got != "argv=env|list\n" {
		t.Errorf("stdout = %q, want the argv the daemon saw", got)
	}
	if len(probe.dialled) != 1 || probe.dialled[0] != "tcp 10.86.0.1:4022" {
		t.Errorf("dialled %v, want the machine's address inside the tunnel", probe.dialled)
	}
}
