package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
)

type fakeRunner struct {
	out    map[string]runner.Result
	prefix map[string]runner.Result
	calls  []string
	users  []string
}

func (f *fakeRunner) Run(_ context.Context, c runner.Cmd) (runner.Result, error) {
	key := strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " "))
	f.calls = append(f.calls, key)
	f.users = append(f.users, c.User)
	if r, ok := f.out[key]; ok {
		return r, nil
	}
	for p, r := range f.prefix {
		if strings.HasPrefix(key, p) {
			return r, nil
		}
	}
	return runner.Result{}, fmt.Errorf("exec: %q: executable file not found in $PATH", c.Name)
}

func ok(stdout string) runner.Result { return runner.Result{Stdout: stdout} }

func labBox(t *testing.T) *fakeRunner {
	t.Helper()
	return &fakeRunner{out: map[string]runner.Result{
		"nproc":                       ok("1\n"),
		"uname -r":                    ok("6.12.86+deb13-amd64\n"),
		"uname -m":                    ok("x86_64\n"),
		"hostname":                    ok("worker1\n"),
		"systemd-detect-virt":         ok("kvm\n"),
		"stat -fc %T /sys/fs/cgroup":  ok(golden(t, "cgroupfs.txt")),
		"findmnt -J -T /mnt/caramelo": ok(golden(t, "findmnt-t.json")),
		"findmnt -M /mnt/caramelo":    {ExitCode: 1},
		"df -B1 --output=source,fstype,size,used,avail,target /mnt/caramelo": ok(golden(t, "df.txt")),
		"lsblk -J -b -o NAME,TYPE,SIZE,MOUNTPOINTS,FSTYPE,ROTA,MODEL":        ok(golden(t, "lsblk.json")),
		"ip -j route get 1.1.1.1":   ok(golden(t, "ip-route-get.json")),
		"ip -j -4 addr":             ok(golden(t, "ip-addr.json")),
		"docker info --format json": ok(golden(t, "docker-info-rootless.json")),
		"docker version":            ok(golden(t, "docker-version-rootless.txt")),
	}}
}

func fakeFiles(t *testing.T, files map[string]string) {
	t.Helper()
	prev := readFile
	readFile = func(name string) ([]byte, error) {
		if s, ok := files[name]; ok {
			return []byte(s), nil
		}
		return nil, fmt.Errorf("open %s: %w", name, os.ErrNotExist)
	}
	t.Cleanup(func() { readFile = prev })
}

func labFiles(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"/proc/meminfo":   golden(t, "meminfo.txt"),
		"/etc/os-release": golden(t, "os-release.txt"),
		"/proc/cpuinfo":   golden(t, "cpuinfo.txt"),
		"/etc/machine-id": golden(t, "machine-id.txt"),
		"/sys/fs/cgroup/user.slice/user-999.slice/user@999.service/cgroup.controllers": "cpuset cpu io memory pids\n",
	}
}

func fakeUser(t *testing.T, name string, uid int) {
	t.Helper()
	prev := lookupUser
	lookupUser = func(n string) (*user.User, error) {
		if n != name {
			return nil, user.UnknownUserError(n)
		}
		return &user.User{Username: n, Uid: fmt.Sprint(uid), Gid: fmt.Sprint(uid), HomeDir: "/var/lib/" + n}, nil
	}
	t.Cleanup(func() { lookupUser = prev })
}

func fakeRoot(t *testing.T) {
	t.Helper()
	prev := geteuid
	geteuid = func() int { return 0 }
	t.Cleanup(func() { geteuid = prev })
}

func fakeClock(t *testing.T, at time.Time) {
	t.Helper()
	prev := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = prev })
}

