package state

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSentinelErrorsRead(t *testing.T) {
	if ErrNotFound.Error() != "not found" {
		t.Errorf("ErrNotFound = %q", ErrNotFound.Error())
	}
	if ErrExists.Error() != "already exists" {
		t.Errorf("ErrExists = %q", ErrExists.Error())
	}

	wrapped := fmt.Errorf("read env %q: %w", "feat-x", ErrNotFound)
	if !errors.Is(wrapped, ErrNotFound) {
		t.Errorf("%v is not ErrNotFound after wrapping", wrapped)
	}
	if errors.Is(wrapped, ErrExists) {
		t.Errorf("%v matched the wrong sentinel; the two must not be equal", wrapped)
	}
}

func TestPathReportsTheFileItWasOpenedFrom(t *testing.T) {
	s, path := tempDB(t)
	p, ok := s.(interface{ Path() string })
	if !ok {
		t.Fatalf("%T does not report its path", s)
	}
	if p.Path() != path {
		t.Errorf("Path() = %q, want %q", p.Path(), path)
	}
}

func TestPathKeepsARelativePathAsGiven(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	s := open(t, "caramelo.db")
	p, ok := s.(interface{ Path() string })
	if !ok {
		t.Fatalf("%T does not report its path", s)
	}
	if p.Path() != "caramelo.db" {
		t.Errorf("Path() = %q, want the relative path it was opened with", p.Path())
	}
	if !strings.Contains(dsn("caramelo.db"), filepath.Join(dir, "caramelo.db")) {
		t.Errorf("dsn(%q) = %q, want the absolute path: SQLite resolves it against its own cwd", "caramelo.db", dsn("caramelo.db"))
	}
}

func TestEveryReadAndWriteFailsOnceTheStoreIsClosed(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	if err := s.AddApp(ctx, sampleApp("shop")); err != nil {
		t.Fatal(err)
	}
	rec, err := s.CreateEnv(ctx, sampleEnv("shop", "feat-x", 20000))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	calls := []struct {
		name string
		call func() error
	}{
		{"Machine", func() error { _, err := s.Machine(ctx); return err }},
		{"SaveMachine", func() error { return s.SaveMachine(ctx, sampleRecord()) }},
		{"Setting", func() error { _, err := s.Setting(ctx, "k"); return err }},
		{"SetSetting", func() error { return s.SetSetting(ctx, "k", "v") }},
		{"Keys", func() error { _, err := s.Keys(ctx); return err }},
		{"AddKey", func() error {
			return s.AddKey(ctx, Key{Name: "laptop", Type: "ssh-ed25519", PublicKey: "AAAA", Fingerprint: "SHA256:x"})
		}},
		{"RemoveKey", func() error { return s.RemoveKey(ctx, "laptop") }},
		{"RecordSetupRun", func() error {
			return s.RecordSetupRun(ctx, SetupRun{RunID: "r", Step: "preflight", Status: "ok", StartedAt: time.Now()})
		}},
		{"SetupRuns", func() error { _, err := s.SetupRuns(ctx, 10); return err }},
		{"Apps", func() error { _, err := s.Apps(ctx); return err }},
		{"App", func() error { _, err := s.App(ctx, "shop"); return err }},
		{"AddApp", func() error { return s.AddApp(ctx, sampleApp("other")) }},
		{"SetAppDefaultBranch", func() error { return s.SetAppDefaultBranch(ctx, "shop", "main") }},
		{"Envs", func() error { _, err := s.Envs(ctx, "shop"); return err }},
		{"Env", func() error { _, err := s.Env(ctx, "shop", "feat-x"); return err }},
		{"CreateEnv", func() error { _, err := s.CreateEnv(ctx, sampleEnv("shop", "feat-y", 20016)); return err }},
		{"UpdateEnvStatus", func() error { return s.UpdateEnvStatus(ctx, rec.ID, EnvReady) }},
		{"UpdateEnv", func() error { return s.UpdateEnv(ctx, *rec) }},
		{"DeleteEnv", func() error { return s.DeleteEnv(ctx, rec.ID) }},
		{"AddResource", func() error {
			return s.AddResource(ctx, EnvResource{EnvID: rec.ID, Kind: ResourceBranch, Name: "feat-x"})
		}},
		{"Resources", func() error { _, err := s.Resources(ctx, rec.ID); return err }},
		{"DeleteResource", func() error { return s.DeleteResource(ctx, 1) }},
		{"AddEvent", func() error { return s.AddEvent(ctx, EnvEvent{EnvID: rec.ID, Action: "create", Status: "ok"}) }},
		{"Events", func() error { _, err := s.Events(ctx, rec.ID, 10); return err }},
		{"Services", func() error { _, err := s.Services(ctx, rec.ID); return err }},
		{"PutService", func() error {
			return s.PutService(ctx, EnvService{EnvID: rec.ID, Name: "web", Status: ServiceStarting})
		}},
		{"DeleteService", func() error { return s.DeleteService(ctx, rec.ID, "web") }},
		{"SetAppStack", func() error { return s.SetAppStack(ctx, "shop", "go") }},
	}
	for _, c := range calls {
		err := c.call()
		if err == nil {
			t.Errorf("%s on a closed store returned no error", c.name)
			continue
		}

		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrExists) {
			t.Errorf("%s on a closed store = %v, want a real failure, not a sentinel", c.name, err)
		}
	}
}

