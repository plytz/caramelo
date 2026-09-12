package dockersetup

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/setup"
)

const StepPackages = "docker-packages"

const keyTimeout = 60 * time.Second

type packagesStep struct {
	readFile     func(path string) ([]byte, error)
	writeFile    func(path string, data []byte, mode os.FileMode) error
	mkdirAll     func(path string, mode os.FileMode) error
	exists       func(path string) bool
	fetch        func(ctx context.Context, url string) ([]byte, error)
	osReleaseSrc string
	keyringPath  string
	sourcesPath  string
}

func Packages() setup.Step {
	return &packagesStep{
		readFile:     os.ReadFile,
		writeFile:    os.WriteFile,
		mkdirAll:     os.MkdirAll,
		exists:       fileExists,
		fetch:        fetchURL,
		osReleaseSrc: OSReleaseSrc,
		keyringPath:  KeyringPath,
		sourcesPath:  SourcesPath,
	}
}

func (s *packagesStep) Name() string { return StepPackages }

func (s *packagesStep) Check(ctx context.Context, env *setup.Env) (bool, string, error) {
	if !env.Opts.InstallPackages {
		return false, "package installation disabled", setup.Skip{Reason: "--no-install-packages"}
	}
	var missing []string
	var dockerVer string
	for _, pkg := range PackageNames {
		res, err := env.Run.Run(ctx, runner.Cmd{
			Name: "dpkg-query", Args: []string{"-W", "-f", "${Status} ${Version}", pkg},
		})
		if err != nil {
			return false, "", fmt.Errorf("query package %s: %w", pkg, err)
		}
		st := parseDpkgStatus(pkg, res.Stdout)
		if !st.Installed {
			missing = append(missing, pkg)
			continue
		}
		if pkg == "docker-ce" {
			dockerVer = upstreamVersion(st.Version)
		}
	}
	if len(missing) > 0 {
		return false, "missing: " + strings.Join(missing, " "), nil
	}
	running, err := s.rootfulUnitsUp(ctx, env)
	if err != nil {
		return false, "", err
	}
	if len(running) > 0 {
		return false, fmt.Sprintf("docker-ce %s, rootful units still on: %s",
			dockerVer, strings.Join(running, " ")), nil
	}
	return true, "docker-ce " + dockerVer, nil
}

func (s *packagesStep) rootfulUnitsUp(ctx context.Context, env *setup.Env) ([]string, error) {
	var up []string
	for _, unit := range rootfulUnits {
		state, err := systemctlState(ctx, env.Run, runner.Cmd{
			Name: "systemctl", Args: []string{"is-enabled", unit},
		})
		if err != nil {
			return nil, err
		}
		active, err := systemctlState(ctx, env.Run, runner.Cmd{
			Name: "systemctl", Args: []string{"is-active", unit},
		})
		if err != nil {
			return nil, err
		}
		switch {
		case state == "enabled" || state == "enabled-runtime":
			up = append(up, unit+" (enabled)")
		case active == "active" || active == "activating":
			up = append(up, unit+" (active)")
		}
	}
	return up, nil
}

func systemctlState(ctx context.Context, r runner.Runner, c runner.Cmd) (string, error) {
	res, err := r.Run(ctx, c)
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", c.Name, strings.Join(c.Args, " "), err)
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" {

		return "not-found", nil
	}
	return strings.TrimSpace(strings.Split(out, "\n")[0]), nil
}

