package dockersetup

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"

	"github.com/plytz/caramelo/internal/runner"
)

var PackageNames = []string{
	"docker-ce",
	"docker-ce-cli",
	"containerd.io",
	"docker-buildx-plugin",
	"docker-compose-plugin",
	"docker-ce-rootless-extras",
	"uidmap",
	"dbus-user-session",
	"slirp4netns",
	"git",
}

var prereqPackages = []string{"ca-certificates", "curl", "gnupg"}

var rootfulUnits = []string{"docker.service", "docker.socket", "containerd.service"}

const (
	KeyringDir   = "/etc/apt/keyrings"
	KeyringPath  = KeyringDir + "/docker.asc"
	SourcesPath  = "/etc/apt/sources.list.d/docker.list"
	OSReleaseSrc = "/etc/os-release"

	DownloadBase = "https://download.docker.com/linux/"
)

var aptEnv = []string{"DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=a"}

var aptLockWait = []string{"-o", "DPkg::Lock::Timeout=120"}

type osRelease struct {
	ID       string
	Codename string
	Version  string
}

func parseOSRelease(s string) osRelease {
	fields := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		fields[strings.TrimSpace(k)] = v
	}
	o := osRelease{ID: fields["ID"], Codename: fields["VERSION_CODENAME"], Version: fields["VERSION_ID"]}
	if o.Codename == "" {
		o.Codename = fields["UBUNTU_CODENAME"]
	}
	return o
}

func (o osRelease) validate() error {
	switch o.ID {
	case "debian", "ubuntu":
	case "":
		return fmt.Errorf("no ID in %s", OSReleaseSrc)
	default:
		return fmt.Errorf("unsupported distribution %q: Caramelo installs Docker on debian and ubuntu", o.ID)
	}
	if o.Codename == "" {
		return fmt.Errorf("no VERSION_CODENAME in %s", OSReleaseSrc)
	}
	return nil
}

func (o osRelease) keyURL() string { return DownloadBase + o.ID + "/gpg" }

func sourcesLine(o osRelease, arch string) string {
	return fmt.Sprintf("deb [arch=%s signed-by=%s] %s%s %s stable\n",
		arch, KeyringPath, DownloadBase, o.ID, o.Codename)
}

type dpkgStatus struct {
	Name      string
	Installed bool
	Version   string
}

func parseDpkgStatus(name, out string) dpkgStatus {
	fields := strings.Fields(out)
	if len(fields) < 3 || fields[0] != "install" || fields[1] != "ok" || fields[2] != "installed" {
		return dpkgStatus{Name: name}
	}
	s := dpkgStatus{Name: name, Installed: true}
	if len(fields) > 3 {
		s.Version = fields[3]
	}
	return s
}

func upstreamVersion(v string) string {
	if _, rest, ok := strings.Cut(v, ":"); ok {
		v = rest
	}
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v = v[:i]
	}
	return v
}

func run(ctx context.Context, r runner.Runner, c runner.Cmd) (runner.Result, error) {
	res, err := r.Run(ctx, c)
	if err != nil {
		return res, fmt.Errorf("%s: %w", c.Name, err)
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("%s %s: exit %d: %s", c.Name, strings.Join(c.Args, " "),
			res.ExitCode, firstLine(res.Stderr, res.Stdout))
	}
	return res, nil
}

func firstLine(ss ...string) string {
	for _, s := range ss {
		for _, line := range strings.Split(s, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				return line
			}
		}
	}
	return ""
}

func lookupUID(name string) (string, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return "", fmt.Errorf("lookup user %q: %w", name, err)
	}
	return u.Uid, nil
}

func lookupHome(name string) (string, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return "", fmt.Errorf("lookup user %q: %w", name, err)
	}
	return u.HomeDir, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, format+"\n", args...)
}
