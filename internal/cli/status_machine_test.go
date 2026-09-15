package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/machine"
	"github.com/plytz/caramelo/internal/state"
)

type fakeAPI struct {
	api.Service

	status  *api.Status
	record  *machine.Record
	err     error
	statusN int
	recordN int

	machines    []fleet.Machine
	machinesErr error
}

func (s *fakeAPI) Machines(context.Context) ([]fleet.Machine, error) {
	return s.machines, s.machinesErr
}

func (s *fakeAPI) Status(context.Context) (*api.Status, error) {
	s.statusN++
	return s.status, s.err
}

func (s *fakeAPI) Machine(context.Context) (*machine.Record, error) {
	s.recordN++
	return s.record, s.err
}

func (s *fakeAPI) Keys(context.Context) ([]state.Key, error) { return nil, s.err }

func (s *fakeAPI) AddKey(context.Context, string, string, string) (*state.Key, error) {
	return nil, s.err
}

func (s *fakeAPI) RemoveKey(context.Context, string) error { return s.err }

func runWithService(t *testing.T, svc api.Service, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunWith(context.Background(), args, &stdout, &stderr, Options{
		Service: svc,
		Session: api.Session{Transport: "socket", Machine: "worker1"},
	})
	t.Logf("caramelo %s -> exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, stdout.String(), stderr.String())
	return code, stdout.String(), stderr.String()
}

func statusFixture() *api.Status {
	return &api.Status{
		Version:   "v0.1.0",
		Hostname:  "worker1",
		Uptime:    93*time.Minute + 12*time.Second,
		StartedAt: time.Date(2026, 9, 8, 14, 0, 0, 0, time.UTC),
		Transport: "socket",
		Machine:   "worker1",
		Identity:  "alex@laptop",
		Docker: api.DockerStatus{
			Running: true, Rootless: true, ServerVersion: "29.8.0",
		},
		HostKeyFingerprint: "SHA256:2Z9x0abcdefghijklmnopqrstuvwxyz0123456789ABC",
		SSHPort:            4022,
		Paths: api.Paths{
			Config: "/etc/caramelo", State: "/var/lib/caramelo",
			Data: "/mnt/caramelo", Socket: "/run/caramelo/caramelod.sock",
		},
	}
}

func machineFixture() *machine.Record {
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

func TestStatusHuman(t *testing.T) {
	svc := &fakeAPI{status: statusFixture()}
	code, stdout, stderr := runWithService(t, svc, "status")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if svc.statusN != 1 {
		t.Errorf("service called %d times, want 1", svc.statusN)
	}
	for _, want := range []string{
		"caramelod v0.1.0 on worker1 (via socket)",
		"1h 33m", "running, rootless, 29.8.0", "4022",
		"SHA256:2Z9x0", "alex@laptop",
		"/etc/caramelo", "/var/lib/caramelo", "/mnt/caramelo", "/run/caramelo/caramelod.sock",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestStatusJSON(t *testing.T) {
	svc := &fakeAPI{status: statusFixture()}
	code, stdout, stderr := runWithService(t, svc, "status", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	var got api.Status
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if got.Version != "v0.1.0" || got.Transport != "socket" || got.SSHPort != 4022 {
		t.Errorf("got %+v", got)
	}
	if !got.Docker.Running || !got.Docker.Rootless {
		t.Errorf("docker = %+v", got.Docker)
	}
	if got.Paths.Socket != "/run/caramelo/caramelod.sock" {
		t.Errorf("paths = %+v", got.Paths)
	}
	if strings.Count(stdout, "\n") != 1 {
		t.Errorf("want exactly one JSON line, got:\n%s", stdout)
	}
}

func TestStatusDockerDown(t *testing.T) {
	st := statusFixture()
	st.Docker = api.DockerStatus{Running: false, Error: "dial unix /run/user/999/docker.sock: connect: no such file"}
	_, stdout, _ := runWithService(t, &fakeAPI{status: st}, "status")
	if !strings.Contains(stdout, "not running") || !strings.Contains(stdout, "no such file") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestStatusServiceError(t *testing.T) {
	code, stdout, stderr := runWithService(t, &fakeAPI{err: errors.New("daemon is shutting down")}, "status")
	if code != ExitError {
		t.Fatalf("exit = %d, want %d", code, ExitError)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "daemon is shutting down") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestStatusTakesNoArgs(t *testing.T) {
	code, _, _ := runWithService(t, &fakeAPI{status: statusFixture()}, "status", "extra")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
}

func TestMachineShowHuman(t *testing.T) {
	svc := &fakeAPI{record: machineFixture()}
	code, stdout, stderr := runWithService(t, svc, "machine", "show")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if svc.recordN != 1 {
		t.Errorf("service called %d times, want 1", svc.recordN)
	}
	for _, want := range []string{
		"worker1", "1a1025b42a75", "2026-09-08T16:32:00Z",
		"debian 13 (trixie)", "kernel 6.12.86+deb13-amd64", "x86_64", "kvm",
		"1 vCPU", "AMD Opteron 63xx class CPU",
		"1.4 GiB total", "1.1 GiB available", "no swap",
		"/mnt/caramelo", "91.4 GiB free of 97.9 GiB", "(ext4 on /dev/vda1)",
		"vda 100.0 GiB hdd",
		"192.168.121.135 on eth0", "also 192.168.56.12",
		"v2", "cpuset cpu io memory pids",
		"29.8.0 rootless, overlayfs, slirp4netns/builtin",
		"256.0 MiB, 0.25 CPU",
		"user caramelo(999)", "ssh port 4022",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestMachineShowJSONIsIndentedAndComplete(t *testing.T) {
	want := machineFixture()
	code, stdout, stderr := runWithService(t, &fakeAPI{record: want}, "machine", "show", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, "\n  \"machine_id\"") {
		t.Errorf("output is not indented:\n%s", stdout)
	}
	var got machine.Record
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if got.MachineID != want.MachineID || got.Docker.NetDriver != "slirp4netns" || got.Caramelo.UID != 999 {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if !got.GaugedAt.Equal(want.GaugedAt) {
		t.Errorf("gauged_at = %v, want %v", got.GaugedAt, want.GaugedAt)
	}
}

func TestMachineShowBareBox(t *testing.T) {

	rec := &machine.Record{
		Hostname: "fresh",
		OS:       machine.OS{ID: "ubuntu", VersionID: "24.04", Codename: "noble", Arch: "aarch64", Virt: "none"},
		CPU:      machine.CPU{Count: 4},
		Memory:   machine.Memory{TotalBytes: 4 << 30, AvailableBytes: 3 << 30, SwapTotalBytes: 2 << 30},
		DataDir:  machine.Mount{Path: "/mnt/caramelo"},
	}
	code, stdout, _ := runWithService(t, &fakeAPI{record: rec}, "machine", "show")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"ubuntu 24.04 (noble)", "4 vCPU", "2.0 GiB swap", "not installed"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "none") {
		t.Errorf("virt \"none\" should not be printed:\n%s", stdout)
	}
	if strings.Contains(stdout, "disks") {
		t.Errorf("no disks were gauged, the row should be omitted:\n%s", stdout)
	}
}

func TestMachineShowWarnings(t *testing.T) {
	rec := machineFixture()
	rec.Docker.Warnings = []string{"WARNING: No swap limit support"}
	_, stdout, _ := runWithService(t, &fakeAPI{record: rec}, "machine", "show")
	if !strings.Contains(stdout, "! WARNING: No swap limit support") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestMachineShowServiceError(t *testing.T) {
	code, _, stderr := runWithService(t, &fakeAPI{err: errors.New("not gauged yet")}, "machine", "show")
	if code != ExitError {
		t.Fatalf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "not gauged yet") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestMachineGroupIsRegistered(t *testing.T) {
	code, stdout, _ := run(t)
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"machine", "status"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("root help does not list %q:\n%s", want, stdout)
		}
	}
}

func TestCommanderCommandsAreForwardedWhenThereIsNoService(t *testing.T) {

	for _, args := range [][]string{{"status"}, {"machine", "show"}} {
		code, stdout, _ := run(t, args...)
		if code == ExitOK {
			t.Errorf("%v: want a non-zero exit without a daemon", args)
		}
		if stdout != "" {
			t.Errorf("%v: stdout = %q, want empty", args, stdout)
		}
	}
}

func TestFmtBytesIEC(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"}, {512, "512 B"}, {1024, "1.0 KiB"}, {1536, "1.5 KiB"},
		{1541255168, "1.4 GiB"}, {107374182400, "100.0 GiB"},
		{1 << 50, "1.0 PiB"}, {-1, "-"},
	} {
		if got := fmtBytesIEC(tc.in); got != tc.want {
			t.Errorf("fmtBytesIEC(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFmtUptime(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"}, {-time.Second, "0s"}, {45 * time.Second, "45s"},
		{90 * time.Second, "1m 30s"}, {93*time.Minute + 12*time.Second, "1h 33m"},
		{50 * time.Hour, "2d 2h 0m"},
	} {
		if got := fmtUptime(tc.in); got != tc.want {
			t.Errorf("fmtUptime(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWriteStatusAndMachineTolerateNil(t *testing.T) {
	var b bytes.Buffer
	if err := writeStatus(&b, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeMachine(&b, nil); err != nil {
		t.Fatal(err)
	}
	if b.Len() == 0 {
		t.Error("want some output")
	}
}

func TestStatusSaysWhoIsFollowingTheFeed(t *testing.T) {
	quiet := statusFixture()
	quiet.Production = &api.ProductionStatus{Envs: 1, Vault: true}
	if out := statusView(quiet, nil, time.Now()).String(); strings.Contains(out, "feed") {
		t.Errorf("a machine nobody is watching printed a feed line:\n%s", out)
	}

	watched := statusFixture()
	watched.Production = &api.ProductionStatus{Envs: 1, Vault: true, Watching: 2}
	if out := statusView(watched, nil, time.Now()).String(); !strings.Contains(out, "2 watching") {
		t.Errorf("status does not say who is watching:\n%s", out)
	}

	behind := statusFixture()
	behind.Production = &api.ProductionStatus{Envs: 1, Vault: true, Watching: 1, FeedDropped: 7}
	out := statusView(behind, nil, time.Now()).String()
	for _, want := range []string{"1 watching", "7 event(s) dropped"} {
		if !strings.Contains(out, want) {
			t.Errorf("status does not say %q:\n%s", want, out)
		}
	}
}
