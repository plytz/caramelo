package remote

import (
	"errors"
	"fmt"
	"github.com/plytz/caramelo/internal/userdir"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const CommanderConfigFile = "config.yaml"

type CommanderConfig struct {
	DefaultMachine string `yaml:"default_machine,omitempty"`

	Machines map[string]string `yaml:"machines,omitempty"`
}

func CommanderConfigPath() (string, error) {
	dir, err := userdir.Config()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "caramelo", CommanderConfigFile), nil
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
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

func (c CommanderConfig) Resolve(name string) (Target, error) {
	if addr, ok := c.Machines[name]; ok {
		t, err := ParseTarget(addr)
		if err != nil {
			return Target{}, fmt.Errorf("machine %q in commander config: %w", name, err)
		}
		return t, nil
	}
	return ParseTarget(name)
}
