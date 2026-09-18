package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/cli/ui"
	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/edge/certs"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vpnclient"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

func init() { time.Local = time.UTC }

const goldenDir = "testdata/golden"

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(goldenDir, name+".txt")
	if *update {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run `go test ./internal/cli -run Golden -update` to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s moved.\n--- got ---\n%s\n--- want ---\n%s", path, got, string(want))
	}
}

func TestGoldenPlainOutput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(w io.Writer) error
	}{
		{"status", func(w io.Writer) error { return writeStatus(w, statusFixture()) }},
		{"status-none", func(w io.Writer) error { return writeStatus(w, nil) }},
		{"member-show", func(w io.Writer) error { return writeMachine(w, machineFixture()) }},
		{"app-list", func(w io.Writer) error { return writeApps(w, appsFixture()) }},
		{"fleet-list", func(w io.Writer) error { return fleetsView(fleetsFixture()).Write(w) }},
		{"fleet-list-empty", func(w io.Writer) error { return fleetsView(nil).Write(w) }},
		{"app-list-empty", func(w io.Writer) error { return writeApps(w, nil) }},
		{"env-list", func(w io.Writer) error { return writeEnvs(w, []env.Env{sampleEnv(), releaseEnvFixture()}) }},
		{"env-list-empty", func(w io.Writer) error { return writeEnvs(w, nil) }},
		{"env-create", func(w io.Writer) error { e := sampleEnv(); return writeEnv(w, &e, nil) }},
		{"env-show", func(w io.Writer) error { return writeEnvDetail(w, envDetailFixture()) }},
		{"env-destroy", func(w io.Writer) error { return writeDestroyed(w, destroyedFixture()) }},
		{"env-expose", func(w io.Writer) error { return writeExposeResult(w, exposeFixture(), "exposed") }},
		{"env-unexpose", func(w io.Writer) error { return writeExposeResult(w, exposeFixture(), "no longer exposed") }},
		{"env-url", func(w io.Writer) error { return writeURLNotes(w) }},
		{"up", func(w io.Writer) error { return writeUpResult(w, upFixture()) }},
		{"up-exposed", func(w io.Writer) error { return writeUpResult(w, stillUpFixture()) }},
		{"up-none", func(w io.Writer) error { return writeUpResult(w, &api.UpResult{Env: sampleEnv()}) }},
		{"down", func(w io.Writer) error { return writeDownResult(w, "feat-x", downFixture()) }},
		{"down-empty", func(w io.Writer) error { return writeDownResult(w, "feat-x", &api.DownResult{}) }},
		{"config-show", func(w io.Writer) error { return writeEffectiveConfig(w, configFixture()) }},
		{"edge-status", func(w io.Writer) error { return writeEdgeStatus(w, steadyEdgeStatus(), edgeNow()) }},
		{"edge-status-off", func(w io.Writer) error {
			return writeEdgeStatus(w, &edge.Status{Error: "no edge on this machine"}, edgeNow())
		}},
		{"key-list", func(w io.Writer) error { return renderKeys(w, keysFixture()) }},
		{"key-list-empty", func(w io.Writer) error { return renderKeys(w, nil) }},
		{"peer-list", func(w io.Writer) error { return renderPeers(w, peersFixture()) }},
		{"peer-list-empty", func(w io.Writer) error { return renderPeers(w, nil) }},
		{"key-added", func(w io.Writer) error {
			return keyAddedView(&keysFixture()[1]).Write(w)
		}},
		{"key-removed", func(w io.Writer) error {
			return removedView("key", removed{Name: "ci", Removed: true}).Write(w)
		}},
		{"peer-added", func(w io.Writer) error {
			return peerAddedView(&peersFixture()[0]).Write(w)
		}},
		{"peer-removed", func(w io.Writer) error {
			return removedView("peer", removed{Name: "ci", Removed: true}).Write(w)
		}},

		{"member-list", func(w io.Writer) error {
			return machinesView(fleetFixture(), fleetNow()).Write(w)
		}},
		{"member-list-one", func(w io.Writer) error {
			return machinesView(fleetFixture()[:1], fleetNow()).Write(w)
		}},
		{"member-list-empty", func(w io.Writer) error {
			return machinesView(nil, fleetNow()).Write(w)
		}},
		{"member-show-fleet", func(w io.Writer) error {
			return machineDetailView(machineDetailFixture(), fleetNow()).Write(w)
		}},
		{"member-token", func(w io.Writer) error { return writeMachineToken(w, machineTokenFixture()) }},
		{"member-added", func(w io.Writer) error {
			return machineAddedView(machineAddedFixture(), fleetNow()).Write(w)
		}},
		{"member-joined", func(w io.Writer) error {
			return machineJoinedView(machineJoinedFixture()).Write(w)
		}},
		{"member-removed", func(w io.Writer) error {
			return removedView("machine", removed{Name: "nx3", Removed: true}).Write(w)
		}},
		{"env-list-fleet", func(w io.Writer) error {
			return ui.NewView().Table(envsTable(fleetEnvRows())).Write(w)
		}},
		{"env-handoff", func(w io.Writer) error {
			e := sampleEnv()
			return ui.NewView().
				Text("%s/%s is now owned by %s", e.App, e.Name, "agent-b").
				Blank().Table(envsTable(fleetEnvRows()[:1])).Write(w)
		}},
		{"status-fleet", func(w io.Writer) error {
			return statusResultView(&statusResult{Status: statusFixture(), Fleet: fleetFixture()},
				fleetNow()).Write(w)
		}},
		{"edge-status-via", func(w io.Writer) error { return writeEdgeStatus(w, viaEdgeStatus(), edgeNow()) }},
		{"edge-status-stale", func(w io.Writer) error { return writeEdgeStatus(w, staleEdgeStatus(), edgeNow()) }},
		{"edge-prune", func(w io.Writer) error { return writeEdgePrune(w, prunedFixture()) }},
		{"edge-prune-none", func(w io.Writer) error {
			return writeEdgePrune(w, &certs.PruneResult{KeepFor: certs.DefaultKeepStale})
		}},
		{"vpn-status", func(w io.Writer) error { return renderVPNState(w, vpnStateFixture()) }},
		{"vpn-status-transparent", func(w io.Writer) error {
			st := vpnStateFixture()
			st.Mode, st.Installed = vpnclient.ModeTransparent, true
			return renderVPNState(w, st)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			if err := tc.write(&b); err != nil {
				t.Fatalf("render: %v", err)
			}
			checkGolden(t, tc.name, b.String())
		})
	}
}

