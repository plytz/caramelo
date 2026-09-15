//go:build integration

package itest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const CommanderTimeout = 2 * time.Minute

const commanderExitUnreachable = 255

func (m *Machine) CommanderMachine(t testing.TB) string {
	t.Helper()
	return m.HostAddr(t, CarameloSSHPort)
}

func (m *Machine) CommanderTarget(t testing.TB) string {
	t.Helper()
	return CarameloUser + "@" + m.CommanderMachine(t)
}

func (m *Machine) BootstrapTarget(t testing.TB) string {
	t.Helper()
	return m.User() + "@" + m.HostAddr(t, 22)
}

func (m *Machine) SSHAlias() string { return m.Hostname }

func (m *Machine) BootstrapAlias() string { return m.Alias + bootstrapAliasSuffix }

func (m *Machine) TunnelTarget(t testing.TB) string {
	t.Helper()
	if _, err := m.HostPortProto(VPNPort, "udp"); err != nil {
		t.Fatalf("itest: %v", err)
	}
	return CarameloUser + "@" + m.SSHAlias()
}

func CommanderHome(t testing.TB, m *Machine) string {
	t.Helper()
	home := t.TempDir()
	if err := WriteCommanderHome(home, m); err != nil {
		t.Fatalf("itest: %v", err)
	}
	return home
}

func CommanderHomeNoPeer(t testing.TB, m *Machine) string {
	t.Helper()
	home := t.TempDir()
	if err := WriteCommanderHomeNoPeer(home, m); err != nil {
		t.Fatalf("itest: %v", err)
	}
	return home
}

func WriteCommanderHome(home string, m *Machine) error {
	if err := WriteCommanderHomeNoPeer(home, m); err != nil {
		return err
	}
	if err := JoinLabPeer(home, m); err != nil {
		return fmt.Errorf("make %s a peer of %s: %w", home, m.Alias, err)
	}
	return nil
}

func WriteCommanderHomeNoPeer(home string, m *Machine) error {
	key, _, err := LabSSHKey()
	if err != nil {
		return err
	}
	if _, err := m.HostPort(CarameloSSHPort); err != nil {
		return err
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", sshDir, err)
	}
	keyBytes, err := os.ReadFile(key)
	if err != nil {
		return fmt.Errorf("read the lab key: %w", err)
	}
	keyCopy := filepath.Join(sshDir, LabKeyName)
	if err := os.WriteFile(keyCopy, keyBytes, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", keyCopy, err)
	}
	blocks, err := commanderHostBlocks(m, keyCopy)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(sshDir, "config"), []byte(blocks), 0o600)
}

func AddCommanderHost(home string, m *Machine) error {
	key, _, err := LabSSHKey()
	if err != nil {
		return err
	}
	if _, err := m.HostPort(CarameloSSHPort); err != nil {
		return err
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", sshDir, err)
	}
	keyBytes, err := os.ReadFile(key)
	if err != nil {
		return fmt.Errorf("read the lab key: %w", err)
	}
	keyCopy := filepath.Join(sshDir, LabKeyName+"_"+m.Alias)
	if err := os.WriteFile(keyCopy, keyBytes, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", keyCopy, err)
	}
	path := filepath.Join(sshDir, "config")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	blocks, err := machineHostBlocks(m, keyCopy)
	if err != nil {
		return err
	}
	if ip := m.HostIP(); !strings.Contains(string(existing), "Host "+ip+"\n") {
		blocks += sshHostBlock(ip, ip, 0, "", keyCopy)
	}
	if strings.Contains(string(existing), blocks) {
		return nil
	}
	return os.WriteFile(path, append(existing, []byte(blocks)...), 0o600)
}

const bootstrapAliasSuffix = "-boot"

func commanderHostBlocks(m *Machine, key string) (string, error) {
	blocks, err := machineHostBlocks(m, key)
	if err != nil {
		return "", err
	}
	host := m.HostIP()
	return blocks + sshHostBlock(host, host, 0, "", key), nil
}

func machineHostBlocks(m *Machine, key string) (string, error) {
	api, err := m.HostPort(CarameloSSHPort)
	if err != nil {
		return "", err
	}
	boot, err := m.HostPort(22)
	if err != nil {
		return "", err
	}
	host := m.HostIP()
	blocks := sshHostBlock(m.SSHAlias(), host, api, CarameloUser, key)
	if m.Alias != m.SSHAlias() {
		blocks += sshHostBlock(m.Alias, host, api, CarameloUser, key)
	}
	return blocks + sshHostBlock(m.BootstrapAlias(), host, boot, m.User(), key), nil
}

func sshHostBlock(alias, host string, port int, user, key string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Host %s\n  HostName %s\n", alias, host)
	if port > 0 {
		fmt.Fprintf(&b, "  Port %d\n", port)
	}
	if user != "" {
		fmt.Fprintf(&b, "  User %s\n", user)
	}
	fmt.Fprintf(&b, `  IdentityFile %s
  IdentitiesOnly yes
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
`, key)
	return b.String()
}

func CommanderEnv(home string, extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CARAMELO_") || strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	if sshCfg := filepath.Join(home, ".ssh", "config"); fileExists(sshCfg) {
		env = append(env, "CARAMELO_SSH_OPTS=-F "+sshCfg)
	}
	return append(env, extra...)
}

type CommanderOptions struct {
	Bin     string
	Dir     string
	Env     []string
	Stdin   io.Reader
	Timeout time.Duration
}

func RunCommander(ctx context.Context, o CommanderOptions, args ...string) (Result, error) {
	bin, err := commanderBinary(o)
	if err != nil {
		return Result{}, err
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = CommanderTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	killProcessGroup(cmd)
	cmd.Dir = o.Dir
	cmd.Env = o.Env
	cmd.Stdin = o.Stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	case errors.Is(runErr, exec.ErrWaitDelay):
	default:
		return res, fmt.Errorf("caramelo %s: %w", strings.Join(args, " "), runErr)
	}
	if ctx.Err() != nil {
		return res, fmt.Errorf("caramelo %s: %w after %s", strings.Join(args, " "), ctx.Err(), timeout)
	}
	if res.ExitCode == commanderExitUnreachable {
		return res, fmt.Errorf("caramelo %s: exit %d, the commander could not reach the machine: %s",
			strings.Join(args, " "), res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res, nil
}

func MustRunCommander(t testing.TB, o CommanderOptions, args ...string) Result {
	t.Helper()
	res, err := RunCommander(context.Background(), o, args...)
	if err != nil {
		t.Fatalf("%v\nstdout:\n%sstderr:\n%s", err, res.Stdout, res.Stderr)
	}
	t.Logf("[commander] caramelo %s: exit %d", strings.Join(args, " "), res.ExitCode)
	return res
}

func CommanderOK(t testing.TB, o CommanderOptions, args ...string) Result {
	t.Helper()
	res := MustRunCommander(t, o, args...)
	if res.ExitCode != 0 {
		t.Fatalf("caramelo %s: exit %d\nstdout:\n%sstderr:\n%s",
			strings.Join(args, " "), res.ExitCode, res.Stdout, res.Stderr)
	}
	return res
}

const ProcessGroupWaitDelay = 5 * time.Second

func killProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = ProcessGroupWaitDelay
}

func commanderBinary(o CommanderOptions) (string, error) {
	if o.Bin != "" {
		return o.Bin, nil
	}
	return HostBinary()
}
