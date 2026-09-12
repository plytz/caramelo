//go:build e2e

package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/test/e2e/inventory"
	"github.com/plytz/caramelo/test/e2e/sshrun"
)

var (
	inv     inventory.Inventory
	invPath string
	runDir  string

	sessionMu sync.Mutex
	sessions  = map[string]*sshrun.Session{}
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	loaded, path, err := inventory.LoadEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "preflight: %v\n", err)
		return 1
	}
	inv, invPath = loaded, path

	dir, err := os.MkdirTemp("", "caramelo-preflight-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "preflight: making the run directory: %v\n", err)
		return 1
	}
	runDir = dir

	code := m.Run()

	closeSessions()
	if err := os.RemoveAll(runDir); err != nil {
		fmt.Fprintf(os.Stderr, "preflight: removing the run directory %s: %v\n", runDir, err)
	}
	return code
}

func closeSessions() {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	for name, s := range sessions {
		if err := s.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "preflight: closing the session to %s: %v\n", name, err)
		}
		delete(sessions, name)
	}
}

func hostOf(m inventory.Machine) sshrun.Host {
	return sshrun.Host{
		Name:    m.Name,
		Addr:    m.Host,
		Port:    m.Port,
		User:    m.User,
		Key:     m.Key,
		HostKey: m.HostKey,
		Arch:    m.Arch,
	}
}

func openSession(t *testing.T, m inventory.Machine) *sshrun.Session {
	t.Helper()
	sessionMu.Lock()
	if s, ok := sessions[m.Name]; ok {
		sessionMu.Unlock()
		return s
	}
	sessionMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	s, err := sshrun.Open(ctx, runDir, hostOf(m))
	if err != nil {
		t.Fatalf("machine %s: ssh could not reach host %s port %d as user %s with key %s from the inventory %s: %v", m.Name, m.Host, m.Port, m.User, m.Key, invPath, err)
	}
	sessionMu.Lock()
	sessions[m.Name] = s
	sessionMu.Unlock()
	return s
}

func mustRun(t *testing.T, s *sshrun.Session, name, what, cmd string) sshrun.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	res, err := s.Run(ctx, cmd)
	if err != nil {
		t.Fatalf("machine %s: %s: %v", name, what, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("machine %s: %s: %q exited %d: %s", name, what, cmd, res.ExitCode, strings.TrimSpace(res.Stderr+res.Stdout))
	}
	return res
}

func TestMachines(t *testing.T) {
	if inv.Count() == 0 {
		t.Fatalf("the inventory %s holds no machine: the end-to-end tier needs at least one", invPath)
	}
	for _, m := range inv.Machines {
		m := m
		t.Run(m.Name, func(t *testing.T) {
			var session *sshrun.Session

			t.Run("host_port_user_key", func(t *testing.T) {
				session = openSession(t, m)
				res := mustRun(t, session, m.Name, "reading the login user", "id -un")
				if got := strings.TrimSpace(res.Stdout); got != m.User {
					t.Fatalf("machine %s: the inventory user is %s but ssh logged in as %s", m.Name, m.User, got)
				}
				t.Logf("machine %s: ssh connected to %s port %d as %s with the inventory key", m.Name, m.Host, m.Port, m.User)
			})
			if session == nil {
				t.Fatalf("machine %s: no ssh session, so the rest of the machine's preflight cannot run", m.Name)
			}

			t.Run("host_key", func(t *testing.T) { checkHostKey(t, session, m) })
			t.Run("sudo", func(t *testing.T) { checkSudo(t, session, m) })
			t.Run("clock", func(t *testing.T) { checkClock(t, session, m) })
			t.Run("arch", func(t *testing.T) { checkArch(t, session, m) })
		})
	}
}

func checkHostKey(t *testing.T, s *sshrun.Session, m inventory.Machine) {
	if m.HostKey == "" {
		t.Skipf("machine %s has no host_key in the inventory %s: ssh accepted the key on first contact instead of pinning it", m.Name, invPath)
	}
	res := mustRun(t, s, m.Name, "reading the public host keys", "cat /etc/ssh/ssh_host_*_key.pub")
	want := strings.Fields(m.HostKey)
	var offered []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		offered = append(offered, fields[0])
		if fields[0] == want[0] && fields[1] == want[1] {
			t.Logf("machine %s: the inventory host_key is the machine's %s key", m.Name, fields[0])
			return
		}
	}
	t.Fatalf("machine %s: the host_key in the inventory %s is a %s key the machine does not have; the machine offers %s", m.Name, invPath, want[0], strings.Join(offered, ", "))
}

