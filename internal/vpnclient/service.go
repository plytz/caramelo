package vpnclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/plytz/caramelo/internal/userdir"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"

	"github.com/plytz/caramelo/internal/vpn"
)

const SocketName = "vpn.sock"

const serviceTimeout = 3 * time.Second

var socketPathForTest string

func ServiceSocketPath() string {
	if socketPathForTest != "" {
		return socketPathForTest
	}
	return filepath.Join(runtimeDir(), "caramelo", SocketName)
}

func runtimeDir() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return dir
	}
	if dir, err := userdir.Cache(); err == nil {
		return dir
	}
	return os.TempDir()
}

type serviceRequest struct {
	Op      string `json:"op"`
	Machine string `json:"machine,omitempty"`
}

type serviceResponse struct {
	OK            bool      `json:"ok"`
	Machine       string    `json:"machine,omitempty"`
	Interface     string    `json:"interface,omitempty"`
	Running       bool      `json:"running"`
	LastHandshake time.Time `json:"last_handshake,omitempty"`
	Error         string    `json:"error,omitempty"`
}

func serviceCall(ctx context.Context, req serviceRequest) (*serviceResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, serviceTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", ServiceSocketPath())
	if err != nil {
		return nil, fmt.Errorf("the transparent-mode service is not running (%s): %w",
			ServiceSocketPath(), err)
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, fmt.Errorf("talk to the transparent-mode service: %w", err)
	}
	line, err := bufio.NewReader(io.LimitReader(conn, 1<<16)).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, fmt.Errorf("the transparent-mode service did not answer: %w", err)
	}
	var resp serviceResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("the transparent-mode service answered something unexpected: %w", err)
	}
	if !resp.OK && resp.Error != "" {
		return &resp, errors.New(resp.Error)
	}
	return &resp, nil
}

func serviceRunning(ctx context.Context, rec Record) (bool, error) {
	resp, err := serviceCall(ctx, serviceRequest{Op: "status", Machine: rec.Fleet})
	if err != nil {
		return false, err
	}
	return resp.Running && (resp.Machine == "" || resp.Machine == rec.Fleet), nil
}

func serviceUp(ctx context.Context, rec Record) error {
	_, err := serviceCall(ctx, serviceRequest{Op: "up", Machine: rec.Fleet})
	return err
}

func serviceDown(ctx context.Context, rec Record) error {
	_, err := serviceCall(ctx, serviceRequest{Op: "down", Machine: rec.Fleet})
	return err
}

type ServiceOptions struct {
	Machine string

	Interface string

	Keys    KeyStore
	Records RecordStore

	Socket string

	Host HostNet

	TUN func(name string, mtu int) (tun.Device, error)

	Log io.Writer
}

type HostNet interface {
	Configure(ctx context.Context, iface string, rec Record) error

	Teardown(ctx context.Context, iface string) error
}

func RunService(ctx context.Context, opts ServiceOptions) error {
	if strings.TrimSpace(opts.Machine) == "" {
		return errors.New("no machine: 'vpn service' carries one machine's network")
	}
	if opts.Interface == "" {
		opts.Interface = DefaultInterface
	}
	if opts.Keys == nil {
		opts.Keys = &FileKeyStore{}
	}
	if opts.Records == nil {
		opts.Records = &FileRecordStore{}
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	if opts.Host == nil {

		opts.Host = NewHostNet(opts.Log)
	}
	if opts.TUN == nil {
		opts.TUN = tun.CreateTUN
	}
	if opts.Socket == "" {
		opts.Socket = ServiceSocketPath()
	}
	rec, err := opts.Records.Load(opts.Machine)
	if err != nil {
		return err
	}

	if err := rec.CheckRoutable(); err != nil {
		return err
	}
	kp, err := opts.Keys.Load(opts.Machine)
	if err != nil {
		return err
	}
	svc := &service{opts: opts, rec: rec, private: kp.Private}
	return svc.run(ctx)
}

type service struct {
	opts    ServiceOptions
	rec     Record
	private string

	mu  sync.Mutex
	dev *device.Device
	up  bool
}

func (s *service) logf(format string, args ...any) {
	fmt.Fprintf(s.opts.Log, format+"\n", args...)
}

func (s *service) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := s.start(ctx); err != nil {
		return err
	}
	defer s.stop(context.WithoutCancel(ctx))

	ln, err := listenSocket(s.opts.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
	s.logf("tunnel to %s is up on %s; control socket %s", s.rec.Fleet, s.opts.Interface, s.opts.Socket)

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept on %s: %w", s.opts.Socket, err)
		}
		stop := s.serve(ctx, conn)
		_ = conn.Close()
		if stop {
			return nil
		}
	}
}

func listenSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if c, err := net.DialTimeout("unix", path, 200*time.Millisecond); err == nil {
		_ = c.Close()
		return nil, fmt.Errorf("another transparent-mode service is already listening on %s", path)
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("set the mode of %s: %w", path, err)
	}
	return ln, nil
}

