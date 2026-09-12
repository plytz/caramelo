package machine

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func golden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return string(b)
}

func TestParseMeminfo(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Memory
		wantErr bool
	}{
		{
			name: "worker2",
			in:   golden(t, "meminfo.txt"),
			want: Memory{TotalBytes: 1505132 * 1024, AvailableBytes: 1289092 * 1024, SwapTotalBytes: 0},
		},
		{
			name: "with swap",
			in:   "MemTotal:       16333184 kB\nMemAvailable:    9000000 kB\nSwapTotal:       2097148 kB\n",
			want: Memory{TotalBytes: 16333184 * 1024, AvailableBytes: 9000000 * 1024, SwapTotalBytes: 2097148 * 1024},
		},
		{
			name: "no unit is bytes",
			in:   "MemTotal:       1024\nMemAvailable:   512\nSwapTotal: 0\n",
			want: Memory{TotalBytes: 1024, AvailableBytes: 512},
		},
		{name: "empty", in: "", wantErr: true},
		{name: "no MemTotal", in: "MemFree: 100 kB\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMeminfo(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseOSRelease(t *testing.T) {
	got := parseOSRelease(golden(t, "os-release.txt"))
	for k, want := range map[string]string{
		"ID": "debian", "VERSION_ID": "13", "VERSION_CODENAME": "trixie",
		"PRETTY_NAME": "Debian GNU/Linux 13 (trixie)",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}

	ubuntu := parseOSRelease(`# comment line
NAME="Ubuntu"
VERSION_ID="24.04"
ID=ubuntu
UBUNTU_CODENAME=noble
VERSION_CODENAME=noble
junk-without-equals
`)
	if ubuntu["ID"] != "ubuntu" || ubuntu["VERSION_ID"] != "24.04" || ubuntu["VERSION_CODENAME"] != "noble" {
		t.Errorf("ubuntu: %+v", ubuntu)
	}
	if _, ok := ubuntu["junk-without-equals"]; ok {
		t.Error("line without = should be ignored")
	}
}

func TestParseFindmntT(t *testing.T) {
	source, fstype, target, err := parseFindmntT(golden(t, "findmnt-t.json"))
	if err != nil {
		t.Fatal(err)
	}
	if source != "/dev/vda1" || fstype != "ext4" || target != "/" {
		t.Errorf("got %q %q %q", source, fstype, target)
	}

	if _, _, _, err := parseFindmntT(""); err == nil {
		t.Error("empty output must be an error")
	}
	if _, _, _, err := parseFindmntT(`{"filesystems":[]}`); err == nil {
		t.Error("no filesystems must be an error")
	}
}

func TestParseDF(t *testing.T) {
	got, err := parseDF(golden(t, "df.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := Mount{Source: "/dev/vda1", FSType: "ext4", SizeBytes: 105088212992, AvailBytes: 98999529472, Path: "/"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}

	t.Run("source wrapped onto its own line", func(t *testing.T) {
		in := "Filesystem     Type  1B-blocks      Used       Avail Mounted on\n" +
			"/dev/mapper/a-very-long-volume-group-name-here\n" +
			"                ext4 105088212992 703250432 98999529472 /mnt/caramelo\n"
		got, err := parseDF(in)
		if err != nil {
			t.Fatal(err)
		}
		if got.Source != "/dev/mapper/a-very-long-volume-group-name-here" || got.Path != "/mnt/caramelo" {
			t.Errorf("got %+v", got)
		}
	})

	for _, in := range []string{"", "Filesystem Type 1B-blocks Used Avail Mounted on\n", "h\na b c d e f\n"} {
		if _, err := parseDF(in); err == nil {
			t.Errorf("parseDF(%q) must fail", in)
		}
	}
}

func TestParseLsblk(t *testing.T) {
	got, err := parseLsblk(golden(t, "lsblk.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Disk{{Name: "vda", SizeBytes: 107374182400, Rotational: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	t.Run("string sizes and rota, non-disks dropped", func(t *testing.T) {

		in := `{"blockdevices":[
		  {"name":"loop0","type":"loop","size":"12288","rota":"0","model":null},
		  {"name":"nbd0","type":"disk","size":0,"rota":false,"model":null},
		  {"name":"nvme0n1","type":"disk","size":"512110190592","rota":"0","model":"Samsung SSD 980 PRO 512GB "},
		  {"name":"nvme0n1p1","type":"part","size":"536870912","rota":"0","model":null}]}`
		got, err := parseLsblk(in)
		if err != nil {
			t.Fatal(err)
		}
		want := []Disk{{Name: "nvme0n1", SizeBytes: 512110190592, Rotational: false, Model: "Samsung SSD 980 PRO 512GB"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	if _, err := parseLsblk("not json"); err == nil {
		t.Error("invalid JSON must be an error")
	}
}

func TestParseCgroupVersion(t *testing.T) {
	for in, want := range map[string]int{
		golden(t, "cgroupfs.txt"): 2,
		"cgroup2fs\n":             2,
		"tmpfs\n":                 1,
		"cgroupfs":                1,
		"":                        0,
		"ext4":                    0,
	} {
		if got := parseCgroupVersion(in); got != want {
			t.Errorf("parseCgroupVersion(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseControllers(t *testing.T) {
	if got, want := parseControllers(golden(t, "cgroup-controllers.txt")), []string{"cpu", "memory", "pids"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got, want := parseControllers("cpuset cpu io memory pids\n"), []string{"cpuset", "cpu", "io", "memory", "pids"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := parseControllers("\n"); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestParseRouteGet(t *testing.T) {
	iface, ip, err := parseRouteGet(golden(t, "ip-route-get.json"))
	if err != nil {
		t.Fatal(err)
	}
	if iface != "eth0" || ip != "192.168.121.135" {
		t.Errorf("got %q %q", iface, ip)
	}
	if _, _, err := parseRouteGet("[]"); err == nil {
		t.Error("no route must be an error")
	}
	if _, _, err := parseRouteGet(""); err == nil {
		t.Error("empty must be an error")
	}
}

func TestParseAddrs(t *testing.T) {
	got, err := parseAddrs(golden(t, "ip-addr.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"192.168.121.135", "192.168.56.12"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	t.Run("host and link scopes dropped", func(t *testing.T) {
		in := `[{"ifname":"lo","flags":["LOOPBACK","UP"],"addr_info":[{"family":"inet","local":"127.0.0.1","scope":"host"}]},
		        {"ifname":"eth0","flags":["UP"],"addr_info":[{"family":"inet","local":"10.0.0.5","scope":"global"},{"family":"inet","local":"169.254.1.1","scope":"link"}]},
		        {"ifname":"eth1","flags":["UP"],"addr_info":[{"family":"inet","local":"10.0.0.5","scope":"global"}]}]`
		got, err := parseAddrs(in)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"10.0.0.5"}; !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	if _, err := parseAddrs("{}"); err == nil {
		t.Error("object instead of array must be an error")
	}
}

func TestParseCPUModel(t *testing.T) {
	if got, want := parseCPUModel(golden(t, "cpuinfo.txt")), "AMD Opteron 63xx class CPU"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	arm := "processor\t: 0\nBogoMIPS\t: 50.00\nCPU implementer\t: 0x41\n\nModel\t\t: Raspberry Pi 4 Model B Rev 1.4\n"
	if got, want := parseCPUModel(arm), "0x41"; got != want {
		t.Errorf("arm fallback = %q, want %q", got, want)
	}
	if got := parseCPUModel(""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestParseNProc(t *testing.T) {
	for in, want := range map[string]int{"1\n": 1, "  4  \n": 4, "16": 16} {
		got, err := parseNProc(in)
		if err != nil || got != want {
			t.Errorf("parseNProc(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "many", "0", "-2"} {
		if _, err := parseNProc(in); err == nil {
			t.Errorf("parseNProc(%q) must fail", in)
		}
	}
}

func TestParseDockerInfo(t *testing.T) {
	t.Run("rootless", func(t *testing.T) {
		got, err := parseDockerInfo(golden(t, "docker-info-rootless.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !got.Installed || !got.Rootless {
			t.Errorf("installed=%v rootless=%v", got.Installed, got.Rootless)
		}
		if got.ServerVersion != "29.8.0" || got.StorageDriver != "overlayfs" || got.DataRoot != "/mnt/caramelo/docker" {
			t.Errorf("got %+v", got)
		}
		if len(got.Warnings) != 2 {
			t.Errorf("warnings = %v", got.Warnings)
		}
	})

	t.Run("rootful", func(t *testing.T) {
		got, err := parseDockerInfo(golden(t, "docker-info-rootful.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !got.Installed {
			t.Error("installed must be true")
		}
		if got.Rootless {
			t.Error("a rootful daemon must not be reported as rootless")
		}
		if got.ServerVersion != "29.8.0" || got.StorageDriver != "overlayfs" || got.DataRoot != "/var/lib/docker" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("daemon down", func(t *testing.T) {
		got, err := parseDockerInfo(`{"ServerVersion":"","ServerErrors":["Cannot connect to the Docker daemon at unix:///run/user/999/docker.sock. Is the docker daemon running?"]}`)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Installed || got.ServerVersion != "" || len(got.Warnings) != 1 {
			t.Errorf("got %+v", got)
		}
	})

	if _, err := parseDockerInfo("Cannot connect to the Docker daemon"); err == nil {
		t.Error("non-JSON must be an error")
	}
}

func TestParseDockerVersionDrivers(t *testing.T) {
	net, port := parseDockerVersionDrivers(golden(t, "docker-version-rootless.txt"))
	if net != "slirp4netns" || port != "builtin" {
		t.Errorf("got %q %q, want slirp4netns builtin", net, port)
	}

	net, port = parseDockerVersionDrivers(golden(t, "docker-version-rootful.txt"))
	if net != "" || port != "" {
		t.Errorf("rootful docker has no rootlesskit drivers, got %q %q", net, port)
	}

	net, port = parseDockerVersionDrivers("")
	if net != "" || port != "" {
		t.Errorf("got %q %q, want empty", net, port)
	}
}
