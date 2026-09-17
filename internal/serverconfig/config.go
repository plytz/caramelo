package serverconfig

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/edge/certs"
	"github.com/plytz/caramelo/internal/vpn"
)

const (
	DefaultConfigDir = "/etc/caramelo"
	DefaultStateDir  = "/var/lib/caramelo"
	DefaultDataDir   = "/mnt/caramelo"
	DefaultRunDir    = "/run/caramelo"
	DefaultUser      = "caramelo"
	DefaultGroup     = "caramelo"
	DefaultSSHPort   = 4022
	DefaultBind      = "0.0.0.0"
	ConfigFile       = "config.yaml"
	SocketFile       = "caramelod.sock"
	BinaryPath       = "/usr/local/bin/caramelo"
)

const (
	DefaultVPNSubnet = vpn.DefaultSubnet

	DefaultVPNListen = vpn.DefaultListen
)

const (
	DefaultEdge = false

	DefaultTLS = string(certs.ModeACME)

	DefaultHTTP3 = true
)

var TLSValues = []string{string(certs.ModeACME), string(certs.ModeInternal)}

const (
	APIListenVPN = "vpn"

	APIListenPublic = "public"

	APIListenBoth = "both"

	DefaultAPIListen = APIListenVPN
)

var APIListenValues = []string{APIListenVPN, APIListenPublic, APIListenBoth}

type Config struct {
	User     string `yaml:"user"`
	Group    string `yaml:"group"`
	StateDir string `yaml:"state_dir"`
	DataDir  string `yaml:"data_dir"`
	RunDir   string `yaml:"run_dir"`
	SSHPort  int    `yaml:"ssh_port"`
	Bind     string `yaml:"bind"`

	VPNSubnet string `yaml:"vpn_subnet"`

	VPNListen string `yaml:"vpn_listen"`

	APIListen string `yaml:"api_listen"`

	Edge bool `yaml:"edge"`

	TLS string `yaml:"tls"`

	ACMECA string `yaml:"acme_ca"`

	ACMEEmail string `yaml:"acme_email"`

	CertsKeep string `yaml:"certs_keep,omitempty"`

	HTTP3 bool `yaml:"http3"`

	Reserve Reserve `yaml:"reserve"`

	Fleet Fleet `yaml:"fleet,omitempty"`
}

type Reserve struct {
	MemoryBytes int64   `yaml:"memory_bytes"`
	CPU         float64 `yaml:"cpu"`
}

func Default() Config {
	return Config{
		User: DefaultUser, Group: DefaultGroup,
		StateDir: DefaultStateDir, DataDir: DefaultDataDir, RunDir: DefaultRunDir,
		SSHPort: DefaultSSHPort, Bind: DefaultBind,
		VPNSubnet: DefaultVPNSubnet, VPNListen: DefaultVPNListen, APIListen: DefaultAPIListen,
		Edge: DefaultEdge, TLS: DefaultTLS, HTTP3: DefaultHTTP3,
		Reserve: Reserve{MemoryBytes: 256 << 20, CPU: 0.25},
	}
}

func (c Config) DBPath() string             { return filepath.Join(c.StateDir, "caramelo.db") }
func (c Config) SSHDir() string             { return filepath.Join(c.StateDir, "ssh") }
func (c Config) HostKeyPath() string        { return filepath.Join(c.SSHDir(), "host_ed25519") }
func (c Config) AuthorizedKeysPath() string { return filepath.Join(c.SSHDir(), "authorized_keys") }
func (c Config) SocketPath() string         { return filepath.Join(c.RunDir, SocketFile) }
func (c Config) DockerDataRoot() string     { return filepath.Join(c.DataDir, "docker") }

func (c Config) VPNDir() string     { return filepath.Join(c.StateDir, filepath.Dir(vpn.KeyFile)) }
func (c Config) VPNKeyPath() string { return filepath.Join(c.StateDir, vpn.KeyFile) }

const (
	VaultKeyFile        = "vault.key"
	VaultSecretsDirName = "secrets"
)