func (s *service) serve(ctx context.Context, conn net.Conn) (stop bool) {
	_ = conn.SetDeadline(time.Now().Add(serviceTimeout))
	line, err := bufio.NewReader(io.LimitReader(conn, 1<<16)).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return false
	}
	var req serviceRequest
	if err := json.Unmarshal(line, &req); err != nil {
		writeResponse(conn, serviceResponse{Error: "unreadable request"})
		return false
	}
	if req.Machine != "" && req.Machine != s.rec.Fleet {
		writeResponse(conn, serviceResponse{
			Machine: s.rec.Fleet,
			Error: fmt.Sprintf("this service carries the network of %s, not %s",
				s.rec.Fleet, req.Machine),
		})
		return false
	}
	switch req.Op {
	case "status":
		writeResponse(conn, s.status())
	case "up":
		if err := s.start(ctx); err != nil {
			writeResponse(conn, serviceResponse{Machine: s.rec.Fleet, Error: err.Error()})
			return false
		}
		writeResponse(conn, s.status())
	case "down":
		s.stop(ctx)
		writeResponse(conn, s.status())
		return true
	default:
		writeResponse(conn, serviceResponse{Error: fmt.Sprintf("unknown request %q", req.Op)})
	}
	return false
}

func writeResponse(w io.Writer, resp serviceResponse) {
	if resp.Error == "" {
		resp.OK = true
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_, _ = w.Write(append(b, '\n'))
}

func (s *service) status() serviceResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := serviceResponse{
		OK:        true,
		Machine:   s.rec.Fleet,
		Interface: s.opts.Interface,
		Running:   s.up,
	}
	if s.dev != nil {
		var b strings.Builder
		if err := s.dev.IpcGetOperation(&b); err == nil {
			resp.LastHandshake = parseHandshake(b.String())
		}
	}
	return resp
}

func (s *service) start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.up {
		return nil
	}
	cfg, err := ipcConfig(s.rec, s.private)
	if err != nil {
		return err
	}
	tdev, err := s.opts.TUN(s.opts.Interface, vpn.MTU)
	if err != nil {
		return fmt.Errorf("create the tunnel interface %s "+
			"(does this binary have CAP_NET_ADMIN? 'sudo caramelo vpn install' grants it): %w",
			s.opts.Interface, err)
	}
	name, err := tdev.Name()
	if err == nil && name != "" {
		s.opts.Interface = name
	}
	dev := device.NewDevice(tdev, conn.NewDefaultBind(), newLogger(device.LogLevelError, s.opts.Log))
	if err := dev.IpcSet(cfg); err != nil {
		dev.Close()
		return fmt.Errorf("configure the tunnel to %s: %w", s.rec.Fleet, err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return fmt.Errorf("bring the tunnel to %s up: %w", s.rec.Fleet, err)
	}
	if err := s.opts.Host.Configure(ctx, s.opts.Interface, s.rec); err != nil {
		dev.Close()
		return err
	}
	s.dev, s.up = dev, true
	return nil
}

func (s *service) stop(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.up {
		return
	}
	if err := s.opts.Host.Teardown(ctx, s.opts.Interface); err != nil {
		s.logf("tunnel: %v", err)
	}
	s.dev.Close()
	s.dev, s.up = nil, false
	s.logf("tunnel to %s is down", s.rec.Fleet)
}

func NewHostNet(log io.Writer) HostNet {
	return &commandHostNet{GOOS: runtime.GOOS, Log: log}
}

type commandHostNet struct {
	GOOS string

	Run func(ctx context.Context, name string, args ...string) error

	Raise func() error

	Log io.Writer
}

func (h *commandHostNet) raise() error {
	if h.Raise != nil {
		return h.Raise()
	}
	return raiseNetAdmin()
}

var _ HostNet = (*commandHostNet)(nil)

func (h *commandHostNet) run(ctx context.Context, name string, args ...string) error {
	if h.Run != nil {
		return h.Run(ctx, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return nil
}

func (h *commandHostNet) logf(format string, args ...any) {
	if h.Log == nil {
		return
	}
	fmt.Fprintf(h.Log, format+"\n", args...)
}

func (h *commandHostNet) Configure(ctx context.Context, iface string, rec Record) error {
	if h.GOOS != "linux" {
		return fmt.Errorf("%s: %w", h.GOOS, ErrUnsupported)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := h.raise(); err != nil {
		h.logf("tunnel: %v", err)
	}

	if err := h.run(ctx, "ip", "address", "replace", rec.IP.String()+"/32", "dev", iface); err != nil {
		return err
	}
	if err := h.run(ctx, "ip", "link", "set", "dev", iface, "up"); err != nil {
		return err
	}
	if err := h.run(ctx, "ip", "route", "replace", rec.Subnet.String(), "dev", iface); err != nil {
		return err
	}

	for _, argv := range ResolvectlCommands(iface, rec.Resolver()) {
		if err := h.run(ctx, argv[0], argv[1:]...); err != nil {
			h.logf("tunnel: split DNS for .%s was not configured: %v", vpn.Domain, err)
			break
		}
	}
	return nil
}

func (h *commandHostNet) Teardown(ctx context.Context, iface string) error { return nil }