func (s *packagesStep) Apply(ctx context.Context, env *setup.Env) error {
	osr, err := s.osRelease()
	if err != nil {
		return err
	}
	arch, err := s.arch(ctx, env)
	if err != nil {
		return err
	}
	repoConfigured := s.exists(s.sourcesPath)
	if !repoConfigured {

		logf(env.Log, "updating apt package lists...")
		if err := s.apt(ctx, env, "update"); err != nil {
			return err
		}
	}
	logf(env.Log, "installing apt prerequisites (%s)...", strings.Join(prereqPackages, " "))
	if err := s.aptInstall(ctx, env, prereqPackages); err != nil {
		return err
	}
	if err := s.installKeyring(ctx, env, osr); err != nil {
		return err
	}
	line := sourcesLine(osr, arch)
	if err := s.writeFile(s.sourcesPath, []byte(line), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", s.sourcesPath, err)
	}
	logf(env.Log, "docker repository: %s", strings.TrimSpace(line))
	if err := s.apt(ctx, env, "update"); err != nil {
		return err
	}
	logf(env.Log, "downloading docker packages (~110 MB)...")
	if err := s.aptInstall(ctx, env, PackageNames); err != nil {
		return err
	}

	if err := s.apt(ctx, env, "clean"); err != nil {
		return err
	}
	return s.disableRootful(ctx, env)
}

func (s *packagesStep) osRelease() (osRelease, error) {
	b, err := s.readFile(s.osReleaseSrc)
	if err != nil {
		return osRelease{}, fmt.Errorf("read %s: %w", s.osReleaseSrc, err)
	}
	osr := parseOSRelease(string(b))
	if err := osr.validate(); err != nil {
		return osRelease{}, err
	}
	return osr, nil
}

func (s *packagesStep) arch(ctx context.Context, env *setup.Env) (string, error) {
	res, err := run(ctx, env.Run, runner.Cmd{Name: "dpkg", Args: []string{"--print-architecture"}})
	if err != nil {
		return "", err
	}
	arch := strings.TrimSpace(res.Stdout)
	if arch == "" {
		return "", fmt.Errorf("dpkg --print-architecture printed nothing")
	}
	return arch, nil
}

func (s *packagesStep) installKeyring(ctx context.Context, env *setup.Env, osr osRelease) error {
	if err := s.mkdirAll(filepath.Dir(s.keyringPath), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(s.keyringPath), err)
	}
	logf(env.Log, "fetching %s", osr.keyURL())
	key, err := s.fetch(ctx, osr.keyURL())
	if err != nil {
		return err
	}
	if !strings.Contains(string(key), "BEGIN PGP PUBLIC KEY BLOCK") {
		return fmt.Errorf("%s did not return a PGP public key", osr.keyURL())
	}
	if err := s.writeFile(s.keyringPath, key, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", s.keyringPath, err)
	}
	return nil
}

func (s *packagesStep) apt(ctx context.Context, env *setup.Env, sub string) error {
	args := append(append([]string{}, aptLockWait...), sub)
	_, err := run(ctx, env.Run, runner.Cmd{Name: "apt-get", Args: args, Env: aptEnv})
	return err
}

func (s *packagesStep) aptInstall(ctx context.Context, env *setup.Env, pkgs []string) error {
	args := append(append([]string{}, aptLockWait...), "install", "-y", "--no-install-recommends")
	args = append(args, pkgs...)
	_, err := run(ctx, env.Run, runner.Cmd{Name: "apt-get", Args: args, Env: aptEnv})
	return err
}

func (s *packagesStep) disableRootful(ctx context.Context, env *setup.Env) error {
	logf(env.Log, "disabling the rootful docker daemon (%s)", strings.Join(rootfulUnits, " "))
	args := append([]string{"disable", "--now"}, rootfulUnits...)
	res, err := env.Run.Run(ctx, runner.Cmd{Name: "systemctl", Args: args})
	if err != nil {
		return fmt.Errorf("systemctl disable: %w", err)
	}
	if res.ExitCode == 0 {
		return nil
	}

	out := res.Stderr + res.Stdout
	low := strings.ToLower(out)
	if strings.Contains(low, "not loaded") || strings.Contains(low, "does not exist") ||
		strings.Contains(low, "no such file") {
		return nil
	}
	return fmt.Errorf("systemctl disable --now %s: exit %d: %s",
		strings.Join(rootfulUnits, " "), res.ExitCode, firstLine(res.Stderr, res.Stdout))
}

func fetchURL(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, keyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	return b, nil
}
