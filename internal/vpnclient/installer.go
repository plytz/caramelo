package vpnclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

var ErrUnsupported = errors.New("transparent mode is not supported on this platform yet")

type SystemInstaller struct {
	GOOS string

	Root string

	Home string

	User string
	UID  string
	GID  string

	Binary string

	Run func(ctx context.Context, name string, args ...string) error

	Machine string
}

var _ Installer = (*SystemInstaller)(nil)

func NewInstaller() Installer { return &SystemInstaller{} }

func (s *SystemInstaller) goos() string {
	if s.GOOS != "" {
		return s.GOOS
	}
	return runtime.GOOS
}

type invoker struct {
	Name string
	UID  string
	GID  string
	Home string
}

func (s *SystemInstaller) invoker() (invoker, error) {
	in := invoker{Name: s.User, UID: s.UID, GID: s.GID, Home: s.Home}
	if in.Name == "" {
		in.Name = os.Getenv("SUDO_USER")
		if in.UID == "" {
			in.UID = os.Getenv("SUDO_UID")
		}
		if in.GID == "" {
			in.GID = os.Getenv("SUDO_GID")
		}
	}
	if in.Name == "" || in.Name == "root" {
		in.Name = ""
	}
	if in.Name != "" && (in.Home == "" || in.UID == "") {
		u, err := user.Lookup(in.Name)
		if err != nil {
			return invoker{}, fmt.Errorf("look up the user %q that ran sudo: %w", in.Name, err)
		}
		if in.Home == "" {
			in.Home = u.HomeDir
		}
		if in.UID == "" {
			in.UID = u.Uid
		}
		if in.GID == "" {
			in.GID = u.Gid
		}
	}
	if in.Home == "" {
		dir, err := os.UserHomeDir()
		if err != nil {
			return invoker{}, fmt.Errorf("locate the home directory: %w", err)
		}
		in.Home = dir
	}
	if in.UID == "" {
		in.UID = strconv.Itoa(os.Getuid())
	}
	return in, nil
}

func (s *SystemInstaller) gid(in invoker) (string, error) {
	if in.GID != "" {
		return in.GID, nil
	}
	if in.Name == "" {
		return strconv.Itoa(os.Getgid()), nil
	}
	u, err := user.Lookup(in.Name)
	if err != nil {
		return "", fmt.Errorf("look up the group of the user %q that ran sudo: %w", in.Name, err)
	}
	return u.Gid, nil
}

func (s *SystemInstaller) own(ctx context.Context, in invoker, paths ...string) error {
	if in.Name == "" {
		return nil
	}
	gid, err := s.gid(in)
	if err != nil {
		return err
	}
	argv := append([]string{in.UID + ":" + gid}, paths...)
	if err := s.run(ctx, "chown", argv...); err != nil {
		return fmt.Errorf("give %s back to %s: %w", strings.Join(paths, " "), in.Name, err)
	}
	return nil
}

func (s *SystemInstaller) home() (string, error) {
	in, err := s.invoker()
	if err != nil {
		return "", err
	}
	return in.Home, nil
}

func (s *SystemInstaller) asUser(ctx context.Context, name string, args ...string) error {
	in, err := s.invoker()
	if err != nil {
		return err
	}
	if in.Name == "" {
		return s.run(ctx, name, args...)
	}

	argv := append([]string{"-u", in.Name, "--", "env",
		"HOME=" + in.Home,
		"XDG_RUNTIME_DIR=/run/user/" + in.UID, name}, args...)
	return s.run(ctx, "runuser", argv...)
}

func (s *SystemInstaller) path(p string) string {
	if s.Root == "" {
		return p
	}
	return filepath.Join(s.Root, strings.TrimPrefix(p, string(filepath.Separator)))
}

func (s *SystemInstaller) binary(opts InstallOptions) (string, error) {
	bin := opts.Binary
	if bin == "" {
		bin = s.Binary
	}
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("locate this binary: %w", err)
		}
		bin = exe
	}
	if !filepath.IsAbs(bin) {
		abs, err := filepath.Abs(bin)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", bin, err)
		}
		bin = abs
	}
	return bin, nil
}

