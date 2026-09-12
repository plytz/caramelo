package machine

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

func GOARCH(unameM string) string {
	switch s := strings.TrimSpace(unameM); s {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l", "armv6l", "armv8l":
		return "arm"
	case "i386", "i486", "i586", "i686":
		return "386"
	default:
		return s
	}
}

type Load struct {
	One     float64 `json:"one"`
	Five    float64 `json:"five"`
	Fifteen float64 `json:"fifteen"`

	Running int `json:"running,omitempty"`
	Tasks   int `json:"tasks,omitempty"`
}

func (l Load) PerCPU(cores int) float64 {
	if cores <= 0 {
		return 0
	}
	return l.One / float64(cores)
}

func (l Load) String() string {
	return fmt.Sprintf("%.2f %.2f %.2f", l.One, l.Five, l.Fifteen)
}

func parseLoadavg(s string) Load {
	var l Load
	fields := strings.Fields(s)
	for i, f := range fields {
		if i > 2 {
			break
		}
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			return l
		}
		switch i {
		case 0:
			l.One = v
		case 1:
			l.Five = v
		case 2:
			l.Fifteen = v
		}
	}
	if len(fields) > 3 {
		if running, tasks, ok := strings.Cut(fields[3], "/"); ok {
			if n, err := strconv.Atoi(running); err == nil {
				l.Running = n
			}
			if n, err := strconv.Atoi(tasks); err == nil {
				l.Tasks = n
			}
		}
	}
	return l
}

type Sample struct {
	Memory Memory    `json:"memory"`
	Load   Load      `json:"load"`
	At     time.Time `json:"at"`
}

func Resample() (Sample, error) {
	b, err := readFile("/proc/meminfo")
	if err != nil {
		return Sample{}, fmt.Errorf("resample memory: %w", err)
	}
	mem, err := parseMeminfo(string(b))
	if err != nil {
		return Sample{}, fmt.Errorf("resample memory: %w", err)
	}
	s := Sample{Memory: mem, At: now().UTC()}
	if b, err := readFile("/proc/loadavg"); err == nil {
		s.Load = parseLoadavg(string(b))
	}
	return s, nil
}

func (r *Record) Apply(s Sample) {
	if r == nil {
		return
	}
	if s.Memory.AvailableBytes > 0 {
		r.Memory.AvailableBytes = s.Memory.AvailableBytes
	}
	if s.Memory.SwapTotalBytes > 0 {
		r.Memory.SwapTotalBytes = s.Memory.SwapTotalBytes
	}
	r.Load = s.Load
	if !s.At.IsZero() {
		r.GaugedAt = s.At.UTC()
	}
}

type Capacity struct {
	FreeBytes int64 `json:"free_bytes"`

	TotalBytes int64 `json:"total_bytes"`

	Cores   int     `json:"cores"`
	FreeCPU float64 `json:"free_cpu"`

	Load       Load    `json:"load"`
	LoadPerCPU float64 `json:"load_per_cpu"`

	Arch string `json:"arch,omitempty"`

	At time.Time `json:"at,omitempty"`
}

func (r *Record) Capacity() Capacity {
	if r == nil {
		return Capacity{}
	}
	c := Capacity{
		TotalBytes: r.Memory.TotalBytes,
		Cores:      r.CPU.Count,
		Load:       r.Load,
		LoadPerCPU: r.Load.PerCPU(r.CPU.Count),
		Arch:       r.OS.Arch,
		At:         r.GaugedAt,
	}
	if c.FreeBytes = r.Memory.AvailableBytes - r.Reserved.MemoryBytes; c.FreeBytes < 0 {
		c.FreeBytes = 0
	}
	if c.FreeCPU = float64(r.CPU.Count) - r.Reserved.CPU; c.FreeCPU < 0 {
		c.FreeCPU = 0
	}
	return c
}

func (c Capacity) Fits(memoryBytes int64, cpu float64) (bool, string) {
	if c.TotalBytes == 0 {
		return false, "has not said how big it is"
	}
	if memoryBytes > c.FreeBytes {
		return false, fmt.Sprintf("%s free, %s asked", FormatBytes(c.FreeBytes), FormatBytes(memoryBytes))
	}
	if cpu > 0 && c.FreeCPU > 0 && cpu > c.FreeCPU {
		return false, fmt.Sprintf("%.2f CPU free, %.2f asked", c.FreeCPU, cpu)
	}
	return true, ""
}

func (c Capacity) String() string {
	s := fmt.Sprintf("%s free of %s", FormatBytes(c.FreeBytes), FormatBytes(c.TotalBytes))
	if c.Cores > 0 {
		cores := "cores"
		if c.Cores == 1 {
			cores = "core"
		}
		s += fmt.Sprintf(", %d %s, load %.2f", c.Cores, cores, c.Load.One)
	}
	return s
}

func FormatBytes(n int64) string {
	if n < 0 {
		return "-"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