func (c Config) VaultKeyPath() string { return filepath.Join(c.StateDir, VaultKeyFile) }

func (c Config) SecretsDir() string { return filepath.Join(c.RunDir, VaultSecretsDirName) }

func (c Config) VPNSubnetPrefix() (netip.Prefix, error) {
	p, err := netip.ParsePrefix(c.VPNSubnet)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("vpn_subnet %q: %w", c.VPNSubnet, err)
	}
	return p.Masked(), nil
}

func (c Config) APIListensPublic() bool {
	return c.APIListen == APIListenPublic || c.APIListen == APIListenBoth
}

func (c Config) APIListensVPN() bool {
	return c.APIListen == APIListenVPN || c.APIListen == APIListenBoth
}
func (c Config) AppsDir() string { return filepath.Join(c.DataDir, "apps") }

func (c Config) EdgeDir() string { return filepath.Join(c.StateDir, edge.StateDirName) }

func (c Config) EdgeCertsDir() string { return edge.CertsPath(c.StateDir) }

func (c Config) EdgeRoutesPath() string { return edge.RoutesPath(c.StateDir) }

func (c Config) EdgeSocketPath() string { return edge.SocketPath(c.RunDir) }

func (c Config) TLSMode() (certs.Mode, error) { return certs.ParseMode(c.TLS) }

func (c Config) ACMEDirectory() string {
	if strings.TrimSpace(c.ACMECA) == "" {
		return certs.LetsEncryptProduction
	}
	return c.ACMECA
}

func (c Config) CertsKeepDuration() (time.Duration, error) {
	keep := strings.TrimSpace(c.CertsKeep)
	if keep == "" {
		return certs.DefaultKeepStale, nil
	}
	d, err := time.ParseDuration(keep)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("certs_keep %q: want a duration such as 720h", c.CertsKeep)
	}
	return d, nil
}

func validateEdge(c Config) []error {
	var errs []error
	if c.TLS != "" && !slices.Contains(TLSValues, c.TLS) {
		errs = append(errs, fmt.Errorf("tls %q: want one of %s", c.TLS, strings.Join(TLSValues, ", ")))
	}
	if ca := strings.TrimSpace(c.ACMECA); ca != "" {
		u, err := url.Parse(ca)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("acme_ca %q: %w", ca, err))
		case u.Scheme != "https":

			errs = append(errs, fmt.Errorf("acme_ca %q: want an ACME directory URL (https://…)", ca))
		case u.Host == "":
			errs = append(errs, fmt.Errorf("acme_ca %q: no host", ca))
		}
	}
	if email := strings.TrimSpace(c.ACMEEmail); email != "" {
		if i := strings.IndexByte(email, '@'); i <= 0 || i == len(email)-1 || strings.ContainsAny(email, " \t") {
			errs = append(errs, fmt.Errorf("acme_email %q: want an email address", email))
		}
	}
	if _, err := c.CertsKeepDuration(); err != nil {
		errs = append(errs, err)
	}
	if c.Edge && c.TLS == string(certs.ModeACME) &&
		strings.TrimSpace(c.ACMEEmail) == "" && strings.TrimSpace(c.ACMECA) == "" {
		errs = append(errs, errors.New(
			"acme_email is required with the default certificate authority: "+
				"pass --acme-email, or --tls internal for names with no public DNS"))
	}
	return errs
}

