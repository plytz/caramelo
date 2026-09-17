package vpnclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"time"
)

type userspaceDialer struct {
	dev *wgDevice
}

var _ Dialer = (*userspaceDialer)(nil)

func (d *userspaceDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	ap, err := d.resolveAddrPort(ctx, address)
	if err != nil {
		return nil, err
	}
	return d.dev.dial(ctx, network, ap)
}

func (d *userspaceDialer) Resolve(ctx context.Context, name string) ([]netip.Addr, error) {
	resolver, err := netip.ParseAddrPort(d.dev.rec.Resolver())
	if err != nil {
		return nil, fmt.Errorf("the resolver address of %s: %w", d.dev.rec.Machine, err)
	}
	return lookupA(ctx, func(ctx context.Context) (net.Conn, error) {
		return d.dev.dial(ctx, "udp", resolver)
	}, name)
}

func (d *userspaceDialer) Close() error {
	d.dev.release()
	return nil
}

func (d *userspaceDialer) MachineAddr() string { return machineAddr(d.dev.rec) }

func (d *userspaceDialer) resolveAddrPort(ctx context.Context, address string) (netip.AddrPort, error) {
	return resolveAddrPort(ctx, d, address)
}

type hostDialer struct {
	rec Record
	d   net.Dialer
}

var _ Dialer = (*hostDialer)(nil)

func (d *hostDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	ap, err := resolveAddrPort(ctx, d, address)
	if err != nil {
		return nil, err
	}
	dialCtx, cancel := withDialTimeout(ctx)
	defer cancel()
	c, err := d.d.DialContext(dialCtx, network, ap.String())
	if err != nil {
		return nil, d.dialError(ctx, ap, err)
	}
	return c, nil
}

func (d *hostDialer) dialError(ctx context.Context, ap netip.AddrPort, err error) error {
	if !errors.Is(err, context.DeadlineExceeded) && !os.IsTimeout(err) {
		return fmt.Errorf("connect to %s through the tunnel to %s: %w", ap, d.rec.Machine, err)
	}

	askCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serviceTimeout)
	defer cancel()
	resp, serr := serviceCall(askCtx, serviceRequest{Op: "status", Machine: d.rec.Machine})
	if serr != nil || resp == nil {
		return silenceError(d.rec, ap, time.Time{}, false)
	}
	return silenceError(d.rec, ap, resp.LastHandshake, true)
}

func silenceError(rec Record, ap netip.AddrPort, last time.Time, known bool) error {
	switch {
	case !known:
		return fmt.Errorf("connect to %s through the tunnel to %s: nothing answered within %s; %s",
			ap, rec.Machine, dialTimeout, firewallHint(rec))
	case last.IsZero():
		return fmt.Errorf("no handshake with %s after %s; is UDP %s reachable, and is this peer still admitted? "+
			"(check 'caramelo peer list' on the machine). %s",
			rec.Machine, dialTimeout, rec.Endpoint, firewallHint(rec))
	default:
		return fmt.Errorf("no answer from %s within %s; the last handshake was %s ago, so this peer "+
			"may no longer be admitted — check 'caramelo peer list' on the machine, and that UDP %s is reachable. %s",
			rec.Machine, dialTimeout, time.Since(last).Round(time.Second), rec.Endpoint, firewallHint(rec))
	}
}

func firewallHint(rec Record) string {
	return fmt.Sprintf("a firewall on the machine or a security group in front of it dropping UDP %s "+
		"looks exactly like this, and 'caramelo server probe %s' from here says which",
		rec.Endpoint, rec.Machine)
}

func (d *hostDialer) Resolve(ctx context.Context, name string) ([]netip.Addr, error) {
	resolver := d.rec.Resolver()
	if resolver == "" {
		return nil, fmt.Errorf("no resolver known for %s; run 'caramelo vpn up'", d.rec.Machine)
	}
	return lookupA(ctx, func(ctx context.Context) (net.Conn, error) {
		var dl net.Dialer
		dl.Timeout = dnsTimeout
		return dl.DialContext(ctx, "udp", resolver)
	}, name)
}

func (d *hostDialer) Close() error { return nil }

func (d *hostDialer) MachineAddr() string { return machineAddr(d.rec) }

func machineAddr(rec Record) string {
	if !rec.MachineIP.IsValid() {
		return ""
	}
	return rec.MachineIP.String()
}

func resolveAddrPort(ctx context.Context, d Dialer, address string) (netip.AddrPort, error) {
	if ap, err := netip.ParseAddrPort(address); err == nil {
		return ap, nil
	}
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("address %q: %w", address, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return netip.AddrPort{}, fmt.Errorf("address %q: bad port %q", address, portStr)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return netip.AddrPortFrom(ip, uint16(port)), nil
	}
	addrs, err := d.Resolve(ctx, host)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if len(addrs) == 0 {
		return netip.AddrPort{}, fmt.Errorf("no address for %s", host)
	}
	return netip.AddrPortFrom(addrs[0], uint16(port)), nil
}

func verify(ctx context.Context, dev *wgDevice, timeout time.Duration) (time.Time, error) {
	if hs, err := dev.lastHandshake(); err == nil && !hs.IsZero() {
		return hs, nil
	}
	probe, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ap, err := netip.ParseAddrPort(dev.rec.APIAddr())
		if err != nil {
			return
		}
		c, err := dev.dial(probe, "tcp", ap)
		if err == nil {
			_ = c.Close()
		}
	}()

	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if hs, err := dev.lastHandshake(); err == nil && !hs.IsZero() {
				return hs, nil
			}
		case <-done:
			if hs, err := dev.lastHandshake(); err == nil && !hs.IsZero() {
				return hs, nil
			}
			return time.Time{}, noHandshake(dev.rec, timeout)
		case <-ctx.Done():
			return time.Time{}, ctx.Err()
		}
	}
}

func noHandshake(rec Record, timeout time.Duration) error {
	return fmt.Errorf("no handshake with %s after %s: nothing came back. "+
		"Check that UDP %s is reachable from here and that this peer is still admitted "+
		"('caramelo peer list' on the machine); a revoked or unknown key gets no reply at all, and %s",
		rec.Machine, timeout, rec.Endpoint, firewallHint(rec))
}
