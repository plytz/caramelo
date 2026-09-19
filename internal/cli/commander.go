package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/cli/ui"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/task"
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
		cmd.AddCommand(a.commanderInitCmd(), a.commanderWriteConfigCmd())
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
			res, err := a.initCommander(cmd.Context(), machineNameSlug(name))
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

func (a *app) initCommander(ctx context.Context, name string) (commanderInitResult, error) {
	paths, err := commanderPathsHere()
	if err != nil {
		return commanderInitResult{}, err
	}
	cfg, err := remote.LoadCommanderConfigFrom(paths.Config)
	if err != nil {
		return commanderInitResult{}, err
	}
	res := commanderInitResult{Role: remote.RoleCommander, Paths: paths, Created: []string{}}

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

	known := []string{paths.Dir, paths.VPNDir, paths.CacheDir, paths.Config, paths.IdentityKey}
	before := whichExist(known)

	f, err := task.Load(commanderSetupTask)
	if err != nil {
		return commanderInitResult{}, err
	}
	engine := &task.Engine{
		Runner:  runner.Exec{},
		Leaf:    a.taskLeaf(),
		Version: version,
		Vars: map[string]string{
			"name":  res.Name,
			"dir":   paths.Dir,
			"vpn":   paths.VPNDir,
			"cache": paths.CacheDir,
		},
	}
	report, err := engine.Execute(ctx, f)
	if err != nil {
		return commanderInitResult{}, fmt.Errorf("name this machine a commander: %w", err)
	}
	if failed, ok := report.FirstFailure(); ok {
		return commanderInitResult{}, fmt.Errorf("name this machine a commander: %s: %s",
			failed.Name, commanderFailure(failed))
	}
	if report.ReportErr != nil {
		return commanderInitResult{}, fmt.Errorf("name this machine a commander: %w", report.ReportErr)
	}

	after := whichExist(known)
	for _, path := range known {
		if !before[path] && after[path] {
			res.Created = append(res.Created, path)
		}
	}

	key, err := vpnclient.LoadIdentity()
	if err != nil {
		return commanderInitResult{}, fmt.Errorf("the commander's identity key: %w", err)
	}
	res.PublicKey = key.Public
	res.Changed = report.Changed > 0
	return res, nil
}

const commanderSetupTask = "commander-setup"

func commanderFailure(res task.Result) string {
	for i := len(res.Commands) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(res.Commands[i].Stderr); line != "" {
			return res.Error + ": " + strings.SplitN(line, "\n", 2)[0]
		}
	}
	return res.Error
}

func whichExist(paths []string) map[string]bool {
	out := map[string]bool{}
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			out[path] = true
		}
	}
	return out
}

func (a *app) commanderWriteConfigCmd() *cobra.Command {
	var path, name string
	var check bool
	cmd := &cobra.Command{
		Use:    "write-config",
		Short:  "Write a commander config.yaml with a name and the commander role (tasks only)",
		Hidden: true,
		Args:   exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case strings.TrimSpace(path) == "":
				return &usageError{errors.New("commander write-config: --path says which file is written")}
			case machineNameSlug(name) == "":
				return &usageError{fmt.Errorf(
					"--name %q has nothing a machine's name can be made of: want lowercase letters, digits and dashes", name)}
			}
			name = machineNameSlug(name)
			cfg, err := remote.LoadCommanderConfigFrom(path)
			if err != nil {
				return err
			}
			written, err := fileIsThere(path)
			if err != nil {
				return err
			}
			if check {
				if written && cfg.Name == name && cfg.Role == remote.RoleCommander {
					fmt.Fprintf(a.stdout, "name %s, role %s\n", cfg.Name, cfg.Role)
					return nil
				}
				if !written {
					fmt.Fprintf(a.stdout, "%s missing\n", path)
				} else {
					fmt.Fprintf(a.stdout, "%s names %s, want %s\n", path, orNothing(cfg.Name), name)
				}
				return &exitError{ExitError}
			}
			cfg.Name, cfg.Role = name, remote.RoleCommander
			if err := remote.SaveCommanderConfigTo(path, cfg); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "%s written\n", path)
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "the config file to write")
	cmd.Flags().StringVar(&name, "name", "", "what to call this commander")
	cmd.Flags().BoolVar(&check, "check", false, "say whether the file already names this commander, and change nothing")
	return available(cmd, fresh.or(onCommander))
}

func fileIsThere(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	}
	return false, fmt.Errorf("stat %s: %w", path, err)
}

func orNothing(s string) string {
	if s == "" {
		return "nothing"
	}
	return s
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
