package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/machine"
	"github.com/plytz/caramelo/internal/state"
)

type keyService struct {
	api.Service

	keys       []state.Key
	addName    string
	addOptions string
	addLine    string
	removed    string
	err        error
}

func (s *keyService) Status(context.Context) (*api.Status, error) { return &api.Status{}, s.err }
func (s *keyService) Machine(context.Context) (*machine.Record, error) {
	return &machine.Record{}, s.err
}
func (s *keyService) Keys(context.Context) ([]state.Key, error) { return s.keys, s.err }

func (s *keyService) AddKey(_ context.Context, name, options, line string) (*state.Key, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.addName, s.addOptions, s.addLine = name, options, line
	return &state.Key{Name: name, Options: options, Type: "ssh-ed25519", Fingerprint: "SHA256:abc"}, nil
}

func (s *keyService) RemoveKey(_ context.Context, name string) error {
	if s.err != nil {
		return s.err
	}
	s.removed = name
	return nil
}

func runService(t *testing.T, ctx context.Context, svc api.Service, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunWith(ctx, args, &stdout, &stderr, Options{Service: svc})
	t.Logf("caramelo %s -> exit %d\nstdout: %q\nstderr: %q", strings.Join(args, " "), code, stdout.String(), stderr.String())
	return code, stdout.String(), stderr.String()
}

const testKeyLine = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ8CmLbXHVQ0h3xz3ETuAHU0Fn7uEfeaP1J8AC0tYsQe alex@laptop"

func TestKeyAddFromFile(t *testing.T) {
	svc := &keyService{}
	path := filepath.Join(t.TempDir(), "id.pub")
	if err := os.WriteFile(path, []byte(testKeyLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := runService(t, context.Background(), svc,
		"key", "add", "--name", "laptop", "--options", "caramelo-role=admin", "--file", path)
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if svc.addName != "laptop" || svc.addOptions != "caramelo-role=admin" || svc.addLine != testKeyLine {
		t.Errorf("service got name=%q options=%q line=%q", svc.addName, svc.addOptions, svc.addLine)
	}
	if !strings.Contains(stdout, "added key laptop") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestKeyAddFromStdin(t *testing.T) {
	svc := &keyService{}
	ctx := withStdin(context.Background(), strings.NewReader(testKeyLine+"\n"))
	code, _, stderr := runService(t, ctx, svc, "key", "add", "--name", "laptop")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if svc.addLine != testKeyLine {
		t.Errorf("line = %q, want the one piped in", svc.addLine)
	}
}

func TestKeyAddJSON(t *testing.T) {
	svc := &keyService{}
	ctx := withStdin(context.Background(), strings.NewReader(testKeyLine))
	code, stdout, _ := runService(t, ctx, svc, "key", "add", "--name", "laptop", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var got state.Key
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not a key: %v\n%s", err, stdout)
	}
	if got.Name != "laptop" || got.Fingerprint != "SHA256:abc" {
		t.Errorf("key = %+v", got)
	}
}

func TestKeyAddNeedsANameAndAKey(t *testing.T) {
	svc := &keyService{}
	ctx := withStdin(context.Background(), strings.NewReader(testKeyLine))
	if code, _, stderr := runService(t, ctx, svc, "key", "add"); code != ExitUsage {
		t.Errorf("without --name: exit %d (%q), want %d", code, stderr, ExitUsage)
	}
	ctx = withStdin(context.Background(), strings.NewReader("   \n"))
	if code, _, stderr := runService(t, ctx, svc, "key", "add", "--name", "x"); code != ExitError {
		t.Errorf("with empty input: exit %d (%q), want %d", code, stderr, ExitError)
	}
	ctx = withStdin(context.Background(), strings.NewReader(testKeyLine+"\n"+testKeyLine+"\n"))
	if code, _, _ := runService(t, ctx, svc, "key", "add", "--name", "x"); code != ExitError {
		t.Errorf("with two keys: exit %d, want %d", code, ExitError)
	}
}

func TestKeyList(t *testing.T) {
	added := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	svc := &keyService{keys: []state.Key{
		{Name: "laptop", Type: "ssh-ed25519", Fingerprint: "SHA256:abc", AddedAt: added},
		{Name: "agent", Type: "ssh-ed25519", Fingerprint: "SHA256:def", Options: "caramelo-role=admin"},
	}}
	code, stdout, _ := runService(t, context.Background(), svc, "key", "list")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"NAME", "laptop", "SHA256:abc", "caramelo-role=admin"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("table %q is missing %q", stdout, want)
		}
	}

	code, stdout, _ = runService(t, context.Background(), svc, "key", "list", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var got []state.Key
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not a key list: %v\n%s", err, stdout)
	}
	if len(got) != 2 || got[0].Name != "laptop" {
		t.Errorf("keys = %+v", got)
	}
}

func TestKeyListEmpty(t *testing.T) {
	svc := &keyService{}
	code, stdout, _ := runService(t, context.Background(), svc, "key", "list")
	if code != ExitOK || !strings.Contains(stdout, "no keys authorized") {
		t.Errorf("exit %d, stdout %q", code, stdout)
	}

	_, stdout, _ = runService(t, context.Background(), svc, "key", "list", "--json")
	if strings.TrimSpace(stdout) != "[]" {
		t.Errorf("stdout = %q, want an empty JSON array", stdout)
	}
}

func TestKeyRemove(t *testing.T) {
	svc := &keyService{}
	code, stdout, _ := runService(t, context.Background(), svc, "key", "remove", "laptop")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if svc.removed != "laptop" {
		t.Errorf("removed %q", svc.removed)
	}
	if !strings.Contains(stdout, "removed key laptop") {
		t.Errorf("stdout = %q", stdout)
	}
	if code, _, _ := runService(t, context.Background(), svc, "key", "remove"); code != ExitUsage {
		t.Errorf("remove without a name = %d, want %d", code, ExitUsage)
	}
}

func TestKeyCommandsReportServiceErrors(t *testing.T) {
	svc := &keyService{err: errors.New("the daemon said no")}
	for _, args := range [][]string{{"key", "list"}, {"key", "remove", "x"}} {
		code, stdout, stderr := runService(t, context.Background(), svc, args...)
		if code != ExitError {
			t.Errorf("%v exit = %d, want %d", args, code, ExitError)
		}
		if !strings.Contains(stderr, "the daemon said no") {
			t.Errorf("%v stderr = %q", args, stderr)
		}
		if stdout != "" {
			t.Errorf("%v stdout = %q, want empty", args, stdout)
		}
	}
}
