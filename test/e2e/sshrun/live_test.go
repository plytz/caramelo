//go:build e2e

package sshrun

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/test/e2e/inventory"
)

func liveHosts(t *testing.T) []Host {
	t.Helper()
	if strings.TrimSpace(os.Getenv(inventory.EnvVar)) == "" {
		t.Skipf("%s is unset: the live smoke test needs an inventory", inventory.EnvVar)
	}
	inv, path, err := inventory.LoadEnv()
	if err != nil {
		t.Fatalf("read the inventory: %v", err)
	}
	if inv.Count() == 0 {
		t.Fatalf("the inventory at %s lists no machine", path)
	}
	hosts := make([]Host, 0, inv.Count())
	for _, m := range inv.Machines {
		hosts = append(hosts, Host{Name: m.Name, Addr: m.Host, Port: m.Port, User: m.User, Key: m.Key, HostKey: m.HostKey, Arch: m.Arch})
	}
	return hosts
}

func TestLiveSmoke(t *testing.T) {
	for _, h := range liveHosts(t) {
		t.Run(h.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			dir := t.TempDir()
			s, err := Open(ctx, dir, h)
			if err != nil {
				t.Fatalf("open a session: %v", err)
			}
			defer func() {
				if err := s.Close(); err != nil {
					t.Errorf("close the session: %v", err)
				}
			}()

			res, err := s.Run(ctx, "id -un")
			if err != nil || res.ExitCode != 0 {
				t.Fatalf("id -un: %v (exit %d, %s)", err, res.ExitCode, firstLine(res.Stderr))
			}
			if strings.TrimSpace(res.Stdout) != h.User {
				t.Errorf("the session runs as %q, the inventory says %q", strings.TrimSpace(res.Stdout), h.User)
			}

			res, err = s.RunAsRoot(ctx, "id -un")
			if err != nil || res.ExitCode != 0 {
				t.Fatalf("sudo -n id -un: %v (exit %d, %s)", err, res.ExitCode, firstLine(res.Stderr))
			}
			if strings.TrimSpace(res.Stdout) != "root" {
				t.Errorf("sudo ran as %q", strings.TrimSpace(res.Stdout))
			}

			local := filepath.Join(dir, "payload")
			body := []byte("sshrun smoke " + h.Name + "\n")
			if err := os.WriteFile(local, body, 0o751); err != nil {
				t.Fatalf("write the payload: %v", err)
			}
			remote := StagingDir + "/sshrun-smoke-" + stageID(h.Name+dir)
			t.Cleanup(func() {
				clean, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if _, err := s.Run(clean, "rm -f "+Quote(remote)); err != nil {
					t.Errorf("remove %s: %v", remote, err)
				}
			})
			if err := s.Copy(ctx, local, remote); err != nil {
				t.Fatalf("copy the payload: %v", err)
			}
			res, err = s.Run(ctx, "stat -c %a "+Quote(remote))
			if err != nil || res.ExitCode != 0 {
				t.Fatalf("stat the payload: %v (exit %d, %s)", err, res.ExitCode, firstLine(res.Stderr))
			}
			if strings.TrimSpace(res.Stdout) != "751" {
				t.Errorf("the remote mode is %q, want 751", strings.TrimSpace(res.Stdout))
			}
			back := filepath.Join(dir, "fetched")
			if err := s.Fetch(ctx, remote, back); err != nil {
				t.Fatalf("fetch the payload: %v", err)
			}
			got, err := os.ReadFile(back)
			if err != nil {
				t.Fatalf("read what came back: %v", err)
			}
			if string(got) != string(body) {
				t.Errorf("the round trip gave %q, want %q", string(got), string(body))
			}

			s.Rows, s.Cols = 24, 118
			res, err = s.RunTTY(ctx, []string{"TERM=xterm"}, "tput cols")
			if err != nil || res.ExitCode != 0 {
				t.Fatalf("tput cols over a terminal: %v (exit %d, %s)", err, res.ExitCode, firstLine(res.Stderr))
			}
			cols := strings.TrimSpace(strings.ReplaceAll(res.Stdout, "\r", ""))
			if cols != strconv.Itoa(s.Cols) {
				t.Errorf("the terminal reports %q columns, want %d", cols, s.Cols)
			}
		})
	}
}
