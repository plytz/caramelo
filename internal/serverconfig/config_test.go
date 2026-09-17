package serverconfig

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/plytz/caramelo/internal/edge/certs"
)

func TestDefault(t *testing.T) {
	c := Default()
	want := Config{
		User: "caramelo", Group: "caramelo",
		StateDir: "/var/lib/caramelo", DataDir: "/mnt/caramelo", RunDir: "/run/caramelo",
		SSHPort: 4022, Bind: "0.0.0.0",
		VPNSubnet: "10.86.0.0/16", VPNListen: "0.0.0.0:4021", APIListen: "vpn",
		Edge: false, TLS: "acme", HTTP3: true,
		Reserve: Reserve{MemoryBytes: 256 << 20, CPU: 0.25},
		Swap:    Swap{Backend: "file", SizeBytes: 4 << 30, Swappiness: 10},
	}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("Default() =\n %+v\nwant\n %+v", c, want)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Default() does not validate: %v", err)
	}
}

func TestPath(t *testing.T) {
	if got, want := Path("/etc/caramelo"), "/etc/caramelo/config.yaml"; got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
	if got, want := Path(""), ConfigFile; got != want {
		t.Errorf("Path(\"\") = %q, want %q", got, want)
	}
}

func TestDerivedPaths(t *testing.T) {
	c := Config{StateDir: "/state", DataDir: "/data", RunDir: "/run"}
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"DBPath", c.DBPath(), "/state/caramelo.db"},
		{"SSHDir", c.SSHDir(), "/state/ssh"},
		{"HostKeyPath", c.HostKeyPath(), "/state/ssh/host_ed25519"},
		{"AuthorizedKeysPath", c.AuthorizedKeysPath(), "/state/ssh/authorized_keys"},
		{"SocketPath", c.SocketPath(), "/run/caramelod.sock"},
		{"DockerDataRoot", c.DockerDataRoot(), "/data/docker"},
		{"SwapFilePath", c.SwapFilePath(), "/state.swapfile"},
		{"AppsDir", c.AppsDir(), "/data/apps"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}

	if !strings.HasPrefix(c.HostKeyPath(), c.SSHDir()+string(filepath.Separator)) {
		t.Errorf("HostKeyPath %q is not under SSHDir %q", c.HostKeyPath(), c.SSHDir())
	}
}

func TestDerivedPathsFollowConfig(t *testing.T) {

	c := Default()
	c.StateDir, c.DataDir, c.RunDir = "/srv/state", "/srv/data", "/srv/run"
	for _, p := range []string{c.DBPath(), c.SSHDir(), c.HostKeyPath(), c.AuthorizedKeysPath()} {
		if !strings.HasPrefix(p, c.StateDir) {
			t.Errorf("%q is not under state_dir %q", p, c.StateDir)
		}
	}
	for _, p := range []string{c.DockerDataRoot(), c.AppsDir()} {
		if !strings.HasPrefix(p, c.DataDir) {
			t.Errorf("%q is not under data_dir %q", p, c.DataDir)
		}
	}
	if !strings.HasPrefix(c.SocketPath(), c.RunDir) {
		t.Errorf("%q is not under run_dir %q", c.SocketPath(), c.RunDir)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Default()
	want.User, want.Group = "someone", "somegroup"
	want.SSHPort = 2222
	want.Bind = "127.0.0.1"
	want.StateDir, want.DataDir, want.RunDir = "/srv/state", "/srv/data", "/srv/run"
	want.Reserve = Reserve{MemoryBytes: 1 << 30, CPU: 1.5}
	want.Swap = Swap{Backend: SwapFile, SizeBytes: 8 << 30, Swappiness: 20}

	if Exists(dir) {
		t.Fatal("Exists = true before anything was written")
	}
	if err := Save(dir, want, 0o640); err != nil {
		t.Fatalf("save: %v", err)
	}
	if !Exists(dir) {
		t.Error("Exists = false after Save")
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip =\n %+v\nwant\n %+v", got, want)
	}

	fi, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o640 {
		t.Errorf("mode = %v, want 0640", perm)
	}

	if _, err := os.Stat(Path(dir) + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat tmp file: %v, want it gone", err)
	}

	b, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal written config: %v", err)
	}
	for _, key := range []string{"user", "group", "state_dir", "data_dir", "run_dir", "ssh_port", "bind", "reserve", "swap", "vpn_subnet", "vpn_listen", "api_listen"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("written config has no %q key: %s", key, b)
		}
	}
}

