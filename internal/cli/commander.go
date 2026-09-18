package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/cli/ui"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/userdir"
	"github.com/plytz/caramelo/internal/vpnclient"
)

func init() {
	register(func(a *app) *cobra.Command {
		cmd := localCmd(&cobra.Command{
			Use:   "commander",
			Short: "The machine you type on: name it and write the files it needs",
			Long: `A commander is the machine a person or an agent types on. It runs no
services and joins no fleet: it holds a name, one identity key and the machines
it can reach, all under the user's own config directory.

'commander init' is the one command a box with no config at all can run.`,
		})
		asGroup(cmd)
		cmd.AddCommand(a.commanderInitCmd())
		return cmd
	})
}

type commanderPaths struct {
	Dir         string `json:"dir"`
	Config      string `json:"config"`
	IdentityKey string `json:"identity_key"`
	VPNDir      string `json:"vpn_dir"`
	CacheDir    string `json:"cache_dir"`
}

type commanderInitResult struct {
	Name      string         `json:"name"`
	Role      string         `json:"role"`
	PublicKey string         `json:"public_key"`
	Paths     commanderPaths `json:"paths"`

	Created []string `json:"created"`

	RenamedFrom string `json:"renamed_from,omitempty"`

	Changed bool `json:"changed"`
}

func (a *app) commanderInitCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Name this machine a commander and write the files it needs",
		Long: `Write the commander's config directory and everything a commander needs:
config.yaml with this machine's name and role, the identity key every fleet is
joined under, an empty vpn directory for the per-fleet records, and the cache
directory. The name defaults to this machine's hostname.

It asks nothing and needs no terminal. Running it again on a commander changes
nothing and says so; --name alone renames this commander.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name != "" && machineNameSlug(name) == "" {
				return &usageError{fmt.Errorf(
					"--name %q has nothing a machine's name can be made of: want lowercase letters, digits and dashes", name)}
			}
			res, err := initCommander(machineNameSlug(name))
			if err != nil {
				return err
			}
			return a.printer().Result(res, func(w io.Writer) error {
				return commanderInitView(res).Write(w)
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "what to call this commander (default: this machine's hostname)")
	return available(cmd, fresh.or(onCommander))
}

func initCommander(name string) (commanderInitResult, error) {
	paths, err := commanderPathsHere()
	if err != nil {
		return commanderInitResult{}, err
	}
	written, _, err := remote.CommanderInitialized()
	if err != nil {
		return commanderInitResult{}, err
	}
	cfg, err := remote.LoadCommanderConfigFrom(paths.Config)
	if err != nil {
		return commanderInitResult{}, err
	}
	res := commanderInitResult{Role: remote.RoleCommander, Paths: paths, Created: []string{}}

	for _, dir := range []string{paths.Dir, paths.VPNDir, paths.CacheDir} {
		created, err := makeDir(dir)
		if err != nil {
			return commanderInitResult{}, err
		}
		if created {
			res.Created = append(res.Created, dir)
		}
	}

	switch {
	case name != "" && cfg.Name != "" && cfg.Name != name:
		res.RenamedFrom, res.Name = cfg.Name, name
	case name != "":
		res.Name = name
	case cfg.Name != "":
		res.Name = cfg.Name
	default:
		res.Name = thisMachineName("")
	}

	rewriting := !written || cfg.Name != res.Name || cfg.Role != remote.RoleCommander
	if rewriting {
		cfg.Name, cfg.Role = res.Name, remote.RoleCommander
		if err := remote.SaveCommanderConfigTo(paths.Config, cfg); err != nil {
			return commanderInitResult{}, err
		}
		if !written {
			res.Created = append(res.Created, paths.Config)
		}
	}

	key, created, err := vpnclient.EnsureIdentity()
	if err != nil {
		return commanderInitResult{}, fmt.Errorf("the commander's identity key: %w", err)
	}
	res.PublicKey = key.Public
	if created {
		res.Created = append(res.Created, paths.IdentityKey)
	}

	res.Changed = rewriting || len(res.Created) > 0
	return res, nil
}

func commanderPathsHere() (commanderPaths, error) {
	dir, err := remote.CommanderDir()
	if err != nil {
		return commanderPaths{}, err
	}
	cache, err := userdir.Cache()
	if err != nil {
		return commanderPaths{}, err
	}
	return commanderPaths{
		Dir:         dir,
		Config:      filepath.Join(dir, remote.CommanderConfigFile),
		IdentityKey: filepath.Join(dir, vpnclient.IdentityKeyFile),
		VPNDir:      filepath.Join(dir, vpnclient.KeyDir),
		CacheDir:    filepath.Join(cache, remote.CommanderDirName),
	}, nil
}

func requireCommander(cmd *cobra.Command) error {
	ok, path, err := remote.CommanderInitialized()
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return &usageError{fmt.Errorf(
		"%s sets up another machine from this one, and only a commander does that: "+
			"run 'caramelo commander init' first (there is no %s)", cmd.CommandPath(), path)}
}

func makeDir(path string) (bool, error) {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		return false, nil
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return false, fmt.Errorf("create %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return false, fmt.Errorf("chmod %s: %w", path, err)
	}
	return true, nil
}

func commanderInitView(r commanderInitResult) *ui.View {
	head := fmt.Sprintf("%s is a commander", r.Name)
	switch {
	case r.RenamedFrom != "":
		head = fmt.Sprintf("renamed %s to %s", r.RenamedFrom, r.Name)
	case !r.Changed:
		head = fmt.Sprintf("%s is already a commander; nothing changed", r.Name)
	}
	f := ui.NewFields(head)
	f.Add("config", "%s", r.Paths.Config)
	f.Add("identity", "%s", r.PublicKey)
	f.Add("fleet records", "%s", r.Paths.VPNDir)
	f.Add("cache", "%s", r.Paths.CacheDir)
	if len(r.Created) > 0 {
		f.Add("created", "%s", strings.Join(r.Created, ", "))
	}
	return ui.NewView().Fields(f)
}
