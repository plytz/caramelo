package vpnclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/plytz/caramelo/internal/testutil"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"github.com/plytz/caramelo/internal/vpn"
)

type fakeHostNet struct {
	mu         sync.Mutex
	configured []string
	tornDown   []string
	err        error
}

var _ HostNet = (*fakeHostNet)(nil)

func (h *fakeHostNet) Configure(_ context.Context, iface string, rec Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil {
		return h.err
	}
	h.configured = append(h.configured, iface+" "+rec.IP.String()+" "+rec.Subnet.String())
	return nil
}

func (h *fakeHostNet) Teardown(_ context.Context, iface string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tornDown = append(h.tornDown, iface)
	return nil
}

func (h *fakeHostNet) calls() ([]string, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.configured...), append([]string(nil), h.tornDown...)
}

func TestServiceLifecycle(t *testing.T) {
	keys, records := stores(t)
	const machine = "svc"
	kp, _, err := keys.Ensure(machine)
	if err != nil {
		t.Fatal(err)
	}
	peerIP := netip.MustParseAddr("10.86.0.2")
	boxIP := netip.MustParseAddr("10.86.0.1")
	b := startBox(t, kp.Public, peerIP, boxIP)
	if err := records.Save(b.record(machine, peerIP)); err != nil {
		t.Fatal(err)
	}
	host := &fakeHostNet{}
	socket := filepath.Join(testutil.ShortDir(t), "vpn.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- RunService(ctx, ServiceOptions{
			Machine: machine,
			Keys:    keys,
			Records: records,
			Socket:  socket,
			Host:    host,

			TUN: func(name string, mtu int) (tun.Device, error) {
				dev, _, err := netstack.CreateNetTUN([]netip.Addr{peerIP}, nil, mtu)
				return dev, err
			},
		})
	}()

	waitFor(t, 10*time.Second, "the service to answer its socket", func() bool {
		_, err := serviceCallAt(ctx, socket, serviceRequest{Op: "status"})
		return err == nil
	})
	resp, err := serviceCallAt(ctx, socket, serviceRequest{Op: "status"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Running || resp.Machine != machine {
		t.Fatalf("status = %+v, want the tunnel running for %s", resp, machine)
	}
	configured, _ := host.calls()
	if len(configured) != 1 || !strings.Contains(configured[0], peerIP.String()) {
		t.Fatalf("the interface was not configured: %v", configured)
	}

	if _, err := serviceCallAt(ctx, socket, serviceRequest{Op: "status", Machine: "other"}); err == nil {
		t.Error("the service answered for a machine it does not carry")
	}

	if _, err := serviceCallAt(ctx, socket, serviceRequest{Op: "down"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the service returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the service did not stop after 'down'")
	}
	_, tornDown := host.calls()
	if len(tornDown) != 1 {
		t.Errorf("Teardown was called %d times, want once", len(tornDown))
	}
}

func TestServiceNeedsARecord(t *testing.T) {
	keys, records := stores(t)
	err := RunService(context.Background(), ServiceOptions{
		Machine: "unknown", Keys: keys, Records: records,
		Socket: filepath.Join(testutil.ShortDir(t), "vpn.sock"), Host: &fakeHostNet{},
	})
	if err == nil || !strings.Contains(err.Error(), ErrNoKey.Error()) {
		t.Fatalf("err = %v, want ErrNoKey", err)
	}
}

func TestServiceRefusesARecordThatWouldRerouteTheCommander(t *testing.T) {
	keys, records := stores(t)
	const machine = "hostile"
	if _, _, err := keys.Ensure(machine); err != nil {
		t.Fatal(err)
	}
	rec := testRecord(machine)
	rec.Subnet = netip.MustParsePrefix("0.0.0.0/0")
	if err := records.Save(rec); err != nil {
		t.Fatal(err)
	}
	host := &fakeHostNet{}
	err := RunService(context.Background(), ServiceOptions{
		Machine: machine, Keys: keys, Records: records,
		Socket: filepath.Join(testutil.ShortDir(t), "vpn.sock"), Host: host,
		TUN: func(string, int) (tun.Device, error) {
			t.Error("an interface was created for a record claiming the default route")
			return nil, errors.New("no")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to route") {
		t.Fatalf("err = %v, want the service to refuse the record", err)
	}
	if configured, _ := host.calls(); len(configured) != 0 {
		t.Errorf("the host was configured anyway: %v", configured)
	}
}

func TestServiceNeedsAMachine(t *testing.T) {
	if err := RunService(context.Background(), ServiceOptions{}); err == nil {
		t.Fatal("the service started with no machine")
	}
}

func TestCommandHostNetConfigure(t *testing.T) {
	run := &recordingRun{}
	h := &commandHostNet{GOOS: "linux", Run: run.run, Raise: func() error { return nil }}
	rec := testRecord("worker1")
	if err := h.Configure(context.Background(), "caramelo0", rec); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ip address replace 10.86.0.2/32 dev caramelo0",
		"ip link set dev caramelo0 up",
		"ip route replace 10.86.0.0/16 dev caramelo0",
		"resolvectl dns caramelo0 10.86.0.1",
		"resolvectl domain caramelo0 ~" + vpn.Domain,
	} {
		if !strings.Contains(run.joined(), want) {
			t.Errorf("Configure did not run %q; it ran:\n%s", want, run.joined())
		}
	}

	run2 := &recordingRun{fail: map[string]error{"resolvectl": errNoResolvectl}}
	var log strings.Builder
	h2 := &commandHostNet{GOOS: "linux", Run: run2.run, Log: &log, Raise: func() error { return nil }}
	if err := h2.Configure(context.Background(), "caramelo0", rec); err != nil {
		t.Fatalf("a missing resolvectl broke the tunnel: %v", err)
	}
	if !strings.Contains(log.String(), "split DNS") {
		t.Errorf("nothing was said about split DNS:\n%s", log.String())
	}

	if err := (&commandHostNet{GOOS: "windows", Run: run.run, Raise: func() error { return nil }}).Configure(context.Background(), "x", rec); err == nil {
		t.Error("an unsupported platform configured an interface")
	}
}

func serviceCallAt(ctx context.Context, socket string, req serviceRequest) (*serviceResponse, error) {
	old := socketPathForTest
	socketPathForTest = socket
	defer func() { socketPathForTest = old }()
	return serviceCall(ctx, req)
}

var errNoResolvectl = errNotFound("resolvectl")

type errNotFound string

func (e errNotFound) Error() string { return string(e) + ": not found" }

func TestTransparentUpAndDownDriveTheService(t *testing.T) {
	keys, records := stores(t)
	const machine = "transparent"
	if _, _, err := keys.Ensure(machine); err != nil {
		t.Fatal(err)
	}
	rec := testRecord(machine)
	if err := records.Save(rec); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(testutil.ShortDir(t), "vpn.sock")
	old := socketPathForTest
	socketPathForTest = socket
	t.Cleanup(func() { socketPathForTest = old })

	inst := &fakeInstaller{socket: socket, machine: machine, t: t}
	c, err := NewWith(Options{Control: &fakeControl{}, Keys: keys, Records: records, Installer: inst})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.(*client).transparentUp(context.Background(), rec); err != nil {
		t.Fatalf("transparent up: %v", err)
	}
	if inst.starts != 1 {
		t.Errorf("the service was started %d times, want once", inst.starts)
	}
	st, err := c.Status(context.Background(), machine)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != ModeTransparent {
		t.Errorf("mode = %q, want %q", st.Mode, ModeTransparent)
	}

	if err := c.Down(context.Background(), machine); err != nil {
		t.Fatalf("down: %v", err)
	}
	if inst.stops != 1 {
		t.Errorf("the service was stopped %d times, want once", inst.stops)
	}
}

type fakeInstaller struct {
	t             *testing.T
	socket        string
	machine       string
	starts, stops int
	ln            net.Listener
	stop          chan struct{}
}

var (
	_ Installer = (*fakeInstaller)(nil)
	_ Starter   = (*fakeInstaller)(nil)
)

func (f *fakeInstaller) Install(context.Context, InstallOptions) error { return nil }
func (f *fakeInstaller) Uninstall(context.Context) error               { return nil }
func (f *fakeInstaller) Installed(context.Context) (bool, error)       { return true, nil }

func (f *fakeInstaller) Start(context.Context) error {
	f.starts++
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		return err
	}
	f.ln, f.stop = ln, make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var req serviceRequest
			line, _ := bufio.NewReader(io.LimitReader(conn, 1<<16)).ReadBytes('\n')
			_ = json.Unmarshal(line, &req)
			writeResponse(conn, serviceResponse{Machine: f.machine, Running: req.Op != "down"})
			_ = conn.Close()
		}
	}()
	return nil
}

