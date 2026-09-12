package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/machine"
)

func open(t *testing.T, path string) Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func tempDB(t *testing.T) (Store, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state", "caramelo.db")
	return open(t, path), path
}

func TestOpenCreatesParentDirectory(t *testing.T) {
	_, path := tempDB(t)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("database file: %v", err)
	}
	if fi.Size() == 0 {
		t.Error("database file is empty")
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm&0o007 != 0 {
		t.Errorf("state directory mode %o must not be world readable", perm)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("want an error")
	}
}

func TestOpenFailsOnUnwritablePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(filepath.Join(dir, "file", "caramelo.db")); err == nil {
		t.Fatal("want an error")
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "caramelo.db")
	for i := 0; i < 3; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if err := s.SetSetting(context.Background(), "n", "v"); err != nil {
			t.Fatalf("write after open %d: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}

	s := open(t, path)
	got, err := s.(*store).appliedVersions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ms, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 {
		t.Fatal("no embedded migrations")
	}
	if len(got) != len(ms) {
		t.Errorf("applied %d versions, want %d", len(got), len(ms))
	}
	for _, m := range ms {
		if !got[m.version] {
			t.Errorf("migration %d not recorded", m.version)
		}
	}
}

func TestMigrationsAreOrdered(t *testing.T) {
	ms, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range ms {
		if m.version <= 0 {
			t.Errorf("migration %d has version %d", i, m.version)
		}
		if strings.TrimSpace(m.sql) == "" {
			t.Errorf("migration %d is empty", m.version)
		}
		if i > 0 && ms[i-1].version >= m.version {
			t.Errorf("migrations out of order: %d then %d", ms[i-1].version, m.version)
		}
	}
}

func sampleRecord() *machine.Record {
	return &machine.Record{
		MachineID: "1a1025b42a7543de8c004982abfc9024",
		Hostname:  "worker1",
		GaugedAt:  time.Date(2026, 9, 8, 16, 32, 0, 0, time.UTC),
		OS:        machine.OS{ID: "debian", VersionID: "13", Codename: "trixie", Kernel: "6.12.86+deb13-amd64", Arch: "x86_64", Virt: "kvm"},
		CPU:       machine.CPU{Count: 1, Model: "AMD Opteron 63xx class CPU"},
		Memory:    machine.Memory{TotalBytes: 1541255168, AvailableBytes: 1218744320},
		Dirs:      machine.Dirs{Config: "/etc/caramelo", State: "/var/lib/caramelo", Data: "/mnt/caramelo"},
		DataDir:   machine.Mount{Path: "/mnt/caramelo", Source: "/dev/vda1", FSType: "ext4", SizeBytes: 105088212992, AvailBytes: 98141057024},
		Disks:     []machine.Disk{{Name: "vda", SizeBytes: 107374182400, Rotational: true}},
		Network:   machine.Network{PrimaryIface: "eth0", PrimaryIP: "192.168.121.135", Addresses: []string{"192.168.121.135", "192.168.56.12"}},
		Cgroup:    machine.Cgroup{Version: 2, Controllers: []string{"cpuset", "cpu", "io", "memory", "pids"}},
		Docker:    machine.Docker{Installed: true, ServerVersion: "29.8.0", Rootless: true, StorageDriver: "overlayfs", NetDriver: "slirp4netns", PortDriver: "builtin", DataRoot: "/mnt/caramelo/docker"},
		Reserved:  machine.Reserved{MemoryBytes: 268435456, CPU: 0.25},
		Caramelo:  machine.Caramelo{Version: "v0.1.0", User: "caramelo", UID: 999, SSHPort: 4022, HostKeyFingerprint: "SHA256:abc"},
	}
}

func TestMachineRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, path := tempDB(t)

	if _, err := s.Machine(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("never gauged: err = %v, want ErrNotFound", err)
	}

	want := sampleRecord()
	if err := s.SaveMachine(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Machine(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	want.Hostname = "renamed"
	want.GaugedAt = want.GaugedAt.Add(time.Hour)
	if err := s.SaveMachine(ctx, want); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.(*store).db.QueryRowContext(ctx, `SELECT count(*) FROM machine`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("machine table has %d rows, want 1", n)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := open(t, path)
	got, err = reopened.Machine(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hostname != "renamed" {
		t.Errorf("hostname after reopen = %q", got.Hostname)
	}
}

func TestSaveMachineRejectsNil(t *testing.T) {
	s, _ := tempDB(t)
	if err := s.SaveMachine(context.Background(), nil); err == nil {
		t.Fatal("want an error")
	}
}

func TestSettings(t *testing.T) {
	ctx := context.Background()
	s, path := tempDB(t)

	if _, err := s.Setting(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if err := s.SetSetting(ctx, "host_key_fingerprint", "SHA256:abc"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Setting(ctx, "host_key_fingerprint"); err != nil || got != "SHA256:abc" {
		t.Fatalf("got %q, %v", got, err)
	}

	if err := s.SetSetting(ctx, "host_key_fingerprint", "SHA256:def"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Setting(ctx, "host_key_fingerprint"); got != "SHA256:def" {
		t.Errorf("got %q, want SHA256:def", got)
	}
	if err := s.SetSetting(ctx, "", "v"); err == nil {
		t.Error("empty key must be rejected")
	}

	s.Close()
	if got, err := open(t, path).Setting(ctx, "host_key_fingerprint"); err != nil || got != "SHA256:def" {
		t.Errorf("after reopen: got %q, %v", got, err)
	}
}

func TestKeys(t *testing.T) {
	ctx := context.Background()
	s, path := tempDB(t)

	if got, err := s.Keys(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty store: got %v, %v", got, err)
	}

	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	laptop := Key{Name: "alex@laptop", Type: "ssh-ed25519", PublicKey: "AAAAC3NzaC1lZDI1NTE5AAAAILAPTOP", Fingerprint: "SHA256:laptop", AddedAt: base}
	agent := Key{Name: "agent@ci", Type: "ssh-ed25519", PublicKey: "AAAAC3NzaC1lZDI1NTE5AAAAIAGENT", Fingerprint: "SHA256:agent", Options: "caramelo-role=admin", AddedAt: base.Add(time.Minute)}
	for _, k := range []Key{laptop, agent} {
		if err := s.AddKey(ctx, k); err != nil {
			t.Fatalf("AddKey(%s): %v", k.Name, err)
		}
	}

	got, err := s.Keys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "alex@laptop" || got[1].Name != "agent@ci" {
		t.Fatalf("keys are not in insertion order: %+v", got)
	}
	if got[1].Options != "caramelo-role=admin" || got[1].PublicKey != agent.PublicKey || got[1].Type != "ssh-ed25519" {
		t.Errorf("round trip lost fields: %+v", got[1])
	}
	if !got[0].AddedAt.Equal(base) {
		t.Errorf("added_at = %v, want %v", got[0].AddedAt, base)
	}

	t.Run("duplicate name", func(t *testing.T) {
		dup := laptop
		dup.Fingerprint = "SHA256:other"
		if err := s.AddKey(ctx, dup); !errors.Is(err, ErrExists) {
			t.Fatalf("err = %v, want ErrExists", err)
		}
	})
	t.Run("duplicate fingerprint", func(t *testing.T) {
		dup := laptop
		dup.Name = "another-name"
		if err := s.AddKey(ctx, dup); !errors.Is(err, ErrExists) {
			t.Fatalf("err = %v, want ErrExists", err)
		}
	})
	t.Run("missing fields", func(t *testing.T) {
		if err := s.AddKey(ctx, Key{Fingerprint: "SHA256:x"}); err == nil {
			t.Error("empty name must be rejected")
		}
		if err := s.AddKey(ctx, Key{Name: "x"}); err == nil {
			t.Error("empty fingerprint must be rejected")
		}
	})
	t.Run("added_at defaults to now", func(t *testing.T) {
		before := time.Now().Add(-time.Second)
		if err := s.AddKey(ctx, Key{Name: "no-time", Type: "ssh-ed25519", PublicKey: "AAAA", Fingerprint: "SHA256:no-time"}); err != nil {
			t.Fatal(err)
		}
		ks, err := s.Keys(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range ks {
			if k.Name == "no-time" && k.AddedAt.Before(before) {
				t.Errorf("added_at = %v, want >= %v", k.AddedAt, before)
			}
		}
		if err := s.RemoveKey(ctx, "no-time"); err != nil {
			t.Fatal(err)
		}
	})

	if err := s.RemoveKey(ctx, "does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if err := s.RemoveKey(ctx, "agent@ci"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Keys(ctx); len(got) != 1 || got[0].Name != "alex@laptop" {
		t.Errorf("after remove: %+v", got)
	}

	if err := s.AddKey(ctx, agent); err != nil {
		t.Fatalf("re-adding a removed key: %v", err)
	}

	s.Close()
	if got, err := open(t, path).Keys(ctx); err != nil || len(got) != 2 {
		t.Errorf("after reopen: got %d keys, %v", len(got), err)
	}
}

func TestSetupRuns(t *testing.T) {
	ctx := context.Background()
	s, path := tempDB(t)

	if got, err := s.SetupRuns(ctx, 10); err != nil || len(got) != 0 {
		t.Fatalf("empty store: %v, %v", got, err)
	}

	start := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	steps := []string{"preflight", "gauge", "dirs", "user", "host-config"}
	for i, step := range steps {
		r := SetupRun{
			RunID: "run-1", Step: step, Status: "changed",
			Detail:    "did " + step,
			StartedAt: start.Add(time.Duration(i) * time.Second),
			Duration:  time.Duration(i+1) * 100 * time.Millisecond,
			Version:   "v0.1.0",
		}
		if step == "user" {
			r.Status, r.Error = "failed", "useradd: exit 1"
		}
		if err := s.RecordSetupRun(ctx, r); err != nil {
			t.Fatalf("RecordSetupRun(%s): %v", step, err)
		}
	}

	got, err := s.SetupRuns(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(steps) {
		t.Fatalf("got %d runs, want %d", len(got), len(steps))
	}

	if got[0].Step != "host-config" || got[len(got)-1].Step != "preflight" {
		t.Errorf("wrong order: %s ... %s", got[0].Step, got[len(got)-1].Step)
	}
	if got[0].Duration != 500*time.Millisecond || got[0].Version != "v0.1.0" || got[0].RunID != "run-1" {
		t.Errorf("round trip lost fields: %+v", got[0])
	}
	if !got[0].StartedAt.Equal(start.Add(4 * time.Second)) {
		t.Errorf("started_at = %v", got[0].StartedAt)
	}
	var failed SetupRun
	for _, r := range got {
		if r.Step == "user" {
			failed = r
		}
	}
	if failed.Status != "failed" || failed.Error != "useradd: exit 1" {
		t.Errorf("failed step: %+v", failed)
	}

	limited, err := s.SetupRuns(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 || limited[0].Step != "host-config" {
		t.Errorf("limit: %+v", limited)
	}

	s.Close()
	if got, err := open(t, path).SetupRuns(ctx, 0); err != nil || len(got) != len(steps) {
		t.Errorf("after reopen: %d runs, %v", len(got), err)
	}
}

func TestConcurrentWriters(t *testing.T) {

	ctx := context.Background()
	s, _ := tempDB(t)
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if err := s.RecordSetupRun(ctx, SetupRun{RunID: "r", Step: "s", Status: "ok", StartedAt: time.Now()}); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent write: %v", err)
	}
	got, err := s.SetupRuns(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 100 {
		t.Errorf("got %d rows, want 100", len(got))
	}
}

func TestDSNCarriesPragmas(t *testing.T) {
	got := dsn("/var/lib/caramelo/caramelo.db")
	for _, want := range []string{"file:///var/lib/caramelo/caramelo.db", "journal_mode%28WAL%29", "busy_timeout%285000%29", "foreign_keys%281%29"} {
		if !strings.Contains(got, want) {
			t.Errorf("dsn = %q, missing %q", got, want)
		}
	}
}

func TestOpenAppliesPragmas(t *testing.T) {
	s, _ := tempDB(t)
	db := s.(*store).db
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
}

func TestCloseIsReported(t *testing.T) {
	s, _ := tempDB(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.Machine(context.Background()); err == nil {
		t.Error("using a closed store must fail")
	}
}

func TestOpenMakesTheDatabaseOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "caramelo.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, p := range []string{path, path + "-wal"} {
		info, err := os.Stat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s is %04o, want 0600", filepath.Base(p), mode)
		}
	}
}

func TestFleetSchema(t *testing.T) {
	s, _ := tempDB(t)
	db := s.(*store).db
	ctx := context.Background()
	for table, columns := range map[string][]string{
		"machines":       {"name", "role", "public_key", "subnet", "arch", "os", "endpoint", "private", "joined_at", "last_seen", "gauge_json"},
		"env_directory":  {"app", "env", "machine", "address", "owner", "mode", "via", "updated_at"},
		"release_images": {"release_id", "service", "arch", "machine", "image_id", "built_at"},
		"join_tokens":    {"token_hash", "created_by", "created_at", "expires_at", "used_by", "used_at"},
		"envs":           {"owner", "via"},
		"releases":       {"machine"},
		"env_events":     {"machine"},
	} {
		have := map[string]bool{}
		rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				t.Fatalf("%s: %v", table, err)
			}
			have[name] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		for _, c := range columns {
			if !have[c] {
				t.Errorf("%s has no column %q", table, c)
			}
		}
	}
}
