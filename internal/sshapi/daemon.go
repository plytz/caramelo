package sshapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/git"
	"github.com/plytz/caramelo/internal/ports"
	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/runtime/docker"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vault"
	"github.com/plytz/caramelo/internal/vpn"
)

func Run(ctx context.Context, cfg serverconfig.Config, configDir, version string, logw io.Writer, exec Exec) error {
	if logw == nil {
		logw = io.Discard
	}
	logf := func(format string, args ...any) {
		fmt.Fprintf(logw, "%s caramelod: "+format+"\n", append([]any{time.Now().UTC().Format(time.RFC3339)}, args...)...)
	}

	store, err := state.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open state store %s: %w", cfg.DBPath(), err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			logf("closing state store: %v", err)
		}
	}()

	signer, err := EnsureHostKey(cfg.HostKeyPath())
	if err != nil {
		return err
	}
	fingerprint := Fingerprint(signer)
	logf("version %s, host key %s", version, fingerprint)

	daemon := NewDaemon(cfg, configDir, store, runner.Exec{}, version)
	daemon.HostKeyFingerprint = fingerprint
	daemon.EnvManager = newEnvManager(cfg, store, runner.Exec{}, version, logw)

	daemon.EnvManager.Background = ctx

	if cipher, created, err := vault.CipherFromKeyFile(cfg.VaultKeyPath()); err != nil {
		logf("vault: %v", err)
	} else if v, err := vault.New(vault.Options{
		Rows: store, Cipher: cipher, Identity: env.IdentityFrom,
	}); err != nil {
		logf("vault: %v", err)
	} else {
		daemon.Vault = v
		daemon.EnvManager.Secrets = v
		if created {
			logf("vault: generated the machine's key at %s", cfg.VaultKeyPath())
		} else {
			logf("vault: key %s, env-files in %s", cfg.VaultKeyPath(), cfg.SecretsDir())
		}
	}

	if err := vault.EnsureSecretsDir(cfg.SecretsDir()); err != nil {
		logf("secrets: %v", err)
	}

	hub := progress.NewHub()
	defer hub.Close()
	store.SetNotifier(hub)
	daemon.EnvManager.Notifier = hub
	daemon.Feed = hub

	if cfg.Edge {
		client := edge.NewClient(cfg.EdgeSocketPath())
		daemon.SetEdgeClient(client)

		daemon.EnvManager.Edge = client
		logf("edge: control socket %s, tls %s, http3 %v", cfg.EdgeSocketPath(), cfg.TLS, cfg.HTTP3)
	}
	daemon.SetEdgeManager(daemon.EnvManager)

	if rec, err := daemon.gauge(ctx); err != nil {
		logf("gauge: %v", err)
	} else if err := store.SaveMachine(ctx, rec); err != nil {
		logf("save machine record: %v", err)
	} else {

		if ip, err := netip.ParseAddr(rec.Network.PrimaryIP); err == nil {
			daemon.EnvManager.PublicIP = ip
		}
		logf("machine %s: %d vCPU, %d MB RAM, docker %s", rec.Hostname, rec.CPU.Count,
			rec.Memory.TotalBytes>>20, rec.Docker.ServerVersion)
	}

	daemon.Log = logw
	daemon.Background = ctx
	daemon.configureFleet(ctx, logf)

	if err := ImportSetupReports(ctx, store, cfg, logf); err != nil {
		logf("import setup reports: %v", err)
	}

	fixUpRepositories(ctx, store, cfg, logf)

	if cfg.Edge {
		if err := daemon.EnvManager.PushRoutes(ctx); err != nil {
			logf("edge: push the route table: %v", err)
		}

		go daemon.keepEdgeTable(ctx, logf)
	}

	if err := daemon.EnvManager.ResumeDeploys(ctx); err != nil {
		logf("resume the deploys that were in flight: %v", err)
	}
	health := env.NewHealthLoop(daemon.EnvManager)
	health.Log = logw
	go func() {
		if err := health.Run(ctx); err != nil {
			logf("health loop: %v", err)
		}
	}()

	var tun *Tunnel
	if cfg.APIListensVPN() {
		t, err := startTunnel(ctx, cfg, logw, func(ctx context.Context, dev vpn.Device) error {
			daemon.SetDevice(dev)

			subnet, err := cfg.VPNSubnetPrefix()
			if err != nil {
				return err
			}
			daemon.EnvManager.VPNAlloc = vpn.NewAllocator(subnet, store)
			daemon.EnvManager.Net = dev
			return daemon.network().Start(ctx)
		})
		switch {
		case err == nil:
			tun = t
			st, _ := t.Device.Status(ctx)
			logf("tunnel: api at %s, device on %s, %d peers, %d routes", t.Addr, cfg.VPNListen, st.Peers, st.Routes)
		case cfg.APIListensPublic():

			daemon.SetDevice(nil)
			daemon.EnvManager.VPNAlloc, daemon.EnvManager.Net = nil, nil
			logf("tunnel: %v; the API still answers on the public listener and the socket", err)
		default:
			return fmt.Errorf("api_listen is %q and the tunnel could not start: %w", cfg.APIListen, err)
		}
	}
	defer func() {

		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if err := tun.Close(stopCtx); err != nil {
			logf("stopping the network device: %v", err)
		}
	}()

	if feeds := daemon.startFleetNetwork(ctx, logf); feeds != nil {
		defer feeds.Close()
	}

	daemon.sweepJoinPeers(ctx)

	srv := &Server{
		Config:   cfg,
		Service:  daemon,
		Version:  version,
		Exec:     exec,
		HostKey:  signer,
		Log:      logw,
		Hostname: daemon.hostname(),
	}
	if tun != nil {
		srv.VPNListener, srv.PeerLookup = tun.Listener, tun.Peers
	}

	srv.Machines = daemonMachines{daemon}
	if err := srv.Serve(ctx); err != nil {
		return err
	}
	logf("stopped")
	return nil
}

