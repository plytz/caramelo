//go:build integration

package fleet

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	capi "github.com/plytz/caramelo/internal/api"
	cedge "github.com/plytz/caramelo/internal/edge"
	cenv "github.com/plytz/caramelo/internal/env"
	cfleet "github.com/plytz/caramelo/internal/fleet"
	cmachine "github.com/plytz/caramelo/internal/machine"
	cprogress "github.com/plytz/caramelo/internal/progress"
	cvpn "github.com/plytz/caramelo/internal/vpn"
	"github.com/plytz/caramelo/test/integration/itest"
)

const (
	appName    = "sampleapp"
	branch     = "fleet"
	dbDep      = "db"
	webService = "web"
	domain     = "shop.test"
)

const (
	edgeControlSocket = "/run/caramelo/edge.sock"
	vaultKeyPath      = "/var/lib/caramelo/vault.key"
)

const fleetSecret = "vaulted9f2a"

const commanderTimeout = 12 * time.Minute

func begin(t *testing.T) *itest.Machine {
	t.Helper()
	if startErr != nil {
		t.Fatalf("fleet suite: %v", startErr)
	}
	if skipReason != "" {
		t.Skip("fleet suite: " + skipReason)
	}
	return hub
}

func memberBox(t *testing.T, n int) *itest.Machine {
	t.Helper()
	if n >= len(memberBoxes) {
		return nil
	}
	return memberBoxes[n]
}

func needSecondMember(t *testing.T, what string) *itest.Machine {
	t.Helper()
	m := memberBox(t, 1)
	if m == nil {
		t.Skipf("fleet suite: this lab binds one member, so %s has no second box", what)
	}
	if !joinedM2 {
		t.Skip("fleet suite: the second member never joined (an earlier case failed)")
	}
	return m
}

func needM1(t *testing.T) *itest.Machine {
	t.Helper()
	if !joinedM1 {
		t.Skip("fleet suite: the first member never joined (an earlier case failed)")
	}
	m := memberBox(t, 0)
	if m == nil {
		t.Skip("fleet suite: no member was bound")
	}
	return m
}

func needRepo(t *testing.T) {
	t.Helper()
	if repo == "" {
		t.Skip("fleet suite: the sample repository was never created (an earlier case failed)")
	}
}

func needRemoteEnv(t *testing.T) {
	t.Helper()
	needRepo(t)
	if remoteEnv == "" {
		t.Skip("fleet suite: no environment was created on a member (an earlier case failed)")
	}
}

func hubCommanderAddress() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(30*time.Second))
	defer cancel()
	ip, err := hub.Address(ctx)
	if err != nil {
		return "", err
	}
	port, err := hub.HostPort(itest.CarameloSSHPort)
	if err != nil {
		return "", err
	}
	return ip + ":" + strconv.Itoa(port), nil
}

func writeHubHome(dir string, peer bool) error {
	write := itest.WriteCommanderHomeNoPeer
	if peer {
		write = itest.WriteCommanderHome
	}
	if err := write(dir, hub); err != nil {
		return err
	}
	return addHubAddressHost(dir)
}

func addHubAddressHost(dir string) error {
	addr, err := hubCommanderAddress()
	if err != nil {
		return err
	}
	ip, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("the hub's commander address %q: %w", addr, err)
	}
	key := filepath.Join(dir, ".ssh", itest.LabKeyName)
	block := fmt.Sprintf("Host %s\n  HostName %s\n  Port %s\n  User %s\n  IdentityFile %s\n"+
		"  IdentitiesOnly yes\n  StrictHostKeyChecking no\n  UserKnownHostsFile /dev/null\n  LogLevel ERROR\n",
		ip, hub.HostIP(), port, itest.CarameloUser, key)
	path := filepath.Join(dir, ".ssh", "config")
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if strings.Contains(string(body), block) {
		return nil
	}
	return os.WriteFile(path, append(body, []byte(block)...), 0o600)
}