func (s *SystemInstaller) run(ctx context.Context, name string, args ...string) error {
	if s.Run != nil {
		return s.Run(ctx, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return nil
}

func (s *SystemInstaller) unitPath() (string, error) {
	home, err := s.home()
	if err != nil {
		return "", err
	}
	switch s.goos() {
	case "linux":
		return s.path(SystemdUnitPath(home)), nil
	case "darwin":
		return s.path(LaunchdPlistPath(home)), nil
	}
	return "", ErrUnsupported
}

func (s *SystemInstaller) Installed(ctx context.Context) (bool, error) {
	path, err := s.unitPath()
	if errors.Is(err, ErrUnsupported) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); err != nil {
		return false, nil
	}
	return true, nil
}

func (s *SystemInstaller) Install(ctx context.Context, opts InstallOptions) error {
	if strings.TrimSpace(opts.Machine) == "" {
		opts.Machine = s.Machine
	}
	if strings.TrimSpace(opts.Machine) == "" {
		return errors.New("no machine: 'vpn install' needs to know whose network the service carries")
	}
	if opts.Interface == "" {
		opts.Interface = DefaultInterface
	}
	bin, err := s.binary(opts)
	if err != nil {
		return err
	}
	switch s.goos() {
	case "linux":
		return s.installLinux(ctx, opts, bin)
	case "darwin":
		return s.installDarwin(ctx, opts, bin)
	}
	return ErrUnsupported
}

func (s *SystemInstaller) installLinux(ctx context.Context, opts InstallOptions, bin string) error {
	home, err := s.home()
	if err != nil {
		return err
	}
	in, err := s.invoker()
	if err != nil {
		return err
	}

	if s.goos() == runtime.GOOS {
		if point := nosuidMount(PrivilegedBinary); point != "" {
			return fmt.Errorf("%s is on %s, which is mounted nosuid: a file capability there is "+
				"silently ignored and the tunnel could never create its interface. "+
				"Transparent mode needs a filesystem that honours capabilities", PrivilegedBinary, point)
		}
	}

	gid, err := s.gid(in)
	if err != nil {
		return err
	}
	priv := s.path(PrivilegedBinary)
	if bin != PrivilegedBinary {
		if err := s.run(ctx, "install", "-D", "-m", PrivilegedMode, "-o", "root", "-g", gid, bin, priv); err != nil {
			return fmt.Errorf("install %s as %s: %w", bin, PrivilegedBinary, err)
		}
	}

	if err := s.run(ctx, "setcap", "cap_net_admin+eip", priv); err != nil {
		return fmt.Errorf("grant %s permission to create a tunnel interface "+
			"(this is what 'vpn install' needs root for): %w", PrivilegedBinary, err)
	}

	unit := s.path(SystemdUnitPath(home))
	if err := writeFile(unit, SystemdUnit(PrivilegedBinary, opts.Machine, opts.Interface), 0o644); err != nil {
		return err
	}

	if err := s.own(ctx, in, filepath.Dir(filepath.Dir(unit)), filepath.Dir(unit), unit); err != nil {
		return err
	}

	if in.Name != "" {
		if err := writeFile(s.path(PolkitRuleFile), PolkitRule(in.Name), 0o644); err != nil {
			return err
		}
	}

	if in.Name != "" {

		_ = s.run(ctx, "loginctl", "enable-linger", in.Name)
	}

	if err := s.asUser(ctx, "systemctl", "--user", "daemon-reload"); err != nil {

		_ = removeFile(unit)
		return err
	}
	if err := s.asUser(ctx, "systemctl", "--user", "enable", "--now", UnitName); err != nil {

		_ = removeFile(unit)
		return err
	}
	return nil
}

func (s *SystemInstaller) installDarwin(ctx context.Context, opts InstallOptions, bin string) error {
	home, err := s.home()
	if err != nil {
		return err
	}
	plist := s.path(LaunchdPlistPath(home))
	if err := writeFile(plist, LaunchdPlist(bin, opts.Machine, opts.Interface), 0o644); err != nil {
		return err
	}
	in0, err := s.invoker()
	if err != nil {
		return err
	}

	if err := s.own(ctx, in0, filepath.Dir(plist), plist); err != nil {
		return err
	}

	resolver := opts.resolver()
	if err := writeFile(s.path(ResolverFile), ResolverEntry(resolver), 0o644); err != nil {
		return err
	}
	in, err := s.invoker()
	if err != nil {
		return err
	}

	_ = s.run(ctx, "launchctl", "bootout", "gui/"+in.UID, plist)
	return s.run(ctx, "launchctl", "bootstrap", "gui/"+in.UID, plist)
}

type Starter interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

var _ Starter = (*SystemInstaller)(nil)

func (s *SystemInstaller) Start(ctx context.Context) error {
	switch s.goos() {
	case "linux":
		return s.asUser(ctx, "systemctl", "--user", "start", UnitName)
	case "darwin":
		in, err := s.invoker()
		if err != nil {
			return err
		}
		return s.run(ctx, "launchctl", "kickstart", "gui/"+in.UID+"/"+LaunchdLabel)
	}
	return ErrUnsupported
}

func (s *SystemInstaller) Stop(ctx context.Context) error {
	switch s.goos() {
	case "linux":
		return s.asUser(ctx, "systemctl", "--user", "stop", UnitName)
	case "darwin":
		in, err := s.invoker()
		if err != nil {
			return err
		}
		return s.run(ctx, "launchctl", "bootout", "gui/"+in.UID+"/"+LaunchdLabel)
	}
	return ErrUnsupported
}

func (s *SystemInstaller) Uninstall(ctx context.Context) error {
	home, err := s.home()
	if err != nil {
		return err
	}
	switch s.goos() {
	case "linux":
		_ = s.asUser(ctx, "systemctl", "--user", "disable", "--now", UnitName)
		if err := removeFile(s.path(SystemdUnitPath(home))); err != nil {
			return err
		}

		if err := removeFile(s.path(PrivilegedBinary)); err != nil {
			return err
		}
		if err := removeFile(s.path(PolkitRuleFile)); err != nil {
			return err
		}
		return s.asUser(ctx, "systemctl", "--user", "daemon-reload")
	case "darwin":
		in, err := s.invoker()
		if err != nil {
			return err
		}
		plist := s.path(LaunchdPlistPath(home))
		_ = s.run(ctx, "launchctl", "bootout", "gui/"+in.UID, plist)
		if err := removeFile(plist); err != nil {
			return err
		}
		return removeFile(s.path(ResolverFile))
	}
	return ErrUnsupported
}

func (o InstallOptions) resolver() string {
	rec, err := (&FileRecordStore{}).Load(o.Machine)
	if err != nil {
		return ""
	}
	return rec.Resolver()
}

func writeFile(path, content string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
