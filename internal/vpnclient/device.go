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
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/vpn"
)

const defaultAPIPort = serverconfig.DefaultSSHPort

const dialTimeout = 10 * time.Second

const keepalive = 25

type wgDevice struct {
	rec Record

	mu     sync.Mutex
	refs   int
	dev    *device.Device
	tnet   *netstack.Net
	closed bool
}

func openDevice(rec Record, private string, logw io.Writer) (*wgDevice, error) {
	if !rec.Valid() {
		return nil, fmt.Errorf("the record for %s is incomplete; run 'caramelo vpn up'", rec.Machine)
	}
	cfg, err := ipcConfig(rec, private)
	if err != nil {
		return nil, err
	}
	tdev, tnet, err := netstack.CreateNetTUN([]netip.Addr{rec.IP}, []netip.Addr{rec.MachineIP}, vpn.MTU)
	if err != nil {
		return nil, fmt.Errorf("create the userspace network stack: %w", err)
	}
	level := device.LogLevelError
	if logw == nil {
		logw = io.Discard
		level = device.LogLevelSilent
	}
	dev := device.NewDevice(tdev, conn.NewDefaultBind(), newLogger(level, logw))
	if err := dev.IpcSet(cfg); err != nil {
		dev.Close()
		return nil, fmt.Errorf("configure the tunnel to %s: %w", rec.Machine, err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("bring the tunnel to %s up: %w", rec.Machine, err)
	}
	return &wgDevice{rec: rec, refs: 1, dev: dev, tnet: tnet}, nil
}

func newLogger(level int, w io.Writer) *device.Logger {
	l := device.NewLogger(level, "")
	if level == device.LogLevelSilent {
		return l
	}
	prefix := func(format string, args ...any) {
		fmt.Fprintf(w, "tunnel: "+format+"\n", args...)
	}
	l.Verbosef = func(format string, args ...any) {}
	l.Errorf = prefix
	return l
}

func ipcConfig(rec Record, private string) (string, error) {
	priv, err := KeyHex(private)
	if err != nil {
		return "", fmt.Errorf("this computer's private key: %w", err)
	}
	pub, err := KeyHex(rec.MachineKey)
	if err != nil {
		return "", fmt.Errorf("the public key of %s: %w", rec.Machine, err)
	}
	var b strings.Builder
	b.WriteString("private_key=" + priv + "\n")

	b.WriteString("listen_port=0\n")
	b.WriteString("replace_peers=true\n")
	b.WriteString("public_key=" + pub + "\n")
	b.WriteString("endpoint=" + rec.Endpoint + "\n")
	b.WriteString("persistent_keepalive_interval=" + strconv.Itoa(keepalive) + "\n")
	b.WriteString("allowed_ip=" + rec.Subnet.String() + "\n")
	return b.String(), nil
}

func (d *wgDevice) acquire() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return false
	}
	d.refs++
	return true
}

func (d *wgDevice) shut() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.closed = true
	d.refs = 0
	d.dev.Close()
}

func (d *wgDevice) release() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.refs--
	if d.refs > 0 {
		return
	}
	d.closed = true
	d.dev.Close()
}

func (d *wgDevice) dial(ctx context.Context, network string, ap netip.AddrPort) (net.Conn, error) {
	ctx, cancel := withDialTimeout(ctx)
	defer cancel()
	switch {
	case strings.HasPrefix(network, "tcp"):
		c, err := d.tnet.DialContextTCPAddrPort(ctx, ap)
		if err != nil {
			return nil, d.dialError(ap, err)
		}
		return c, nil
	case strings.HasPrefix(network, "udp"):
		c, err := d.tnet.DialUDPAddrPort(netip.AddrPort{}, ap)
		if err != nil {
			return nil, d.dialError(ap, err)
		}
		return c, nil
	}
	return nil, fmt.Errorf("the tunnel carries tcp and udp, not %q", network)
}

func (d *wgDevice) dialError(ap netip.AddrPort, err error) error {
	if !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("connect to %s through the tunnel to %s: %w", ap, d.rec.Machine, err)
	}
	hs, herr := d.lastHandshake()
	return silenceError(d.rec, ap, hs, herr == nil)
}

func (d *wgDevice) lastHandshake() (time.Time, error) {
	var b strings.Builder
	if err := d.dev.IpcGetOperation(&b); err != nil {
		return time.Time{}, fmt.Errorf("read the tunnel's state: %w", err)
	}
	return parseHandshake(b.String()), nil
}

func parseHandshake(doc string) time.Time {
	var sec, nsec int64
	for _, line := range strings.Split(doc, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "last_handshake_time_sec":
			sec = n
		case "last_handshake_time_nsec":
			nsec = n
		}
	}
	if sec == 0 && nsec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, nsec)
}

func withDialTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, dialTimeout)
}

type devicePool struct {
	mu      sync.Mutex
	devices map[string]*wgDevice
}

var pool = &devicePool{devices: map[string]*wgDevice{}}

func (p *devicePool) get(rec Record, keys KeyStore, log io.Writer) (*wgDevice, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if dev, ok := p.devices[rec.Machine]; ok && dev.acquire() {
		return dev, nil
	}
	if keys == nil {
		keys = &FileKeyStore{}
	}
	kp, err := keys.Load(rec.Machine)
	if err != nil {
		return nil, err
	}
	dev, err := openDevice(rec, kp.Private, log)
	if err != nil {
		return nil, err
	}

	dev.refs++
	p.devices[rec.Machine] = dev
	return dev, nil
}

func (p *devicePool) close(machine string) {
	p.mu.Lock()
	dev := p.devices[machine]
	delete(p.devices, machine)
	p.mu.Unlock()
	if dev != nil {
		dev.release()
	}
}

func CloseTunnels() {
	pool.mu.Lock()
	devs := make([]*wgDevice, 0, len(pool.devices))
	for machine, dev := range pool.devices {
		devs = append(devs, dev)
		delete(pool.devices, machine)
	}
	pool.mu.Unlock()
	for _, dev := range devs {
		dev.shut()
	}
}