func reconnect(t *testing.T) {
	t.Helper()
	addr, err := hubCommanderAddress()
	if err != nil {
		t.Fatalf("the hub's commander address: %v", err)
	}
	machineAddr = addr
	if err := writeHubHome(home, false); err != nil {
		t.Fatalf("point the first commander at %s again: %v", hub.Alias, err)
	}
	if err := writeHubHome(otherHome, false); err != nil {
		t.Fatalf("point the second commander at %s again: %v", hub.Alias, err)
	}
	if otherKey != "" {
		if err := useIdentity(otherHome, otherKey); err != nil {
			t.Fatalf("give the second commander its own key again: %v", err)
		}
	}
	if err := writeHubHome(peerHome, true); err != nil {
		t.Fatalf("join the fleet's network again: %v", err)
	}
	for _, m := range memberBoxes {
		if err := itest.AddCommanderHost(home, m); err != nil {
			t.Fatalf("point the first commander at %s again: %v", m.Alias, err)
		}
	}
	t.Logf("the hub answers on %s again", machineAddr)
}

func commanderEnv(extra ...string) []string {
	return itest.GitEnv(home, append([]string{"CARAMELO_MACHINE=" + machineAddr}, extra...)...)
}

func otherCommanderEnv(extra ...string) []string {
	return itest.GitEnv(otherHome, append([]string{"CARAMELO_MACHINE=" + machineAddr}, extra...)...)
}

type commanderOpts struct {
	Dir     string
	Timeout time.Duration
	Stdin   string
	Home    string
}

func commander(t *testing.T, o commanderOpts, args ...string) itest.Result {
	t.Helper()
	timeout := o.Timeout
	if timeout == 0 {
		timeout = commanderTimeout
	}
	env := commanderEnv()
	if o.Home == otherHome && otherHome != "" {
		env = otherCommanderEnv()
	}
	opts := itest.CommanderOptions{Dir: o.Dir, Env: env, Timeout: itest.Scale(timeout)}
	if o.Stdin != "" {
		opts.Stdin = strings.NewReader(o.Stdin)
	}
	t.Logf("[commander] caramelo %s (in %s)", strings.Join(args, " "), o.Dir)
	return itest.MustRunCommander(t, opts, args...)
}

func inRepo(t *testing.T, args ...string) itest.Result {
	t.Helper()
	return commander(t, commanderOpts{Dir: repo}, args...)
}

func mustInRepo(t *testing.T, args ...string) itest.Result {
	t.Helper()
	res := inRepo(t, args...)
	if res.ExitCode != 0 {
		t.Fatalf("caramelo %s: exit %d\nstdout:\n%sstderr:\n%s",
			strings.Join(args, " "), res.ExitCode, res.Stdout, res.Stderr)
	}
	return res
}

func decode[T any](t *testing.T, what, stdout string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &v); err != nil {
		t.Fatalf("%s --json: %v\nstdout: %q", what, err, stdout)
	}
	return v
}

func decodeInto(stdout string, v any) error {
	return json.Unmarshal([]byte(strings.TrimSpace(stdout)), v)
}

func machineAdd(t *testing.T, target, name string, extra ...string) (capi.MachineAddResult, itest.Result) {
	t.Helper()
	args := append([]string{"member", "add", target, "--name", name, "--json"}, extra...)
	res := commander(t, commanderOpts{Dir: repo, Timeout: 20 * time.Minute}, args...)
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "" {
		return capi.MachineAddResult{}, res
	}
	return decode[capi.MachineAddResult](t, "member add "+name, res.Stdout), res
}

func machineToken(t *testing.T, extra ...string) (capi.MachineTokenResult, itest.Result) {
	t.Helper()
	args := append([]string{"member", "token", "--json"}, extra...)
	res := inRepo(t, args...)
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "" {
		return capi.MachineTokenResult{}, res
	}
	return decode[capi.MachineTokenResult](t, "member token", res.Stdout), res
}

func machineJoinOn(t *testing.T, m *itest.Machine, hubEndpoint, token, name string, extra ...string) itest.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(20*time.Minute))
	defer cancel()
	if err := itest.InstallBinaryOn(ctx, m); err != nil {
		t.Fatalf("install the binary on %s: %v", m.Alias, err)
	}
	argv := append([]string{itest.CarameloBinary, "member", "join", hubEndpoint,
		"--token", token, "--name", name, "--json"}, extra...)
	res, err := m.Run(ctx, "sudo -n "+strings.Join(argv, " "))
	if err != nil {
		t.Fatalf("member join on %s: %v", m.Alias, err)
	}
	return res
}

