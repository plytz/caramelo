package machine

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("parse integer %s: %w", b, err)
	}
	*f = flexInt(n)
	return nil
}

type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	switch s := strings.Trim(string(b), `"`); s {
	case "true", "1":
		*f = true
	case "false", "0", "", "null":
		*f = false
	default:
		return fmt.Errorf("parse boolean %s", b)
	}
	return nil
}

func parseMeminfo(s string) (Memory, error) {
	var m Memory
	var seenTotal bool
	for _, line := range strings.Split(s, "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && strings.EqualFold(fields[1], "kB") {
			v *= 1024
		}
		switch key {
		case "MemTotal":
			m.TotalBytes, seenTotal = v, true
		case "MemAvailable":
			m.AvailableBytes = v
		case "SwapTotal":
			m.SwapTotalBytes = v
		}
	}
	if !seenTotal {
		return Memory{}, fmt.Errorf("no MemTotal in meminfo")
	}
	return m, nil
}

func parseOSRelease(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

func parseFindmntT(s string) (source, fstype, target string, err error) {
	var doc struct {
		Filesystems []struct {
			Target string `json:"target"`
			Source string `json:"source"`
			FSType string `json:"fstype"`
		} `json:"filesystems"`
	}
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		return "", "", "", fmt.Errorf("parse findmnt JSON: %w", err)
	}
	if len(doc.Filesystems) == 0 {
		return "", "", "", fmt.Errorf("findmnt reported no filesystem")
	}
	f := doc.Filesystems[0]
	return f.Source, f.FSType, f.Target, nil
}

func parseDF(s string) (m Mount, err error) {
	var fields []string
	for i, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if i == 0 {
			continue
		}
		fields = append(fields, strings.Fields(line)...)
	}
	if len(fields) < 6 {
		return Mount{}, fmt.Errorf("df output has %d fields, want at least 6", len(fields))
	}
	nums := make([]int64, 3)
	for i, f := range fields[2:5] {
		v, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return Mount{}, fmt.Errorf("df field %q: %w", f, err)
		}
		nums[i] = v
	}
	return Mount{
		Source:     fields[0],
		FSType:     fields[1],
		SizeBytes:  nums[0],
		AvailBytes: nums[2],
		Path:       strings.Join(fields[5:], " "),
	}, nil
}

func parseLsblk(s string) ([]Disk, error) {
	var doc struct {
		BlockDevices []struct {
			Name  string   `json:"name"`
			Type  string   `json:"type"`
			Size  flexInt  `json:"size"`
			Rota  flexBool `json:"rota"`
			Model *string  `json:"model"`
		} `json:"blockdevices"`
	}
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		return nil, fmt.Errorf("parse lsblk JSON: %w", err)
	}
	var disks []Disk
	for _, d := range doc.BlockDevices {
		if d.Type != "disk" || d.Size <= 0 {
			continue
		}
		disk := Disk{Name: d.Name, SizeBytes: int64(d.Size), Rotational: bool(d.Rota)}
		if d.Model != nil {
			disk.Model = strings.TrimSpace(*d.Model)
		}
		disks = append(disks, disk)
	}
	return disks, nil
}

func parseCgroupVersion(s string) int {
	switch strings.TrimSpace(s) {
	case "cgroup2fs":
		return 2
	case "tmpfs", "cgroupfs":
		return 1
	default:
		return 0
	}
}

func parseControllers(s string) []string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return nil
	}
	return f
}

func parseRouteGet(s string) (iface, ip string, err error) {
	var routes []struct {
		Dev     string `json:"dev"`
		PrefSrc string `json:"prefsrc"`
	}
	if err := json.Unmarshal([]byte(s), &routes); err != nil {
		return "", "", fmt.Errorf("parse ip route JSON: %w", err)
	}
	if len(routes) == 0 {
		return "", "", fmt.Errorf("ip route returned no route")
	}
	return routes[0].Dev, routes[0].PrefSrc, nil
}

func parseAddrs(s string) ([]string, error) {
	var links []struct {
		IfName   string   `json:"ifname"`
		Flags    []string `json:"flags"`
		AddrInfo []struct {
			Family string `json:"family"`
			Local  string `json:"local"`
			Scope  string `json:"scope"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(s), &links); err != nil {
		return nil, fmt.Errorf("parse ip addr JSON: %w", err)
	}
	var out []string
	seen := map[string]bool{}
	for _, l := range links {
		if l.IfName == "lo" || hasFlag(l.Flags, "LOOPBACK") {
			continue
		}
		for _, a := range l.AddrInfo {
			if a.Local == "" || a.Scope == "host" || a.Scope == "link" {
				continue
			}
			if seen[a.Local] {
				continue
			}
			seen[a.Local] = true
			out = append(out, a.Local)
		}
	}
	return out, nil
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

func parseCPUModel(s string) string {
	var fallback string
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "model name":
			if v != "" {
				return v
			}
		case "Model", "Hardware", "CPU implementer":
			if fallback == "" {
				fallback = v
			}
		}
	}
	return fallback
}

func parseNProc(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("parse nproc output %q: %w", strings.TrimSpace(s), err)
	}
	if n < 1 {
		return 0, fmt.Errorf("nproc reported %d CPUs", n)
	}
	return n, nil
}

type dockerInfo struct {
	ServerVersion   string   `json:"ServerVersion"`
	Driver          string   `json:"Driver"`
	DockerRootDir   string   `json:"DockerRootDir"`
	SecurityOptions []string `json:"SecurityOptions"`
	Warnings        []string `json:"Warnings"`
	ServerErrors    []string `json:"ServerErrors"`
}

func parseDockerInfo(s string) (Docker, error) {
	var i dockerInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &i); err != nil {
		return Docker{}, fmt.Errorf("parse docker info JSON: %w", err)
	}
	d := Docker{
		Installed:     true,
		ServerVersion: i.ServerVersion,
		StorageDriver: i.Driver,
		DataRoot:      i.DockerRootDir,
	}
	for _, o := range i.SecurityOptions {

		for _, part := range strings.Split(o, ",") {
			if strings.TrimSpace(part) == "name=rootless" {
				d.Rootless = true
			}
		}
	}
	d.Warnings = append(d.Warnings, i.Warnings...)
	d.Warnings = append(d.Warnings, i.ServerErrors...)
	return d, nil
}

func parseDockerVersionDrivers(s string) (netDriver, portDriver string) {
	var section string
	var anyNet, anyPort string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent <= 1 && strings.HasSuffix(trimmed, ":") {
			section = strings.ToLower(strings.TrimSuffix(trimmed, ":"))
			continue
		}
		k, v, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "NetworkDriver":
			anyNet = v
			if section == "rootlesskit" {
				netDriver = v
			}
		case "PortDriver":
			anyPort = v
			if section == "rootlesskit" {
				portDriver = v
			}
		}
	}

	if netDriver == "" {
		netDriver = anyNet
	}
	if portDriver == "" {
		portDriver = anyPort
	}
	return netDriver, portDriver
}
