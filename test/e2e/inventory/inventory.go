//go:build e2e

package inventory

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	EnvVar   = "CARAMELO_E2E_INVENTORY"
	ResetEnv = "CARAMELO_E2E_RESET"

	Version     = 1
	DefaultPort = 22
)

type Machine struct {
	Name    string `json:"name"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	User    string `json:"user"`
	Key     string `json:"key"`
	Arch    string `json:"arch"`
	HostKey string `json:"host_key"`
}

type Inventory struct {
	Version  int       `json:"version"`
	Machines []Machine `json:"machines"`
}

func (inv Inventory) Count() int { return len(inv.Machines) }

func (inv Inventory) Hub() (Machine, bool) {
	if len(inv.Machines) == 0 {
		return Machine{}, false
	}
	return inv.Machines[0], true
}

func (inv Inventory) Nodes() []Machine {
	if len(inv.Machines) < 2 {
		return nil
	}
	nodes := make([]Machine, len(inv.Machines)-1)
	copy(nodes, inv.Machines[1:])
	return nodes
}

func LoadEnv() (Inventory, string, error) {
	path := os.Getenv(EnvVar)
	if strings.TrimSpace(path) == "" {
		return Inventory{}, "", fmt.Errorf("%s is not set: it must name the JSON inventory file describing the machines the end-to-end tier drives", EnvVar)
	}
	inv, err := Load(path)
	if err != nil {
		return Inventory{}, path, err
	}
	return inv, path, nil
}

func Load(path string) (Inventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Inventory{}, fmt.Errorf("inventory file %s does not exist: %s must name an existing JSON inventory file", path, EnvVar)
		}
		return Inventory{}, fmt.Errorf("reading inventory file %s: %w", path, err)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var inv Inventory
	if err := dec.Decode(&inv); err != nil {
		return Inventory{}, fmt.Errorf("inventory file %s is not a valid inventory: %w", path, err)
	}

	if inv.Version != Version {
		return Inventory{}, fmt.Errorf("inventory file %s has version %d, but the end-to-end tier only understands version %d", path, inv.Version, Version)
	}

	seen := make(map[string]int, len(inv.Machines))
	for i := range inv.Machines {
		m := &inv.Machines[i]
		if err := normalize(path, i, m); err != nil {
			return Inventory{}, err
		}
		if first, dup := seen[m.Name]; dup {
			return Inventory{}, fmt.Errorf("inventory file %s: machines[%d] and machines[%d] are both named %q, but machine names must be unique", path, first, i, m.Name)
		}
		seen[m.Name] = i
	}
	return inv, nil
}

func normalize(path string, i int, m *Machine) error {
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("inventory file %s: machines[%d] has no name", path, i)
	}
	if strings.TrimSpace(m.Host) == "" {
		return fmt.Errorf("inventory file %s: machine %s has no host", path, m.Name)
	}
	if strings.TrimSpace(m.User) == "" {
		return fmt.Errorf("inventory file %s: machine %s has no user", path, m.Name)
	}
	if m.Port == 0 {
		m.Port = DefaultPort
	}
	if m.Port < 1 || m.Port > 65535 {
		return fmt.Errorf("inventory file %s: machine %s has port %d, which is not a port between 1 and 65535", path, m.Name, m.Port)
	}
	if strings.TrimSpace(m.Key) == "" {
		return fmt.Errorf("inventory file %s: machine %s has no key, which must be the path to the ssh private key that reaches it", path, m.Name)
	}
	key, err := expandHome(m.Key)
	if err != nil {
		return fmt.Errorf("inventory file %s: machine %s key %s cannot be expanded: %w", path, m.Name, m.Key, err)
	}
	if _, err := os.Stat(key); err != nil {
		return fmt.Errorf("inventory file %s: machine %s key %s does not exist: %w", path, m.Name, key, err)
	}
	m.Key = key
	if m.HostKey != "" {
		if err := checkHostKey(m.HostKey); err != nil {
			return fmt.Errorf("inventory file %s: machine %s has host_key %q, which %s: it must be \"<type> <base64>\"", path, m.Name, m.HostKey, err)
		}
	}
	return nil
}

func checkHostKey(hostKey string) error {
	fields := strings.Fields(hostKey)
	if len(fields) != 2 {
		return fmt.Errorf("has %d space-separated fields instead of 2", len(fields))
	}
	if _, err := base64.StdEncoding.DecodeString(fields[1]); err != nil {
		return errors.New("does not carry base64 key material")
	}
	return nil
}

func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
}
