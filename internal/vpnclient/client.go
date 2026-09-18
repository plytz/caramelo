package vpnclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vpn"
)

var verifyTimeout = 10 * time.Second

type Control interface {
	Run(ctx context.Context, machine string, argv []string, stdout, stderr io.Writer) (int, error)
}

type ControlFunc func(ctx context.Context, machine string, argv []string, stdout, stderr io.Writer) (int, error)

func (f ControlFunc) Run(ctx context.Context, machine string, argv []string, stdout, stderr io.Writer) (int, error) {
	return f(ctx, machine, argv, stdout, stderr)
}

type Options struct {
	Control Control

	Keys    KeyStore
	Records RecordStore

	Installer Installer

	Log io.Writer
}

type client struct {
	opts Options
}

var _ Client = (*client)(nil)

func NewWith(opts Options) (Client, error) {
	if opts.Control == nil {
		return nil, errors.New("vpnclient: no way to reach the machine (Control is nil)")
	}
	if opts.Keys == nil {
		opts.Keys = &FileKeyStore{}
	}
	if opts.Records == nil {
		opts.Records = &FileRecordStore{}
	}
	if opts.Installer == nil {
		opts.Installer = NewInstaller()
	}
	return &client{opts: opts}, nil
}

func peerNameFor(req UpRequest, recorded Record) string {
	for _, name := range []string{req.PeerName, recorded.PeerName, req.Identity} {
		if n := strings.TrimSpace(name); n != "" {
			return n
		}
	}
	return DefaultPeerName()
}

func (c *client) Up(ctx context.Context, req UpRequest) (*State, error) {
	machine := strings.TrimSpace(req.Machine)
	if machine == "" {
		return nil, errors.New("no fleet: pass --fleet NAME (or CARAMELO_FLEET), or set commander.default_fleet in the commander config")
	}
	kp, created, err := c.opts.Keys.Ensure(machine)
	if err != nil {
		return nil, err
	}
	prev, prevErr := c.opts.Records.Load(machine)
	if prevErr != nil {
		prev = Record{}
	}
	if req.Transparent {
		if err := c.requireInstalled(ctx); err != nil {
			return nil, err
		}

		if prev.Valid() {
			if err := c.transparentUp(ctx, prev); err != nil {
				return nil, err
			}
		}
	}
	st, err := c.machineStatus(ctx, machine)
	if err != nil {
		return nil, err
	}
	peerName := peerNameFor(req, prev)
	peer, err := c.addPeer(ctx, machine, peerName, kp.Public)
	if err != nil {
		return nil, err
	}
	rec, err := RecordFrom(machine, peerName, kp.Public, st, peer)
	if err != nil {
		return nil, err
	}
	rec.LastHandshake = prev.LastHandshake
	rec.UpdatedAt = time.Now().UTC()
	if err := c.opts.Records.Save(rec); err != nil {
		return nil, err
	}
	_ = created

	if req.Transparent {

		if err := c.transparentUp(ctx, rec); err != nil {
			return nil, err
		}
		return c.Status(ctx, machine)
	}

	dev, err := c.device(rec)
	if err != nil {
		return nil, err
	}
	defer dev.release()
	hs, err := verify(ctx, dev, verifyTimeout)
	if err != nil {
		return nil, err
	}
	rec.LastHandshake = hs
	if err := c.opts.Records.Save(rec); err != nil {
		return nil, err
	}
	return c.state(ctx, rec), nil
}

