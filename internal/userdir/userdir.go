package userdir

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	ConfigHomeEnv = "XDG_CONFIG_HOME"

	CacheHomeEnv = "XDG_CACHE_HOME"

	ConfigHomeDefault = ".config"

	CacheHomeDefault = ".cache"
)

func Config() (string, error) {
	return home(ConfigHomeEnv, ConfigHomeDefault)
}

func Cache() (string, error) {
	return home(CacheHomeEnv, CacheHomeDefault)
}

func home(env, under string) (string, error) {
	if dir := os.Getenv(env); filepath.IsAbs(dir) {
		return dir, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate the home directory for %s: %w", filepath.Join("~", under), err)
	}
	return filepath.Join(h, under), nil
}
