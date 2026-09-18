package remote

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/plytz/caramelo/internal/userdir"
)

const (
	CommanderConfigFile = "config.yaml"

	CommanderDirName = "caramelo"

	RoleCommander = "commander"
)

type CommanderConfig struct {
	Name string `yaml:"name,omitempty"`

	Role string `yaml:"role,omitempty"`

	Commander Commander `yaml:"commander"`
}

type Commander struct {
	DefaultMachine string `yaml:"default_machine,omitempty"`

	Machines map[string]string `yaml:"machines,omitempty"`
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
	if role := strings.TrimSpace(c.Role); role != "" && role != RoleCommander {
		return CommanderConfig{}, fmt.Errorf(
			"parse %s: role %q: this file is a commander's, the only role it can carry is %s",
			path, role, RoleCommander)
	}
	return c, nil
}

func refuseRetiredCommanderKeys(b []byte) error {
	var probe struct {
		DefaultMachine yaml.Node `yaml:"default_machine"`
		Machines       yaml.Node `yaml:"machines"`
	}
	if err := yaml.Unmarshal(b, &probe); err != nil {
		return nil
	}
	var retired []string
	if !probe.DefaultMachine.IsZero() {
		retired = append(retired, "default_machine is now commander.default_machine")
	}
	if !probe.Machines.IsZero() {
		retired = append(retired, "machines is now commander.machines")
	}
	if len(retired) == 0 {
		return nil
	}
	return fmt.Errorf("a commander's settings live under the commander: key: %s",
		strings.Join(retired, "; "))
}

func SaveCommanderConfig(c CommanderConfig) error {
	path, err := CommanderConfigPath()
	if err != nil {
		return err
	}
	return SaveCommanderConfigTo(path, c)
}

func SaveCommanderConfigTo(path string, c CommanderConfig) error {
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

func (c CommanderConfig) Resolve(name string) (Target, error) {
	if addr, ok := c.Commander.Machines[name]; ok {
		t, err := ParseTarget(addr)
		if err != nil {
			return Target{}, fmt.Errorf("machine %q in commander config: %w", name, err)
		}
		return t, nil
	}
	return ParseTarget(name)
}