func TestGaugeFullBox(t *testing.T) {
	run := labBox(t)
	fakeFiles(t, labFiles(t))
	fakeUser(t, "caramelo", 999)
	fakeRoot(t)
	at := time.Date(2026, 9, 8, 16, 32, 0, 0, time.UTC)
	fakeClock(t, at)

	cfg := serverconfig.Default()
	got, err := Gauge(context.Background(), run, cfg, "v1.2.3")
	if err != nil {
		t.Fatalf("Gauge: %v", err)
	}

	want := &Record{
		MachineID: "1a1025b42a7543de8c004982abfc9024",
		Hostname:  "worker1",
		GaugedAt:  at,
		OS: OS{ID: "debian", VersionID: "13", Codename: "trixie",
			Kernel: "6.12.86+deb13-amd64", Arch: "amd64", Hardware: "x86_64", Virt: "kvm"},
		CPU:    CPU{Count: 1, Model: "AMD Opteron 63xx class CPU"},
		Memory: Memory{TotalBytes: 1505132 * 1024, AvailableBytes: 1289092 * 1024},
		Dirs:   Dirs{Config: "/etc/caramelo", State: "/var/lib/caramelo", Data: "/mnt/caramelo"},
		DataDir: Mount{Path: "/mnt/caramelo", OwnMountPoint: false, Source: "/dev/vda1", FSType: "ext4",
			SizeBytes: 105088212992, AvailBytes: 98999529472},
		Disks:   []Disk{{Name: "vda", SizeBytes: 107374182400, Rotational: true}},
		Network: Network{PrimaryIface: "eth0", PrimaryIP: "192.168.121.135", Addresses: []string{"192.168.121.135", "192.168.56.12"}},
		Cgroup:  Cgroup{Version: 2, Controllers: []string{"cpuset", "cpu", "io", "memory", "pids"}},
		Docker: Docker{Installed: true, ServerVersion: "29.8.0", Rootless: true,
			StorageDriver: "overlayfs", NetDriver: "slirp4netns", PortDriver: "builtin",
			DataRoot: "/mnt/caramelo/docker",
			Warnings: []string{"WARNING: No swap limit support", "WARNING: daemon is not using the default seccomp profile"}},
		Reserved: Reserved{MemoryBytes: 256 << 20, CPU: 0.25},
		Caramelo: Caramelo{Version: "v1.2.3", User: "caramelo", UID: 999, SSHPort: 4022},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("record mismatch\n got: %+v\nwant: %+v", got, want)
	}
	if !strings.Contains(strings.Join(run.users, ","), "caramelo") {
		t.Error("docker must be run as the caramelo user when we are root")
	}
}

func TestGaugeBareBox(t *testing.T) {

	run := &fakeRunner{out: map[string]runner.Result{
		"nproc":                       ok("4\n"),
		"uname -r":                    ok("6.8.0-51-generic\n"),
		"uname -m":                    ok("aarch64\n"),
		"hostname":                    ok("box\n"),
		"stat -fc %T /sys/fs/cgroup":  ok("cgroup2fs\n"),
		"findmnt -J -T /mnt/caramelo": {ExitCode: 1},
		"findmnt -M /mnt/caramelo":    {ExitCode: 1},
		"df -B1 --output=source,fstype,size,used,avail,target /mnt/caramelo": {
			ExitCode: 1, Stderr: "df: /mnt/caramelo: No such file or directory\n"},
		"ip -j route get 1.1.1.1": ok(`[{"dst":"1.1.1.1","dev":"enp1s0","prefsrc":"10.0.0.7"}]`),
		"ip -j -4 addr":           ok(`[{"ifname":"enp1s0","flags":["UP"],"addr_info":[{"family":"inet","local":"10.0.0.7","scope":"global"}]}]`),
	}}
	fakeFiles(t, map[string]string{
		"/proc/meminfo":   "MemTotal:        1505132 kB\nMemAvailable:    1289092 kB\nSwapTotal:             0 kB\n",
		"/etc/os-release": "ID=ubuntu\nVERSION_ID=\"24.04\"\nVERSION_CODENAME=noble\n",
	})
	fakeUser(t, "nobody-else", 0)

	got, err := GaugeWith(context.Background(), run, serverconfig.Default(), Options{Version: "dev"})
	if err != nil {
		t.Fatalf("a bare box must still gauge: %v", err)
	}
	if got.CPU.Count != 4 || got.OS.ID != "ubuntu" || got.OS.Codename != "noble" {
		t.Errorf("got %+v", got)
	}
	if got.Docker.Installed {
		t.Error("docker must be reported as not installed")
	}
	if got.Disks != nil || got.OS.Virt != "" || got.MachineID != "" || got.CPU.Model != "" {
		t.Errorf("missing tools must leave fields empty: %+v", got)
	}
	if got.Caramelo.UID != 0 || got.Cgroup.Controllers != nil {
		t.Errorf("missing user must leave uid and controllers empty: %+v", got.Caramelo)
	}
	if got.DataDir.Path != "/mnt/caramelo" || got.DataDir.SizeBytes != 0 || got.DataDir.OwnMountPoint {
		t.Errorf("missing data dir: %+v", got.DataDir)
	}
	if got.Cgroup.Version != 2 {
		t.Errorf("cgroup version = %d", got.Cgroup.Version)
	}
}