func TestSaveOverwrites(t *testing.T) {
	dir := t.TempDir()
	first := Default()
	if err := Save(dir, first, 0o644); err != nil {
		t.Fatalf("save: %v", err)
	}
	second := Default()
	second.SSHPort = 5555
	if err := Save(dir, second, 0o644); err != nil {
		t.Fatalf("save again: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.SSHPort != 5555 {
		t.Errorf("ssh_port = %d, want the second write's 5555", got.SSHPort)
	}
}

func TestSaveRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	bad := Default()
	bad.SSHPort = 0
	if err := Save(dir, bad, 0o644); err == nil {
		t.Fatal("save: want an error for an invalid config")
	}
	if Exists(dir) {
		t.Error("an invalid config was written to disk")
	}
}

func TestSaveMissingDir(t *testing.T) {

	dir := filepath.Join(t.TempDir(), "not-created-yet")
	err := Save(dir, Default(), 0o644)
	if err == nil {
		t.Fatal("save: want an error when the directory does not exist")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want it to wrap os.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), "write config") {
		t.Errorf("err = %v, want a 'write config' error", err)
	}
}

func TestLoadFillsDefaults(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "ssh_port: 2200\n")
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := Default()
	want.SSHPort = 2200
	if !reflect.DeepEqual(got, want) {
		t.Errorf("load =\n %+v\nwant\n %+v", got, want)
	}
}

func TestLoadWithoutASwapKeyKeepsTheDefault(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "ssh_port: 2200\nstate_dir: /srv/state\n")
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Swap != (Swap{Backend: SwapFile, SizeBytes: 4 << 30, Swappiness: 10}) {
		t.Errorf("swap = %+v, want the default", got.Swap)
	}
	if want := "/srv/state.swapfile"; got.SwapFilePath() != want {
		t.Errorf("SwapFilePath = %q, want %q", got.SwapFilePath(), want)
	}
}

func TestSwapFilePathSitsBesideTheStateTree(t *testing.T) {
	tests := []struct {
		state string
		want  string
	}{
		{"/var/lib/caramelo", "/var/lib/caramelo.swapfile"},
		{"/var/lib/caramelo/", "/var/lib/caramelo.swapfile"},
		{"/srv/state", "/srv/state.swapfile"},
	}
	for _, tc := range tests {
		c := Default()
		c.StateDir = tc.state
		if got := c.SwapFilePath(); got != tc.want {
			t.Errorf("SwapFilePath(%q) = %q, want %q", tc.state, got, tc.want)
		}
		if strings.HasPrefix(c.SwapFilePath(), strings.TrimRight(tc.state, "/")+"/") {
			t.Errorf("SwapFilePath(%q) = %q is inside the state tree", tc.state, c.SwapFilePath())
		}
	}
}

func TestLoadMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(dir)
	if err == nil {
		t.Fatal("load: want an error for a missing file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want it to wrap os.ErrNotExist", err)
	}
	if Exists(dir) {
		t.Error("Exists = true for a missing file")
	}
}

func TestLoadMalformed(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"not yaml", "ssh_port: [unclosed\n"},
		{"tab indentation", "reserve:\n\tcpu: 1\n"},
		{"wrong type", "ssh_port: not-a-number\n"},
		{"scalar document", "just a string\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, tc.yaml)
			_, err := Load(dir)
			if err == nil {
				t.Fatal("load: want a parse error")
			}
			if !strings.Contains(err.Error(), Path(dir)) {
				t.Errorf("err = %v, want the file path in the message", err)
			}
		})
	}
}