func machineList(t *testing.T) []cfleet.Machine {
	t.Helper()
	res := mustInRepo(t, "member", "list", "--json")
	return decode[[]cfleet.Machine](t, "member list", res.Stdout)
}

func machineNamed(t *testing.T, list []cfleet.Machine, name string) cfleet.Machine {
	t.Helper()
	for _, m := range list {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no machine %q in %v", name, machineSummary(list))
	return cfleet.Machine{}
}

func machineFound(list []cfleet.Machine, name string) bool {
	for _, m := range list {
		if m.Name == name {
			return true
		}
	}
	return false
}

func machineSummary(list []cfleet.Machine) []string {
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, fmt.Sprintf("%s(%s,%s,%s,%d envs)", m.Name, m.Role, m.Arch, m.Subnet, m.Envs))
	}
	return out
}

func machineShow(t *testing.T, name string) capi.MachineDetail {
	t.Helper()
	res := mustInRepo(t, "member", "show", name, "--json")
	return decode[capi.MachineDetail](t, "member show "+name, res.Stdout)
}

func hubName(t *testing.T) string {
	t.Helper()
	for _, m := range machineList(t) {
		if m.Role.IsHub() {
			return m.Name
		}
	}
	t.Fatal("no machine in the list has the hub role")
	return ""
}

type envDoc struct {
	cenv.Env
	Machine     string `json:"machine,omitempty"`
	Owner       string `json:"owner,omitempty"`
	Via         string `json:"via,omitempty"`
	Unreachable bool   `json:"unreachable,omitempty"`
}

type envDetailDoc struct {
	Env         envDoc        `json:"env"`
	Routes      []cedge.Route `json:"routes,omitempty"`
	Machine     string        `json:"machine,omitempty"`
	Unreachable bool          `json:"unreachable,omitempty"`
}

func createEnv(t *testing.T, name string, extra ...string) envDoc {
	t.Helper()
	e, res := tryCreateEnv(t, name, extra...)
	if res.ExitCode != 0 {
		t.Fatalf("env create %s: exit %d\nstdout:\n%sstderr:\n%s", name, res.ExitCode, res.Stdout, res.Stderr)
	}
	if e.Status != cenv.StatusReady {
		t.Fatalf("env create %s: status %q, want %q\nstderr:\n%s", name, e.Status, cenv.StatusReady, res.Stderr)
	}
	return e
}

func tryCreateEnv(t *testing.T, name string, extra ...string) (envDoc, itest.Result) {
	t.Helper()
	args := append([]string{"env", "create", name, "--json"}, extra...)
	res := commander(t, commanderOpts{Dir: repo}, args...)
	if strings.TrimSpace(res.Stdout) == "" {
		return envDoc{}, res
	}
	return decode[envDoc](t, "env create "+name, res.Stdout), res
}

var destroyedEnvs = map[string]bool{}

func destroyEnv(t *testing.T, name string) {
	t.Helper()
	if destroyedEnvs[name] {
		return
	}
	destroyedEnvs[name] = true
	res := inRepo(t, "env", "destroy", name, "--yes", "--json")
	if res.ExitCode != 0 {
		t.Logf("tidying %s away: exit %d\n%s%s", name, res.ExitCode, res.Stdout, res.Stderr)
	}
}

func showEnv(t *testing.T, name string) envDetailDoc {
	t.Helper()
	res := mustInRepo(t, "env", "show", name, "--json")
	return decode[envDetailDoc](t, "env show "+name, res.Stdout)
}

func listEnvs(t *testing.T, extra ...string) []envDoc {
	t.Helper()
	args := append([]string{"env", "list", "--json"}, extra...)
	res := mustInRepo(t, args...)
	return decode[[]envDoc](t, "env list "+strings.Join(extra, " "), res.Stdout)
}

