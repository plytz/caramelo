package sshapi

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/plytz/caramelo/internal/testutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/machine"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup"
	"github.com/plytz/caramelo/internal/state"
)

type fakeStore struct {
	state.Store

	record    *machine.Record
	settings  map[string]string
	keys      []state.Key
	setupRuns []state.SetupRun
	addKeyErr error
	saves     int
	envs      []state.EnvRecord
	deploys   []state.Deploy
	events    []state.EnvEvent
}

func newFakeStore() *fakeStore { return &fakeStore{settings: map[string]string{}} }

func (s *fakeStore) Envs(context.Context, string) ([]state.EnvRecord, error) {
	return s.envs, nil
}

func (s *fakeStore) UnfinishedDeploys(context.Context) ([]state.Deploy, error) {
	return s.deploys, nil
}

func (s *fakeStore) Env(_ context.Context, app, name string) (*state.EnvRecord, error) {
	for i := range s.envs {
		if s.envs[i].App == app && s.envs[i].Name == name {
			return &s.envs[i], nil
		}
	}
	return nil, state.ErrNotFound
}

func (s *fakeStore) AddEvent(_ context.Context, e state.EnvEvent) error {
	s.events = append(s.events, e)
	return nil
}

func (s *fakeStore) Machine(context.Context) (*machine.Record, error) {
	if s.record == nil {
		return nil, state.ErrNotFound
	}
	return s.record, nil
}

func (s *fakeStore) SaveMachine(_ context.Context, r *machine.Record) error {
	s.record = r
	s.saves++
	return nil
}

func (s *fakeStore) Setting(_ context.Context, key string) (string, error) {
	v, ok := s.settings[key]
	if !ok {
		return "", state.ErrNotFound
	}
	return v, nil
}

func (s *fakeStore) SetSetting(_ context.Context, key, value string) error {
	s.settings[key] = value
	return nil
}

func (s *fakeStore) Keys(context.Context) ([]state.Key, error) { return s.keys, nil }

func (s *fakeStore) AddKey(_ context.Context, k state.Key) error {
	if s.addKeyErr != nil {
		return s.addKeyErr
	}
	for _, x := range s.keys {
		if x.Name == k.Name || x.Fingerprint == k.Fingerprint {
			return state.ErrExists
		}
	}
	s.keys = append(s.keys, k)
	return nil
}

func (s *fakeStore) RemoveKey(_ context.Context, name string) error {
	for i, k := range s.keys {
		if k.Name == name {
			s.keys = append(s.keys[:i], s.keys[i+1:]...)
			return nil
		}
	}
	return state.ErrNotFound
}

func (s *fakeStore) RecordSetupRun(_ context.Context, r state.SetupRun) error {
	s.setupRuns = append(s.setupRuns, r)
	return nil
}

func (s *fakeStore) SetupRuns(_ context.Context, limit int) ([]state.SetupRun, error) {
	return s.setupRuns, nil
}

func (s *fakeStore) Close() error { return nil }

type fakeRunner struct {
	res runner.Result
	err error
	got runner.Cmd
}

func (r *fakeRunner) Run(_ context.Context, c runner.Cmd) (runner.Result, error) {
	r.got = c
	return r.res, r.err
}

func newTestDaemon(t *testing.T) (*Daemon, *fakeStore) {
	t.Helper()
	dir := t.TempDir()
	cfg := serverconfig.Default()
	cfg.APIListen = serverconfig.APIListenPublic
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.RunDir = filepath.Join(testutil.ShortDir(t), "run")
	store := newFakeStore()
	d := &Daemon{
		Config:             cfg,
		ConfigDir:          filepath.Join(dir, "etc"),
		Store:              store,
		Runner:             &fakeRunner{res: runner.Result{Stdout: `{"ServerVersion":"29.8.0","SecurityOptions":["name=seccomp,profile=builtin","name=rootless"]}`}},
		Version:            "test",
		StartedAt:          time.Now().Add(-time.Minute),
		HostKeyFingerprint: "SHA256:host",
		Hostname:           "testbox",
	}
	return d, store
}