func TestGaugeOwnMountPoint(t *testing.T) {
	run := labBox(t)
	run.out["findmnt -M /mnt/caramelo"] = ok("TARGET        SOURCE    FSTYPE OPTIONS\n/mnt/caramelo /dev/vdb1 ext4   rw,relatime\n")
	fakeFiles(t, labFiles(t))
	fakeUser(t, "caramelo", 999)

	got, err := Gauge(context.Background(), run, serverconfig.Default(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !got.DataDir.OwnMountPoint {
		t.Error("a data volume mounted at the data dir must be its own mount point")
	}
}

func TestGaugeRequiredFacts(t *testing.T) {
	files := func(t *testing.T, drop string) {
		f := labFiles(t)
		delete(f, drop)
		fakeFiles(t, f)
	}
	t.Run("no nproc", func(t *testing.T) {
		run := labBox(t)
		delete(run.out, "nproc")
		fakeFiles(t, labFiles(t))
		if _, err := Gauge(context.Background(), run, serverconfig.Default(), "dev"); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("no meminfo", func(t *testing.T) {
		files(t, "/proc/meminfo")
		if _, err := Gauge(context.Background(), labBox(t), serverconfig.Default(), "dev"); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("no os-release", func(t *testing.T) {
		files(t, "/etc/os-release")
		if _, err := Gauge(context.Background(), labBox(t), serverconfig.Default(), "dev"); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("no runner", func(t *testing.T) {
		if _, err := Gauge(context.Background(), nil, serverconfig.Default(), "dev"); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestGaugeConfigDir(t *testing.T) {
	run := labBox(t)
	fakeFiles(t, labFiles(t))
	fakeUser(t, "caramelo", 999)
	got, err := GaugeWith(context.Background(), run, serverconfig.Default(), Options{ConfigDir: "/opt/caramelo/etc"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Dirs.Config != "/opt/caramelo/etc" {
		t.Errorf("config dir = %q", got.Dirs.Config)
	}
}

func TestDockerUser(t *testing.T) {
	t.Run("root runs docker as the app user", func(t *testing.T) {
		fakeRoot(t)
		if got := dockerUser("caramelo"); got != "caramelo" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("another user runs its own docker", func(t *testing.T) {
		prevU, prevE := currentUser, geteuid
		currentUser = func() (*user.User, error) { return &user.User{Username: "dev"}, nil }
		geteuid = func() int { return 1000 }
		t.Cleanup(func() { currentUser, geteuid = prevU, prevE })
		if got := dockerUser("caramelo"); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("the app user itself gets its session env", func(t *testing.T) {
		prevU, prevE := currentUser, geteuid
		currentUser = func() (*user.User, error) { return &user.User{Username: "caramelo"}, nil }
		geteuid = func() int { return 999 }
		t.Cleanup(func() { currentUser, geteuid = prevU, prevE })
		if got := dockerUser("caramelo"); got != "caramelo" {
			t.Errorf("got %q", got)
		}
	})
	if got := dockerUser(""); got != "" {
		t.Errorf("no user configured: got %q", got)
	}
}

func TestGaugeDockerDaemonDown(t *testing.T) {
	run := labBox(t)
	run.out["docker info --format json"] = runner.Result{
		ExitCode: 1,
		Stderr:   "Cannot connect to the Docker daemon at unix:///run/user/999/docker.sock. Is the docker daemon running?\n",
	}
	fakeFiles(t, labFiles(t))
	fakeUser(t, "caramelo", 999)

	got, err := Gauge(context.Background(), run, serverconfig.Default(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Docker.Installed {
		t.Error("docker(1) ran, so it is installed")
	}
	if got.Docker.ServerVersion != "" || len(got.Docker.Warnings) != 1 {
		t.Errorf("got %+v", got.Docker)
	}
}

func TestGaugeAgainstThisHost(t *testing.T) {
	if _, err := exec.LookPath("nproc"); err != nil {
		t.Skip("no nproc on this host")
	}
	if _, err := os.Stat("/proc/meminfo"); err != nil {
		t.Skip("not a Linux host")
	}
	cfg := serverconfig.Default()
	cfg.DataDir = t.TempDir()
	got, err := GaugeWith(context.Background(), runner.Exec{}, cfg, Options{Version: "test"})
	if err != nil {
		t.Fatalf("Gauge: %v", err)
	}

	if b, err := json.MarshalIndent(got, "", "  "); err == nil {
		t.Logf("record of this host:\n%s", b)
	}
	if got.CPU.Count < 1 || got.Memory.TotalBytes <= 0 {
		t.Errorf("cpu %d memory %d", got.CPU.Count, got.Memory.TotalBytes)
	}
	if got.OS.ID == "" || got.OS.Kernel == "" || got.OS.Arch == "" {
		t.Errorf("os = %+v", got.OS)
	}
	if got.Hostname == "" {
		t.Error("no hostname")
	}
	if got.DataDir.Path == "" || got.DataDir.SizeBytes <= 0 {
		t.Errorf("data dir = %+v", got.DataDir)
	}
	if got.Caramelo.Version != "test" || got.Caramelo.SSHPort != serverconfig.DefaultSSHPort {
		t.Errorf("caramelo = %+v", got.Caramelo)
	}
}