func (c *client) transparentUp(ctx context.Context, rec Record) error {
	if err := serviceUp(ctx, rec); err == nil {
		return nil
	}
	starter, ok := c.opts.Installer.(Starter)
	if !ok {
		return ErrNotInstalled
	}
	if err := starter.Start(ctx); err != nil {
		return fmt.Errorf("start the transparent-mode service: %w", err)
	}

	deadline := time.Now().Add(serviceStartTimeout)
	var err error
	for time.Now().Before(deadline) {
		if err = serviceUp(ctx, rec); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the transparent-mode service did not come up: %w", err)
}

var serviceStartTimeout = 10 * time.Second

func (c *client) Down(ctx context.Context, machine string) error {
	rec, err := c.opts.Records.Load(machine)
	if err != nil {
		return err
	}
	pool.close(machine)
	installed, _ := c.opts.Installer.Installed(ctx)
	if !installed {
		return nil
	}

	downErr := serviceDown(ctx, rec)
	if starter, ok := c.opts.Installer.(Starter); ok {
		if err := starter.Stop(ctx); err != nil && downErr == nil {
			return fmt.Errorf("stop the transparent-mode service: %w", err)
		}
		return nil
	}
	return downErr
}

func (c *client) Status(ctx context.Context, machine string) (*State, error) {
	rec, err := c.opts.Records.Load(machine)
	if errors.Is(err, ErrNoKey) {
		installed, _ := c.opts.Installer.Installed(ctx)
		st := &State{Mode: ModeOff, Machine: machine, Installed: installed}

		if kp, _, kerr := c.opts.Keys.Ensure(machine); kerr == nil {
			st.PublicKey = kp.Public
		}
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	return c.state(ctx, rec), nil
}

func (c *client) Dialer(ctx context.Context, machine string) (Dialer, error) {
	rec, err := c.opts.Records.Load(machine)
	if err != nil {
		return nil, err
	}
	if installed, _ := c.opts.Installer.Installed(ctx); installed {
		if up, _ := serviceRunning(ctx, rec); up {
			return &hostDialer{rec: rec}, nil
		}
	}
	dev, err := c.device(rec)
	if err != nil {
		return nil, err
	}
	return &userspaceDialer{dev: dev}, nil
}

func (c *client) Config(ctx context.Context, machine string) (string, error) {
	rec, err := c.opts.Records.Load(machine)
	if err != nil {
		return "", err
	}
	kp, err := c.opts.Keys.Load(machine)
	if err != nil {
		return "", err
	}
	return WGQuick(rec, kp.Private)
}

func (c *client) state(ctx context.Context, rec Record) *State {
	installed, _ := c.opts.Installer.Installed(ctx)
	mode := ModeUserspace
	if installed {
		if up, _ := serviceRunning(ctx, rec); up {
			mode = ModeTransparent
		}
	}
	return &State{
		Mode:          mode,
		Machine:       rec.Fleet,
		Endpoint:      rec.Endpoint,
		PublicKey:     rec.PublicKey,
		PeerName:      rec.PeerName,
		IP:            rec.IP,
		Subnet:        rec.Subnet,
		Resolver:      rec.Resolver(),
		LastHandshake: rec.LastHandshake,
		Installed:     installed,
	}
}

func (c *client) device(rec Record) (*wgDevice, error) {
	return pool.get(rec, c.opts.Keys, c.opts.Log)
}

func (c *client) requireInstalled(ctx context.Context) error {
	installed, err := c.opts.Installer.Installed(ctx)
	if err != nil {
		return err
	}
	if !installed {
		return ErrNotInstalled
	}
	return nil
}

func (c *client) machineStatus(ctx context.Context, machine string) (*api.Status, error) {
	var st api.Status
	if err := c.runJSON(ctx, machine, []string{"status", "--json"}, &st); err != nil {
		return nil, err
	}
	if st.VPN == nil || !st.VPN.Enabled {
		reason := "it has no WireGuard device"
		if st.VPN != nil && st.VPN.Error != "" {
			reason = st.VPN.Error
		}
		return nil, fmt.Errorf("%s has no private network (%s); "+
			"re-run 'caramelo hub setup' on it to add one", machine, reason)
	}
	return &st, nil
}

func (c *client) addPeer(ctx context.Context, machine, name, publicKey string) (*state.Peer, error) {
	var peer state.Peer
	argv := []string{"peer", "add", name, publicKey, "--json"}
	if err := c.runJSON(ctx, machine, argv, &peer); err != nil {
		return nil, err
	}
	if peer.IP == "" {
		return nil, fmt.Errorf("%s did not allocate an address for peer %q", machine, name)
	}
	return &peer, nil
}

func (c *client) runJSON(ctx context.Context, machine string, argv []string, v any) error {
	var out, errOut bytes.Buffer
	code, err := c.opts.Control.Run(ctx, machine, argv, &out, &errOut)
	if err != nil {
		return fmt.Errorf("run 'caramelo %s' on %s: %w", strings.Join(argv, " "), machine, err)
	}
	if code != 0 {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = fmt.Sprintf("exit %d", code)
		}
		return fmt.Errorf("'caramelo %s' on %s: %s", strings.Join(argv, " "), machine, msg)
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), v); err != nil {
		return fmt.Errorf("read the answer of 'caramelo %s' on %s: %w",
			strings.Join(argv, " "), machine, err)
	}
	return nil
}

