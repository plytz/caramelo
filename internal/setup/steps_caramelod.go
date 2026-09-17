package setup

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/plytz/caramelo/internal/firewall"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
)

const UserUnit = "caramelod.service"

var startTimeout = 30 * time.Second

var getenv = os.Getenv

func UserUnitDir(stateDir string) string {
	return filepath.Join(stateDir, ".config", "systemd", "user")
}

func UserUnitPath(stateDir string) string { return filepath.Join(UserUnitDir(stateDir), UserUnit) }

type CaramelodStep struct{}

func NewCaramelodStep() *CaramelodStep { return &CaramelodStep{} }

func (s *CaramelodStep) Name() string { return "caramelod" }

func (s *CaramelodStep) Check(ctx context.Context, env *Env) (bool, string, error) {
	cfg := env.Config
	var problems []string

	same, err := s.binaryUpToDate(ctx, env)
	if err != nil {
		return false, "", err
	}
	if !same {
		problems = append(problems, serverconfig.BinaryPath+" differs")
	}

	unit := s.unitSpec(env)
	unitOK, detail, err := unit.check(ctx, env)
	if err != nil {
		return false, "", err
	}
	if !unitOK {
		problems = append(problems, detail)
	}

	keysOK, keysDetail, err := s.keysSeeded(ctx, env)
	if err != nil {
		return false, "", err
	}
	if !keysOK {
		problems = append(problems, keysDetail)
	}

	userThere, err := userExists(ctx, env, cfg.User)
	if err != nil {
		return false, "", err
	}
	if !userThere {
		problems = append(problems, "user "+cfg.User+" does not exist yet")
	} else {
		active, err := s.unitActive(ctx, env)
		if err != nil {
			return false, "", err
		}
		if !active {
			problems = append(problems, UserUnit+" not active")
		}
	}

	sock, err := statPath(ctx, env, cfg.SocketPath())
	if err != nil {
		return false, "", err
	}
	if !sock.Exists {
		problems = append(problems, cfg.SocketPath()+" missing")
	}

	if env.ConfigChanged {
		problems = append(problems, serverconfig.Path(env.ConfigDir)+" changed since caramelod started")
	}

	if len(problems) > 0 {
		return false, strings.Join(problems, "; "), nil
	}
	where := fmt.Sprintf("%s:%d", cfg.Bind, cfg.SSHPort)
	if !cfg.APIListensPublic() {
		where = fmt.Sprintf("udp %s (the API answers inside the tunnel)", cfg.VPNListen)
	}
	return true, fmt.Sprintf("caramelod active, %s, listening on %s", cfg.SocketPath(), where), nil
}

func (s *CaramelodStep) Apply(ctx context.Context, env *Env) error {
	cfg := env.Config

	binaryChanged, err := s.installBinary(ctx, env)
	if err != nil {
		return err
	}
	if err := s.seedKeys(ctx, env); err != nil {
		return err
	}

	unit := s.unitSpec(env)
	unitOK, _, err := unit.check(ctx, env)
	if err != nil {
		return err
	}
	if !unitOK {
		if err := (dirSpec{Path: UserUnitDir(cfg.StateDir), Mode: "0755", AsUser: cfg.User}).apply(ctx, env); err != nil {
			return err
		}
		if err := unit.apply(ctx, env); err != nil {
			return fmt.Errorf("write %s: %w", unit.Path, err)
		}
	}

	wasActive, err := s.unitActive(ctx, env)
	if err != nil {
		return err
	}
	if _, err := mustRun(ctx, env, s.userctl(env, "daemon-reload")); err != nil {
		return err
	}
	if _, err := mustRun(ctx, env, s.userctl(env, "enable", "--now", UserUnit)); err != nil {
		return fmt.Errorf("start caramelod: %w", err)
	}
	if wasActive && (binaryChanged || !unitOK || env.ConfigChanged) {
		if _, err := mustRun(ctx, env, s.userctl(env, "restart", UserUnit)); err != nil {
			return fmt.Errorf("restart caramelod: %w", err)
		}
	}

	if err := s.waitListening(ctx, env); err != nil {
		return err
	}
	if err := s.smokeTest(ctx, env); err != nil {
		return err
	}

	env.ConfigChanged = false
	return nil
}