func envNamed(t *testing.T, list []envDoc, name string) envDoc {
	t.Helper()
	for _, e := range list {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no environment %q in %v", name, envNames(list))
	return envDoc{}
}

func findEnv(list []envDoc, name string) (envDoc, bool) {
	for _, e := range list {
		if e.Name == name {
			return e, true
		}
	}
	return envDoc{}, false
}

func envNames(list []envDoc) []string {
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, fmt.Sprintf("%s(%s on %s)", e.Name, e.Mode, e.Machine))
	}
	return out
}

func tryDeploy(t *testing.T, name string, extra ...string) (capi.DeployResult, itest.Result) {
	t.Helper()
	args := append([]string{"deploy", name, "--json"}, extra...)
	res := commander(t, commanderOpts{Dir: repo, Timeout: 20 * time.Minute}, args...)
	t.Logf("[deploy %s] progress:\n%s", name, res.Stderr)
	if strings.TrimSpace(res.Stdout) == "" {
		return capi.DeployResult{}, res
	}
	return decode[capi.DeployResult](t, "deploy "+name, res.Stdout), res
}

func deploy(t *testing.T, name string, extra ...string) (capi.DeployResult, itest.Result) {
	t.Helper()
	out, res := tryDeploy(t, name, extra...)
	if res.ExitCode != 0 {
		t.Fatalf("deploy %s: exit %d\nstdout:\n%sstderr:\n%s", name, res.ExitCode, res.Stdout, res.Stderr)
	}
	if out.Deploy == nil {
		t.Fatalf("deploy %s answered no deploy row:\n%s", name, res.Stdout)
	}
	return out, res
}

func rollback(t *testing.T, name string, extra ...string) (capi.DeployResult, itest.Result) {
	t.Helper()
	args := append([]string{"rollback", name, "--json"}, extra...)
	res := commander(t, commanderOpts{Dir: repo, Timeout: 20 * time.Minute}, args...)
	if strings.TrimSpace(res.Stdout) == "" {
		return capi.DeployResult{}, res
	}
	return decode[capi.DeployResult](t, "rollback "+name, res.Stdout), res
}

type releaseDoc struct {
	ID      int64             `json:"id"`
	Tree    string            `json:"tree"`
	Machine string            `json:"machine,omitempty"`
	Arch    string            `json:"arch,omitempty"`
	Images  []releaseImageDoc `json:"built,omitempty"`
}

type releaseImageDoc struct {
	Service string `json:"service"`
	Arch    string `json:"arch"`
	Machine string `json:"machine"`
	ImageID string `json:"image_id,omitempty"`
}