func TestLoadValidates(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "ssh_port: 0\nstate_dir: relative/path\n")
	_, err := Load(dir)
	if err == nil {
		t.Fatal("load: want a validation error")
	}
	for _, want := range []string{"ssh_port", "state_dir"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
}

func TestValidate(t *testing.T) {
	empty := func(field string) Config {
		c := Default()
		switch field {
		case "user":
			c.User = ""
		case "group":
			c.Group = ""
		case "state_dir":
			c.StateDir = ""
		case "data_dir":
			c.DataDir = ""
		case "run_dir":
			c.RunDir = ""
		case "bind":
			c.Bind = ""
		}
		return c
	}
	relative := func(field string) Config {
		c := Default()
		switch field {
		case "state_dir":
			c.StateDir = "state"
		case "data_dir":
			c.DataDir = "data"
		case "run_dir":
			c.RunDir = "run"
		}
		return c
	}
	port := func(p int) Config {
		c := Default()
		c.SSHPort = p
		return c
	}
	swap := func(s Swap) Config {
		c := Default()
		c.Swap = s
		return c
	}

	tests := []struct {
		name string
		cfg  Config
		want []string
	}{
		{name: "default", cfg: Default()},
		{name: "min port", cfg: port(1)},
		{name: "max port", cfg: port(65535)},
		{name: "empty user", cfg: empty("user"), want: []string{"user must not be empty"}},
		{name: "empty group", cfg: empty("group"), want: []string{"group must not be empty"}},
		{name: "empty bind", cfg: empty("bind"), want: []string{"bind must not be empty"}},

		{name: "empty state_dir", cfg: empty("state_dir"), want: []string{"state_dir must not be empty"}},
		{name: "empty data_dir", cfg: empty("data_dir"), want: []string{"data_dir must not be empty"}},
		{name: "empty run_dir", cfg: empty("run_dir"), want: []string{"run_dir must not be empty"}},
		{name: "relative state_dir", cfg: relative("state_dir"), want: []string{"state_dir must be absolute"}},
		{name: "relative data_dir", cfg: relative("data_dir"), want: []string{"data_dir must be absolute"}},
		{name: "relative run_dir", cfg: relative("run_dir"), want: []string{"run_dir must be absolute"}},
		{name: "port zero", cfg: port(0), want: []string{"ssh_port 0 out of range"}},
		{name: "port negative", cfg: port(-1), want: []string{"ssh_port -1 out of range"}},
		{name: "port too high", cfg: port(65536), want: []string{"ssh_port 65536 out of range"}},
		{name: "swap off", cfg: swap(Swap{Backend: SwapOff, Swappiness: 10})},
		{name: "swap file at the floor", cfg: swap(Swap{Backend: SwapFile, SizeBytes: 256 << 20, Swappiness: 10})},
		{
			name: "unknown swap backend",
			cfg:  swap(Swap{Backend: "tmpfs", SizeBytes: 4 << 30, Swappiness: 10}),
			want: []string{`swap.backend "tmpfs": want one of file, zram, off`},
		},
		{
			name: "swap backend zram",
			cfg:  swap(Swap{Backend: SwapZram, Swappiness: 10}),
			want: []string{"zram is not implemented yet: use a size such as 4G, or off"},
		},
		{
			name: "swap file without a size",
			cfg:  swap(Swap{Backend: SwapFile, Swappiness: 10}),
			want: []string{"swap.size_bytes 0: a swapfile is at least 256 MiB"},
		},
		{
			name: "swap file below the floor",
			cfg:  swap(Swap{Backend: SwapFile, SizeBytes: 64 << 20, Swappiness: 10}),
			want: []string{"swap.size_bytes 67108864: a swapfile is at least 256 MiB"},
		},
		{
			name: "swap off carrying a size",
			cfg:  swap(Swap{Backend: SwapOff, SizeBytes: 4 << 30, Swappiness: 10}),
			want: []string{"swap.size_bytes 4294967296: swap.backend off takes no size"},
		},
		{
			name: "swappiness out of range",
			cfg:  swap(Swap{Backend: SwapFile, SizeBytes: 4 << 30, Swappiness: 101}),
			want: []string{"swap.swappiness 101 out of range"},
		},
		{
			name: "swappiness negative",
			cfg:  swap(Swap{Backend: SwapFile, SizeBytes: 4 << 30, Swappiness: -1}),
			want: []string{"swap.swappiness -1 out of range"},
		},
		{
			name: "everything wrong is reported at once",
			cfg:  Config{},
			want: []string{
				"user must not be empty", "group must not be empty", "bind must not be empty",
				"state_dir must not be empty", "data_dir must not be empty", "run_dir must not be empty",
				"ssh_port 0 out of range",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate = nil, want %v", tc.want)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Validate = %v, want it to mention %q", err, want)
				}
			}
		})
	}
}