func (s *CaramelodStep) userctl(env *Env, args ...string) runner.Cmd {
	return runner.Cmd{Name: "systemctl", Args: append([]string{"--user"}, args...), User: env.Config.User}
}

func (s *CaramelodStep) unitActive(ctx context.Context, env *Env) (bool, error) {
	res, err := runCmd(ctx, env, s.userctl(env, "is-active", "--quiet", UserUnit))
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

func (s *CaramelodStep) binaryUpToDate(ctx context.Context, env *Env) (bool, error) {
	if env.BinaryPath == "" {
		return false, fmt.Errorf("no binary to install: BinaryPath is empty")
	}
	if env.BinaryPath == serverconfig.BinaryPath {
		return true, nil
	}
	installed, err := fileSHA256(ctx, env, serverconfig.BinaryPath)
	if err != nil {
		return false, err
	}
	if installed == "" {
		return false, nil
	}
	src, err := fileSHA256(ctx, env, env.BinaryPath)
	if err != nil {
		return false, err
	}
	if src == "" {
		return false, fmt.Errorf("cannot read %s", env.BinaryPath)
	}
	return src == installed, nil
}

func (s *CaramelodStep) installBinary(ctx context.Context, env *Env) (bool, error) {
	same, err := s.binaryUpToDate(ctx, env)
	if err != nil || same {
		return false, err
	}
	if _, err := mustRun(ctx, env, runner.Cmd{
		Name: "install", Args: []string{"-m", "0755", "-o", "root", "-g", "root", env.BinaryPath, serverconfig.BinaryPath},
	}); err != nil {
		return false, fmt.Errorf("install %s: %w", serverconfig.BinaryPath, err)
	}
	logf(env, "installed %s", serverconfig.BinaryPath)
	return true, nil
}

func fileSHA256(ctx context.Context, env *Env, path string) (string, error) {
	res, err := runCmd(ctx, env, runner.Cmd{Name: "sha256sum", Args: []string{"--", path}})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", nil
	}
	fields := strings.Fields(res.Stdout)
	if len(fields) == 0 {
		return "", fmt.Errorf("sha256sum %s: unexpected output %q", path, res.Stdout)
	}
	return fields[0], nil
}

func (s *CaramelodStep) unitSpec(env *Env) fileSpec {
	return fileSpec{
		Path:    UserUnitPath(env.Config.StateDir),
		Content: unitContent(env),
		Mode:    "0644",
		Owner:   env.Config.User,
		Group:   env.Config.Group,
		AsUser:  env.Config.User,
	}
}

func unitContent(env *Env) string {
	execStart := serverconfig.BinaryPath + " server run"
	if env.ConfigDir != "" && env.ConfigDir != serverconfig.DefaultConfigDir {
		execStart += " --config-dir " + env.ConfigDir
	}
	return "# Installed by caramelo server setup.\n" +
		"[Unit]\n" +
		"Description=Caramelo control plane\n" +
		"Documentation=https://github.com/plytz/caramelo\n" +
		"After=docker.service\n" +
		"Wants=docker.service\n" +
		"\n" +
		"[Service]\n" +
		"ExecStart=" + execStart + "\n" +
		"Restart=always\n" +
		"RestartSec=2\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=default.target\n"
}