func TestCommanderRenderMatchesTheDaemon(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		path string
		info renderInfo
		svc  api.Service
	}{
		{name: "status", args: []string{"status"}, path: "status",
			svc: &fakeAPI{status: statusFixture()}},
		{name: "member-show", args: []string{"member", "show"}, path: "member show",
			svc: &fakeAPI{record: machineFixture()}},
		{name: "app-list", args: []string{"app", "list"}, path: "app list",
			svc: &envService{apps: appsFixture()}},
		{name: "env-list", args: []string{"env", "list", "--app", "shop"}, path: "env list",
			svc: &envService{envs: []env.Env{sampleEnv(), releaseEnvFixture()}}},
		{name: "env-show", args: []string{"env", "show", "feat-x", "--app", "shop"}, path: "env show",
			info: renderInfo{app: "shop", env: "feat-x", args: []string{"feat-x"}},
			svc:  &envService{detail: envDetailFixture()}},
		{name: "env-create", args: []string{"env", "create", "feat-x", "--app", "shop"}, path: "env create",
			info: renderInfo{app: "shop", env: "feat-x", args: []string{"feat-x"}},
			svc:  func() api.Service { e := sampleEnv(); return &envService{env: &e} }()},
		{name: "env-destroy", args: []string{"env", "destroy", "feat-x", "--app", "shop", "--yes"},
			path: "env destroy",
			info: renderInfo{app: "shop", env: "feat-x", args: []string{"feat-x"}},
			svc:  &envService{}},
		{name: "up", args: []string{"up", "feat-x", "--app", "shop"}, path: "up",
			info: renderInfo{app: "shop", env: "feat-x", args: []string{"feat-x"}},
			svc:  &m4Service{up: upFixture()}},
		{name: "up-exposed", args: []string{"up", "feat-x", "--app", "shop"}, path: "up",
			info: renderInfo{app: "shop", env: "feat-x", args: []string{"feat-x"}},
			svc:  &m4Service{up: stillUpFixture()}},
		{name: "down", args: []string{"down", "feat-x", "--app", "shop"}, path: "down",
			info: renderInfo{app: "shop", env: "feat-x", args: []string{"feat-x"}},
			svc:  &m4Service{down: downFixture()}},
		{name: "down-empty", args: []string{"down", "feat-x", "--app", "shop"}, path: "down",
			info: renderInfo{app: "shop", env: "feat-x", args: []string{"feat-x"}},
			svc:  &m4Service{down: &api.DownResult{}}},
		{name: "config-show", args: []string{"config", "show", "feat-x", "--app", "shop"},
			path: "config show",
			info: renderInfo{app: "shop", env: "feat-x", args: []string{"feat-x"}},
			svc:  &configService{cfg: configFixture()}},
		{name: "edge-status", args: []string{"edge", "status"}, path: "edge status",
			svc: &edgeService{status: steadyEdgeStatus()}},
		{name: "env-expose", args: []string{"env", "expose", "feat-x", "--app", "shop"},
			path: "env expose",
			info: renderInfo{app: "shop", env: "feat-x", args: []string{"feat-x"}},
			svc:  &edgeService{expose: exposeFixture()}},
		{name: "env-unexpose", args: []string{"env", "unexpose", "feat-x", "--app", "shop"},
			path: "env unexpose",
			info: renderInfo{app: "shop", env: "feat-x", args: []string{"feat-x"}},
			svc:  &edgeService{expose: exposeFixture()}},
		{name: "key-list", args: []string{"key", "list"}, path: "key list",
			svc: &keyService{keys: keysFixture()}},
		{name: "key-remove", args: []string{"key", "remove", "ci"}, path: "key remove",
			svc: &keyService{}},
		{name: "peer-add", args: []string{"peer", "add", "ci", "abcdefghijklmnop="}, path: "peer add",
			svc: &peerService{}},
		{name: "peer-remove", args: []string{"peer", "remove", "ci", "--force"}, path: "peer remove",
			svc: &peerService{}},

		{name: "edge-ca", args: []string{"edge", "ca"}, path: "edge ca",
			svc: &edgeService{ca: &certs.CA{
				Subject: "Caramelo Internal CA", Fingerprint: "a1b2c3",
				PEM: "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
			}}},
		{name: "peer-list", args: []string{"peer", "list"}, path: "peer list",
			svc: &peerService{peers: peersFixture()}},

		{name: "member-list", args: []string{"member", "list"}, path: "member list",
			svc: &fleetService{machines: quietFleetFixture()}},
		{name: "member-show-fleet", args: []string{"member", "show", "nx2"}, path: "member show",
			svc: &fleetService{detail: machineDetailFixture()}},
		{name: "member-token", args: []string{"member", "token"}, path: "member token",
			svc: &fleetService{token: machineTokenFixture()}},
		{name: "member-remove", args: []string{"member", "remove", "nx3", "--yes"},
			path: "member remove", svc: &fleetService{}},
		{name: "env-handoff", args: []string{"env", "handoff", "feat-x", "--app", "shop", "--to", "agent-b"},
			path: "env handoff",
			info: renderInfo{app: "shop", env: "feat-x", args: []string{"feat-x"}},
			svc:  func() api.Service { e := sampleEnv(); return &fleetService{handed: &e} }()},
		{name: "status-fleet", args: []string{"status"}, path: "status",
			svc: &fakeAPI{status: statusFixture(), machines: quietFleetFixture()}},
		{name: "edge-status-via", args: []string{"edge", "status"}, path: "edge status",
			svc: &edgeService{status: viaEdgeStatus()}},
		{name: "edge-prune", args: []string{"edge", "prune"}, path: "edge prune",
			svc: &edgeService{pruned: prunedFixture()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, human, stderr := runWithService(t, tc.svc, tc.args...)
			if code != ExitOK {
				t.Fatalf("exit = %d (stderr %q)", code, stderr)
			}
			jsonArgs := append(append([]string{}, tc.args...), "--json")
			code, body, stderr := runWithService(t, tc.svc, jsonArgs...)
			if code != ExitOK {
				t.Fatalf("--json exit = %d (stderr %q)", code, stderr)
			}
			if !json.Valid([]byte(strings.TrimSpace(body))) {
				t.Fatalf("--json did not print one JSON value: %q", body)
			}
			a := &app{stdout: io.Discard, stderr: io.Discard, render: tc.info}
			fn := rendererFor(a, tc.path)
			if fn == nil {
				t.Fatalf("no renderer registered for %q", tc.path)
			}
			var b bytes.Buffer
			if err := fn(&b, json.RawMessage(strings.TrimSpace(body))); err != nil {
				t.Fatalf("commander render: %v", err)
			}
			if b.String() != human {
				t.Errorf("the commander's rendering is not the daemon's.\n--- commander ---\n%s\n--- daemon ---\n%s",
					b.String(), human)
			}
		})
	}
}