func releasesOf(t *testing.T, extra ...string) []releaseDoc {
	t.Helper()
	args := append([]string{"releases", "--json"}, extra...)
	res := mustInRepo(t, args...)
	var doc struct {
		Releases []releaseDoc `json:"releases"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &doc); err != nil {
		t.Fatalf("releases --json: %v\nstdout: %q", err, res.Stdout)
	}
	return doc.Releases
}

func secretsSet(t *testing.T, args ...string) capi.VaultResult {
	t.Helper()
	res := mustInRepo(t, append([]string{"secrets", "set"}, append(args, "--json")...)...)
	return decode[capi.VaultResult](t, "secrets set", res.Stdout)
}

func events(t *testing.T, args ...string) []cprogress.Event {
	t.Helper()
	res := mustInRepo(t, append([]string{"events"}, append(args, "--json")...)...)
	return itest.ParseEvents(t, "events", res.Stdout)
}

func eventMatching(list []cprogress.Event, ok func(cprogress.Event) bool) (cprogress.Event, bool) {
	for _, e := range list {
		if ok(e) {
			return e, true
		}
	}
	return cprogress.Event{}, false
}

func describeEvents(list []cprogress.Event) string {
	var b strings.Builder
	for _, e := range list {
		fmt.Fprintf(&b, "  %s [%s] %s/%s %s %s %s %s\n",
			e.At.Format(time.RFC3339), e.Machine, e.App, e.Env, e.Action, e.Step, e.Status, e.Detail)
	}
	return b.String()
}

type follower struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	mu     sync.Mutex
	lines  []string
	done   chan struct{}
}

func startFollower(t *testing.T, args ...string) *follower {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	argv := append([]string{"events", "--follow", "--json"}, args...)
	cmd := exec.CommandContext(ctx, itest.BinaryPath(t), argv...)
	cmd.Dir = repo
	cmd.Env = commanderEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("events --follow: %v", err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("events --follow: %v", err)
	}
	f := &follower{cmd: cmd, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(f.done)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			f.mu.Lock()
			f.lines = append(f.lines, sc.Text())
			f.mu.Unlock()
		}
	}()
	t.Cleanup(func() { f.Stop(t) })
	return f
}

func (f *follower) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.lines)
}

func (f *follower) Stop(t *testing.T) []cprogress.Event {
	t.Helper()
	if f.cmd != nil && f.cmd.Process != nil {
		_ = syscall.Kill(-f.cmd.Process.Pid, syscall.SIGKILL)
	}
	f.cancel()
	select {
	case <-f.done:
	case <-time.After(itest.Scale(10 * time.Second)):
		t.Logf("events --follow did not close its stream; reading the %d line(s) it gave", f.Count())
	}
	f.mu.Lock()
	stream := strings.Join(f.lines, "\n")
	f.mu.Unlock()
	if stream == "" {
		return nil
	}
	return itest.ParseEvents(t, "events --follow", stream)
}

func relogin(t *testing.T, m *itest.Machine) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
	defer cancel()
	if err := m.Relogin(ctx); err != nil {
		t.Fatalf("log in to %s again after a setup gave its login user new groups: %v", m.Alias, err)
	}
}

func onBox(t *testing.T, m *itest.Machine, cmd string) itest.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
	defer cancel()
	res, err := m.Run(ctx, cmd)
	if err != nil {
		t.Fatalf("[%s] %s: %v", m.Alias, cmd, err)
	}
	return res
}

func asCaramelo(t *testing.T, m *itest.Machine, cmd string) itest.Result {
	t.Helper()
	return onBox(t, m, itest.AsUser(m, itest.CarameloUser, cmd))
}

func dockerNames(t *testing.T, m *itest.Machine, filter string, all bool) []string {
	t.Helper()
	flag := ""
	if all {
		flag = "-a "
	}
	res := asCaramelo(t, m, fmt.Sprintf("docker ps %s--filter %s --format '{{.Names}}'", flag, filter))
	if res.ExitCode != 0 {
		t.Fatalf("[%s] docker ps: exit %d: %s", m.Alias, res.ExitCode, res.Stderr)
	}
	return strings.Fields(res.Stdout)
}

func imageExists(t *testing.T, m *itest.Machine, ref string) bool {
	t.Helper()
	res := asCaramelo(t, m, "docker image inspect "+itest.ShellQuote(ref)+" >/dev/null 2>&1")
	return res.ExitCode == 0
}

func gaugeOf(t *testing.T, name string) *cmachine.Record {
	t.Helper()
	return machineShow(t, name).Gauge
}

func restartDaemon(t *testing.T, m *itest.Machine) {
	t.Helper()
	res := onBox(t, m, itest.AsUserSession(m, itest.CarameloUser, "systemctl --user restart caramelod"))
	if res.ExitCode != 0 {
		t.Fatalf("[%s] restart caramelod: exit %d: %s", m.Alias, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
	defer cancel()
	if err := itest.WaitForCaramelod(ctx, m); err != nil {
		t.Fatalf("[%s] caramelod did not come back: %v", m.Alias, err)
	}
}

var sampleDir = filepath.Join("..", "run", "testdata", "sampleapp")

func initSampleRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(tmpDir, "sampleapp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create the sample checkout: %v", err)
	}
	t.Logf("sample checkout: %s", dir)
	copyIn(t, dir, "app.py", "version.py", "health.py", "behaviour.py",
		"pgwire.py", "migrate.py", "smoke.py")
	copyAs(t, dir, "greeting.secret.py", "greeting.py")
	copyAs(t, dir, "caramelo.fleet.yaml", "caramelo.yaml")
	copyAs(t, dir, "gitignore", ".gitignore")
	itest.Git(t, dir, commanderEnv(), "init", "-b", branch)
	firstCommit = itest.GitCommitAll(t, dir, commanderEnv(), "sampleapp: run on a fleet")
	return dir
}

func copyIn(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		copyAs(t, dir, name, name)
	}
}

func copyAs(t *testing.T, dir, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(sampleDir, src))
	if err != nil {
		t.Fatalf("read the sample app: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, dst), b, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func restoreConfig(t *testing.T, name string) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join(sampleDir, name))
	if err != nil {
		t.Fatalf("read the sample app: %v", err)
	}
	have, err := os.ReadFile(filepath.Join(repo, "caramelo.yaml"))
	if err != nil {
		t.Fatalf("read the checkout's caramelo.yaml: %v", err)
	}
	if string(want) == string(have) {
		return
	}
	useConfig(t, name)
	pushBranch(t, branch+":"+branch)
}

func useConfig(t *testing.T, name string) string {
	t.Helper()
	copyAs(t, repo, name, "caramelo.yaml")
	return itest.GitCommitAll(t, repo, commanderEnv(), "sampleapp: "+name)
}

func setVersion(t *testing.T, want string) string {
	t.Helper()
	path := filepath.Join(repo, "version.py")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read version.py: %v", err)
	}
	body := string(b)
	old := fmt.Sprintf("VERSION = %q", currentVersion)
	if !strings.Contains(body, old) {
		t.Fatalf("version.py does not contain %s:\n%s", old, body)
	}
	body = strings.Replace(body, old, fmt.Sprintf("VERSION = %q", want), 1)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write version.py: %v", err)
	}
	currentVersion = want
	return itest.GitCommitAll(t, repo, commanderEnv(), "sampleapp: version "+want)
}

func hubRemote() string { return itest.GitRemoteAt(machineAddr, appName) }

func pushBranch(t *testing.T, refspec string) itest.Result {
	t.Helper()
	return itest.Git(t, repo, commanderEnv(), "push", hubRemote(), refspec)
}

func tryPushBranch(t *testing.T, refspec string) (itest.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
	defer cancel()
	return itest.GitRun(ctx, repo, commanderEnv(), "push", hubRemote(), refspec)
}

func field(body, name string) string {
	for _, line := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), name+" "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func versionOf(body string) string { return field(body, "version") }

func greetingOf(body string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
	word, _, _ := strings.Cut(strings.TrimSpace(line), " ")
	return word
}

func envHost(env string) string { return env + "." + domain }

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func stepNames(out capi.DeployResult) []string {
	if out.Deploy == nil {
		return nil
	}
	names := make([]string, 0, len(out.Deploy.Steps))
	for _, s := range out.Deploy.Steps {
		names = append(names, fmt.Sprintf("%s=%s(%s)", s.Step, s.Status, s.Detail))
	}
	return names
}

func withEnvPorts(t *testing.T, envName string, f func(local string)) {
	t.Helper()
	deadline := time.Now().Add(itest.Scale(3 * time.Minute))
	for attempt := 1; ; attempt++ {
		reached := false
		err := tryTunnel(t, func(s *itest.ConnectSession) error {
			local, found := s.Local(webService, cvpn.TCP)
			if !found {
				return fmt.Errorf("connect lent no port for %s of %s:\n%s", webService, envName, s.Stderr())
			}
			if _, err := getOnce(local); err != nil {
				return fmt.Errorf("GET http://%s/: %w\nconnect said:\n%s", local, err, s.Stderr())
			}
			reached = true
			f(local)
			return nil
		}, envName)
		if reached {
			return
		}
		if time.Now().After(deadline) {
			t.Log(diagnoseEnv(t, envName))
			t.Fatalf("%s did not answer through the tunnel after %d attempt(s): %v", envName, attempt, err)
		}
		time.Sleep(itest.Scale(5 * time.Second))
	}
}

func getOnce(address string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(20*time.Second))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/", nil)
	if err != nil {
		return "", err
	}
	res, err := (&http.Client{Timeout: itest.Scale(20 * time.Second)}).Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	return string(body), err
}

func tryTunnel(t *testing.T, f func(*itest.ConnectSession) error, names ...string) error {
	t.Helper()
	if err := writeHubHome(peerHome, true); err != nil {
		return fmt.Errorf("join the fleet's network: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
	defer cancel()
	args := append([]string{}, names...)
	s, err := itest.Connect(ctx, itest.CommanderOptions{
		Dir:     repo,
		Env:     itest.CommanderEnv(peerHome, "CARAMELO_MACHINE="+tunnelAddr),
		Timeout: itest.Scale(90 * time.Second),
	}, args...)
	if err != nil {
		return fmt.Errorf("caramelo connect %v: %w", names, err)
	}
	defer s.Close()
	t.Logf("[connect] %s on %s: %v", s.Env, s.Machine, s.Listeners)
	return f(s)
}

func getFrom(t *testing.T, address string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(90*time.Second))
	defer cancel()
	c := &http.Client{Timeout: itest.Scale(20 * time.Second)}
	var last error
	for ctx.Err() == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/", nil)
		if err != nil {
			t.Fatalf("GET http://%s/: %v", address, err)
		}
		res, err := c.Do(req)
		if err != nil {
			last = err
			time.Sleep(time.Second)
			continue
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			last = err
			time.Sleep(time.Second)
			continue
		}
		return string(body)
	}
	t.Fatalf("GET http://%s/: %v", address, last)
	return ""
}

func assertServes(t *testing.T, m *itest.Machine, host string) {
	t.Helper()
	assertServesWith(t, m, host, rootFor(m.Alias))
}

func rootFor(alias string) string {
	if roots := memberRoots[alias]; len(roots) > 0 {
		return strings.Join(roots, "\n")
	}
	return pebble.RootPEM
}

func rememberRoot(alias, root string) {
	root = strings.TrimSpace(root)
	if root == "" {
		return
	}
	for _, have := range memberRoots[alias] {
		if have == root {
			return
		}
	}
	memberRoots[alias] = append(memberRoots[alias], root)
}

func refreshMemberPebble(t *testing.T, m *itest.Machine) {
	t.Helper()
	refreshPebble(t, m, memberPebble[m.Alias])
}

func refreshHubPebble(t *testing.T) {
	t.Helper()
	refreshPebble(t, hub, pebble)
	c, err := itest.NewEdgeClient(hub, rootFor(hub.Alias))
	if err != nil {
		t.Fatalf("an edge client for %s: %v", hub.Alias, err)
	}
	internet = c
}

func refreshPebble(t *testing.T, m *itest.Machine, p *itest.Pebble) {
	t.Helper()
	if p == nil {
		return
	}
	deadline := time.Now().Add(itest.Scale(3 * time.Minute))
	for attempt := 1; ; attempt++ {
		err := p.Refresh(m)
		if err == nil {
			rememberRoot(m.Alias, p.RootPEM)
			return
		}
		if time.Now().After(deadline) {
			t.Logf("re-read the ACME root on %s after %d attempt(s): %v", m.Alias, attempt, err)
			return
		}
		time.Sleep(itest.Scale(5 * time.Second))
	}
}

func assertServesWith(t *testing.T, m *itest.Machine, host, root string) {
	t.Helper()
	c, err := itest.NewEdgeClient(m, root)
	if err != nil {
		t.Fatalf("an edge client for %s: %v", m.Alias, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
	defer cancel()
	res, err := c.GetWithin(ctx, "https://"+host+"/", itest.Scale(2*time.Minute))
	if err != nil {
		t.Fatalf("GET https://%s/ at %s: %v", host, m.Alias, err)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("GET https://%s/ at %s: %d\n%s", host, m.Alias, res.Status, res.Body)
	}
	if versionOf(res.Body) == "" {
		t.Errorf("https://%s/ answered something that is not the sample app:\n%s", host, res.Body)
	}
}

func edgeStatusOn(t *testing.T, m *itest.Machine) cedge.Status {
	t.Helper()
	res := asCaramelo(t, m, itest.CarameloBinary+" edge status --json")
	if res.ExitCode != 0 {
		t.Fatalf("[%s] edge status: exit %d\n%s", m.Alias, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return decode[cedge.Status](t, "edge status on "+m.Alias, res.Stdout)
}

func routeOf(t *testing.T, routes []cedge.Route, host string) cedge.Route {
	t.Helper()
	for _, r := range routes {
		if r.Host == host {
			return r
		}
	}
	t.Fatalf("no route for %s in %+v", host, routes)
	return cedge.Route{}
}

func memberEdgeArgs() []string {
	return []string{
		"--edge",
		"--acme-ca", itest.PebbleDirectory,
		"--acme-email", itest.LabACMEEmail,
	}
}

func startMemberEdge(t *testing.T, m *itest.Machine) {
	t.Helper()
	p, err := itest.StartPebbleOn(m)
	if err != nil {
		t.Fatalf("pebble on %s: %v", m.Alias, err)
	}
	if err := itest.TrustOnBox(m, itest.PebbleTrustName, p.MinicaPEM); err != nil {
		t.Fatalf("trust pebble on %s: %v", m.Alias, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
	defer cancel()
	if err := restartEdgeOn(ctx, m); err != nil {
		t.Fatalf("restart the edge on %s: %v", m.Alias, err)
	}
	memberPebble[m.Alias] = p
	rememberRoot(m.Alias, p.RootPEM)
}

func diagnoseEnv(t *testing.T, envName string) string {
	t.Helper()
	var b strings.Builder
	res := inRepo(t, "env", "show", envName, "--json")
	fmt.Fprintf(&b, "env show %s:\n%s\n", envName, strings.TrimSpace(res.Stdout+res.Stderr))
	box := memberBox(t, 0)
	if box == nil {
		return b.String()
	}
	ps := asCaramelo(t, box, "docker ps -a --format '{{.Names}} {{.Status}} {{.Ports}}'")
	fmt.Fprintf(&b, "  on %s:\n%s\n", box.Alias, strings.TrimSpace(ps.Stdout))
	logs := asCaramelo(t, box, "docker logs --tail 15 "+
		itest.ShellQuote(cenv.ContainerName(appName, envName, webService)+"-1")+" 2>&1 || true")
	fmt.Fprintf(&b, "  web-1's last lines:\n%s", strings.TrimSpace(logs.Stdout+logs.Stderr))
	return b.String()
}

const secondCommanderName = "commander-b"

func secondCommander(t *testing.T) string {
	t.Helper()
	me, other := whoAmI(t, home), whoAmI(t, otherHome)
	if me != other {
		return other
	}
	priv := filepath.Join(t.TempDir(), "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-C", secondCommanderName,
		"-f", priv).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen for the second commander: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(priv + ".pub")
	if err != nil {
		t.Fatalf("read the second commander's public key: %v", err)
	}
	add := commander(t, commanderOpts{Dir: repo, Stdin: string(pub)},
		"key", "add", "--name", secondCommanderName, "--json")
	if add.ExitCode != 0 {
		t.Fatalf("key add %s: exit %d\nstdout:\n%sstderr:\n%s",
			secondCommanderName, add.ExitCode, add.Stdout, add.Stderr)
	}
	if err := writeHubHome(otherHome, false); err != nil {
		t.Fatalf("write the second commander's home: %v", err)
	}
	if err := useIdentity(otherHome, priv); err != nil {
		t.Fatalf("give the second commander its own key: %v", err)
	}
	otherKey = priv
	return whoAmI(t, otherHome)
}

func useIdentity(commanderHome, key string) error {
	path := filepath.Join(commanderHome, ".ssh", "config")
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var out strings.Builder
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "IdentityFile ") {
			out.WriteString("  IdentityFile " + key + "\n")
			continue
		}
		out.WriteString(line + "\n")
	}
	return os.WriteFile(path, []byte(out.String()), 0o600)
}

func whoAmI(t *testing.T, which string) string {
	t.Helper()
	res := commander(t, commanderOpts{Dir: repo, Home: which}, "status", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("status: exit %d\nstderr:\n%s", res.ExitCode, res.Stderr)
	}
	var doc struct {
		Identity string `json:"identity"`
	}
	if err := decodeInto(res.Stdout, &doc); err != nil {
		t.Fatalf("status --json: %v\nstdout: %q", err, res.Stdout)
	}
	return doc.Identity
}
