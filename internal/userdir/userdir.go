package userdir

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

var ServerConfigured = func() bool { return false }

func Config() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return dir, nil
	}
	if home, ok := InvokingUserHome(); ok {
		return filepath.Join(home, ".config"), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return dir, nil
}

func Cache() (string, error) {
	if dir := os.Getenv("XDG_CACHE_HOME"); filepath.IsAbs(dir) {
		return dir, nil
	}
	if home, ok := InvokingUserHome(); ok {
		return filepath.Join(home, ".cache"), nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate user cache dir: %w", err)
	}
	return dir, nil
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