func TestEveryRendererNamesACommand(t *testing.T) {
	root := NewRootCmd(io.Discard, io.Discard)
	for _, path := range registeredRenderers() {
		cmd, _, err := root.Find(strings.Fields(path))
		if err != nil {
			t.Errorf("renderer for %q: %v", path, err)
			continue
		}
		if got := normalizeCommandPath(cmd.CommandPath()); got != path {
			t.Errorf("renderer for %q resolved to %q", path, got)
		}
	}
	for _, path := range registeredViews() {
		cmd, _, err := root.Find(strings.Fields(path))
		if err != nil {
			t.Errorf("view for %q: %v", path, err)
			continue
		}
		if got := normalizeCommandPath(cmd.CommandPath()); got != path {
			t.Errorf("view for %q resolved to %q", path, got)
		}
	}
}

func appsFixture() []api.AppInfo {
	return []api.AppInfo{
		{Name: "shop", DefaultBranch: "main", Stack: "node", EnvCount: 3, RepoBytes: 4_194_304},
		{Name: "api", DefaultBranch: "main", Stack: "go", EnvCount: 1, RepoBytes: 1_048_576},
	}
}

func releaseEnvFixture() env.Env {
	e := sampleEnv()
	e.ID, e.Name, e.Branch = 2, "production", "production"
	e.SourceBranch, e.PushedBy, e.PushedAt = "", "", time.Time{}
	e.Mode, e.Protected = env.ModeRelease, true
	e.PortBase, e.PortCount = 20032, 32
	e.CreatedBy = "ci"
	return e
}

