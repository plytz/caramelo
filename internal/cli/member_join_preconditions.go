package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"strings"

	dockerrt "github.com/plytz/caramelo/internal/runtime/docker"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup"
	dockersetup "github.com/plytz/caramelo/internal/setup/docker"
)

type joinPreflight struct {
	cfg     serverconfig.Config
	loaded  bool
	missing []string
}

const asRootHere = "(run this on the machine that is joining, as root)"

func absent(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func joinPreflightOf(configDir string) (joinPreflight, error) {
	cfg := serverconfig.Default()
	loaded := false
	path := serverconfig.Path(configDir)
	gone, err := absent(path)
	if err != nil {
		return joinPreflight{}, fmt.Errorf(
			"read this machine's configuration at %s: %w %s", path, err, asRootHere)
	}
	if !gone {
		c, err := serverconfig.Load(configDir)
		if err != nil {
			return joinPreflight{}, fmt.Errorf("read this machine's configuration: %w", err)
		}
		cfg, loaded = c, true
	}
	return joinPreflightFor(configDir, cfg, loaded)
}

func joinPreflightFor(configDir string, cfg serverconfig.Config, loaded bool) (joinPreflight, error) {
	p := joinPreflight{cfg: cfg, loaded: loaded}
	u, userErr := user.Lookup(cfg.User)
	if userErr != nil {
		p.missing = append(p.missing, fmt.Sprintf(
			"no user %s on this machine: `caramelo fleet setup` creates it in its %s step",
			cfg.User, setup.NewUserStep().Name()))
	}
	if !loaded {
		p.missing = append(p.missing, fmt.Sprintf(
			"no configuration at %s: `caramelo fleet setup` writes it in its %s step",
			serverconfig.Path(configDir), setup.NewDirsStep().Name()))
	}
	if userErr != nil {
		p.missing = append(p.missing, fmt.Sprintf(
			"no rootless Docker for %s: `caramelo fleet setup` installs and starts it in its %s step",
			cfg.User, dockersetup.StepRootless))
	} else if sock := dockerrt.SocketPath(u.Uid); !socketExists(sock) {
		p.missing = append(p.missing, fmt.Sprintf(
			"no rootless Docker answering at %s: `caramelo fleet setup` installs and starts it in its %s step",
			sock, dockersetup.StepRootless))
	}
	keyPath := cfg.VPNKeyPath()
	noKey, err := absent(keyPath)
	if err != nil {
		return joinPreflight{}, fmt.Errorf(
			"read this machine's WireGuard key at %s: %w %s", keyPath, err, asRootHere)
	}
	if noKey {
		p.missing = append(p.missing, fmt.Sprintf(
			"no WireGuard key at %s: `caramelo fleet setup` makes it in its %s step",
			keyPath, setup.NewVPNStep().Name()))
	}
	return p, nil
}

func (p joinPreflight) err() error {
	if len(p.missing) == 0 {
		return nil
	}
	lines := []string{"this machine has not been set up: `caramelo member join` needs what " +
		"`sudo caramelo fleet setup` makes here, and `caramelo member add` does both halves"}
	for _, m := range p.missing {
		lines = append(lines, "  - "+m)
	}
	return errors.New(strings.Join(lines, "\n"))
}
