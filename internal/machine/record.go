package machine

import "time"

type Record struct {
	MachineID string    `json:"machine_id"`
	Hostname  string    `json:"hostname"`
	GaugedAt  time.Time `json:"gauged_at"`

	OS     OS     `json:"os"`
	CPU    CPU    `json:"cpu"`
	Memory Memory `json:"memory"`

	Load    Load    `json:"load"`
	Dirs    Dirs    `json:"dirs"`
	DataDir Mount   `json:"data_dir"`
	Disks   []Disk  `json:"disks"`
	Network Network `json:"network"`
	Cgroup  Cgroup  `json:"cgroup"`
	Docker  Docker  `json:"docker"`

	Reserved Reserved `json:"reserved"`
	Caramelo Caramelo `json:"caramelo"`
}

type OS struct {
	ID        string `json:"id"`
	VersionID string `json:"version_id"`
	Codename  string `json:"codename"`
	Kernel    string `json:"kernel"`

	Arch string `json:"arch"`

	Hardware string `json:"hardware,omitempty"`
	Virt     string `json:"virt"`
}

type CPU struct {
	Count int    `json:"count"`
	Model string `json:"model"`
}

type Memory struct {
	TotalBytes     int64 `json:"total_bytes"`
	AvailableBytes int64 `json:"available_bytes"`
	SwapTotalBytes int64 `json:"swap_total_bytes"`
	SwapManaged    bool  `json:"swap_managed,omitempty"`
}

type Dirs struct {
	Config string `json:"config"`
	State  string `json:"state"`
	Data   string `json:"data"`
}

type Mount struct {
	Path          string `json:"path"`
	OwnMountPoint bool   `json:"own_mount_point"`
	Source        string `json:"source"`
	FSType        string `json:"fstype"`
	SizeBytes     int64  `json:"size_bytes"`
	AvailBytes    int64  `json:"avail_bytes"`
}

type Disk struct {
	Name       string `json:"name"`
	SizeBytes  int64  `json:"size_bytes"`
	Rotational bool   `json:"rotational"`
	Model      string `json:"model,omitempty"`
}

type Network struct {
	PrimaryIface string   `json:"primary_iface"`
	PrimaryIP    string   `json:"primary_ip"`
	Addresses    []string `json:"addresses"`
}

type Cgroup struct {
	Version     int      `json:"version"`
	Controllers []string `json:"controllers"`
}

type Docker struct {
	Installed     bool     `json:"installed"`
	ServerVersion string   `json:"server_version,omitempty"`
	Rootless      bool     `json:"rootless"`
	StorageDriver string   `json:"storage_driver,omitempty"`
	NetDriver     string   `json:"net_driver,omitempty"`
	PortDriver    string   `json:"port_driver,omitempty"`
	DataRoot      string   `json:"data_root,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

type Reserved struct {
	MemoryBytes int64   `json:"memory_bytes"`
	CPU         float64 `json:"cpu"`
}

type Caramelo struct {
	Version            string `json:"version"`
	User               string `json:"user"`
	UID                int    `json:"uid"`
	SSHPort            int    `json:"ssh_port"`
	HostKeyFingerprint string `json:"host_key_fingerprint,omitempty"`
}