func (s *CaramelodStep) seedKeys(ctx context.Context, env *Env) error {
	cfg := env.Config
	source, lines, err := s.sourceKeys(ctx, env)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		logf(env, "warning: no SSH key to seed (%s); add one with 'caramelo key add --name NAME < key.pub'", source)
		return nil
	}

	existing, _, err := readFile(ctx, env, cfg.AuthorizedKeysPath())
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, l := range strings.Split(existing, "\n") {
		if blob, _, _, err := parseKeyLine(l); err == nil {
			have[blob] = true
		}
	}

	content := existing
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	seeded := 0
	for _, line := range lines {
		blob, comment, keyType, err := parseKeyLine(line)
		if err != nil {
			continue
		}
		if have[blob] {
			continue
		}
		have[blob] = true
		if comment == "" {
			comment = s.defaultKeyName(ctx, env)
			line = strings.TrimRight(line, " \t") + " " + comment
		}
		content += strings.TrimRight(line, "\n") + "\n"
		seeded++
		logf(env, "seeded key %s %s… (%s) from %s", keyType, blob[:min(20, len(blob))], comment, source)
	}
	if seeded == 0 {
		return nil
	}
	spec := fileSpec{
		Path: cfg.AuthorizedKeysPath(), Content: content, Mode: "0600",
		Owner: cfg.User, Group: cfg.Group, AsUser: cfg.User,
	}
	return spec.apply(ctx, env)
}

func (s *CaramelodStep) keysSeeded(ctx context.Context, env *Env) (bool, string, error) {
	_, lines, err := s.sourceKeys(ctx, env)
	if err != nil {
		return false, "", err
	}
	if len(lines) == 0 {
		return true, "", nil
	}
	existing, _, err := readFile(ctx, env, env.Config.AuthorizedKeysPath())
	if err != nil {
		return false, "", err
	}
	have := map[string]bool{}
	for _, l := range strings.Split(existing, "\n") {
		if blob, _, _, err := parseKeyLine(l); err == nil {
			have[blob] = true
		}
	}
	missing := 0
	for _, line := range lines {
		blob, _, _, err := parseKeyLine(line)
		if err != nil {
			continue
		}
		if !have[blob] {
			missing++
		}
	}
	if missing > 0 {
		return false, fmt.Sprintf("%d key(s) not authorized yet", missing), nil
	}
	return true, "", nil
}

func (s *CaramelodStep) sourceKeys(ctx context.Context, env *Env) (source string, lines []string, err error) {
	if f := env.Opts.AuthorizedKeysFile; f != "" {
		content, exists, err := readFile(ctx, env, f)
		if err != nil {
			return f, nil, err
		}
		if !exists {
			return f, nil, fmt.Errorf("authorized keys file %s not found", f)
		}
		return f, keyLines(content), nil
	}

	sudoUser := getenv("SUDO_USER")
	if sudoUser == "" {
		return "no SUDO_USER", nil, nil
	}
	home, err := homeOf(ctx, env, sudoUser)
	if err != nil || home == "" {
		return "user " + sudoUser, nil, nil
	}
	return s.keysOfHome(ctx, env, home)
}

func (s *CaramelodStep) keysOfHome(ctx context.Context, env *Env, home string) (string, []string, error) {
	ak := filepath.Join(home, ".ssh", "authorized_keys")
	content, exists, err := readFile(ctx, env, ak)
	if err != nil {
		return ak, nil, err
	}
	if exists && len(keyLines(content)) > 0 {
		return ak, keyLines(content), nil
	}
	dir := filepath.Join(home, ".ssh")
	res, err := runCmd(ctx, env, runner.Cmd{Name: "find", Args: []string{dir, "-maxdepth", "1", "-type", "f", "-name", "*.pub"}})
	if err != nil {
		return dir, nil, err
	}
	var lines []string
	for _, path := range strings.Fields(res.Stdout) {
		content, exists, err := readFile(ctx, env, path)
		if err != nil {
			return dir, nil, err
		}
		if exists {
			lines = append(lines, keyLines(content)...)
		}
	}
	return dir + "/*.pub", lines, nil
}

