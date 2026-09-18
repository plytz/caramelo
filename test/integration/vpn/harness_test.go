//go:build integration

package vpn

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"
	"gopkg.in/yaml.v3"

	capi "github.com/plytz/caramelo/internal/api"
	cenv "github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vpn"
	"github.com/plytz/caramelo/internal/vpnclient"
	"github.com/plytz/caramelo/test/integration/itest"
)

const (
	appName       = "sampleapp"
	envName       = "feat-x"
	nextEnvName   = "feat-z"
	defaultBranch = "main"
)

const (
	hostPeer      = itest.LabPeerName
	commanderPeer = "vpn-itest-commander"
	agentPeer     = "vpn-itest-agent"
	vpnFleetName  = "lab"
)

var commanderTimeout = itest.Scale(5 * time.Minute)

type suite struct {
	lab       *itest.Lab
	box       *itest.Machine
	commander *itest.Machine

	home      string
	repo      string
	noSSHPath string
	tmp       string

	boxIP       string
	commanderIP string
	tunnel      string

	envAddress string
}

func start(t *testing.T) *suite {
	t.Helper()
	lab := itest.New(t, itest.Options{
		Suite: "vpn",
		State: itest.StateProvisioned,
		Roles: []string{itest.RoleHub, itest.RoleCommander},
	})
	itest.NeedRoles(t, lab, itest.RoleHub, itest.RoleCommander)
	s := &suite{
		lab:       lab,
		box:       lab.Machine(itest.RoleHub),
		commander: lab.Machine(itest.RoleCommander),
		tmp:       t.TempDir(),
	}
	itest.MustReset(t, s.commander, itest.StateClean)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(10*time.Minute))
	defer cancel()
	if err := itest.WaitForCaramelod(ctx, s.box); err != nil {
		t.Fatalf("caramelod on %s: %v", s.box.Alias, err)
	}
	if err := itest.SeedImagesTo(s.box, itest.RunImages...); err != nil {
		t.Fatalf("seed the run images onto %s: %v", s.box.Alias, err)
	}
	s.boxIP = s.box.MustAddress(t)
	s.commanderIP = s.commander.MustAddress(t)
	s.tunnel = s.recordMachine(t)

	s.prepareCommander(t)

	s.home = itest.CommanderHome(t, s.box)
	path, err := itest.NoSSHPath(filepath.Join(s.tmp, "no-ssh-bin"), os.Getenv("PATH"))
	if err != nil {
		t.Fatalf("build a PATH with no ssh on it: %v", err)
	}
	s.noSSHPath = path
	s.repo = s.initSampleRepo(t)

	t.Logf("machine %s at %s (tunnel target %s), commander %s at %s",
		s.box.Alias, s.boxIP, s.tunnel, s.commander.Alias, s.commanderIP)
	return s
}

func (s *suite) prepareCommander(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(10*time.Minute))
	defer cancel()
	if err := itest.InstallBinaryOn(ctx, s.commander); err != nil {
		t.Fatalf("install the binary on %s: %v", s.commander.Alias, err)
	}
	if err := s.prepareCommanderDNS(ctx); err != nil {
		t.Fatalf("prepare %s: %v", s.commander.Alias, err)
	}
	if err := s.seedCommanderSSHKey(ctx); err != nil {
		t.Fatalf("prepare %s: %v", s.commander.Alias, err)
	}
	if res := s.commanderCaramelo(t, "commander init --json"); res.ExitCode != 0 {
		t.Fatalf("caramelo commander init on %s: exit %d\nstdout:%s\nstderr:%s",
			s.commander.Alias, res.ExitCode, res.Stdout, res.Stderr)
	}
}

