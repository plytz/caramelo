package docker

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

const (
	UserUnit = "docker"

	SetupTool = "dockerd-rootless-setuptool.sh"

	SocketName = "docker.sock"

	ConfigDirName = ".docker"
)

func RuntimeDir(uid string) string { return "/run/user/" + uid }

func SocketPath(uid string) string { return filepath.Join(RuntimeDir(uid), SocketName) }

func BusPath(uid string) string { return filepath.Join(RuntimeDir(uid), "bus") }

func DockerHost(uid string) string { return "unix://" + SocketPath(uid) }

func DaemonDir(home string) string { return filepath.Join(home, ".config", "docker") }

func DaemonJSONPath(home string) string { return filepath.Join(DaemonDir(home), "daemon.json") }

type DaemonConfig struct {
	DataRoot      string
	UserlandProxy bool
}

func RenderDaemonJSON(cfg DaemonConfig) ([]byte, error) {

	m := map[string]any{
		"data-root":      cfg.DataRoot,
		"userland-proxy": cfg.UserlandProxy,
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render daemon.json: %w", err)
	}
	return append(b, '\n'), nil
}