func envDetailFixture() *api.EnvDetail {
	e := sampleEnv()
	return &api.EnvDetail{
		Env: e,
		Deps: []env.DepState{
			{Name: "db", Container: "caramelo-shop-feat-x-db", Status: env.DepRunning, Port: 20001},
			{Name: "cache", Container: "caramelo-shop-feat-x-cache", Status: env.DepRunning, Port: 20002},
		},
		Services: []env.Service{{
			Name: "web", Container: "caramelo-shop-feat-x-web", Image: "node:22-alpine",
			Status: env.ServiceRunning, Health: env.HealthOK, URL: "http://127.0.0.1:20003",
		}},
		Network: "caramelo-shop-feat-x",
		Routes: []edge.Route{{
			Host: "feat-x.shop.test", Kind: edge.KindHTTPS, App: "shop", Env: "feat-x",
			Service: "web", CreatedAt: time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC),
			Targets: []edge.Target{{Replica: 1, Port: 20003, State: edge.TargetActive}},
		}},
		Events: []env.Event{
			{Action: "create", Status: "ok", Identity: "alex@laptop", Detail: "worktree at feat-x",
				At: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)},
			{Action: "up", Status: "changed", Identity: "alex@laptop", Detail: "web started",
				At: time.Date(2026, 9, 8, 12, 1, 0, 0, time.UTC)},
		},
	}
}

