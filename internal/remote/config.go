package remote

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/userdir"
)

const (
	CommanderConfigFile = "config.yaml"

	CommanderDirName = "caramelo"

	RoleCommander = serverconfig.RoleCommander
)

type CommanderConfig struct {
	Name string `yaml:"name,omitempty"`

	Role string `yaml:"role,omitempty"`

	Commander Commander `yaml:"commander"`
}

type Commander struct {
	DefaultFleet string `yaml:"default_fleet,omitempty"`

	Fleets map[string]Fleet `yaml:"fleets,omitempty"`
}

type Fleet struct {
	Hub string `yaml:"hub"`

	PublicKey string `yaml:"public_key,omitempty"`

	Apps []string `yaml:"apps,omitempty"`
}

func CommanderDir() (string, error) {
	dir, err := userdir.Config()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, CommanderDirName), nil
}

func CommanderConfigPath() (string, error) {
	dir, err := CommanderDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, CommanderConfigFile), nil
}

func CommanderInitialized() (bool, string, error) {
	path, err := CommanderConfigPath()
	if err != nil {
		return false, "", err
	}
	_, err = os.Stat(path)
	switch {
	case err == nil:
		return true, path, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, path, nil
	default:
		return false, path, fmt.Errorf("stat %s: %w", path, err)
	}
}

func LoadCommanderConfig() (CommanderConfig, error) {
	path, err := CommanderConfigPath()
	if err != nil {
		return CommanderConfig{}, err
	}
	return LoadCommanderConfigFrom(path)
}

