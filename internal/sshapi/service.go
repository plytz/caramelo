package sshapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/machine"
	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vault"
	"github.com/plytz/caramelo/internal/vpn"
)

type Daemon struct {
	Config    serverconfig.Config
	ConfigDir string
	Store     state.Store
	Runner    runner.Runner
	Version   string
	StartedAt time.Time

	HostKeyFingerprint string

	Hostname string

	Gauge func(ctx context.Context) (*machine.Record, error)

	EnvManager *env.Manager

	Vault vault.Store

	Feed *progress.Hub

	Device vpn.Device

	netMu sync.Mutex
	net   *api.Network

	edgeMu      sync.Mutex
	edgeManager EdgeManager
	edgeClient  edge.Client

	api.LocalOnlyFleet

	Resolver api.Resolver

	Forwarder api.Forwarder

	KnownMachines MachineLookup

	Now func() time.Time

	fleetMu      sync.Mutex
	fleetBeginMu sync.Mutex

	hubDoorMu    sync.Mutex
	hubDoorUsers int
	joinMu       sync.Mutex
	joinPeers    map[string]string

	joinTickets map[string]time.Time

	Background context.Context

	feeds *fleetFeeds

	Log io.Writer
}

var _ api.Service = (*Daemon)(nil)

func NewDaemon(cfg serverconfig.Config, configDir string, store state.Store, run runner.Runner, version string) *Daemon {
	d := &Daemon{
		Config:    cfg,
		ConfigDir: configDir,
		Store:     store,
		Runner:    run,
		Version:   version,
		StartedAt: time.Now(),
	}
	d.Hostname, _ = os.Hostname()
	d.Gauge = func(ctx context.Context) (*machine.Record, error) {
		return machine.Gauge(ctx, d.Runner, d.Config, d.Version)
	}
	return d
}

func (d *Daemon) Status(ctx context.Context) (*api.Status, error) {
	st := &api.Status{
		Version:            d.Version,
		Hostname:           d.hostname(),
		StartedAt:          d.StartedAt,
		Uptime:             time.Since(d.StartedAt).Round(time.Second),
		Machine:            d.hostname(),
		HostKeyFingerprint: d.HostKeyFingerprint,
		SSHPort:            d.Config.SSHPort,
		Paths: api.Paths{
			Config: d.configDir(),
			State:  d.Config.StateDir,
			Data:   d.Config.DataDir,
			Socket: d.Config.SocketPath(),
		},
	}
	if sess, ok := SessionFrom(ctx); ok {
		st.Transport = sess.Transport
		st.Identity = sess.Identity
		if sess.Machine != "" {
			st.Machine = sess.Machine
		}
	}
	st.Docker = d.dockerStatus(ctx)

	if vst, err := d.VPNStatus(ctx); err == nil {
		st.VPN = vst
	} else {
		st.VPN = &api.VPNStatus{Enabled: false, Error: err.Error(), APIListen: d.Config.APIListen}
	}

	st.Production = d.productionStatus(ctx)
	return st, nil
}

func (d *Daemon) hostname() string {
	if d.Hostname != "" {
		return d.Hostname
	}
	h, _ := os.Hostname()
	return h
}

func (d *Daemon) configDir() string {
	if d.ConfigDir != "" {
		return d.ConfigDir
	}
	return serverconfig.DefaultConfigDir
}

type dockerInfo struct {
	ServerVersion   string   `json:"ServerVersion"`
	SecurityOptions []string `json:"SecurityOptions"`
	ServerErrors    []string `json:"ServerErrors"`
}

func (d *Daemon) dockerStatus(ctx context.Context) api.DockerStatus {
	if d.Runner == nil {
		return api.DockerStatus{Error: "no command runner configured"}
	}
	res, err := d.Runner.Run(ctx, runner.Cmd{
		Name: "docker",
		Args: []string{"info", "--format", "json"},
		User: d.Config.User,
	})
	if err != nil {
		return api.DockerStatus{Error: err.Error()}
	}
	var info dockerInfo
	if jsonErr := json.Unmarshal([]byte(res.Stdout), &info); jsonErr != nil && res.ExitCode == 0 {
		return api.DockerStatus{Error: fmt.Sprintf("parse docker info: %v", jsonErr)}
	}
	ds := api.DockerStatus{
		ServerVersion: info.ServerVersion,
		Rootless:      hasSecurityOption(info.SecurityOptions, "rootless"),
	}
	switch {
	case res.ExitCode != 0:
		ds.Error = firstLine(res.Stderr)
		if ds.Error == "" {
			ds.Error = fmt.Sprintf("docker info exited %d", res.ExitCode)
		}
	case len(info.ServerErrors) > 0:
		ds.Error = info.ServerErrors[0]
	case info.ServerVersion == "":
		ds.Error = "docker info reported no server version"
	default:
		ds.Running = true
	}
	return ds
}

