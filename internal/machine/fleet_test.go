package machine

import (
	"context"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/serverconfig"
)

func TestGOARCH(t *testing.T) {
	for in, want := range map[string]string{
		"x86_64":    "amd64",
		"amd64":     "amd64",
		"aarch64":   "arm64",
		"arm64":     "arm64",
		" aarch64 ": "arm64",
		"armv7l":    "arm",
		"i686":      "386",

		"riscv64": "riscv64",
		"":        "",
	} {
		if got := GOARCH(in); got != want {
			t.Errorf("GOARCH(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGaugeRecordsBothSpellingsOfTheArchitecture(t *testing.T) {
	r := &Record{OS: OS{Hardware: "aarch64", Arch: GOARCH("aarch64")}}
	if r.OS.Arch != "arm64" || r.OS.Hardware != "aarch64" {
		t.Fatalf("os = %+v", r.OS)
	}
	if r.Capacity().Arch != "arm64" {
		t.Error("capacity must carry the GOARCH: it is what a release is built for")
	}
}

func TestParseLoadavg(t *testing.T) {
	got := parseLoadavg("0.42 0.35 0.31 3/412 8123\n")
	want := Load{One: 0.42, Five: 0.35, Fifteen: 0.31, Running: 3, Tasks: 412}
	if got != want {
		t.Errorf("parseLoadavg = %+v, want %+v", got, want)
	}
	if s := got.String(); s != "0.42 0.35 0.31" {
		t.Errorf("String = %q", s)
	}

	if l := parseLoadavg("nonsense"); l != (Load{}) {
		t.Errorf("a broken loadavg must leave the field zero, got %+v", l)
	}

	if pc := (Load{One: 4}).PerCPU(4); pc != 1 {
		t.Errorf("PerCPU(4) = %v, want 1", pc)
	}
	if pc := (Load{One: 4}).PerCPU(0); pc != 0 {
		t.Errorf("PerCPU(0) = %v, want 0: no cores is not a division", pc)
	}
}

func TestGaugeReadsTheLoadAverage(t *testing.T) {
	run := labBox(t)
	fakeFiles(t, map[string]string{
		"/proc/meminfo":   "MemTotal:        1505132 kB\nMemAvailable:    1289092 kB\nSwapTotal:             0 kB\n",
		"/etc/os-release": "ID=debian\nVERSION_ID=\"13\"\n",
		"/proc/loadavg":   "1.50 1.20 0.90 2/300 991\n",
	})
	fakeUser(t, "caramelo", 999)
	got, err := GaugeWith(context.Background(), run, serverconfig.Default(), Options{Version: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Load.One != 1.50 || got.Load.Fifteen != 0.90 {
		t.Errorf("load = %+v", got.Load)
	}
}

func TestResampleAndApply(t *testing.T) {
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	fakeClock(t, at)
	fakeFiles(t, map[string]string{
		"/proc/meminfo": "MemTotal:        1505132 kB\nMemAvailable:     512000 kB\nSwapTotal:        102400 kB\n",
		"/proc/loadavg": "0.10 0.20 0.30 1/100 5\n",
	})
	s, err := Resample()
	if err != nil {
		t.Fatal(err)
	}
	if s.Memory.AvailableBytes != 512000*1024 || s.Load.One != 0.10 || !s.At.Equal(at) {
		t.Fatalf("sample = %+v", s)
	}

	r := &Record{
		Hostname: "nx2",
		OS:       OS{ID: "debian", Arch: "arm64"},
		CPU:      CPU{Count: 4},
		Memory:   Memory{TotalBytes: 1505132 * 1024, AvailableBytes: 1289092 * 1024},
		GaugedAt: at.Add(-time.Hour),
	}
	r.Apply(s)
	if r.Memory.AvailableBytes != 512000*1024 {
		t.Errorf("available = %d, want the sample's", r.Memory.AvailableBytes)
	}
	if r.Memory.TotalBytes != 1505132*1024 {
		t.Error("a sample must not rewrite how big the machine is")
	}
	if r.Load.One != 0.10 || !r.GaugedAt.Equal(at) || r.Hostname != "nx2" || r.CPU.Count != 4 {
		t.Errorf("record after Apply = %+v", r)
	}

	(*Record)(nil).Apply(s)
}

func TestResampleNeedsMemoryAndForgivesLoad(t *testing.T) {
	fakeFiles(t, map[string]string{"/proc/loadavg": "0.10 0.20 0.30 1/100 5\n"})
	if _, err := Resample(); err == nil {
		t.Error("a resample with no meminfo must fail")
	}
	fakeFiles(t, map[string]string{"/proc/meminfo": "MemTotal: 1024 kB\n"})
	s, err := Resample()
	if err != nil {
		t.Fatalf("a box with no loadavg must still resample: %v", err)
	}
	if s.Load != (Load{}) {
		t.Errorf("load = %+v, want zero", s.Load)
	}
}

func TestCapacity(t *testing.T) {
	r := &Record{
		OS:       OS{Arch: "arm64"},
		CPU:      CPU{Count: 4},
		Memory:   Memory{TotalBytes: 949 << 20, AvailableBytes: 700 << 20},
		Load:     Load{One: 2},
		Reserved: Reserved{MemoryBytes: 256 << 20, CPU: 0.25},
	}
	c := r.Capacity()
	if c.FreeBytes != (700-256)<<20 {
		t.Errorf("free = %d, want available less the reserve", c.FreeBytes)
	}
	if c.FreeCPU != 3.75 || c.Cores != 4 || c.LoadPerCPU != 0.5 || c.Arch != "arm64" {
		t.Errorf("capacity = %+v", c)
	}
	if s := c.String(); s != "444.0 MiB free of 949.0 MiB, 4 cores, load 2.00" {
		t.Errorf("String = %q", s)
	}

	over := &Record{
		CPU:      CPU{Count: 1},
		Memory:   Memory{TotalBytes: 1 << 30, AvailableBytes: 100 << 20},
		Reserved: Reserved{MemoryBytes: 256 << 20, CPU: 2},
	}
	oc := over.Capacity()
	if oc.FreeBytes != 0 || oc.FreeCPU != 0 {
		t.Errorf("capacity over the reserve = %+v, want zeroes", oc)
	}
	if s := oc.String(); s != "0 B free of 1.0 GiB, 1 core, load 0.00" {
		t.Errorf("String = %q", s)
	}

	if c := (*Record)(nil).Capacity(); c != (Capacity{}) {
		t.Errorf("nil record capacity = %+v", c)
	}
}

func TestCapacityFits(t *testing.T) {
	c := (&Record{
		CPU:      CPU{Count: 4},
		Memory:   Memory{TotalBytes: 949 << 20, AvailableBytes: 700 << 20},
		Reserved: Reserved{MemoryBytes: 256 << 20, CPU: 0.25},
	}).Capacity()

	if ok, why := c.Fits(0, 0); !ok || why != "" {
		t.Errorf("an environment that declared nothing must fit: %v %q", ok, why)
	}
	if ok, why := c.Fits(400<<20, 1); !ok || why != "" {
		t.Errorf("400 MiB in 444 MiB must fit: %v %q", ok, why)
	}
	ok, why := c.Fits(1536<<20, 0)
	if ok || why != "444.0 MiB free, 1.5 GiB asked" {
		t.Errorf("Fits = %v, %q", ok, why)
	}
	if ok, why := c.Fits(0, 8); ok || why != "3.75 CPU free, 8.00 asked" {
		t.Errorf("Fits on cpu = %v, %q", ok, why)
	}

	if ok, why := (Capacity{}).Fits(0, 0); ok || why != "has not said how big it is" {
		t.Errorf("a machine with no gauge = %v, %q", ok, why)
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{
		0:               "0 B",
		512:             "512 B",
		949276672:       "905.3 MiB",
		1 << 30:         "1.0 GiB",
		1536 << 20:      "1.5 GiB",
		-1:              "-",
		5 << 40:         "5.0 TiB",
		1024*1024 - 1:   "1024.0 KiB",
		2*1024*1024 + 1: "2.0 MiB",
	} {
		if got := FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