func validateVPN(c Config) []error {
	var errs []error
	if c.VPNSubnet != "" {
		p, err := netip.ParsePrefix(c.VPNSubnet)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("vpn_subnet %q: %w", c.VPNSubnet, err))
		case !p.Addr().Is4():
			errs = append(errs, fmt.Errorf("vpn_subnet %q: must be IPv4 (IPv6 inside the tunnel is not in M5)", c.VPNSubnet))
		case p.Bits() > MaxVPNSubnetBits:
			errs = append(errs, fmt.Errorf("vpn_subnet %q: /%d is too small; at most /%d so peers and environments have separate ranges",
				c.VPNSubnet, p.Bits(), MaxVPNSubnetBits))
		}
	}
	if c.VPNListen != "" {
		host, port, err := net.SplitHostPort(c.VPNListen)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("vpn_listen %q: want host:port: %w", c.VPNListen, err))
		default:
			if host != "" {
				if _, err := netip.ParseAddr(host); err != nil {
					errs = append(errs, fmt.Errorf("vpn_listen %q: %q is not an IP address", c.VPNListen, host))
				}
			}
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				errs = append(errs, fmt.Errorf("vpn_listen %q: port %q out of range", c.VPNListen, port))
			}
		}
	}
	if c.APIListen != "" && !slices.Contains(APIListenValues, c.APIListen) {
		errs = append(errs, fmt.Errorf("api_listen %q: want one of %s", c.APIListen, strings.Join(APIListenValues, ", ")))
	}
	return errs
}

const MaxVPNSubnetBits = 22

func Path(dir string) string { return filepath.Join(dir, ConfigFile) }

func Load(dir string) (Config, error) {
	b, err := os.ReadFile(Path(dir))
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	c := Default()
	if err := yaml.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", Path(dir), err)
	}
	return c, c.Validate()
}

func Exists(dir string) bool {
	_, err := os.Stat(Path(dir))
	return err == nil
}

func Save(dir string, c Config, mode os.FileMode) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp := Path(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return fmt.Errorf("chmod config: %w", err)
	}
	if err := os.Rename(tmp, Path(dir)); err != nil {
		return fmt.Errorf("rename config: %w", err)
	}
	return nil
}

func (c Config) Validate() error {
	var errs []error
	for name, v := range map[string]string{"user": c.User, "group": c.Group, "state_dir": c.StateDir, "data_dir": c.DataDir, "run_dir": c.RunDir, "bind": c.Bind, "vpn_subnet": c.VPNSubnet, "vpn_listen": c.VPNListen, "api_listen": c.APIListen} {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s must not be empty", name))
		}
	}
	for name, v := range map[string]string{"state_dir": c.StateDir, "data_dir": c.DataDir, "run_dir": c.RunDir} {
		if v != "" && !filepath.IsAbs(v) {
			errs = append(errs, fmt.Errorf("%s must be absolute", name))
		}
	}
	if c.SSHPort < 1 || c.SSHPort > 65535 {
		errs = append(errs, fmt.Errorf("ssh_port %d out of range", c.SSHPort))
	}
	errs = append(errs, validateVPN(c)...)
	errs = append(errs, validateEdge(c)...)
	errs = append(errs, validateFleet(c)...)
	return errors.Join(errs...)
}

func MachineIP(p netip.Prefix) (netip.Addr, error) {
	if !p.IsValid() {
		return netip.Addr{}, fmt.Errorf("invalid subnet")
	}
	if !p.Addr().Is4() {
		return netip.Addr{}, fmt.Errorf("subnet %s: must be IPv4 (IPv6 inside the tunnel is not in M5)", p)
	}
	ip := p.Masked().Addr().Next()
	if !p.Contains(ip) {
		return netip.Addr{}, fmt.Errorf("subnet %s: no room for the machine's own address", p)
	}
	return ip, nil
}

func (c Config) VPNMachineIP() (netip.Addr, error) {
	p, err := c.VPNSubnetPrefix()
	if err != nil {
		return netip.Addr{}, err
	}
	return MachineIP(p)
}

func (c Config) VPNAPIAddrPort() (netip.AddrPort, error) {
	ip, err := c.VPNMachineIP()
	if err != nil {
		return netip.AddrPort{}, err
	}
	if c.SSHPort < 1 || c.SSHPort > 65535 {
		return netip.AddrPort{}, fmt.Errorf("ssh_port %d out of range", c.SSHPort)
	}
	return netip.AddrPortFrom(ip, uint16(c.SSHPort)), nil
}

func (c Config) VPNResolverAddrPort() (netip.AddrPort, error) {
	ip, err := c.VPNMachineIP()
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(ip, vpn.ResolverPort), nil
}