func hasSecurityOption(opts []string, want string) bool {
	for _, o := range opts {
		for _, field := range strings.Split(o, ",") {
			if strings.TrimSpace(field) == "name="+want {
				return true
			}
		}
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func (d *Daemon) Machine(ctx context.Context) (*machine.Record, error) {
	if d.Store == nil {
		return nil, errors.New("no state store")
	}
	rec, err := d.Store.Machine(ctx)
	if err == nil {
		return rec, nil
	}
	if !errors.Is(err, state.ErrNotFound) {
		return nil, fmt.Errorf("read machine record: %w", err)
	}
	rec, err = d.gauge(ctx)
	if err != nil {
		return nil, err
	}
	if err := d.Store.SaveMachine(ctx, rec); err != nil {
		return nil, fmt.Errorf("save machine record: %w", err)
	}
	return rec, nil
}

func (d *Daemon) gauge(ctx context.Context) (*machine.Record, error) {
	if d.Gauge == nil {
		return nil, errors.New("no machine gauge configured")
	}
	rec, err := d.Gauge(ctx)
	if err != nil {
		return nil, fmt.Errorf("gauge machine: %w", err)
	}
	if rec == nil {
		return nil, errors.New("gauge machine: no record")
	}
	if d.HostKeyFingerprint != "" {
		rec.Caramelo.HostKeyFingerprint = d.HostKeyFingerprint
	}
	return rec, nil
}

func (d *Daemon) Keys(ctx context.Context) ([]state.Key, error) {
	entries, err := ListKeys(d.Config.AuthorizedKeysPath())
	if err != nil {
		return nil, err
	}
	added := map[string]time.Time{}
	if d.Store != nil {
		rows, err := d.Store.Keys(ctx)
		if err != nil {
			return nil, fmt.Errorf("read keys: %w", err)
		}
		for _, r := range rows {
			added[r.Fingerprint] = r.AddedAt
		}
	}
	keys := make([]state.Key, 0, len(entries))
	for _, e := range entries {
		keys = append(keys, state.Key{
			Name:        e.Name,
			Type:        e.Type,
			PublicKey:   e.Key,
			Fingerprint: e.Fingerprint,
			Options:     e.Options,
			AddedAt:     added[e.Fingerprint],
		})
	}
	return keys, nil
}

func (d *Daemon) AddKey(ctx context.Context, name, options, authorizedKeyLine string) (*state.Key, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	entry, err := AppendKeyWithOptions(d.Config.AuthorizedKeysPath(), authorizedKeyLine, name, options)
	if err != nil {
		return nil, err
	}
	key := &state.Key{
		Name:        entry.Name,
		Type:        entry.Type,
		PublicKey:   entry.Key,
		Fingerprint: entry.Fingerprint,
		Options:     entry.Options,
		AddedAt:     time.Now().UTC(),
	}
	if d.Store != nil {
		if err := d.Store.AddKey(ctx, *key); err != nil {
			if _, rmErr := RemoveKey(d.Config.AuthorizedKeysPath(), entry.Name); rmErr != nil {
				return nil, fmt.Errorf("record key %q: %w (and rolling back authorized_keys failed: %v)", entry.Name, err, rmErr)
			}
			return nil, fmt.Errorf("record key %q: %w", entry.Name, err)
		}
	}
	return key, nil
}

func (d *Daemon) RemoveKey(ctx context.Context, name string) error {
	if _, err := RemoveKey(d.Config.AuthorizedKeysPath(), name); err != nil {
		return err
	}
	if d.Store != nil {
		if err := d.Store.RemoveKey(ctx, name); err != nil && !errors.Is(err, state.ErrNotFound) {
			return fmt.Errorf("forget key %q: %w", name, err)
		}
	}
	return nil
}

func (d *Daemon) setResolver(r api.Resolver) {
	d.fleetMu.Lock()
	defer d.fleetMu.Unlock()
	d.Resolver = r
}

func (d *Daemon) resolver() api.Resolver {
	d.fleetMu.Lock()
	defer d.fleetMu.Unlock()
	return d.Resolver
}

func (d *Daemon) setForwarder(f api.Forwarder) {
	d.fleetMu.Lock()
	defer d.fleetMu.Unlock()
	d.Forwarder = f
}

func (d *Daemon) forwarder() api.Forwarder {
	d.fleetMu.Lock()
	defer d.fleetMu.Unlock()
	return d.Forwarder
}

func (d *Daemon) setKnownMachines(m MachineLookup) {
	d.fleetMu.Lock()
	defer d.fleetMu.Unlock()
	d.KnownMachines = m
}

func (d *Daemon) machineLookup() MachineLookup {
	d.fleetMu.Lock()
	defer d.fleetMu.Unlock()
	return d.KnownMachines
}

func (d *Daemon) setFeeds(f *fleetFeeds) {
	d.fleetMu.Lock()
	defer d.fleetMu.Unlock()
	d.feeds = f
}

func (d *Daemon) fleetFeedsNow() *fleetFeeds {
	d.fleetMu.Lock()
	defer d.fleetMu.Unlock()
	return d.feeds
}
