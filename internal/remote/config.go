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

const ClientConfigFile = "config.yaml"

type ClientConfig struct {
	DefaultMachine string `yaml:"default_machine,omitempty"`

	Machines map[string]string `yaml:"machines,omitempty"`
}

func ClientConfigPath() (string, error) {
	dir, err := userdir.Config()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "caramelo", ClientConfigFile), nil
}

func LoadClientConfig() (ClientConfig, error) {
	path, err := ClientConfigPath()
	if err != nil {
		return ClientConfig{}, err
	}
	return LoadClientConfigFrom(path)
}

func LoadClientConfigFrom(path string) (ClientConfig, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ClientConfig{}, nil
	}
	if err != nil {
		return ClientConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	var c ClientConfig
	if err := yaml.Unmarshal(b, &c); err != nil {
		return ClientConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}

func SaveClientConfig(c ClientConfig) error {
	path, err := ClientConfigPath()
	if err != nil {
		return err
	}
	return SaveClientConfigTo(path, c)
}

func SaveClientConfigTo(path string, c ClientConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode client config: %w", err)
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

func (c ClientConfig) Resolve(name string) (Target, error) {
	if addr, ok := c.Machines[name]; ok {
		t, err := ParseTarget(addr)
		if err != nil {
			return Target{}, fmt.Errorf("machine %q in client config: %w", name, err)
		}
		return t, nil
	}
	return ParseTarget(name)
}