func TestDaemonStatus(t *testing.T) {
	d, _ := newTestDaemon(t)
	ctx := WithSession(context.Background(), api.Session{Transport: TransportSSH, Identity: "commander", Machine: "box"})

	st, err := d.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Transport != TransportSSH || st.Identity != "commander" || st.Machine != "box" {
		t.Errorf("session fields = %+v, want the ones from the context", st)
	}
	if st.HostKeyFingerprint != "SHA256:host" {
		t.Errorf("fingerprint = %q", st.HostKeyFingerprint)
	}
	if st.Paths.Socket != d.Config.SocketPath() || st.Paths.State != d.Config.StateDir {
		t.Errorf("paths = %+v", st.Paths)
	}
	if st.Uptime <= 0 {
		t.Errorf("uptime = %v, want positive", st.Uptime)
	}
	if !st.Docker.Running || !st.Docker.Rootless || st.Docker.ServerVersion != "29.8.0" {
		t.Errorf("docker = %+v, want a running rootless 29.8.0", st.Docker)
	}
	if got := d.Runner.(*fakeRunner).got.User; got != d.Config.User {
		t.Errorf("docker was run as %q, want the daemon user %q", got, d.Config.User)
	}
}

func TestDaemonStatusWithoutASession(t *testing.T) {
	d, _ := newTestDaemon(t)
	st, err := d.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Transport != "" || st.Identity != "" {
		t.Errorf("in-process call reported transport %q identity %q, want empty", st.Transport, st.Identity)
	}
	if st.Machine != "testbox" {
		t.Errorf("machine = %q, want the local hostname", st.Machine)
	}
}

func TestDaemonStatusWhenDockerIsDown(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.Runner = &fakeRunner{res: runner.Result{ExitCode: 1, Stderr: "Cannot connect to the Docker daemon\nmore\n"}}
	st, err := d.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Docker.Running {
		t.Error("docker reported as running")
	}
	if st.Docker.Error != "Cannot connect to the Docker daemon" {
		t.Errorf("error = %q, want the first stderr line", st.Docker.Error)
	}
}

func TestDaemonMachineGaugesOnceThenReads(t *testing.T) {
	d, store := newTestDaemon(t)
	gauges := 0
	d.Gauge = func(context.Context) (*machine.Record, error) {
		gauges++
		return &machine.Record{Hostname: "testbox"}, nil
	}
	rec, err := d.Machine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rec.Caramelo.HostKeyFingerprint != "SHA256:host" {
		t.Errorf("fingerprint = %q, want the daemon's", rec.Caramelo.HostKeyFingerprint)
	}
	if store.saves != 1 {
		t.Errorf("saves = %d, want 1", store.saves)
	}
	if _, err := d.Machine(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gauges != 1 {
		t.Errorf("gauged %d times, want 1: the stored record answers afterwards", gauges)
	}
}

func TestDaemonKeys(t *testing.T) {
	d, store := newTestDaemon(t)
	ctx := context.Background()
	line, _ := newKeyLine(t, "")

	key, err := d.AddKey(ctx, "laptop", "caramelo-role=admin", line)
	if err != nil {
		t.Fatal(err)
	}
	if key.Name != "laptop" || key.Options != "caramelo-role=admin" {
		t.Errorf("key = %+v", key)
	}
	if len(store.keys) != 1 {
		t.Errorf("store has %d keys, want 1", len(store.keys))
	}

	keys, err := d.Keys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Name != "laptop" || keys[0].AddedAt.IsZero() {
		t.Fatalf("Keys = %+v, want one key with an added_at from the store", keys)
	}

	if _, err := d.AddKey(ctx, "", "", line); err == nil {
		t.Error("a key with no name was accepted")
	}
	if err := d.RemoveKey(ctx, "laptop"); err != nil {
		t.Fatal(err)
	}
	if keys, _ := d.Keys(ctx); len(keys) != 0 {
		t.Errorf("Keys after remove = %+v", keys)
	}
	if len(store.keys) != 0 {
		t.Errorf("store still has %d keys", len(store.keys))
	}
	if err := d.RemoveKey(ctx, "laptop"); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("removing a missing key = %v, want ErrKeyNotFound", err)
	}
}