func destroyedFixture() destroyed {
	return destroyed{App: "shop", Name: "feat-x", Destroyed: true, BranchDeleted: true}
}

func exposeFixture() *api.ExposeResult {
	return &api.ExposeResult{
		Env: sampleEnv(),
		Routes: []edge.Route{{
			Host: "feat-x.shop.test", Kind: edge.KindHTTPS, App: "shop", Env: "feat-x",
			Service: "web", CreatedAt: time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC),
			Targets: []edge.Target{{Replica: 1, Port: 20003, State: edge.TargetActive}},
		}},
		Changed: []string{"feat-x.shop.test"},
		URL:     "https://feat-x.shop.test",
	}
}

func writeURLNotes(w io.Writer) error {
	urls := []env.URL{
		{Kind: env.KindService, Name: "web", URL: "http://127.0.0.1:20003",
			InternalURL: "http://feat-x.shop.internal:3000", InternalAddress: "10.86.1.7:3000"},
		{Kind: env.KindDep, Name: "db", URL: "postgres://127.0.0.1:20001",
			InternalURL: "postgres://feat-x.shop.internal:5432", InternalAddress: "10.86.1.7:5432"},
	}
	if err := writeURLs(w, urls, "feat-x", ""); err != nil {
		return err
	}
	writeInternalURLs(w, urls, "")
	writePublicURLs(w, map[string]string{"web": "https://feat-x.shop.test"}, "")
	return nil
}

func upFixture() *api.UpResult {
	return &api.UpResult{
		Env:     sampleEnv(),
		Stack:   "node",
		Image:   "node:22-alpine",
		Network: "caramelo-shop-feat-x",
		Services: []env.Service{
			{Name: "web", Container: "caramelo-shop-feat-x-web", Image: "node:22-alpine",
				Status: env.ServiceRunning, Health: env.HealthOK, Change: env.ChangeCreated,
				URL: "http://127.0.0.1:20003"},
			{Name: "worker", Container: "caramelo-shop-feat-x-worker", Image: "node:22-alpine",
				Status: env.ServiceRunning, Change: env.ChangeUnchanged},
		},
	}
}

func stillUpFixture() *api.UpResult {
	r := exposedUp()
	for i := range r.Services {
		for j := range r.Services[i].Replicas {
			r.Services[i].Replicas[j].Since = time.Time{}
		}
	}
	for i := range r.Routes {
		r.Routes[i].CreatedAt = time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
		for j := range r.Routes[i].Targets {
			r.Routes[i].Targets[j].Since = time.Time{}
		}
	}
	return r
}

func downFixture() *api.DownResult {
	return &api.DownResult{Services: []env.Service{
		{Name: "web", Status: env.ServiceStopped, Change: env.ChangeRemoved},
		{Name: "worker", Status: env.ServiceStopped, Change: env.ChangeRemoved},
	}}
}

func configFixture() *api.EffectiveConfig {
	return &api.EffectiveConfig{
		App: "shop", Env: "feat-x", Stack: "node",

		Config: &config.App{Name: "shop"},
		Fields: []api.ConfigField{
			{Key: "services.web.run", Value: "npm start", Source: api.SourceFile},
			{Key: "services.web.port", Value: "3000", Source: api.SourceFile},
			{Key: "install", Value: "npm ci", Source: api.SourceDetected, Evidence: "package-lock.json"},
			{Key: "services.web.protocol", Value: "tcp", Source: api.SourceDefault},
		},
	}
}

func steadyEdgeStatus() *edge.Status {
	return &edge.Status{
		Running: true, Version: "v0.1.0", HTTP3: true,
		Listeners: []string{"tcp 0.0.0.0:80", "tcp 0.0.0.0:443", "udp 0.0.0.0:443"},
		TLS:       certs.ModeACME, ACMEDirectory: "https://acme-v02.api.letsencrypt.org/directory",
		Routes: []edge.Route{{
			Host: "feat-x.shop.test", Kind: edge.KindHTTPS, App: "shop", Env: "feat-x",
			Service: "web",
			Targets: []edge.Target{
				{Replica: 1, Port: 20003, State: edge.TargetActive},
				{Replica: 2, Port: 20004, State: edge.TargetDraining, Inflight: 3},
			},
		}, {
			Host: "api.shop.test", Kind: edge.KindHTTPS, App: "shop", Env: "production", Service: "api",
		}},
	}
}