func RecordFrom(machine, peerName, publicKey string, st *api.Status, peer *state.Peer) (Record, error) {
	vs := st.VPN
	ip, err := netip.ParseAddr(strings.TrimSpace(peer.IP))
	if err != nil {
		return Record{}, fmt.Errorf("the address %q allocated for the commander: %w", peer.IP, err)
	}
	machineIP, err := netip.ParseAddr(strings.TrimSpace(vs.Address))
	if err != nil {
		return Record{}, fmt.Errorf("the address of %s (%q): %w", machine, vs.Address, err)
	}
	subnet, err := netip.ParsePrefix(strings.TrimSpace(vs.Subnet))
	if err != nil {
		return Record{}, fmt.Errorf("the subnet of %s (%q): %w", machine, vs.Subnet, err)
	}

	if reach := strings.TrimSpace(vs.Reach); reach != "" {
		if p, err := netip.ParsePrefix(reach); err == nil {
			subnet = p
		}
	}
	endpoint, err := endpointFor(machine, vs)
	if err != nil {
		return Record{}, err
	}
	if !ValidKey(vs.PublicKey) {
		return Record{}, fmt.Errorf("%s reported no usable public key", machine)
	}
	return Record{
		Fleet:       machine,
		MachineName: st.Hostname,
		Endpoint:    endpoint,
		MachineKey:  vs.PublicKey,
		Subnet:      subnet,
		MachineIP:   machineIP,
		PeerName:    peerName,
		IP:          ip,
		PublicKey:   publicKey,
		APIPort:     defaultAPIPort,
	}, nil
}

func DefaultPeerName() string {
	name := "peer"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		if i := strings.Index(host, "."); i > 0 {
			host = host[:i]
		}
		name += "-" + host
	}
	return Slug(name)
}

func Slug(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "peer"
	}
	if len(out) > env.MaxSlugLen {
		out = strings.Trim(out[:env.MaxSlugLen], "-")
	}
	if out == "" {
		return "peer"
	}
	return out
}

func endpointFor(machine string, vs *api.VPNStatus) (string, error) {
	port := listenPort(vs)
	if e := strings.TrimSpace(vs.Endpoint); e != "" {
		if _, _, err := net.SplitHostPort(e); err != nil {
			return net.JoinHostPort(e, strconv.Itoa(port)), nil
		}
		return e, nil
	}
	host, err := resolveMachineHost(machine)
	if err != nil {
		return "", fmt.Errorf("work out the UDP endpoint of %s: %w", machine, err)
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func listenPort(vs *api.VPNStatus) int {
	if _, p, err := net.SplitHostPort(strings.TrimSpace(vs.Listen)); err == nil {
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return vpn.DefaultListenPort
}

var resolveMachineHost = func(fleet string) (string, error) {
	cfg, err := remote.LoadCommanderConfig()
	if err != nil {
		return "", err
	}
	target, ferr := cfg.FleetTarget(fleet)
	if ferr != nil {
		raw, perr := remote.ParseTarget(fleet)
		if perr != nil {
			return "", ferr
		}
		target = raw
	}
	if target.Host == "" {
		return "", fmt.Errorf("fleet %q has no hub address", fleet)
	}
	return target.Host, nil
}