func TestDaemonAddKeyRollsBackWhenTheStoreFails(t *testing.T) {
	d, store := newTestDaemon(t)
	store.addKeyErr = errors.New("disk full")
	line, _ := newKeyLine(t, "")

	if _, err := d.AddKey(context.Background(), "laptop", "", line); err == nil {
		t.Fatal("want an error")
	}
	entries, err := ListKeys(d.Config.AuthorizedKeysPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("authorized_keys still holds %+v; the file must be rolled back", entries)
	}
}

func TestImportSetupReports(t *testing.T) {
	d, store := newTestDaemon(t)
	dir := SetupReportsDir(d.Config)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	report := setup.Report{
		RunID:   "run-1",
		Version: "test",
		Results: []setup.Result{
			{Step: "preflight", Status: setup.StatusOK, Detail: "debian 13", Duration: time.Second},
			{Step: "user", Status: setup.StatusChanged, Duration: 2 * time.Second},
		},
		Changed: 1,
	}
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "run-1.json")
	if err := os.WriteFile(path, b, 0o640); err != nil {
		t.Fatal(err)
	}

	logf := func(string, ...any) {}
	ctx := context.Background()
	if err := ImportSetupReports(ctx, store, d.Config, logf); err != nil {
		t.Fatal(err)
	}
	if len(store.setupRuns) != 2 {
		t.Fatalf("recorded %d runs, want 2", len(store.setupRuns))
	}
	if store.setupRuns[0].Step != "preflight" || store.setupRuns[0].RunID != "run-1" {
		t.Errorf("first run = %+v", store.setupRuns[0])
	}
	if store.setupRuns[1].Status != string(setup.StatusChanged) {
		t.Errorf("second status = %q", store.setupRuns[1].Status)
	}

	if err := ImportSetupReports(ctx, store, d.Config, logf); err != nil {
		t.Fatal(err)
	}
	if len(store.setupRuns) != 2 {
		t.Errorf("re-importing recorded %d runs, want the original 2", len(store.setupRuns))
	}
}

func TestImportSetupReportsSkipsBrokenFiles(t *testing.T) {
	d, store := newTestDaemon(t)
	dir := SetupReportsDir(d.Config)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0o640); err != nil {
		t.Fatal(err)
	}
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, format) }
	if err := ImportSetupReports(context.Background(), store, d.Config, logf); err != nil {
		t.Fatalf("one broken report must not fail the daemon: %v", err)
	}
	if len(logged) == 0 {
		t.Error("the broken report was not reported")
	}
	if len(store.setupRuns) != 0 {
		t.Errorf("recorded %+v", store.setupRuns)
	}
}

func TestImportSetupReportsWithNoDirectory(t *testing.T) {
	d, store := newTestDaemon(t)
	if err := ImportSetupReports(context.Background(), store, d.Config, func(string, ...any) {}); err != nil {
		t.Fatalf("a machine that never ran setup must not fail: %v", err)
	}
}

func TestHasSecurityOption(t *testing.T) {
	opts := []string{"name=seccomp,profile=builtin", "name=rootless", "name=cgroupns"}
	if !hasSecurityOption(opts, "rootless") {
		t.Error("rootless not found")
	}
	if hasSecurityOption(opts, "selinux") {
		t.Error("selinux found where there is none")
	}
	if hasSecurityOption(nil, "rootless") {
		t.Error("found something in an empty list")
	}
}

func TestSanitizeVersion(t *testing.T) {

	if got := sanitizeVersion("v0.1.0-3-gabc def"); strings.ContainsAny(got, " -") {
		t.Errorf("sanitizeVersion = %q, want no spaces or dashes", got)
	}
	if got := sanitizeVersion(""); got != "dev" {
		t.Errorf("sanitizeVersion(\"\") = %q, want dev", got)
	}
}