func TestWritesRejectIncompleteRows(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	if err := s.AddApp(ctx, sampleApp("shop")); err != nil {
		t.Fatal(err)
	}
	rec, err := s.CreateEnv(ctx, sampleEnv("shop", "feat-x", 20000))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		want string
		call func() error
	}{
		{"app without a name", "name", func() error {
			return s.AddApp(ctx, App{RepoPath: "/mnt/caramelo/apps/x/repo.git"})
		}},
		{"app without a repository", "repository", func() error { return s.AddApp(ctx, App{Name: "x"}) }},
		{"env without an app", "app", func() error {
			e := sampleEnv("", "feat-y", 20016)
			_, err := s.CreateEnv(ctx, e)
			return err
		}},
		{"env without a name", "name", func() error {
			e := sampleEnv("shop", "", 20016)
			_, err := s.CreateEnv(ctx, e)
			return err
		}},
		{"status update with no status", "status", func() error { return s.UpdateEnvStatus(ctx, rec.ID, "") }},
		{"env update with no id", "id", func() error { return s.UpdateEnv(ctx, EnvRecord{App: "shop", Name: "feat-x"}) }},
		{"resource with no env", "env id", func() error {
			return s.AddResource(ctx, EnvResource{Kind: ResourceBranch, Name: "feat-x"})
		}},
		{"resource with no kind", "kind", func() error {
			return s.AddResource(ctx, EnvResource{EnvID: rec.ID, Name: "feat-x"})
		}},
		{"resource with no name", "name", func() error {
			return s.AddResource(ctx, EnvResource{EnvID: rec.ID, Kind: ResourceBranch})
		}},

		{"event with no action", "action", func() error { return s.AddEvent(ctx, EnvEvent{}) }},
	}
	for _, c := range cases {
		err := c.call()
		if err == nil {
			t.Errorf("%s was accepted", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not name %q", c.name, err, c.want)
		}
	}

	envs, err := s.Envs(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 1 {
		t.Errorf("store holds %d envs, want only the valid one", len(envs))
	}
	res, err := s.Resources(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Errorf("store holds %+v, want no resources", res)
	}
}

func TestUpdatesOfAVanishedRowAreNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	if err := s.AddApp(ctx, sampleApp("shop")); err != nil {
		t.Fatal(err)
	}
	rec, err := s.CreateEnv(ctx, sampleEnv("shop", "feat-x", 20000))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteEnv(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateEnvStatus(ctx, rec.ID, EnvReady); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateEnvStatus on a deleted env = %v, want ErrNotFound", err)
	}
	if err := s.UpdateEnv(ctx, *rec); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateEnv on a deleted env = %v, want ErrNotFound", err)
	}
	if err := s.SetAppDefaultBranch(ctx, "nosuchapp", "main"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetAppDefaultBranch on a missing app = %v, want ErrNotFound", err)
	}

	if err := s.DeleteEnv(ctx, rec.ID); err != nil {
		t.Errorf("deleting a deleted env = %v, want nil", err)
	}
	if err := s.DeleteResource(ctx, 12345); err != nil {
		t.Errorf("deleting a missing resource = %v, want nil", err)
	}
}
