//go:build integration

package machine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/test/integration/itest"
)

const markerName = ".caramelo-itest-marker"

const volumeMarker = itest.DockerDataDir + "/" + markerName

func markerOn(m *itest.Machine) string { return m.Home() + "/" + markerName }

func TestBinaryRunsOnTheMachine(t *testing.T) {
	lab := itest.New(t, itest.Options{Suite: "machine"})
	m := lab.Machine(itest.RoleHub)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(5*time.Minute))
	defer cancel()

	arch := m.MustArch(t)
	if m.Target() == itest.TargetDocker {
		dockerArch, err := itest.DockerArch()
		if err != nil {
			t.Fatalf("docker arch: %v", err)
		}
		if arch != dockerArch {
			t.Fatalf("the machine is %s but the docker server is %s", arch, dockerArch)
		}
	}
	bin, err := itest.BinaryFor(ctx, m)
	if err != nil {
		t.Fatalf("build the machine binary: %v", err)
	}
	t.Logf("%s is linux/%s at %s; shipping %s", m.Alias, arch, m.MustAddress(t), bin)

	if err := m.Copy(ctx, bin, itest.RemoteBin); err != nil {
		t.Fatalf("copy the binary: %v", err)
	}
	m.MustRun(t, "chmod +x "+itest.RemoteBin)

	res := m.MustRun(t, itest.RemoteBin+" version --json")
	var got struct {
		Version string `json:"version"`
		Go      string `json:"go"`
		OS      string `json:"os"`
		Arch    string `json:"arch"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &got); err != nil {
		t.Fatalf("version --json is not JSON: %v\nstdout: %q\nstderr: %q", err, res.Stdout, res.Stderr)
	}
	if got.Version == "" {
		t.Errorf("empty version in %q", res.Stdout)
	}
	if got.OS != "linux" || got.Arch != arch {
		t.Errorf("os/arch = %s/%s, want linux/%s", got.OS, got.Arch, arch)
	}
	if got.Go == "" {
		t.Errorf("empty go runtime version in %q", res.Stdout)
	}

	human := m.MustRun(t, itest.RemoteBin+" version")
	if human.Stdout == "" || human.Stdout[0] == '{' {
		t.Errorf("human output looks wrong: %q", human.Stdout)
	}

	bad, err := m.Run(ctx, itest.RemoteBin+" definitely-not-a-command")
	if err != nil {
		t.Fatalf("run a bogus command: %v", err)
	}
	if bad.ExitCode != 2 {
		t.Errorf("unknown command exit = %d, want 2 (stderr %q)", bad.ExitCode, bad.Stderr)
	}
}

func TestMachinesSeeEachOther(t *testing.T) {
	lab := itest.New(t, itest.Options{
		Suite: "machine",
		Roles: []string{itest.RoleHub, itest.RoleNode},
	})
	itest.NeedRoles(t, lab, itest.RoleHub, itest.RoleNode)
	machines := lab.Machines()
	if len(machines) != 2 {
		t.Fatalf("expected two machines, got %d", len(machines))
	}
	for _, from := range machines {
		for _, to := range machines {
			if from.Name == to.Name {
				continue
			}
			addr := to.MustAddress(t)
			res := from.MustRun(t, "ping -c1 -W3 "+addr)
			if res.ExitCode != 0 {
				t.Errorf("%s cannot reach %s at %s: %s", from.Alias, to.Alias, addr, res.Stderr)
			}
			if from.Target() != itest.TargetDocker {
				continue
			}
			byName := from.MustRun(t, "getent hosts "+to.Alias)
			if !strings.Contains(byName.Stdout, addr) {
				t.Errorf("%s resolves %s to %q, want %s", from.Alias, to.Alias, byName.Stdout, addr)
			}
		}
	}
}

func TestCleanMachineMatchesItsSpec(t *testing.T) {
	lab := itest.New(t, itest.Options{Suite: "machine"})
	m := lab.Machine(itest.RoleHub)
	itest.RunGoss(t, m, itest.MustGossSpec(t, "clean.yaml"))
}

func TestResetPutsTheMachineBack(t *testing.T) {
	lab := itest.New(t, itest.Options{Suite: "machine"})
	m := lab.Machine(itest.RoleHub)

	marker := markerOn(m)
	m.MustRun(t, "touch "+marker)
	m.MustRun(t, "test -f "+marker)
	before := m.MustHostPort(t, 22)

	itest.MustReset(t, m, itest.StateClean)

	ctx, cancel := context.WithTimeout(context.Background(), m.Budget().Boot)
	defer cancel()
	if err := m.WaitFor(ctx, "true", time.Second); err != nil {
		t.Fatalf("%s is not reachable after the reset: %v", m.Alias, err)
	}
	res, err := m.Run(ctx, "test -f "+marker)
	if err != nil {
		t.Fatalf("check the marker: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("the marker %s survived the reset of %s", marker, m.Alias)
	}
	if after := m.MustHostPort(t, 22); after == before {
		t.Logf("the reset kept the published ssh port %d", after)
	}
	if err := m.CollectLogs(t, t.TempDir()); err != nil {
		t.Logf("collect logs (non-fatal): %v", err)
	}
}

func TestRestartKeepsTheVolume(t *testing.T) {
	lab := itest.New(t, itest.Options{Suite: "machine"})
	m := lab.Machine(itest.RoleHub)

	marker := markerOn(m)
	m.MustRun(t, "sudo -n mkdir -p "+itest.DockerDataDir+" && sudo -n touch "+volumeMarker)
	m.MustRun(t, "touch "+marker)

	itest.MustRestart(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), m.Budget().Boot)
	defer cancel()
	if err := m.WaitFor(ctx, "true", time.Second); err != nil {
		t.Fatalf("%s is not reachable after the restart: %v", m.Alias, err)
	}
	if res := m.MustRun(t, "systemctl is-system-running"); strings.TrimSpace(res.Stdout) == "" {
		t.Errorf("systemd said nothing after the restart")
	}
	kept, err := m.Run(ctx, "test -f "+volumeMarker)
	if err != nil {
		t.Fatalf("check the volume marker: %v", err)
	}
	if kept.ExitCode != 0 {
		t.Errorf("the volume marker %s did not survive the restart", volumeMarker)
	}
	rootfs, err := m.Run(ctx, "test -f "+marker)
	if err != nil {
		t.Fatalf("check the marker: %v", err)
	}
	if rootfs.ExitCode != 0 {
		t.Errorf("the container filesystem lost %s across a restart", marker)
	}
}

func TestLaptopCanSSHIntoTheMachine(t *testing.T) {
	lab := itest.New(t, itest.Options{
		Suite:   "machine",
		Roles:   []string{itest.RoleHub},
		Laptops: []string{itest.RoleClient},
	})
	m := lab.Machine(itest.RoleHub)
	laptop := lab.Laptop(itest.RoleClient)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
	defer cancel()
	if err := itest.WaitForPort(ctx, m, 22); err != nil {
		t.Fatalf("sshd on %s: %v", m.Alias, err)
	}
	addr := m.MustAddress(t)
	res := laptop.MustRun(t, "ssh -o BatchMode=yes -o ConnectTimeout=10 "+m.User()+"@"+addr+" hostname")
	if got := strings.TrimSpace(res.Stdout); got != m.Hostname {
		t.Errorf("ssh hostname = %q, want %q", got, m.Hostname)
	}
}