func newEnvManager(cfg serverconfig.Config, store state.Store, run runner.Runner, version string, logw io.Writer) *env.Manager {
	alloc := ports.New(store, nil)

	alloc.Log = logw
	driver := docker.NewCLI(run, cfg.User)
	repo := git.NewCLI(run, cfg.User)
	m := env.New(
		store,
		driver,
		repo,
		alloc,
		run,

		env.Dirs{Data: cfg.DataDir, User: cfg.User, Run: cfg.RunDir},
	)
	m.Version = version
	m.Log = logw

	m.Builder = release.New(release.Config{
		Store:    store,
		Driver:   driver,
		Git:      repo,
		Runner:   run,
		User:     cfg.User,
		Version:  version,
		Identity: env.IdentityFrom,

		Machine: machineNameOf(cfg),
		Arch:    runtime.GOARCH,
		Images:  releaseImages{store},
	})

	m.Images = releaseImages{store}
	return m
}

func machineNameOf(cfg serverconfig.Config) string {
	if n := strings.TrimSpace(cfg.Name); n != "" {
		return n
	}
	host, _ := os.Hostname()
	return machineSlug(host)
}

func fixUpRepositories(ctx context.Context, store state.Store, cfg serverconfig.Config, logf func(string, ...any)) {
	apps, err := store.Apps(ctx)
	if err != nil {
		logf("configure repositories for push: list apps: %v", err)
		return
	}
	repos := make([]string, 0, len(apps))
	for _, a := range apps {
		repo := a.RepoPath
		if repo == "" {
			repo = env.RepoPath(cfg.DataDir, a.Name)
		}
		repos = append(repos, repo)
	}
	repo := git.NewCLI(runner.Exec{}, cfg.User)
	n, err := git.FixUpUpdateInstead(ctx, repo, repos)
	if err != nil {
		logf("configure repositories for push: %v", err)
	}
	if n > 0 {
		logf("configured %d repositories so a push updates the environment's worktree", n)
	}

	hooked := 0
	recorders := 0
	for _, r := range repos {
		changed, err := repo.EnsurePreReceive(ctx, r)
		if err != nil {
			logf("install the pre-receive hook in %s: %v", r, err)
		} else if changed {
			hooked++
		}
		changed, err = repo.EnsurePostReceive(ctx, r)
		if err != nil {
			logf("install the post-receive hook in %s: %v", r, err)
			continue
		}
		if changed {
			recorders++
		}
	}
	if hooked > 0 {
		logf("installed the pre-receive hook in %d repositories", hooked)
	}
	if recorders > 0 {
		logf("installed the post-receive hook in %d repositories", recorders)
	}
}

func SetupReportsDir(cfg serverconfig.Config) string {
	return filepath.Join(cfg.StateDir, "setup")
}

func ImportSetupReports(ctx context.Context, store state.Store, cfg serverconfig.Config, logf func(string, ...any)) error {
	dir := SetupReportsDir(cfg)
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return fmt.Errorf("scan %s: %w", dir, err)
	}
	sort.Strings(paths)
	for _, path := range paths {
		key := "setup_imported:" + filepath.Base(path)
		if _, err := store.Setting(ctx, key); err == nil {
			continue
		}
		if err := importSetupReport(ctx, store, path); err != nil {
			logf("setup report %s: %v", filepath.Base(path), err)
			continue
		}
		if err := store.SetSetting(ctx, key, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return fmt.Errorf("mark %s imported: %w", filepath.Base(path), err)
		}
		logf("imported setup report %s", filepath.Base(path))
	}
	return nil
}

func importSetupReport(ctx context.Context, store state.Store, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	var rep setup.Report
	if err := json.Unmarshal(b, &rep); err != nil {
		return fmt.Errorf("parse: %w", err)
	}

	startedAt := time.Now().UTC()
	if fi, err := os.Stat(path); err == nil {
		startedAt = fi.ModTime().UTC()
	}
	for _, r := range rep.Results {
		row := state.SetupRun{
			RunID:     rep.RunID,
			Step:      r.Step,
			Status:    string(r.Status),
			Detail:    r.Detail,
			Error:     r.Error,
			StartedAt: startedAt,
			Duration:  r.Duration,
			Version:   rep.Version,
		}
		if err := store.RecordSetupRun(ctx, row); err != nil {
			return fmt.Errorf("record step %q: %w", r.Step, err)
		}
		startedAt = startedAt.Add(r.Duration)
	}
	return nil
}