func edgeNow() time.Time { return time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC) }

func staleEdgeStatus() *edge.Status {
	st := steadyEdgeStatus()
	st.ACMEDirectory = "https://acme-v02.api.letsencrypt.org/directory"
	st.Certificates = []certs.Certificate{
		{Host: "api.shop.test", Issuer: "E7", Serial: "0A1B", Managed: true, State: certs.Live,
			IssuerKey: "acme-v02.api.letsencrypt.org-directory",
			NotBefore: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
			NotAfter:  time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)},
		{Host: "feat-x.shop.test", Issuer: "E7", Serial: "0C3D", Managed: true, State: certs.Live,
			IssuerKey: "acme-v02.api.letsencrypt.org-directory",
			NotBefore: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
			NotAfter:  time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)},
		{Host: "feat-x.shop.test", Issuer: "Caramelo Internal CA", Serial: "0B7F",
			State: certs.Stale, IssuerKey: "internal",
			NotBefore: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
			NotAfter:  time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)},
	}
	return st
}

func prunedFixture() *certs.PruneResult {
	return &certs.PruneResult{
		KeepFor: certs.DefaultKeepStale,
		Removed: []certs.Certificate{
			{Host: "feat-x.shop.test", IssuerKey: "internal", State: certs.Stale,
				NotAfter: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)},
			{Host: "old.shop.test", IssuerKey: "acme-staging-v02.api.letsencrypt.org-directory",
				State: certs.Stale, NotAfter: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)},
		},
		Kept: []certs.Certificate{
			{Host: "api.shop.test", IssuerKey: "internal", State: certs.Stale,
				NotAfter: time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)},
		},
	}
}

func keysFixture() []state.Key {
	return []state.Key{
		{Name: "alex@laptop", Type: "ssh-ed25519", Fingerprint: "SHA256:abc123",
			AddedAt: time.Date(2026, 9, 8, 9, 0, 0, 0, time.UTC)},
		{Name: "ci", Type: "ssh-ed25519", Fingerprint: "SHA256:def456", Options: "caramelo-role=admin",
			AddedAt: time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)},
	}
}

func vpnStateFixture() *vpnclient.State {
	return &vpnclient.State{
		Mode: vpnclient.ModeUserspace, Machine: "worker1",
		Endpoint: "192.168.56.11:4021", PeerName: "alex-laptop",
		IP:       netip.MustParseAddr("10.86.255.2"),
		Subnet:   netip.MustParsePrefix("10.86.0.0/16"),
		Resolver: "10.86.0.1:53",
	}
}

func peersFixture() []state.Peer {
	return []state.Peer{
		{Name: "alex-laptop", PublicKey: "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG=", IP: "10.86.255.2",
			AddedBy: "setup", CreatedAt: time.Date(2026, 9, 9, 9, 0, 0, 0, time.UTC)},
		{Name: "ci", PublicKey: "zyxwvutsrqponmlkjihgfedcba9876543210ZYXWVUT=", IP: "10.86.255.3",
			CreatedAt: time.Date(2026, 9, 9, 9, 30, 0, 0, time.UTC)},
	}
}

func fleetsFixture() []fleetView {
	return fleetViews(remote.CommanderConfig{
		Name: "eric-laptop",
		Role: remote.RoleCommander,
		Commander: remote.Commander{
			DefaultFleet: "home",
			Fleets: map[string]remote.Fleet{
				"home": {
					Hub:       "eric@box.example.com:4022",
					PublicKey: "Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE=",
					Apps:      []string{"shop", "blog"},
				},
				"work": {Hub: "ops@hub.work.example:4022"},
			},
		},
	})
}