func (s *suite) prepareCommanderDNS(ctx context.Context) error {
	script := `set -e
ns=$(awk '/^nameserver/ {print $2; exit}' /etc/resolv.conf || true)
missing=""
for p in systemd-resolved polkitd python3 libcap2-bin openssh-client; do
  dpkg -s "$p" >/dev/null 2>&1 || missing="$missing $p"
done
if [ -n "$missing" ]; then
  sudo apt-get update -qq
  sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends $missing
fi
sudo systemctl restart dbus || true
sudo systemctl restart systemd-logind || true
sudo systemctl enable systemd-resolved
sudo systemctl restart systemd-resolved
for i in $(seq 60); do resolvectl status >/dev/null 2>&1 && break; sleep 1; done
printf 'nameserver 127.0.0.53\noptions edns0 trust-ad\n' | sudo tee /etc/resolv.conf >/dev/null
if [ -n "$ns" ]; then
  for i in $(seq 60); do sudo resolvectl dns eth0 "$ns" && break; sleep 1; done
  sudo resolvectl domain eth0 '~.'
fi
resolvectl status >/dev/null
sudo loginctl enable-linger ` + s.commander.User() + `
for i in $(seq 60); do [ -d /run/user/$(id -u) ] && break; sleep 1; done
[ -d /run/user/$(id -u) ]`
	res, err := s.commander.Run(ctx, script)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("split DNS and a user session on %s: exit %d: %s",
			s.commander.Alias, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func (s *suite) seedCommanderSSHKey(ctx context.Context) error {
	key, _, err := itest.LabSSHKey()
	if err != nil {
		return err
	}
	home := itest.HomeOf(s.commander)
	if err := s.commander.Copy(ctx, key, home+"/.ssh/lab_key"); err != nil {
		return err
	}
	cfg := fmt.Sprintf("Host %s\\n  IdentityFile %s/.ssh/lab_key\\n  IdentitiesOnly yes\\n"+
		"  StrictHostKeyChecking no\\n  UserKnownHostsFile /dev/null\\n  LogLevel ERROR\\n", s.boxIP, home)
	cmd := fmt.Sprintf("chmod 600 %s/.ssh/lab_key && printf '%s' > %s/.ssh/config && chmod 600 %s/.ssh/config",
		home, cfg, home, home)
	res, err := s.commander.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("seed the ssh key on %s: exit %d: %s",
			s.commander.Alias, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func (s *suite) initSampleRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(s.tmp, "sampleapp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create the sample checkout: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join("testdata", "sampleapp"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join("testdata", "sampleapp", e.Name()))
		if err != nil {
			t.Fatalf("read testdata: %v", err)
		}
		name := e.Name()
		if name == "gitignore" {
			name = ".gitignore"
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	t.Logf("sample checkout: %s", dir)
	return dir
}

func (s *suite) recordMachine(t *testing.T) string {
	t.Helper()
	if _, err := s.box.HostPortProto(itest.VPNPort, "udp"); err != nil {
		t.Fatalf("itest: %v", err)
	}
	return s.box.HostIP()
}

func (s *suite) commanderEnv(extra ...string) []string {
	env := itest.GitEnv(s.home, append([]string{"CARAMELO_MACHINE=" + s.tunnel}, extra...)...)
	return itest.EnvWithPath(env, s.noSSHPath)
}

func (s *suite) withSSH(extra ...string) []string {
	return itest.GitEnv(s.home, append([]string{"CARAMELO_MACHINE=" + s.tunnel}, extra...)...)
}

func (s *suite) opts(env []string) itest.CommanderOptions {
	return itest.CommanderOptions{Dir: s.repo, Env: env, Timeout: commanderTimeout}
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

func (s *suite) runOnCommander(t *testing.T, cmd string, timeout time.Duration) itest.Result {
	t.Helper()
	if timeout == 0 {
		timeout = itest.Scale(time.Minute)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res, err := s.commander.Run(ctx, inSession(cmd))
	if err != nil {
		t.Fatalf("[%s] %s: %v", s.commander.Alias, cmd, err)
	}
	return res
}

func inSession(cmd string) string {
	return "export XDG_RUNTIME_DIR=/run/user/$(id -u); " +
		"export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus; " + cmd
}

func (s *suite) commanderCaramelo(t *testing.T, args string) itest.Result {
	t.Helper()
	return s.runOnCommander(t, itest.CarameloBinary+" "+args, itest.Scale(3*time.Minute))
}

func (s *suite) sudoCommanderCaramelo(t *testing.T, args string) itest.Result {
	t.Helper()
	return s.runOnCommander(t, "sudo -n "+itest.CarameloBinary+" "+args, itest.Scale(3*time.Minute))
}

func (s *suite) writeCommanderConfig(t *testing.T, hub string) {
	t.Helper()
	body, err := yaml.Marshal(remote.CommanderConfig{
		Name: itest.CommanderName,
		Role: remote.RoleCommander,
		Commander: remote.Commander{
			DefaultFleet: vpnFleetName,
			Fleets:       map[string]remote.Fleet{vpnFleetName: {Hub: hub}},
		},
	})
	if err != nil {
		t.Fatalf("encoding the commander config for %s: %v", s.commander.Alias, err)
	}
	dir := filepath.Join(itest.HomeOf(s.commander), ".config", remote.CommanderDirName)
	path := filepath.Join(dir, remote.CommanderConfigFile)
	cmd := fmt.Sprintf("mkdir -p %s && chmod 0700 %s && printf %%s %s | base64 -d > %s && chmod 0600 %s",
		dir, dir, base64.StdEncoding.EncodeToString(body), path, path)
	if res := s.runOnCommander(t, cmd, itest.Scale(time.Minute)); res.ExitCode != 0 {
		t.Fatalf("writing the commander config on %s: exit %d: %s", s.commander.Alias, res.ExitCode, res.Stderr)
	}
}

type envRecord struct {
	ID       int64       `json:"id"`
	App      string      `json:"app"`
	Name     string      `json:"name"`
	Status   cenv.Status `json:"status"`
	PortBase int         `json:"port_base"`
	VPNIP    string      `json:"vpn_ip"`
}

type envDetail struct {
	Env    envRecord       `json:"env"`
	Deps   []cenv.DepState `json:"deps"`
	Events []cenv.Event    `json:"events"`
}

func addr(t *testing.T, what, text string) netip.Addr {
	t.Helper()
	ip, err := netip.ParseAddr(strings.TrimSpace(text))
	if err != nil {
		t.Fatalf("%s = %q: not an address: %v", what, text, err)
	}
	return ip
}

func inSubnet(t *testing.T, what string, ip netip.Addr, kind vpn.AddressKind) {
	t.Helper()
	prefix, err := netip.ParsePrefix(vpn.DefaultSubnet)
	if err != nil {
		t.Fatalf("parsing %s: %v", vpn.DefaultSubnet, err)
	}
	if !prefix.Contains(ip) {
		t.Errorf("%s = %s, outside the machine's subnet %s", what, ip, prefix)
		return
	}
	third := ip.As4()[2]
	switch kind {
	case vpn.KindPeer:
		if third != 0 {
			t.Errorf("%s = %s, want a peer address in the x.y.0.0/24 block", what, ip)
		}
	case vpn.KindEnv:
		if third == 0 {
			t.Errorf("%s = %s, want an environment address from x.y.1.1 upward", what, ip)
		}
	}
}

func machineAddress(t *testing.T) netip.Addr {
	t.Helper()
	prefix, err := netip.ParsePrefix(vpn.DefaultSubnet)
	if err != nil {
		t.Fatalf("parsing %s: %v", vpn.DefaultSubnet, err)
	}
	b := prefix.Addr().As4()
	b[3] = 1
	return netip.AddrFrom4(b)
}

func (s *suite) commanderKeyPath() string {
	return filepath.Join(itest.HomeOf(s.commander), ".config", "caramelo", vpnclient.KeyDir,
		vpnclient.KeyFileName(s.boxIP))
}

func (s *suite) needEnv(t *testing.T) {
	t.Helper()
	if s.envAddress == "" {
		t.Skip("vpn suite: no environment with an address (an earlier step failed)")
	}
}

func (s *suite) pushSample(t *testing.T, branch string) {
	t.Helper()
	env := s.gitOverTunnelEnv(t)
	if _, err := os.Stat(filepath.Join(s.repo, ".git")); err != nil {
		itest.Git(t, s.repo, env, "init", "-b", branch)
		itest.GitCommitAll(t, s.repo, env, "sampleapp: first commit")
	}
	itest.Git(t, s.repo, env, "push", s.internalRemote(t), branch)
}

func (s *suite) internalRemote(t *testing.T) string {
	t.Helper()
	return itest.GitRemote(vpn.MachineHost(s.machineHostname(t)), appName)
}

func (s *suite) machineHostname(t *testing.T) string {
	t.Helper()
	res := itest.CommanderOK(t, s.opts(s.commanderEnv()), "status", "--json")
	st := decode[capi.Status](t, "status", res.Stdout)
	if st.Hostname == "" {
		t.Fatal("status reported no hostname")
	}
	return st.Hostname
}

func (s *suite) gitOverTunnelEnv(t *testing.T) []string {
	t.Helper()
	bin, err := itest.HostBinary()
	if err != nil {
		t.Fatalf("the binary under test: %v", err)
	}
	return s.commanderEnv("GIT_SSH_COMMAND="+bin+" git-ssh", "GIT_SSH_VARIANT=ssh")
}

func (s *suite) showEnv(t *testing.T, name string) envDetail {
	t.Helper()
	res := itest.CommanderOK(t, s.opts(s.commanderEnv()), "env", "show", name, "--json")
	return decode[envDetail](t, "env show "+name, res.Stdout)
}

func (s *suite) listPeers(t *testing.T, env []string) map[string]state.Peer {
	t.Helper()
	res := itest.CommanderOK(t, s.opts(env), "peer", "list", "--json")
	var peers []state.Peer
	if err := decodeInto(res.Stdout, &peers); err != nil {
		t.Fatalf("peer list --json: %v\nstdout: %q", err, res.Stdout)
	}
	out := make(map[string]state.Peer, len(peers))
	for _, p := range peers {
		out[p.Name] = p
	}
	return out
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func listenerFor(s *itest.ConnectSession, name string, proto vpn.Protocol) (vpnclient.Listener, bool) {
	return s.Listener(name, proto)
}

func (s *suite) envURLs(t *testing.T, name string) map[string]cenv.URL {
	t.Helper()
	res := itest.CommanderOK(t, s.opts(s.commanderEnv()), "env", "url", name, "--json")
	var urls []cenv.URL
	if err := decodeInto(res.Stdout, &urls); err != nil {
		t.Fatalf("env url %s --json: %v\nstdout: %q", name, err, res.Stdout)
	}
	out := make(map[string]cenv.URL, len(urls))
	for _, u := range urls {
		out[u.Name] = u
	}
	return out
}

func (s *suite) upEnv(t *testing.T, name string) {
	t.Helper()
	o := s.opts(s.commanderEnv())
	o.Timeout = itest.Scale(8 * time.Minute)
	itest.CommanderOK(t, o, "up", name, "--json")
}

func redisPing(t *testing.T, local string) string {
	t.Helper()
	got, err := itest.RedisPing(local)
	if err != nil {
		t.Errorf("redis at %s: %v", local, err)
	}
	return got
}

func postgresHandshake(t *testing.T, local string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", local, itest.Scale(10*time.Second))
	if err != nil {
		t.Errorf("dial postgres at %s: %v", local, err)
		return
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(itest.Scale(10 * time.Second))); err != nil {
		t.Error(err)
		return
	}
	req := []byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}
	if _, err := conn.Write(req); err != nil {
		t.Errorf("write to postgres at %s: %v", local, err)
		return
	}
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err != nil {
		t.Errorf("read from postgres at %s: %v", local, err)
		return
	}
	if buf[0] != 'S' && buf[0] != 'N' {
		t.Errorf("postgres at %s answered %q, want S or N to an SSLRequest", local, buf[0])
	}
}

func generatePublicKey(t *testing.T) string {
	t.Helper()
	var priv [32]byte
	if _, err := rand.Read(priv[:]); err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	priv[0] &= 248
	priv[31] = (priv[31] & 127) | 64
	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(pub)
}

func httpGet(t *testing.T, url string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(30*time.Second))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	res, err := (&http.Client{Timeout: itest.Scale(30 * time.Second)}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
		return ""
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET %s: %s\n%s", url, res.Status, body)
	}
	return string(body)
}

func udpEcho(t *testing.T, local, payload string) string {
	t.Helper()
	got, err := udpRoundTrip(local, payload)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func udpRoundTrip(local, payload string) (string, error) {
	const attempts = 5
	var err error
	for i := 0; i < attempts; i++ {
		var got string
		got, err = udpOnce(local, payload, itest.Scale(4*time.Second))
		if err == nil {
			return got, nil
		}
	}
	return "", fmt.Errorf("no answer from %s after %d datagrams: %w", local, attempts, err)
}

func udpOnce(local, payload string, wait time.Duration) (string, error) {
	conn, err := net.DialTimeout("udp", local, itest.Scale(10*time.Second))
	if err != nil {
		return "", fmt.Errorf("dial udp %s: %w", local, err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(wait)); err != nil {
		return "", err
	}
	if _, err := conn.Write([]byte(payload)); err != nil {
		return "", fmt.Errorf("write to %s: %w", local, err)
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return "", fmt.Errorf("read from %s: %w", local, err)
	}
	return string(buf[:n]), nil
}

func replaceLine(t *testing.T, dir, name, old, replacement string) {
	t.Helper()
	path := filepath.Join(dir, name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := string(b)
	if !strings.Contains(body, old) {
		t.Fatalf("%s does not contain %q", name, old)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(body, old, replacement, 1)), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