func LoadCommanderConfigFrom(path string) (CommanderConfig, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return CommanderConfig{}, nil
	}
	if err != nil {
		return CommanderConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	var c CommanderConfig
	if err := yaml.Unmarshal(b, &c); err != nil {
		return CommanderConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := refuseRetiredCommanderKeys(b); err != nil {
		return CommanderConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return CommanderConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func SaveCommanderConfig(c CommanderConfig) error {
	path, err := CommanderConfigPath()
	if err != nil {
		return err
	}
	return SaveCommanderConfigTo(path, c)
}

func SaveCommanderConfigTo(path string, c CommanderConfig) error {
	if err := c.Validate(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode commander config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

const retiredCommanderKeys = "default_machine and machines: are retired: a commander says name: and " +
	"role: commander at the top and keeps its fleets under commander: (default_fleet, fleets), " +
	"each fleet being a hub address, the key pinned for it and the apps it holds"

func refuseRetiredCommanderKeys(b []byte) error {
	var probe struct {
		DefaultMachine yaml.Node `yaml:"default_machine"`
		Machines       yaml.Node `yaml:"machines"`
	}
	if err := yaml.Unmarshal(b, &probe); err != nil {
		return nil
	}
	if probe.DefaultMachine.IsZero() && probe.Machines.IsZero() {
		return nil
	}
	return errors.New(retiredCommanderKeys)
}

var slugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func ValidFleetName(s string) bool { return slugRe.MatchString(s) }

func (c CommanderConfig) Validate() error {
	var errs []error
	switch role := strings.ToLower(strings.TrimSpace(c.Role)); role {
	case "", RoleCommander:
	case serverconfig.RoleHub, serverconfig.RoleMember:
		errs = append(errs, fmt.Errorf(
			"role %s: this is the commander's own file, and a %s says what it is in its machine config: want role %s",
			role, role, RoleCommander))
	default:
		errs = append(errs, fmt.Errorf("role %q: want %s", c.Role, RoleCommander))
	}
	if n := strings.TrimSpace(c.Name); n != "" && !ValidFleetName(n) {
		errs = append(errs, fmt.Errorf(
			"name %q: a machine's name is a slug — lowercase letters, digits and dashes", n))
	}
	for _, name := range c.FleetNames() {
		if !ValidFleetName(name) {
			errs = append(errs, fmt.Errorf(
				"commander.fleets.%s: a fleet's name is a slug — lowercase letters, digits and dashes", name))
		}
		f := c.Commander.Fleets[name]
		if strings.TrimSpace(f.Hub) == "" {
			errs = append(errs, fmt.Errorf(
				"commander.fleets.%s.hub must say where the fleet's hub answers: user@host[:port]", name))
			continue
		}
		if _, err := ParseTarget(f.Hub); err != nil {
			errs = append(errs, fmt.Errorf("commander.fleets.%s.hub: %w", name, err))
		}
	}
	if d := strings.TrimSpace(c.Commander.DefaultFleet); d != "" {
		if _, ok := c.Commander.Fleets[d]; !ok {
			errs = append(errs, fmt.Errorf(
				"commander.default_fleet %q is not one of the fleets this commander knows (%s)",
				d, c.FleetList()))
		}
	}
	return errors.Join(errs...)
}

func (c CommanderConfig) MachineName() string {
	if n := strings.TrimSpace(c.Name); n != "" {
		return n
	}
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	if i := strings.Index(host, "."); i > 0 {
		host = host[:i]
	}
	return host
}

func (c CommanderConfig) FleetNames() []string {
	names := make([]string, 0, len(c.Commander.Fleets))
	for name := range c.Commander.Fleets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (c CommanderConfig) FleetList() string {
	names := c.FleetNames()
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

func (c CommanderConfig) FleetTarget(name string) (Target, error) {
	f, ok := c.Commander.Fleets[name]
	if !ok {
		return Target{}, fmt.Errorf(
			"fleet %q is not one of the fleets this commander knows (%s); add it with "+
				"'caramelo fleet add %s user@host'", name, c.FleetList(), name)
	}
	t, err := ParseTarget(f.Hub)
	if err != nil {
		return Target{}, fmt.Errorf("commander.fleets.%s.hub: %w", name, err)
	}
	return t, nil
}

func (c CommanderConfig) FleetForApp(app string) string {
	app = strings.TrimSpace(app)
	if app == "" {
		return ""
	}
	var found string
	for _, name := range c.FleetNames() {
		if !slices.Contains(c.Commander.Fleets[name].Apps, app) {
			continue
		}
		if found != "" {
			return ""
		}
		found = name
	}
	return found
}

func (c *CommanderConfig) SetFleet(name string, f Fleet) {
	if c.Commander.Fleets == nil {
		c.Commander.Fleets = map[string]Fleet{}
	}
	c.Commander.Fleets[name] = f
	if c.Commander.DefaultFleet == "" {
		c.Commander.DefaultFleet = name
	}
}

func (c *CommanderConfig) RemoveFleet(name string) bool {
	if _, ok := c.Commander.Fleets[name]; !ok {
		return false
	}
	delete(c.Commander.Fleets, name)
	if c.Commander.DefaultFleet == name {
		c.Commander.DefaultFleet = ""
	}
	return true
}

func (c *CommanderConfig) SetDefaultFleet(name string) error {
	if _, ok := c.Commander.Fleets[name]; !ok {
		return fmt.Errorf("fleet %q is not one of the fleets this commander knows (%s)", name, c.FleetList())
	}
	c.Commander.DefaultFleet = name
	return nil
}

func (c *CommanderConfig) Pin(name, key string) (bool, error) {
	f, ok := c.Commander.Fleets[name]
	if !ok {
		return false, fmt.Errorf("fleet %q is not one of the fleets this commander knows (%s)", name, c.FleetList())
	}
	key = strings.TrimSpace(key)
	switch {
	case key == "":
		return false, nil
	case f.PublicKey == "":
		f.PublicKey = key
		c.Commander.Fleets[name] = f
		return true, nil
	case f.PublicKey == key:
		return false, nil
	}
	return false, fmt.Errorf(
		"the hub of fleet %s at %s answered with the key %s, not the key %s pinned for that fleet "+
			"when this commander first reached it: refusing to talk to it. If the hub really was rebuilt, "+
			"'caramelo fleet remove %s' and add it again",
		name, f.Hub, ElideKey(key), ElideKey(f.PublicKey), name)
}

func (c *CommanderConfig) RecordApp(name, app string) bool {
	f, ok := c.Commander.Fleets[name]
	if !ok || strings.TrimSpace(app) == "" || slices.Contains(f.Apps, app) {
		return false
	}
	f.Apps = append(f.Apps, app)
	sort.Strings(f.Apps)
	c.Commander.Fleets[name] = f
	return true
}

func ElideKey(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12] + "…"
}
