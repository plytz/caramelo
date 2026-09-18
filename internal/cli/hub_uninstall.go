package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/firewall"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup"
)

var dockerPackages = []string{
	"docker-ce", "docker-ce-cli", "docker-ce-rootless-extras",
	"docker-buildx-plugin", "docker-compose-plugin", "containerd.io",
}

func init() {
	registerHub(func(a *app) *cobra.Command { return a.hubUninstallCmd() })
}

type uninstallAction struct {
	Action string `json:"action"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"`
}

type uninstallReport struct {
	Purge   bool              `json:"purge"`
	DryRun  bool              `json:"dry_run"`
	Actions []uninstallAction `json:"actions"`
	Failed  int               `json:"failed"`
}

func (a *app) hubUninstallCmd() *cobra.Command {
	var (
		configDir string
		purge     bool
		yes       bool
		dryRun    bool
	)
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop caramelod and undo what setup changed on this machine (run as root)",
		Long: `Stop and remove the caramelod user unit and the host configuration setup
wrote, leaving the configuration, the state, the data and the Docker packages in
place.

--purge additionally deletes /etc/caramelo, the state directory, the data
directory (apps, images, volumes: everything), the caramelo user, the installed
binary and the Docker packages setup installed. It is not reversible.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := serverconfig.Default()
			if serverconfig.Exists(configDir) {
				loaded, err := serverconfig.Load(configDir)
				if err != nil {
					return fmt.Errorf("read %s: %w", serverconfig.Path(configDir), err)
				}
				cfg = loaded
			} else {
				fmt.Fprintf(a.stderr, "no %s: assuming the default layout\n", serverconfig.Path(configDir))
			}
			if purge && !yes && !dryRun {
				proceed, err := a.confirmPurge(cmd.Context(), cfg)
				if err != nil {
					return err
				}
				if !proceed {
					return errors.New("cancelled")
				}
			}
			u := &uninstaller{
				cfg: cfg, configDir: configDir, purge: purge, dryRun: dryRun,
				exec: runner.Exec{},
			}
			report := u.run(cmd.Context())
			if err := a.printer().Result(report, func(w io.Writer) error {
				for _, act := range report.Actions {
					msg := act.Detail
					if act.Error != "" {
						msg = act.Error
					}
					if _, err := fmt.Fprintf(w, "[%s] %s: %s\n", act.Status, act.Action, msg); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return err
			}
			if report.Failed > 0 {
				return &exitError{ExitError}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configDir, "config-dir", serverconfig.DefaultConfigDir, "directory holding config.yaml")
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete the config, state and data directories, the user and the Docker packages")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be removed, remove nothing")
	return cmd
}

type uninstaller struct {
	cfg       serverconfig.Config
	configDir string
	purge     bool
	dryRun    bool
	exec      runner.Runner
	report    uninstallReport
}

func (u *uninstaller) run(ctx context.Context) uninstallReport {
	u.report.Purge, u.report.DryRun = u.purge, u.dryRun

	uid, userExists := u.user(ctx)
	if userExists {
		u.do(ctx, "stop-caramelod", "caramelod stopped and disabled", runner.Cmd{
			Name: "systemctl", Args: []string{"--user", "disable", "--now", setup.UserUnit}, User: u.cfg.User,
		})
		if u.purge {

			u.tryTo(ctx, "stop-docker", "rootless Docker stopped", runner.Cmd{
				Name: "systemctl", Args: []string{"--user", "disable", "--now", "docker.service"}, User: u.cfg.User,
			})
		}
	} else {
		u.skip("stop-caramelod", "user "+u.cfg.User+" does not exist")
	}

	type hostFile struct{ name, path string }

	swapfile := u.cfg.SwapFilePath()
	swapUnit, swapUnitErr := setup.SwapUnitPath(ctx, u.exec, swapfile)
	if swapUnitErr != nil {
		u.skip("stop-swap", swapUnitErr.Error()+"; the swap unit, if there is one, is left in place")
	} else {
		u.tryTo(ctx, "stop-swap", "swap unit "+filepath.Base(swapUnit)+" disabled", runner.Cmd{
			Name: "systemctl", Args: []string{"disable", "--now", filepath.Base(swapUnit)},
		})
	}
	u.tryTo(ctx, "swapoff", swapfile+" swapped off", runner.Cmd{Name: "swapoff", Args: []string{"--", swapfile}})

	u.remove(ctx, "unit", setup.UserUnitPath(u.cfg.StateDir))
	files := []hostFile{
		{"tmpfiles", setup.TmpfilesFile},
		{"modules", setup.ModulesFile},
		{"delegate", setup.DelegateFile},
		{"sysctl", setup.SysctlFile},
	}
	if swapUnitErr == nil {
		files = append(files, hostFile{"swap-unit", swapUnit})
	}
	files = append(files, hostFile{"swap-sysctl", setup.SwapSysctlFile}, hostFile{"swapfile", swapfile})
	for _, f := range files {
		u.remove(ctx, f.name, f.path)
	}
	u.do(ctx, "daemon-reload", "systemd reloaded", runner.Cmd{Name: "systemctl", Args: []string{"daemon-reload"}})
	u.skip("firewall", fmt.Sprintf(
		"a rule for udp %d may have been added by 'hub setup --open-ports'; it is left in place — "+
			"remove it with 'ufw delete allow %d/udp' if you want it gone",
		firewall.VPNPort(u.cfg.VPNListen), firewall.VPNPort(u.cfg.VPNListen)))

	if !u.purge {
		u.skip("purge", "kept "+strings.Join([]string{u.configDir, u.cfg.StateDir, u.cfg.DataDir}, ", ")+" and the Docker packages (--purge removes them)")
		return u.report
	}

	if userExists {

		u.tryTo(ctx, "disable-linger", "linger disabled", runner.Cmd{Name: "loginctl", Args: []string{"disable-linger", u.cfg.User}})
		if uid > 0 {
			u.tryTo(ctx, "stop-session", fmt.Sprintf("user@%d.service stopped", uid), runner.Cmd{
				Name: "systemctl", Args: []string{"stop", fmt.Sprintf("user@%d.service", uid)},
			})
		}
	}
	u.purgePackages(ctx)
	for _, dir := range []string{u.configDir, u.cfg.StateDir, u.cfg.DataDir, u.cfg.RunDir} {
		u.do(ctx, "remove "+dir, "removed", runner.Cmd{Name: "rm", Args: []string{"-rf", "--", dir}})
	}
	u.remove(ctx, "binary", serverconfig.BinaryPath)
	if userExists {
		u.do(ctx, "remove user", "user "+u.cfg.User+" removed", runner.Cmd{Name: "userdel", Args: []string{u.cfg.User}})

		if u.groupExists(ctx) {
			u.do(ctx, "remove group", "group "+u.cfg.Group+" removed", runner.Cmd{Name: "groupdel", Args: []string{u.cfg.Group}})
		} else {
			u.skip("remove group", "group "+u.cfg.Group+" is gone with the user")
		}
	}
	return u.report
}

func (u *uninstaller) user(ctx context.Context) (int, bool) {
	res, err := u.exec.Run(ctx, runner.Cmd{Name: "getent", Args: []string{"passwd", u.cfg.User}})
	if err != nil || res.ExitCode != 0 {
		return 0, false
	}
	fields := strings.Split(strings.TrimSpace(res.Stdout), ":")
	if len(fields) < 3 {
		return 0, true
	}
	uid, _ := strconv.Atoi(fields[2])
	return uid, true
}

func (u *uninstaller) groupExists(ctx context.Context) bool {
	res, err := u.exec.Run(ctx, runner.Cmd{Name: "getent", Args: []string{"group", u.cfg.Group}})
	return err == nil && res.ExitCode == 0
}

func (u *uninstaller) purgePackages(ctx context.Context) {
	var installed []string
	for _, pkg := range dockerPackages {
		res, err := u.exec.Run(ctx, runner.Cmd{Name: "dpkg-query", Args: []string{"-W", "-f=${Status}", pkg}})
		if err == nil && res.ExitCode == 0 && strings.Contains(res.Stdout, "install ok installed") {
			installed = append(installed, pkg)
		}
	}
	if len(installed) == 0 {
		u.skip("purge packages", "no Docker packages installed")
		return
	}
	args := append([]string{"purge", "-y", "--auto-remove"}, installed...)
	u.do(ctx, "purge packages", "removed "+strings.Join(installed, ", "), runner.Cmd{
		Name: "apt-get", Args: args, Env: []string{"DEBIAN_FRONTEND=noninteractive"},
	})
}

func (u *uninstaller) remove(ctx context.Context, name, path string) {
	if _, err := os.Lstat(path); err != nil {
		u.skip("remove "+name, path+" is not there")
		return
	}
	u.do(ctx, "remove "+name, "removed "+path, runner.Cmd{Name: "rm", Args: []string{"-rf", "--", path}})
}

func (u *uninstaller) do(ctx context.Context, action, detail string, c runner.Cmd) {
	if u.dryRun {
		u.report.Actions = append(u.report.Actions, uninstallAction{Action: action, Status: "would-change", Detail: detail})
		return
	}
	res, err := u.exec.Run(ctx, c)
	switch {
	case err != nil:
		u.fail(action, err.Error())
	case res.ExitCode != 0:
		u.fail(action, fmt.Sprintf("exit %d: %s", res.ExitCode, strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout))))
	default:
		u.report.Actions = append(u.report.Actions, uninstallAction{Action: action, Status: "done", Detail: detail})
	}
}

func (u *uninstaller) tryTo(ctx context.Context, action, detail string, c runner.Cmd) {
	before := u.report.Failed
	u.do(ctx, action, detail, c)
	if u.report.Failed > before {
		last := len(u.report.Actions) - 1
		u.report.Actions[last] = uninstallAction{
			Action: action, Status: "skipped", Detail: u.report.Actions[last].Error,
		}
		u.report.Failed--
	}
}

func (u *uninstaller) skip(action, detail string) {
	u.report.Actions = append(u.report.Actions, uninstallAction{Action: action, Status: "skipped", Detail: detail})
}

func (u *uninstaller) fail(action, msg string) {
	u.report.Actions = append(u.report.Actions, uninstallAction{Action: action, Status: "failed", Error: msg})
	u.report.Failed++
}

var errPurgeNeedsYes = errors.New("--purge deletes every app, image and volume on this machine: re-run with --yes to confirm")

func (a *app) confirmPurge(ctx context.Context, cfg serverconfig.Config) (bool, error) {
	plan := fmt.Sprintf("This deletes %s, %s and %s (every app, image and volume), the %s user and the Docker packages.\n",
		serverconfig.DefaultConfigDir, cfg.StateDir, cfg.DataDir, cfg.User)
	return a.confirmName(ctx, errPurgeNeedsYes, plan, "Type 'purge' to confirm: ", "purge")
}

func firstNonEmpty(candidates ...string) string {
	for _, s := range candidates {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
