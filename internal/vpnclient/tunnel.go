package vpnclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/vpn"
)

type TunnelOptions struct {
	Keys    KeyStore
	Records RecordStore

	Installer Installer

	Log io.Writer
}

func InstallTunnelDialer(opts TunnelOptions) {
	remote.TunnelDialer = func(ctx context.Context, fleet string, t remote.Target) (remote.Dialer, error) {
		return TunnelDialer(ctx, fleet, t, opts)
	}
}

func TunnelDialer(ctx context.Context, fleet string, t remote.Target, opts TunnelOptions) (remote.Dialer, error) {
	records := opts.Records
	if records == nil {
		records = &FileRecordStore{}
	}
	rec, err := findRecord(records, fleet, t.Host)
	if err != nil {
		return nil, err
	}
	return dialerFor(ctx, rec, opts)
}

func dialerFor(ctx context.Context, rec Record, opts TunnelOptions) (Dialer, error) {
	installer := opts.Installer
	if installer == nil {
		installer = NewInstaller()
	}
	if installed, _ := installer.Installed(ctx); installed {
		if up, _ := serviceRunning(ctx, rec); up {
			return &hostDialer{rec: rec}, nil
		}
	}
	dev, err := pool.get(rec, opts.Keys, opts.Log)
	if err != nil {
		return nil, fmt.Errorf("bring the tunnel to %s up: %w", rec.Fleet, err)
	}
	return &userspaceDialer{dev: dev}, nil
}

func findRecord(records RecordStore, names ...string) (Record, error) {
	var host string
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if host == "" {
			host = name
		}
		if rec, err := records.Load(name); err == nil && rec.Valid() {
			return rec, nil
		} else if err != nil && !errors.Is(err, ErrNoKey) {
			return Record{}, err
		}
	}
	if host == "" {
		return Record{}, remote.ErrNoTunnel
	}
	all, err := records.List()
	if err != nil {
		return Record{}, err
	}

	base := strings.TrimSuffix(vpn.Normalize(host), vpn.Suffix)
	for _, rec := range all {
		if !rec.Valid() {
			continue
		}
		switch host {
		case rec.Host(), rec.MachineName, rec.Fleet:
			return rec, nil
		}
		if base != "" && (base == vpn.Normalize(rec.MachineName) || base == vpn.Normalize(rec.Fleet)) {
			return rec, nil
		}
		if rec.MachineIP.String() == host {
			return rec, nil
		}
	}
	return Record{}, remote.ErrNoTunnel
}

type SSHTarget struct {
	Dialer Dialer

	Address string

	Machine string
}

func DialTarget(ctx context.Context, host string, port int, opts TunnelOptions) (*SSHTarget, error) {
	records := opts.Records
	if records == nil {
		records = &FileRecordStore{}
	}
	rec, err := findRecord(records, host)
	if errors.Is(err, remote.ErrNoTunnel) {
		return nil, fmt.Errorf("no machine of the commander's answers to %q: %w", host, ErrNoKey)
	}
	if err != nil {
		return nil, err
	}
	address := rec.APIAddr()
	if port != 0 && port != rec.APIPort {
		address = fmt.Sprintf("%s:%d", rec.MachineIP, port)
	}
	dialer, err := dialerFor(ctx, rec, opts)
	if err != nil {
		return nil, err
	}
	return &SSHTarget{Dialer: dialer, Address: address, Machine: rec.Fleet}, nil
}

func OneOff(rec Record, private string, logw io.Writer) (Dialer, error) {
	dev, err := openDevice(rec, private, logw)
	if err != nil {
		return nil, err
	}
	return &userspaceDialer{dev: dev}, nil
}