func (s *CaramelodStep) defaultKeyName(ctx context.Context, env *Env) string {
	user := getenv("SUDO_USER")
	if user == "" {
		user = "root"
	}
	host, err := mustRun(ctx, env, runner.Cmd{Name: "hostname"})
	if err != nil || strings.TrimSpace(host) == "" {
		return user
	}
	return user + "@" + strings.TrimSpace(host)
}

func keyLines(content string) []string {
	var out []string
	for _, l := range strings.Split(content, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, l)
	}
	return out
}

func parseKeyLine(line string) (blob, comment, keyType string, err error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", "", fmt.Errorf("not a key")
	}
	key, comment, _, _, err := gossh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return "", "", "", err
	}
	return base64.StdEncoding.EncodeToString(key.Marshal()), comment, key.Type(), nil
}

func userExists(ctx context.Context, env *Env, name string) (bool, error) {
	res, err := runCmd(ctx, env, runner.Cmd{Name: "getent", Args: []string{"passwd", name}})
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

func homeOf(ctx context.Context, env *Env, user string) (string, error) {
	res, err := runCmd(ctx, env, runner.Cmd{Name: "getent", Args: []string{"passwd", user}})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("unknown user %q", user)
	}
	fields := strings.Split(strings.TrimSpace(res.Stdout), ":")
	if len(fields) < 6 {
		return "", fmt.Errorf("unexpected passwd entry for %q", user)
	}
	return fields[5], nil
}

func (s *CaramelodStep) waitListening(ctx context.Context, env *Env) error {
	cfg := env.Config
	if err := waitPath(ctx, env, cfg.SocketPath(), startTimeout); err != nil {
		return fmt.Errorf("caramelod did not create %s: %w (see: journalctl _UID=$(id -u %s) -u %s)",
			cfg.SocketPath(), err, cfg.User, UserUnit)
	}
	if cfg.APIListensPublic() {
		return waitFor(ctx, startTimeout, fmt.Sprintf("port %d", cfg.SSHPort), func() (bool, error) {
			return portListening(ctx, env, cfg.SSHPort)
		})
	}
	port := strconv.Itoa(firewall.VPNPort(cfg.VPNListen))
	return waitFor(ctx, startTimeout, "udp "+port, func() (bool, error) {
		return udpListening(ctx, env, port)
	})
}

func udpListening(ctx context.Context, env *Env, port string) (bool, error) {
	res, err := runCmd(ctx, env, runner.Cmd{Name: "ss", Args: []string{"-lun"}})
	if err != nil {
		return false, err
	}
	if res.ExitCode != 0 {
		return false, fmt.Errorf("ss -lun: exit %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	return hasUDPListener(res.Stdout, port), nil
}

func hasUDPListener(out string, port string) bool {
	want := ":" + port
	for _, line := range strings.Split(out, "\n") {
		for _, f := range strings.Fields(line) {
			if strings.HasSuffix(f, want) {
				return true
			}
		}
	}
	return false
}

func portListening(ctx context.Context, env *Env, port int) (bool, error) {
	res, err := runCmd(ctx, env, runner.Cmd{Name: "ss", Args: []string{"-ltn"}})
	if err != nil {
		return false, err
	}
	if res.ExitCode != 0 {
		return false, fmt.Errorf("ss -ltn: exit %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	return hasListener(res.Stdout, port), nil
}

func hasListener(out string, port int) bool {
	want := fmt.Sprintf(":%d", port)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if strings.HasSuffix(fields[3], want) {
			return true
		}
	}
	return false
}

func (s *CaramelodStep) smokeTest(ctx context.Context, env *Env) error {
	res, err := runCmd(ctx, env, runner.Cmd{
		Name: serverconfig.BinaryPath, Args: []string{"status", "--json"}, User: env.Config.User,
	})
	if err != nil {
		return fmt.Errorf("smoke test: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("smoke test '%s status --json' failed: exit %d: %s",
			serverconfig.BinaryPath, res.ExitCode, firstLine(res.Stderr, res.Stdout))
	}
	return nil
}