func checkSudo(t *testing.T, s *sshrun.Session, m inventory.Machine) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	res, err := s.RunAsRoot(ctx, "id -u")
	if err != nil {
		t.Fatalf("machine %s: user %s from the inventory %s cannot run sudo -n: %v", m.Name, m.User, invPath, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("machine %s: user %s from the inventory %s cannot run sudo -n: exit %d: %s", m.Name, m.User, invPath, res.ExitCode, strings.TrimSpace(res.Stderr+res.Stdout))
	}
	if got := strings.TrimSpace(res.Stdout); got != "0" {
		t.Fatalf("machine %s: sudo -n id -u reported uid %s instead of 0", m.Name, got)
	}
}

func checkClock(t *testing.T, s *sshrun.Session, m inventory.Machine) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	probe, err := s.Run(ctx, hasCommand("timedatectl"))
	if err != nil {
		t.Fatalf("machine %s: looking for timedatectl: %v", m.Name, err)
	}
	if probe.ExitCode != 0 {
		t.Logf("machine %s: timedatectl is not installed, so only the five-second skew is asserted", m.Name)
	} else {
		res := mustRun(t, s, m.Name, "reading the NTP state", "timedatectl show -p NTPSynchronized")
		synced, err := parseNTPSynchronized(res.Stdout)
		if err != nil {
			t.Fatalf("machine %s: %v", m.Name, err)
		}
		if !synced {
			t.Fatalf("machine %s: the clock is not synchronised (timedatectl reports NTPSynchronized=no)", m.Name)
		}
	}

	before := time.Now().UTC()
	res := mustRun(t, s, m.Name, "reading the clock", "date -u +%s")
	after := time.Now().UTC()
	remote, err := remoteEpoch(res.Stdout)
	if err != nil {
		t.Fatalf("machine %s: %v", m.Name, err)
	}
	skew := clockSkew(remote, before.Add(-time.Second), after)
	if skew > ClockTolerance {
		t.Fatalf("machine %s: the clock reads %s, %s away from the clock of the machine running the tests (%s), more than the %s the tier allows", m.Name, remote.Format(time.RFC3339), skew, after.Format(time.RFC3339), ClockTolerance)
	}
	t.Logf("machine %s: the clock is within %s of the clock of the machine running the tests", m.Name, skew)
}

func checkArch(t *testing.T, s *sshrun.Session, m inventory.Machine) {
	if m.Arch == "" {
		t.Fatalf("machine %s has no arch in the inventory %s: the field must name the machine's Go architecture (amd64 or arm64) so a binary can be picked for it", m.Name, invPath)
	}
	res := mustRun(t, s, m.Name, "reading the hardware name", "uname -m")
	reported, err := archFromUname(res.Stdout)
	if err != nil {
		t.Fatalf("machine %s: %v", m.Name, err)
	}
	if reported != m.Arch {
		t.Fatalf("machine %s: the arch in the inventory %s is %s but the machine runs %s", m.Name, invPath, m.Arch, reported)
	}

	local, source, err := machineBinary(m.Arch)
	if err != nil {
		t.Fatalf("machine %s: no caramelo binary for the inventory arch %s: %v", m.Name, m.Arch, err)
	}
	t.Logf("machine %s: shipping the linux/%s binary %s, found through %s", m.Name, m.Arch, local, source)

	shipCtx, cancel := context.WithTimeout(context.Background(), shipTimeout)
	defer cancel()
	if err := s.Copy(shipCtx, local, RemoteBinary); err != nil {
		t.Fatalf("machine %s: shipping %s to %s: %v", m.Name, local, RemoteBinary, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		if _, err := s.Run(ctx, "rm -f "+RemoteBinary); err != nil {
			t.Logf("machine %s: removing %s: %v", m.Name, RemoteBinary, err)
		}
	})
	mustRun(t, s, m.Name, "making the shipped binary executable", "chmod +x "+RemoteBinary)

	out := mustRun(t, s, m.Name, "running the shipped binary", RemoteBinary+" version --json")
	var report versionReport
	if err := json.Unmarshal([]byte(out.Stdout), &report); err != nil {
		t.Fatalf("machine %s: %s version --json printed %q, which is not the JSON version report: %v", m.Name, RemoteBinary, strings.TrimSpace(out.Stdout), err)
	}
	if report.OS != "linux" {
		t.Fatalf("machine %s: the shipped binary reports os %s instead of linux", m.Name, report.OS)
	}
	if report.Arch != m.Arch {
		t.Fatalf("machine %s: the shipped binary reports arch %s, but the arch in the inventory %s is %s", m.Name, report.Arch, invPath, m.Arch)
	}
	t.Logf("machine %s: %s version %s runs on %s/%s", m.Name, RemoteBinary, report.Version, report.OS, report.Arch)
}