func TestValidateEmptyDirIsNotAlsoRelative(t *testing.T) {
	c := Default()
	c.StateDir = ""
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate = nil, want an error")
	}
	if strings.Contains(err.Error(), "state_dir must be absolute") {
		t.Errorf("Validate = %v, want only the empty complaint", err)
	}
}

func write(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(Path(dir), []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestVPNDefaultsAndPaths(t *testing.T) {
	c := Default()
	if c.VPNSubnet != "10.86.0.0/16" || c.VPNListen != "0.0.0.0:4021" {
		t.Errorf("vpn defaults = %q %q", c.VPNSubnet, c.VPNListen)
	}

	if c.APIListen != APIListenVPN {
		t.Errorf("api_listen default = %q, want %q", c.APIListen, APIListenVPN)
	}
	c.StateDir = "/state"
	if got, want := c.VPNKeyPath(), "/state/vpn/private.key"; got != want {
		t.Errorf("VPNKeyPath = %q, want %q", got, want)
	}
	if got, want := c.VPNDir(), "/state/vpn"; got != want {
		t.Errorf("VPNDir = %q, want %q", got, want)
	}
	p, err := c.VPNSubnetPrefix()
	if err != nil {
		t.Fatalf("VPNSubnetPrefix: %v", err)
	}
	if p.String() != "10.86.0.0/16" {
		t.Errorf("VPNSubnetPrefix = %s", p)
	}
}

func TestAPIListenPredicates(t *testing.T) {
	tests := []struct {
		value                string
		public, insideTunnel bool
	}{
		{APIListenVPN, false, true},
		{APIListenPublic, true, false},
		{APIListenBoth, true, true},
	}
	for _, tc := range tests {
		c := Default()
		c.APIListen = tc.value
		if got := c.APIListensPublic(); got != tc.public {
			t.Errorf("%q: APIListensPublic = %v, want %v", tc.value, got, tc.public)
		}
		if got := c.APIListensVPN(); got != tc.insideTunnel {
			t.Errorf("%q: APIListensVPN = %v, want %v", tc.value, got, tc.insideTunnel)
		}
	}
}

func TestValidateVPNKeys(t *testing.T) {
	tests := []struct {
		name  string
		mut   func(*Config)
		field string
	}{
		{"subnet is not a CIDR", func(c *Config) { c.VPNSubnet = "10.86.0.0" }, "vpn_subnet"},
		{"subnet is IPv6", func(c *Config) { c.VPNSubnet = "fd00::/64" }, "vpn_subnet"},
		{"subnet is too small", func(c *Config) { c.VPNSubnet = "10.86.0.0/24" }, "vpn_subnet"},
		{"subnet is empty", func(c *Config) { c.VPNSubnet = "" }, "vpn_subnet"},
		{"listen has no port", func(c *Config) { c.VPNListen = "0.0.0.0" }, "vpn_listen"},
		{"listen host is not an IP", func(c *Config) { c.VPNListen = "box.example.com:4021" }, "vpn_listen"},
		{"listen port is out of range", func(c *Config) { c.VPNListen = "0.0.0.0:70000" }, "vpn_listen"},
		{"listen is empty", func(c *Config) { c.VPNListen = "" }, "vpn_listen"},
		{"api_listen is unknown", func(c *Config) { c.APIListen = "everywhere" }, "api_listen"},
		{"api_listen is empty", func(c *Config) { c.APIListen = "" }, "api_listen"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mut(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate() accepted %+v", c)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("err = %v, want it to name %s", err, tc.field)
			}
		})
	}

	c := Default()
	c.VPNListen = ":4021"
	if err := c.Validate(); err != nil {
		t.Errorf("Validate(:4021) = %v, want it accepted", err)
	}
	for _, v := range APIListenValues {
		c := Default()
		c.APIListen = v
		if err := c.Validate(); err != nil {
			t.Errorf("api_listen %q rejected: %v", v, err)
		}
	}
}

func TestMachineIP(t *testing.T) {
	tests := []struct {
		subnet string
		want   string
	}{
		{"10.86.0.0/16", "10.86.0.1"},
		{"10.86.0.0/22", "10.86.0.1"},
		{"192.168.40.0/22", "192.168.40.1"},

		{"10.86.7.9/16", "10.86.0.1"},
	}
	for _, tc := range tests {
		got, err := MachineIP(netip.MustParsePrefix(tc.subnet))
		if err != nil {
			t.Fatalf("MachineIP(%s): %v", tc.subnet, err)
		}
		if got.String() != tc.want {
			t.Errorf("MachineIP(%s) = %s, want %s", tc.subnet, got, tc.want)
		}
	}
	if _, err := MachineIP(netip.MustParsePrefix("fd00::/64")); err == nil {
		t.Error("MachineIP accepted an IPv6 subnet")
	}
	if _, err := MachineIP(netip.Prefix{}); err == nil {
		t.Error("MachineIP accepted the zero prefix")
	}

	if _, err := MachineIP(netip.MustParsePrefix("10.86.0.0/32")); err == nil {
		t.Error("MachineIP accepted a /32")
	}
}

func TestVPNDerivedAddresses(t *testing.T) {
	c := Default()
	api, err := c.VPNAPIAddrPort()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := api.String(), "10.86.0.1:4022"; got != want {
		t.Errorf("VPNAPIAddrPort() = %s, want %s", got, want)
	}
	res, err := c.VPNResolverAddrPort()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := res.String(), "10.86.0.1:53"; got != want {
		t.Errorf("VPNResolverAddrPort() = %s, want %s", got, want)
	}

	c.VPNSubnet = "10.99.0.0/16"
	c.SSHPort = 2222
	api, err = c.VPNAPIAddrPort()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := api.String(), "10.99.0.1:2222"; got != want {
		t.Errorf("VPNAPIAddrPort() = %s, want %s", got, want)
	}
	bad := Default()
	bad.VPNSubnet = "not a subnet"
	if _, err := bad.VPNAPIAddrPort(); err == nil {
		t.Error("VPNAPIAddrPort accepted an unparseable vpn_subnet")
	}
}

func TestValidateEdgeKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"an unknown tls mode", func(c *Config) { c.TLS = "selfsigned" }, "tls"},
		{"an acme_ca that is not a URL", func(c *Config) { c.ACMECA = "pebble:14000" }, "acme_ca"},
		{"an acme_ca with no host", func(c *Config) { c.ACMECA = "https://" }, "acme_ca"},

		{"an acme_ca in the clear", func(c *Config) { c.ACMECA = "http://ca.internal/dir" }, "acme_ca"},
		{"an acme_email that is not one", func(c *Config) { c.ACMEEmail = "ops.example.com" }, "acme_email"},
		{"an edge with no way to get a certificate", func(c *Config) { c.Edge = true }, "acme_email is required"},
		{"a certs_keep that is not a duration", func(c *Config) { c.CertsKeep = "a month" }, "certs_keep"},
		{"a negative certs_keep", func(c *Config) { c.CertsKeep = "-720h" }, "certs_keep"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.edit(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error about %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %v, want it to mention %q", err, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{"acme with an address", func(c *Config) { c.Edge, c.ACMEEmail = true, "ops@example.com" }},
		{"acme against a test CA", func(c *Config) { c.Edge, c.ACMECA = true, "https://pebble:14000/dir" }},
		{"an internal CA", func(c *Config) { c.Edge, c.TLS = true, "internal" }},
		{"a retention of its own", func(c *Config) { c.Edge, c.TLS, c.CertsKeep = true, "internal", "168h" }},
		{"a retention of nothing at all", func(c *Config) { c.Edge, c.TLS, c.CertsKeep = true, "internal", "0s" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.edit(&c)
			if err := c.Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestEdgePaths(t *testing.T) {
	c := Default()
	for _, tc := range []struct{ got, want string }{
		{c.EdgeDir(), "/var/lib/caramelo/edge"},
		{c.EdgeCertsDir(), "/var/lib/caramelo/edge/certs"},
		{c.EdgeRoutesPath(), "/var/lib/caramelo/edge/routes.json"},
		{c.EdgeSocketPath(), "/run/caramelo/edge.sock"},
	} {
		if tc.got != tc.want {
			t.Errorf("path = %q, want %q", tc.got, tc.want)
		}
	}
	if got := c.ACMEDirectory(); !strings.Contains(got, "letsencrypt.org") {
		t.Errorf("ACMEDirectory() = %q, want Let's Encrypt production by default", got)
	}
	c.ACMECA = "https://pebble:14000/dir"
	if got := c.ACMEDirectory(); got != c.ACMECA {
		t.Errorf("ACMEDirectory() = %q, want %q", got, c.ACMECA)
	}
}

func TestCertsKeepDuration(t *testing.T) {
	c := Default()
	keep, err := c.CertsKeepDuration()
	if err != nil {
		t.Fatalf("an unset certs_keep: %v", err)
	}
	if keep != certs.DefaultKeepStale {
		t.Errorf("an unset certs_keep = %s, want the store's own default %s", keep, certs.DefaultKeepStale)
	}
	for in, want := range map[string]time.Duration{
		"168h":  168 * time.Hour,
		" 24h ": 24 * time.Hour,
		"0s":    0,
	} {
		c.CertsKeep = in
		got, err := c.CertsKeepDuration()
		if err != nil || got != want {
			t.Errorf("certs_keep %q = %s, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"a month", "30 days", "-1h", "720"} {
		c.CertsKeep = in
		if _, err := c.CertsKeepDuration(); err == nil {
			t.Errorf("certs_keep %q was accepted", in)
		}
	}
}

func TestCertsKeepSurvivesARoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := Default()
	c.Edge, c.ACMEEmail, c.CertsKeep = true, "ops@example.com", "168h"
	if err := Save(dir, c, 0o640); err != nil {
		t.Fatalf("Save: %v", err)
	}
	back, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if back.CertsKeep != "168h" {
		t.Errorf("certs_keep came back %q", back.CertsKeep)
	}

	plain := Default()
	plain.Edge, plain.ACMEEmail = true, "ops@example.com"
	if err := Save(dir, plain, 0o640); err != nil {
		t.Fatalf("Save: %v", err)
	}
	b, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "certs_keep") {
		t.Errorf("a machine that never set a retention has certs_keep in its config.yaml:\n%s", b)
	}
}
