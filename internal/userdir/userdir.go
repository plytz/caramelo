package userdir

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

const (
	ConfigHomeEnv = "XDG_CONFIG_HOME"

	CacheHomeEnv = "XDG_CACHE_HOME"

	ConfigHomeDefault = ".config"

	CacheHomeDefault = ".cache"
)

var ServerConfigured = func() bool { return false }

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
	if h, ok := InvokingUserHome(); ok {
		return filepath.Join(h, under), nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate the home directory for %s: %w", filepath.Join("~", under), err)
	}
	return filepath.Join(h, under), nil
}

func InvokingUserHome() (string, bool) {
	name := strings.TrimSpace(os.Getenv("SUDO_USER"))
	if name == "" || name == "root" || ServerConfigured() {
		return "", false
	}
	u, err := user.Lookup(name)
	if err != nil || !filepath.IsAbs(u.HomeDir) {
		return "", false
	}
	return u.HomeDir, true
}