func (f *fakeInstaller) Stop(context.Context) error {
	f.stops++
	if f.ln != nil {
		_ = f.ln.Close()
		f.ln = nil
	}
	_ = os.Remove(f.socket)
	return nil
}

func TestTransparentUpBringsTheInterfaceUpBeforeTalking(t *testing.T) {
	keys, records := stores(t)
	const machine = "transparent-first"
	if _, _, err := keys.Ensure(machine); err != nil {
		t.Fatal(err)
	}
	if err := records.Save(testRecord(machine)); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(testutil.ShortDir(t), "vpn.sock")
	old := socketPathForTest
	socketPathForTest = socket
	t.Cleanup(func() { socketPathForTest = old })

	inst := &fakeInstaller{socket: socket, machine: machine, t: t}

	control := &orderedControl{}
	c, err := NewWith(Options{Control: control, Keys: keys, Records: records, Installer: inst})
	if err != nil {
		t.Fatal(err)
	}
	control.started = func() bool { return inst.starts > 0 }

	_, _ = c.Up(context.Background(), UpRequest{Machine: machine, Transparent: true})

	if inst.starts == 0 {
		t.Fatal("the service was never started; the interface has to come up before anything is asked")
	}
	if len(control.calls) == 0 {
		t.Fatal("the machine was never asked anything")
	}
	if !control.firstCallAfterStart {
		t.Errorf("the machine was asked %q before the interface was up; that is a second device on one key",
			control.calls[0])
	}
}

type orderedControl struct {
	calls               []string
	started             func() bool
	firstCallAfterStart bool
}

var _ Control = (*orderedControl)(nil)

func (o *orderedControl) Run(_ context.Context, _ string, argv []string, _, stderr io.Writer) (int, error) {
	if len(o.calls) == 0 && o.started != nil {
		o.firstCallAfterStart = o.started()
	}
	o.calls = append(o.calls, strings.Join(argv, " "))
	fmt.Fprintln(stderr, "no machine in this test")
	return 1, nil
}
