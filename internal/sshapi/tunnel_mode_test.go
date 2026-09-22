package sshapi

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/vpn"
)

func TestStartTunnelHandsTheConfiguredModeToTheDevice(t *testing.T) {
	cfg := serverconfig.Default()
	cfg.Name, cfg.Hub.Fleet = "box", "box"
	cfg.StateDir = t.TempDir()
	cfg.VPNListen = "127.0.0.1:0"

	var got vpn.Mode
	stop := errors.New("the device was built, that is all this test wants")
	_, err := startTunnel(context.Background(), cfg, io.Discard, func(ctx context.Context, dev vpn.Device) error {
		st, err := dev.Status(ctx)
		if err != nil {
			return err
		}
		got = st.Mode
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("startTunnel = %v, want it to carry %v", err, stop)
	}
	if want := vpn.Mode(cfg.VPNMode); got != want {
		t.Errorf("the device reports mode %q, want the configured %q", got, want)
	}
}

func TestStartTunnelRefusesAModeTheDeviceCannotRun(t *testing.T) {
	cfg := serverconfig.Default()
	cfg.Name, cfg.Hub.Fleet = "box", "box"
	cfg.StateDir = t.TempDir()
	cfg.VPNListen = "127.0.0.1:0"
	cfg.VPNMode = "kernel"

	started := false
	_, err := startTunnel(context.Background(), cfg, io.Discard, func(context.Context, vpn.Device) error {
		started = true
		return nil
	})
	if err == nil {
		t.Fatal("startTunnel accepted a mode the device cannot run")
	}
	if started {
		t.Error("startTunnel started a device it should never have built")
	}
}
