//go:build integration

package setup

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"
	"gopkg.in/yaml.v3"

	capi "github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/firewall"
	"github.com/plytz/caramelo/internal/place"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/serverconfig"
	setuppkg "github.com/plytz/caramelo/internal/setup"
	"github.com/plytz/caramelo/internal/state"
	taskpkg "github.com/plytz/caramelo/internal/task"
	"github.com/plytz/caramelo/internal/vpn"
	"github.com/plytz/caramelo/test/integration/itest"
)

const (
	webContainer = "web"
	webImage     = "nginx:alpine"
	webPort      = 20080
	oomImage     = "alpine"
)

func TestSetup(t *testing.T) {
	begin(t)
	if firstRunErr != nil {
		t.Fatalf("hub setup failed: %v\noutput:\n%s", firstRunErr, firstRunRaw)
	}
	for _, r := range firstRun.Results {
		t.Logf("step %-18s %-12s %s%s", r.Step, r.Status, r.Detail, r.Error)
	}
	if firstRun.Failed != 0 {
		t.Errorf("setup reported %d failed step(s)\noutput:\n%s", firstRun.Failed, firstRunRaw)
	}
	if firstRun.Changed == 0 {
		t.Errorf("setup on a clean box changed nothing; report: %s", firstRunRaw)
	}
	for _, r := range firstRun.Results {
		if r.Status == setuppkg.StatusFailed {
			t.Errorf("step %s failed: %s", r.Step, r.Error)
		}
	}
}

func TestSwap(t *testing.T) {
	_, m := begin(t)
	if firstRunErr != nil {
		t.Skip("first run failed; the swap step proves nothing")
	}
	var res *setuppkg.Result
	for i := range firstRun.Results {
		if firstRun.Results[i].Step == "swap" {
			res = &firstRun.Results[i]
		}
	}
	if res == nil {
		t.Fatalf("setup ran no swap step at all: %s", firstRunRaw)
	}
	t.Logf("swap: %s %s%s", res.Status, res.Detail, res.Error)
	if res.Status == setuppkg.StatusFailed {
		t.Fatalf("the swap step failed: %s", res.Error)
	}
	said := res.Detail
	if res.Status == setuppkg.StatusSkipped && said == "" {
		t.Fatal("the swap step skipped without saying why")
	}
	if m.Target() == itest.TargetDocker {
		if res.Status != setuppkg.StatusSkipped {
			t.Errorf("swap = %s on a container, want it skipped: a container shares the host kernel's swap", res.Status)
		}
		if !strings.Contains(said, "container") {
			t.Errorf("swap skipped on a container without saying so: %q", said)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
	defer cancel()
	fstype, avail := swapGround(ctx, t, m)
	roomy := swapFilesystems[fstype] && avail >= serverconfig.Default().Swap.SizeBytes+swapFreeHeadroomBytes
	t.Logf("swap ground: %s filesystem, %d bytes free, caramelo had room: %v", fstype, avail, roomy)

	switch res.Status {
	case setuppkg.StatusSkipped:
		if !documentedSwapRefusal(said) {
			t.Errorf("the swap step skipped for a reason nothing documents: %q", said)
		}
		if roomy {
			t.Errorf("swap skipped on a %s filesystem with %d bytes free, where caramelo had room to make it: %q",
				fstype, avail, said)
		}
		t.Logf("swap was not made on this machine: %s", said)
	case setuppkg.StatusOK, setuppkg.StatusChanged:
		if said == "" {
			t.Error("the swap step says nothing about the swap it made or found")
		}
		marker := "sudo test -f " + setuppkg.SwapSysctlFile
		made, err := m.Run(ctx, marker)
		if err != nil {
			t.Fatalf("look for %s: %v", setuppkg.SwapSysctlFile, err)
		}
		if made.ExitCode != 0 {
			if roomy && !strings.Contains(said, "left alone") {
				t.Errorf("swap = %s on a %s filesystem with %d bytes free, but %s is not there: %q",
					res.Status, fstype, avail, setuppkg.SwapSysctlFile, said)
			}
			t.Logf("caramelo left the swap this machine already had: %s", said)
			return
		}
		on, err := m.Run(ctx, "sudo swapon --show=NAME --noheadings")
		if err != nil {
			t.Fatalf("swapon --show: %v", err)
		}
		if !strings.Contains(on.Stdout, serverconfig.Default().SwapFilePath()) {
			t.Errorf("the swapfile is not swapped on:\n%s", on.Stdout)
		}
	default:
		t.Errorf("swap = %s, want ok, changed or skipped", res.Status)
	}
}

const swapFreeHeadroomBytes = 5 << 30

var swapFilesystems = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true}

var swapRefusals = []string{
	"swap off: this machine is set up without swap",
	"zram is not implemented yet",
	"container shares the host kernel's swap",
	"is on btrfs",
	"is on ZFS",
	"flash storage",
	"free on",
	"name the swap unit",
	"swap backend",
}

func documentedSwapRefusal(reason string) bool {
	for _, r := range swapRefusals {
		if strings.Contains(reason, r) {
			return true
		}
	}
	return false
}

func swapGround(ctx context.Context, t *testing.T, m *itest.Machine) (fstype string, avail int64) {
	t.Helper()
	dir := filepath.Dir(serverconfig.Default().SwapFilePath())
	fs, err := m.Run(ctx, "findmnt -n -o FSTYPE -T "+dir)
	if err != nil {
		t.Fatalf("findmnt -T %s: %v", dir, err)
	}
	if fs.ExitCode != 0 {
		t.Logf("findmnt -T %s exited %d: %s", dir, fs.ExitCode, fs.Stderr)
		return "", 0
	}
	fstype = strings.TrimSpace(fs.Stdout)
	df, err := m.Run(ctx, "df -B1 --output=avail "+dir)
	if err != nil {
		t.Fatalf("df %s: %v", dir, err)
	}
	if df.ExitCode != 0 {
		t.Logf("df %s exited %d: %s", dir, df.ExitCode, df.Stderr)
		return fstype, 0
	}
	fields := strings.Fields(df.Stdout)
	if len(fields) < 2 {
		t.Logf("unexpected df output %q", df.Stdout)
		return fstype, 0
	}
	n, err := strconv.ParseInt(fields[len(fields)-1], 10, 64)
	if err != nil {
		t.Logf("unexpected df output %q", df.Stdout)
		return fstype, 0
	}
	return fstype, n
}

func TestGossSetup(t *testing.T) {
	_, m := begin(t)
	itest.RunGoss(t, m, itest.MustGossSpec(t, "setup.yaml"))
}

func TestRootlessDocker(t *testing.T) {
	_, m := begin(t)
	if m.Target() == itest.TargetDocker {
		itest.SeedImages(t, m, oomImage, webImage)
	}

	t.Run("published port keeps the client IP", func(t *testing.T) {
		run := fmt.Sprintf("docker rm -f %s >/dev/null 2>&1; docker run -d --name %s --restart unless-stopped -p %d:80 %s",
			webContainer, webContainer, webPort, webImage)
		itest.MustRunAsUser(t, m, itest.CarameloUser, run)

		from := clientFor(t, m)
		ip := m.MustAddress(t)
		body, clientIP := getWithRetry(t, from, fmt.Sprintf("http://%s:%d/", ip, webPort), itest.Scale(90*time.Second))
		if !strings.Contains(body, "nginx") {
			t.Errorf("unexpected body from %s: %q", ip, body)
		}
		if want := from.MustAddress(t); clientIP != want {
			t.Errorf("%s reached %s from %s, want its own address %s", from.Alias, m.Alias, clientIP, want)
		}
		if !sameNetwork(clientIP, ip) {
			t.Fatalf("%s reached %s (%s) from %s, which is not on the same network", from.Alias, m.Alias, ip, clientIP)
		}
		logs := itest.MustRunAsUser(t, m, itest.CarameloUser, "docker logs "+webContainer)
		if !strings.Contains(logs.Stdout+logs.Stderr, clientIP) {
			t.Errorf("nginx did not see the real client IP %s; access log:\n%s%s", clientIP, logs.Stdout, logs.Stderr)
		}
	})

	t.Run("memory limit is enforced", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
		defer cancel()
		cmd := fmt.Sprintf("docker run --rm --memory 64m --memory-swap 64m %s sh -c 'head -c 200000000 /dev/zero | tail'", oomImage)
		res, err := itest.RunAsUser(ctx, m, itest.CarameloUser, cmd)
		if err != nil {
			t.Fatalf("oom run: %v", err)
		}
		if res.ExitCode != 137 {
			t.Errorf("exit = %d, want 137 (SIGKILL by the memory cgroup)\nstdout:%s\nstderr:%s",
				res.ExitCode, res.Stdout, res.Stderr)
		}
	})

	if m.Target() == itest.TargetDocker {
		itest.ExportImages(t, m, oomImage, webImage)
	}
}

func TestIdempotent(t *testing.T) {
	_, m := begin(t)
	if firstRunErr != nil {
		t.Skip("first run failed; a second run proves nothing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(10*time.Minute))
	defer cancel()
	report, raw, err := runSetup(ctx, m, itest.CarameloBinary)
	if err != nil {
		t.Fatalf("second hub setup failed: %v\noutput:\n%s", err, raw)
	}
	for _, r := range report.Results {
		t.Logf("step %-18s %-12s %s", r.Step, r.Status, r.Detail)
	}
	if report.Changed != 0 {
		t.Errorf("second run changed %d step(s), want 0", report.Changed)
	}
	if report.Failed != 0 {
		t.Errorf("second run failed %d step(s), want 0", report.Failed)
	}
}

func TestSetupSurvivesAPowerCycle(t *testing.T) {
	_, m := begin(t)
	if firstRunErr != nil {
		t.Skip("first run failed; a power cycle proves nothing")
	}
	ensureWeb := fmt.Sprintf("docker inspect %s >/dev/null 2>&1 || docker run -d --name %s --restart unless-stopped -p %d:80 %s",
		webContainer, webContainer, webPort, webImage)
	itest.MustRunAsUser(t, m, itest.CarameloUser, ensureWeb)
	itest.MustRestart(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(10*time.Minute))
	defer cancel()
	if err := itest.WaitForPort(ctx, m, vpn.DefaultListenPort); err != nil {
		t.Fatalf("the tunnel did not come back after the power cycle: %v", err)
	}
	for _, unit := range []string{"caramelod", "docker"} {
		cmd := itest.AsUserSession(m, itest.CarameloUser, "systemctl --user is-active "+unit)
		if err := m.WaitFor(ctx, cmd, 2*time.Second); err != nil {
			t.Fatalf("the %s user unit did not come back after the power cycle: %v", unit, err)
		}
	}

	res := m.MustRun(t, "sudo "+itest.CarameloBinary+" hub status --json")
	var st struct {
		Installed bool `json:"installed"`
		Caramelod struct {
			Active bool `json:"active"`
		} `json:"caramelod"`
		Docker struct {
			Active bool `json:"active"`
		} `json:"docker"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &st); err != nil {
		t.Fatalf("hub status --json is not JSON after the power cycle: %v\nstdout: %q", err, res.Stdout)
	}
	if !st.Installed || !st.Caramelod.Active || !st.Docker.Active {
		t.Errorf("after the power cycle: installed=%v caramelod=%v docker=%v, want all true",
			st.Installed, st.Caramelod.Active, st.Docker.Active)
	}

	from := clientFor(t, m)
	ip := m.MustAddress(t)
	body, clientIP := getWithRetry(t, from, fmt.Sprintf("http://%s:%d/", ip, webPort), itest.Scale(3*time.Minute))
	if !strings.Contains(body, "nginx") {
		t.Errorf("the container published before the power cycle answers %q", body)
	}
	if want := from.MustAddress(t); clientIP != want {
		t.Errorf("%s reached %s from %s after the power cycle, want %s", from.Alias, m.Alias, clientIP, want)
	}

	report, raw, err := runSetup(ctx, m, itest.CarameloBinary)
	if err != nil {
		t.Fatalf("hub setup after the power cycle: %v\noutput:\n%s", err, raw)
	}
	if report.Changed != 0 || report.Failed != 0 {
		t.Errorf("setup after the power cycle changed %d and failed %d step(s), want 0 and 0\n%s",
			report.Changed, report.Failed, raw)
	}
}

func TestServerStatus(t *testing.T) {
	_, m := begin(t)
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(time.Minute))
	defer cancel()
	res, err := m.Run(ctx, "sudo "+itest.CarameloBinary+" hub status --json")
	if err != nil {
		t.Fatalf("hub status: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("hub status --json: exit %d\nstdout:%s\nstderr:%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	var st struct {
		Installed bool `json:"installed"`
		Caramelod struct {
			Active bool   `json:"active"`
			State  string `json:"state"`
		} `json:"caramelod"`
		Docker struct {
			Active bool `json:"active"`
		} `json:"docker"`
		Socket struct {
			Present bool `json:"present"`
		} `json:"socket"`
		Port struct {
			Port         int    `json:"port"`
			Listening    bool   `json:"listening"`
			APIListen    string `json:"api_listen"`
			VPNPort      int    `json:"vpn_port"`
			VPNListening bool   `json:"vpn_listening"`
		} `json:"port"`
		Swap struct {
			TotalBytes int64  `json:"total_bytes"`
			Managed    bool   `json:"managed"`
			Backend    string `json:"backend"`
			SizeBytes  int64  `json:"size_bytes"`
		} `json:"swap"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &st); err != nil {
		t.Fatalf("hub status --json is not JSON: %v\nstdout: %q", err, res.Stdout)
	}
	t.Logf("hub status: %s", strings.TrimSpace(res.Stdout))
	if !st.Installed {
		t.Error("hub status says the machine is not installed")
	}
	if !st.Caramelod.Active {
		t.Errorf("caramelod is not active: state %q", st.Caramelod.State)
	}
	if !st.Docker.Active {
		t.Error("the rootless docker user unit is not active")
	}
	if !st.Socket.Present {
		t.Error("the local socket is missing")
	}
	if st.Port.Port != itest.CarameloSSHPort {
		t.Errorf("api port = %d, want %d", st.Port.Port, itest.CarameloSSHPort)
	}
	if st.Port.APIListen != serverconfig.DefaultAPIListen {
		t.Errorf("api_listen = %q, want the default %q", st.Port.APIListen, serverconfig.DefaultAPIListen)
	}
	if wantPublic := st.Port.APIListen != serverconfig.APIListenVPN; st.Port.Listening != wantPublic {
		t.Errorf("tcp %d listening=%v with api_listen %q, want %v",
			st.Port.Port, st.Port.Listening, st.Port.APIListen, wantPublic)
	}
	if st.Port.VPNPort != vpn.DefaultListenPort || !st.Port.VPNListening {
		t.Errorf("tunnel port = %d listening=%v, want %d listening: it is the one port a "+
			"Caramelo machine needs reachable", st.Port.VPNPort, st.Port.VPNListening, vpn.DefaultListenPort)
	}
	if st.Swap.Backend != serverconfig.SwapFile {
		t.Errorf("swap backend = %q, want the default %q", st.Swap.Backend, serverconfig.SwapFile)
	}
	if st.Swap.SizeBytes != serverconfig.DefaultSwapSizeBytes {
		t.Errorf("swap size = %d, want the default %d", st.Swap.SizeBytes, serverconfig.DefaultSwapSizeBytes)
	}
	made, err := m.Run(ctx, "sudo test -f "+setuppkg.SwapSysctlFile)
	if err != nil {
		t.Fatalf("look for %s: %v", setuppkg.SwapSysctlFile, err)
	}
	if carameloMadeIt := made.ExitCode == 0; carameloMadeIt != st.Swap.Managed {
		t.Errorf("hub status says managed=%v, but %s is %s",
			st.Swap.Managed, setuppkg.SwapSysctlFile, map[bool]string{true: "there", false: "not there"}[carameloMadeIt])
	}
	if st.Swap.Managed && st.Swap.TotalBytes <= 0 {
		t.Errorf("hub status credits caramelo with %d bytes of swap", st.Swap.TotalBytes)
	}
}

func TestVPNAfterSetup(t *testing.T) {
	_, m := begin(t)
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
	defer cancel()

	res, err := m.Run(ctx, "sudo cat "+serverconfig.Path(serverconfig.DefaultConfigDir))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("read config.yaml: exit %d: %s", res.ExitCode, res.Stderr)
	}
	var cfg serverconfig.Config
	if err := yaml.Unmarshal([]byte(res.Stdout), &cfg); err != nil {
		t.Fatalf("config.yaml is not a serverconfig.Config: %v\n%s", err, res.Stdout)
	}
	if cfg.VPNSubnet != serverconfig.DefaultVPNSubnet {
		t.Errorf("vpn_subnet = %q, want %q", cfg.VPNSubnet, serverconfig.DefaultVPNSubnet)
	}
	if cfg.VPNListen != serverconfig.DefaultVPNListen {
		t.Errorf("vpn_listen = %q, want %q", cfg.VPNListen, serverconfig.DefaultVPNListen)
	}
	if cfg.APIListen == "" {
		t.Error("api_listen is empty; setup must write where the API answers")
	}

	if res, err := m.Run(ctx, fmt.Sprintf("ss -H -uln | grep -q ':%d '", vpn.DefaultListenPort)); err != nil {
		t.Fatalf("ss: %v", err)
	} else if res.ExitCode != 0 {
		t.Errorf("nothing is listening on udp:%d", vpn.DefaultListenPort)
	}

	status, err := m.Run(ctx, "sudo -u "+itest.CarameloUser+" "+itest.CarameloBinary+" status --json")
	if err != nil {
		t.Fatalf("status on the box: %v", err)
	}
	if status.ExitCode != 0 {
		t.Fatalf("status on the box: exit %d\nstderr:%s", status.ExitCode, status.Stderr)
	}
	var st capi.Status
	if err := json.Unmarshal([]byte(strings.TrimSpace(status.Stdout)), &st); err != nil {
		t.Fatalf("status --json: %v\nstdout: %q", err, status.Stdout)
	}
	if st.VPN == nil || !st.VPN.Enabled {
		t.Fatalf("the machine reports no working network: %+v", st.VPN)
	}
	if st.VPN.PublicKey == "" {
		t.Error("no public key in the tunnel status; a peer cannot configure itself without it")
	}
	if st.VPN.APIListen != cfg.APIListen {
		t.Errorf("status says api_listen %q, config.yaml says %q", st.VPN.APIListen, cfg.APIListen)
	}
}

func TestTheContextOnAHub(t *testing.T) {
	_, m := begin(t)
	if firstRunErr != nil {
		t.Skip("hub setup failed; what the box says it is proves nothing")
	}
	cfg := itest.MustServerConfigOn(t, m)
	got := itest.MustContextOn(t, m, itest.CarameloBinary)

	if got.Role != place.RoleHub {
		t.Fatalf("a box that ran hub setup says role %q, want %q: %+v", got.Role, place.RoleHub, got)
	}
	if got.Name == "" || got.Name != cfg.Name {
		t.Errorf("context calls this box %q and config.yaml calls it %q", got.Name, cfg.Name)
	}
	wantConfig := serverconfig.Path(serverconfig.DefaultConfigDir)
	if got.ConfigFile != wantConfig {
		t.Errorf("context read %q, want %s", got.ConfigFile, wantConfig)
	}
	if got.Problem != "" {
		t.Errorf("context on a set-up box reports a problem: %s", got.Problem)
	}
	if got.Server == nil {
		t.Fatalf("context on a hub carries no server half: %+v", got)
	}
	s := got.Server
	if s.Fleet != cfg.FleetName() {
		t.Errorf("context says fleet %q, config.yaml says %q", s.Fleet, cfg.FleetName())
	}
	if s.Hub != nil {
		t.Errorf("context on a hub names a hub it joined: %+v", s.Hub)
	}
	for _, p := range []struct{ what, got, want string }{
		{"state", s.Paths.State, cfg.StateDir},
		{"data", s.Paths.Data, cfg.DataDir},
		{"run", s.Paths.Run, cfg.RunDir},
		{"socket", s.Paths.Socket, cfg.SocketPath()},
		{"apps", s.Paths.Apps, cfg.AppsDir()},
		{"secrets", s.Paths.Secrets, cfg.SecretsDir()},
	} {
		if p.got != p.want {
			t.Errorf("context says the %s path is %q, config.yaml derives %q", p.what, p.got, p.want)
		}
	}
	if !s.Services.Daemon.Present {
		t.Errorf("context finds no caramelod socket at %s, and %s is in the %s group: the daemon setup started "+
			"is invisible to the person typing", s.Services.Daemon.Path, itest.UserOf(m), cfg.Group)
	}
	if got.TalksTo.Kind != place.TalksSocket {
		t.Errorf("a command typed on this hub would talk to %q (%s), want the caramelod on this machine",
			got.TalksTo.Kind, got.TalksTo.Problem)
	}

	text := itest.ContextTextOn(t, m, itest.CarameloBinary)
	for _, want := range []string{cfg.Name + ", " + place.RoleHub, "of fleet " + cfg.FleetName(), "state " + cfg.StateDir} {
		if !strings.Contains(text, want) {
			t.Errorf("caramelo context on %s never says %q:\n%s", m.Alias, want, text)
		}
	}
}

func TestTheContextOnACommander(t *testing.T) {
	begin(t)
	if commander == nil {
		t.Skip("setup suite: this lab has no commander container of its own")
	}
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(10*time.Minute))
	defer cancel()
	if err := itest.InstallBinaryOn(ctx, commander); err != nil {
		t.Fatalf("install the binary on %s: %v", commander.Alias, err)
	}

	fresh, err := itest.ContextOn(ctx, commander, itest.CarameloBinary)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if fresh.Role != place.RoleFresh {
		t.Fatalf("a box with no config at all says role %q, want %q: %+v", fresh.Role, place.RoleFresh, fresh)
	}
	if fresh.Server != nil || fresh.Commander != nil {
		t.Errorf("a fresh box carries a server or a commander half: %+v", fresh)
	}
	if !strings.Contains(fresh.TalksTo.Problem, "commander init") {
		t.Errorf("a fresh box does not name 'commander init' as the one command it can run: %q", fresh.TalksTo.Problem)
	}

	const name = "commander"
	init, err := commander.Run(ctx, itest.CarameloBinary+" commander init --name "+name+" --json")
	if err != nil {
		t.Fatalf("commander init on %s: %v", commander.Alias, err)
	}
	if init.ExitCode != 0 {
		t.Fatalf("commander init on %s: exit %d\nstdout:\n%sstderr:\n%s",
			commander.Alias, init.ExitCode, init.Stdout, init.Stderr)
	}

	got := itest.MustContextOn(t, commander, itest.CarameloBinary)
	if got.Role != place.RoleCommander {
		t.Fatalf("a box that ran commander init says role %q, want %q: %+v", got.Role, place.RoleCommander, got)
	}
	if got.Name != name {
		t.Errorf("context calls this commander %q, want %q", got.Name, name)
	}
	if got.Problem != "" {
		t.Errorf("context on an initialized commander reports a problem: %s", got.Problem)
	}
	if got.Server != nil {
		t.Errorf("context on a commander carries a server half: %+v", got.Server)
	}
	if got.Commander == nil {
		t.Fatalf("context on a commander carries no commander half: %+v", got)
	}
	if !strings.HasSuffix(got.ConfigFile, "/.config/caramelo/"+remote.CommanderConfigFile) {
		t.Errorf("the commander config is %q, want it under the user's own config directory", got.ConfigFile)
	}
	if !got.Commander.Identified {
		t.Errorf("commander init wrote no identity key at %s", got.Commander.IdentityKey)
	}
	if len(got.Commander.Fleets) != 0 {
		t.Errorf("a commander that joined nothing holds fleets %v", got.FleetNames())
	}
	if got.TalksTo.Kind != place.TalksNothing || !strings.Contains(got.TalksTo.Problem, "fleet add") {
		t.Errorf("a commander with no fleet would talk to %q (%s), want nothing and a line naming 'fleet add'",
			got.TalksTo.Kind, got.TalksTo.Problem)
	}

	text := itest.ContextTextOn(t, commander, itest.CarameloBinary)
	for _, want := range []string{name + ", " + place.RoleCommander, "no fleets"} {
		if !strings.Contains(text, want) {
			t.Errorf("caramelo context on %s never says %q:\n%s", commander.Alias, want, text)
		}
	}
}

func TestSetupRecordsAPeer(t *testing.T) {
	_, m := begin(t)
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(5*time.Minute))
	defer cancel()

	const peerName = "setup-lab-peer"
	pub := wireguardPublicKey(t)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), itest.Scale(time.Minute))
		defer cancel()
		if _, err := m.Run(cleanupCtx, "sudo -u "+itest.CarameloUser+" "+itest.CarameloBinary+" peer remove "+peerName); err != nil {
			t.Logf("removing %s again: %v", peerName, err)
		}
	})

	cmd := fmt.Sprintf("sudo %s hub setup --yes --json --authorized-keys %s --peer %s %s",
		itest.CarameloBinary, authorizedKeys(m), peerName, pub)
	res, err := m.Run(ctx, cmd)
	if err != nil {
		t.Fatalf("hub setup --peer: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("hub setup --peer: exit %d\nstdout:%s\nstderr:%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	var report setuppkg.Report
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &report); err != nil {
		t.Fatalf("hub setup --peer: stdout is not a report: %v\n%s", err, res.Stdout)
	}
	if report.Failed != 0 {
		t.Errorf("%d step(s) failed with --peer\n%s", report.Failed, res.Stdout)
	}

	peers := peerList(t, m)
	found := findPeer(peers, peerName)
	if found == nil {
		t.Fatalf("peer %q was not recorded: %+v", peerName, peers)
	}
	if found.PublicKey != pub {
		t.Errorf("peer %q public key = %q, want the one setup was given", peerName, found.PublicKey)
	}
	if found.IP == "" {
		t.Errorf("peer %q has no address", peerName)
	}
}

func TestFirewallIsCheckedAndReported(t *testing.T) {
	begin(t)
	if firstRunErr != nil {
		t.Skip("first run failed; the firewall report proves nothing")
	}
	var res *setuppkg.Result
	for i := range firstRun.Results {
		if firstRun.Results[i].Step == "firewall" {
			res = &firstRun.Results[i]
		}
	}
	if res == nil {
		t.Fatalf("the run has no firewall step:\n%s", firstRunRaw)
	}
	t.Logf("firewall: %s %s", res.Status, res.Detail)
	if res.Status != setuppkg.StatusOK {
		t.Errorf("the firewall step is %q on a box with nothing in the way: %s%s", res.Status, res.Detail, res.Error)
	}
	if strings.Contains(res.Detail, string(firewall.VerdictBlocked)) {
		t.Errorf("a box with no firewall reports a blocked port: %s", res.Detail)
	}
	if strings.Contains(firstRunRaw, "make sure these ports reach this machine") {
		t.Errorf("setup still hands out advice nobody checked:\n%s", firstRunRaw)
	}
}

func TestSetupStopsWhenTheTunnelPortIsDropped(t *testing.T) {
	_, m := begin(t)
	if firstRunErr != nil {
		t.Skip("first run failed; a firewall in the way proves nothing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(20*time.Minute))
	defer cancel()

	if res, err := m.Run(ctx, "sudo sh -c 'command -v nft'"); err != nil || res.ExitCode != 0 {
		t.Skip("this machine has no nft; the blocked-port case needs one")
	}
	const table = "caramelo_itest"
	install := strings.Join([]string{
		"sudo nft add table inet " + table,
		"sudo nft add chain inet " + table + " input '{ type filter hook input priority 0 ; }'",
		fmt.Sprintf("sudo nft add rule inet %s input udp dport %d drop", table, vpn.DefaultListenPort),
	}, " && ")
	res, err := m.Run(ctx, install)
	if err != nil {
		t.Fatalf("install the deny rule: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("install the deny rule: exit %d\nstdout:%s\nstderr:%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
		defer cancel()
		if res, err := m.Run(ctx, "sudo nft delete table inet "+table); err != nil || res.ExitCode != 0 {
			t.Fatalf("the deny rule was not removed: %v (exit %d: %s)", err, res.ExitCode, res.Stderr)
		}
		res, err := m.Run(ctx, "sudo nft list ruleset")
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("read the ruleset back: %v (exit %d: %s)", err, res.ExitCode, res.Stderr)
		}
		if strings.Contains(res.Stdout, table) {
			t.Fatalf("the deny rule is still in force; every later suite would start from a blocked machine:\n%s", res.Stdout)
		}
	})

	report, raw, err := runSetup(ctx, m, itest.CarameloBinary)
	if err == nil {
		t.Fatalf("setup reported success with udp %d dropped:\n%s", vpn.DefaultListenPort, raw)
	}
	var blocked *setuppkg.Result
	for i := range report.Results {
		if report.Results[i].Step == "firewall" {
			blocked = &report.Results[i]
		}
		if report.Results[i].Step == "vpn" {
			t.Errorf("the run reached the vpn step: a blocked tunnel port must stop setup first\n%s", raw)
		}
	}
	if blocked == nil || blocked.Status != setuppkg.StatusFailed {
		t.Fatalf("the firewall step did not fail: %+v\n%s", blocked, raw)
	}
	for _, want := range []string{fmt.Sprintf("udp %d", vpn.DefaultListenPort), "nft add rule"} {
		if !strings.Contains(blocked.Error, want) {
			t.Errorf("the failure %q does not mention %q", blocked.Error, want)
		}
	}

	forced, forcedRaw, err := runSetupWith(ctx, m, itest.CarameloBinary, "--force")
	if err != nil {
		t.Fatalf("--force did not get past the firewall: %v\n%s", err, forcedRaw)
	}
	if forced.Failed != 0 {
		t.Errorf("--force run failed %d step(s)\n%s", forced.Failed, forcedRaw)
	}
}

func TestZZProvisionForTheLab(t *testing.T) {
	_, m := begin(t)
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(10*time.Minute))
	defer cancel()

	kp, err := itest.LabPeerKey()
	if err != nil {
		t.Fatalf("the lab identity's key: %v", err)
	}
	cmd := itest.SetupCommandFor(m.Home(), kp.Public)
	if !strings.Contains(cmd, serverconfig.APIListenBoth) || !strings.Contains(cmd, "--edge") ||
		!strings.Contains(cmd, itest.PebbleDirectory) || !strings.Contains(cmd, itest.LabPeerName) {
		t.Fatalf("the driver provisions machines with a command this suite does not recognise: %s", cmd)
	}
	res, err := m.Run(ctx, cmd)
	if err != nil {
		t.Fatalf("hub setup for the lab: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("hub setup for the lab: exit %d\nstdout:%s\nstderr:%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	var report setuppkg.Report
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &report); err != nil {
		t.Fatalf("hub setup for the lab: stdout is not a report: %v\n%s", err, res.Stdout)
	}
	if report.Failed != 0 {
		t.Fatalf("%d step(s) failed\n%s", report.Failed, res.Stdout)
	}

	if err := itest.WaitForPort(ctx, m, itest.CarameloSSHPort); err != nil {
		t.Fatalf("the public API did not come back with api_listen=both: %v", err)
	}
	found := findPeer(peerList(t, m), itest.LabPeerName)
	if found == nil {
		t.Fatalf("the lab identity %q is not admitted", itest.LabPeerName)
	}
	if found.PublicKey != kp.Public {
		t.Errorf("%s holds %q, want the cached lab key %q", itest.LabPeerName, found.PublicKey, kp.Public)
	}
	t.Logf("%s admitted on %s; api_listen is %s for the lab", itest.LabPeerName, found.IP, serverconfig.APIListenBoth)

	t.Run("the edge is installed and listening", func(t *testing.T) {
		for _, unit := range []string{setuppkg.EdgeSocketUnit, setuppkg.EdgeServiceUnit} {
			active, err := m.Run(ctx, "systemctl is-active "+unit)
			if err != nil {
				t.Fatalf("systemctl is-active %s: %v", unit, err)
			}
			if got := strings.TrimSpace(active.Stdout); got != "active" {
				t.Errorf("%s is %q, want active", unit, got)
			}
		}
		for _, probe := range []struct{ what, cmd string }{
			{"tcp 80", "ss -H -ltn | grep -q ':80 '"},
			{"tcp 443", "ss -H -ltn | grep -q ':443 '"},
			{"udp 443", "ss -H -lun | grep -q ':443 '"},
		} {
			res, err := m.Run(ctx, probe.cmd)
			if err != nil {
				t.Fatalf("%s: %v", probe.what, err)
			}
			if res.ExitCode != 0 {
				t.Errorf("nothing is listening on %s after `hub setup --edge`", probe.what)
			}
		}
		cfg, err := m.Run(ctx, "sudo grep -E '^(edge|tls|acme_ca):' /etc/caramelo/config.yaml")
		if err != nil {
			t.Fatalf("read the machine config: %v", err)
		}
		if !strings.Contains(cfg.Stdout, itest.PebbleDirectory) {
			t.Errorf("config.yaml does not point acme_ca at the lab's CA:\n%s", cfg.Stdout)
		}
		if !strings.Contains(cfg.Stdout, "edge: true") {
			t.Errorf("config.yaml does not say the edge is on:\n%s", cfg.Stdout)
		}
	})
}

func TestZZSaveProvisioned(t *testing.T) {
	if startErr != nil {
		t.Fatalf("setup suite: %v", startErr)
	}
	if skipReason != "" {
		t.Skip("setup suite: " + skipReason)
	}
	if n := failures.Load(); n > 0 {
		t.Skipf("%d earlier test(s) failed; not building the %q state other suites start from", n, itest.StateProvisioned)
	}
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(30*time.Minute))
	defer cancel()

	if machine.Target() == itest.TargetDocker {
		hash, err := itest.ProvisionedHash()
		if err != nil {
			t.Fatalf("the provisioned hash: %v", err)
		}
		tag, err := itest.EnsureProvisionedImage(ctx)
		if err != nil {
			t.Fatalf("build the %q image: %v", itest.StateProvisioned, err)
		}
		if !strings.HasSuffix(tag, hash) {
			t.Errorf("image %q does not carry the hash %q of the binary it was built from", tag, hash)
		}
		t.Logf("the %q image other suites start from is %s", itest.StateProvisioned, tag)
	}

	fresh := itest.New(t, itest.Options{Suite: "setup-provisioned", State: itest.StateProvisioned})
	saved := fresh.Machine(itest.RoleHub)
	if err := itest.WaitForPort(ctx, saved, itest.CarameloSSHPort); err != nil {
		t.Fatalf("a machine in the %q state has no API: %v", itest.StateProvisioned, err)
	}
	kp, err := itest.LabPeerKey()
	if err != nil {
		t.Fatalf("the lab identity's key: %v", err)
	}
	found := findPeer(peerList(t, saved), itest.LabPeerName)
	if found == nil || found.PublicKey != kp.Public {
		t.Fatalf("a machine in the %q state does not admit %q: %+v", itest.StateProvisioned, itest.LabPeerName, found)
	}
	for _, unit := range []string{setuppkg.EdgeSocketUnit, setuppkg.EdgeServiceUnit} {
		active, err := saved.Run(ctx, "systemctl is-active "+unit)
		if err != nil {
			t.Fatalf("systemctl is-active %s: %v", unit, err)
		}
		if got := strings.TrimSpace(active.Stdout); got != "active" {
			t.Errorf("%s is %q on a machine in the %q state, want active", unit, got, itest.StateProvisioned)
		}
	}
}

func clientFor(t *testing.T, m *itest.Machine) *itest.Machine {
	t.Helper()
	if m.Target() == itest.TargetDocker {
		return commander
	}
	t.Logf("%s is not a container: the published port is probed from the machine itself, because a "+
		"commander container on the host reaches it through NAT and would never see its own address in the access log",
		m.Alias)
	return m
}

func peerList(t *testing.T, m *itest.Machine) []state.Peer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
	defer cancel()
	list, err := m.Run(ctx, "sudo -u "+itest.CarameloUser+" "+itest.CarameloBinary+" peer list --json")
	if err != nil {
		t.Fatalf("peer list: %v", err)
	}
	if list.ExitCode != 0 {
		t.Fatalf("peer list: exit %d\nstderr:%s", list.ExitCode, list.Stderr)
	}
	var peers []state.Peer
	if err := json.Unmarshal([]byte(strings.TrimSpace(list.Stdout)), &peers); err != nil {
		t.Fatalf("peer list --json: %v\nstdout: %q", err, list.Stdout)
	}
	return peers
}

func findPeer(peers []state.Peer, name string) *state.Peer {
	for i := range peers {
		if peers[i].Name == name {
			return &peers[i]
		}
	}
	return nil
}

func wireguardPublicKey(t *testing.T) string {
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

func sameNetwork(a, b string) bool {
	ap, aerr := netip.ParseAddr(a)
	bp, berr := netip.ParseAddr(b)
	if aerr != nil || berr != nil || !ap.Is4() || !bp.Is4() {
		return false
	}
	x, y := ap.As4(), bp.As4()
	return x[0] == y[0] && x[1] == y[1] && x[2] == y[2]
}

func getWithRetry(t *testing.T, from *itest.Machine, url string, timeout time.Duration) (body, clientIP string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout+itest.Scale(time.Minute))
	defer cancel()
	cmd := "curl -sS -m 10 -w '\\n%{http_code} %{local_ip}\\n' " + itest.ShellQuote(url)
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		res, err := from.Run(ctx, cmd)
		if err != nil {
			t.Fatalf("curl on %s: %v", from.Alias, err)
		}
		if res.ExitCode == 0 {
			b, code, ip := parseCurl(res.Stdout)
			if code == "200" {
				return b, ip
			}
			last = "HTTP " + code
		} else {
			last = fmt.Sprintf("curl exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("GET %s from %s never answered 200 within %s: %s", url, from.Alias, timeout, last)
	return "", ""
}

func parseCurl(stdout string) (body, code, clientIP string) {
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) == 0 {
		return "", "", ""
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) == 2 {
		code, clientIP = fields[0], fields[1]
	}
	return strings.Join(lines[:len(lines)-1], "\n"), code, clientIP
}

func TestTheTaskRunnerOnAHub(t *testing.T) {
	_, m := begin(t)
	t.Log("written while the container and end-to-end tiers are on hold, and not run")
	if firstRunErr != nil {
		t.Skip("hub setup failed; what the task runner does on this box proves nothing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(5*time.Minute))
	defer cancel()

	list := m.MustRun(t, itest.CarameloBinary+" task list")
	if !strings.Contains(list.Stdout, taskCommanderSetup) {
		t.Errorf("task list does not carry %s:\n%s", taskCommanderSetup, list.Stdout)
	}
	if !strings.Contains(list.Stdout, "Name this machine a commander") {
		t.Errorf("task list says nothing about what %s does:\n%s", taskCommanderSetup, list.Stdout)
	}

	show := m.MustRun(t, itest.CarameloBinary+" task show "+taskCommanderSetup)
	if first := strings.SplitN(show.Stdout, "\n", 2)[0]; first != "version: 1" {
		t.Errorf("task show opens with %q, want the file itself", first)
	}
	if strings.Contains(show.Stdout, "\n#") {
		t.Errorf("the embedded task carries a comment:\n%s", show.Stdout)
	}

	before := filesUnder(t, m, taskHubDirs(m))
	res := m.MustRun(t, itest.CarameloBinary+" task run "+taskCommanderSetup+" --dry-run --json")
	var report taskpkg.Report
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &report); err != nil {
		t.Fatalf("task run --dry-run --json is not a task.Report: %v\nstdout:\n%s", err, res.Stdout)
	}
	if !report.DryRun || report.Failed != 0 || report.Changed != 0 {
		t.Errorf("report = %+v, want a dry run that changed nothing", report)
	}
	for _, r := range taskLeaves(report.Results) {
		if r.Status != taskpkg.StatusOK && r.Status != taskpkg.StatusWouldChange {
			t.Errorf("item %s reported %q on a dry run", r.Name, r.Status)
		}
	}
	if after := filesUnder(t, m, taskHubDirs(m)); after != before {
		t.Errorf("a dry run on a hub wrote something:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}

	unknown, err := m.Run(ctx, itest.CarameloBinary+" task show nosuchtask")
	if err != nil {
		t.Fatalf("task show nosuchtask: %v", err)
	}
	if unknown.ExitCode != 2 {
		t.Errorf("task show nosuchtask: exit %d, want 2", unknown.ExitCode)
	}
	if !strings.Contains(unknown.Stderr, taskCommanderSetup) {
		t.Errorf("task show nosuchtask does not name the tasks there are: %q", unknown.Stderr)
	}
}

func TestTheTaskRunnerOnACommander(t *testing.T) {
	begin(t)
	t.Log("written while the container and end-to-end tiers are on hold, and not run")
	if commander == nil {
		t.Skip("setup suite: this lab has no commander container of its own")
	}
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(10*time.Minute))
	defer cancel()
	if err := itest.InstallBinaryOn(ctx, commander); err != nil {
		t.Fatalf("install the binary on %s: %v", commander.Alias, err)
	}

	const name = "commander"
	init, err := commander.Run(ctx, itest.CarameloBinary+" commander init --name "+name+" --json")
	if err != nil {
		t.Fatalf("commander init on %s: %v", commander.Alias, err)
	}
	if init.ExitCode != 0 {
		t.Fatalf("commander init on %s: exit %d\nstdout:\n%sstderr:\n%s",
			commander.Alias, init.ExitCode, init.Stdout, init.Stderr)
	}

	res, err := commander.Run(ctx, itest.CarameloBinary+" task run "+taskCommanderSetup+" --json")
	if err != nil {
		t.Fatalf("task run on %s: %v", commander.Alias, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("task run on %s: exit %d\nstdout:\n%sstderr:\n%s",
			commander.Alias, res.ExitCode, res.Stdout, res.Stderr)
	}
	var report taskpkg.Report
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &report); err != nil {
		t.Fatalf("task run --json is not a task.Report: %v\nstdout:\n%s", err, res.Stdout)
	}
	if report.Changed != 0 || report.Failed != 0 {
		t.Errorf("the run after commander init reported %+v, want it to change nothing", report)
	}
	for _, r := range taskLeaves(report.Results) {
		if r.Status != taskpkg.StatusOK {
			t.Errorf("item %s reported %q, want ok on a commander that is already one", r.Name, r.Status)
		}
	}

	got := itest.MustContextOn(t, commander, itest.CarameloBinary)
	if got.Role != place.RoleCommander || got.Name != name {
		t.Errorf("after the task the box says %q %q, want a commander still called %s", got.Role, got.Name, name)
	}
}

const taskCommanderSetup = "commander-setup"

func taskHubDirs(m *itest.Machine) []string {
	home := itest.HomeOf(m)
	return []string{home + "/.config", home + "/.cache"}
}

func filesUnder(t *testing.T, m *itest.Machine, dirs []string) string {
	t.Helper()
	res := m.MustRun(t, "find "+strings.Join(quoteAll(dirs), " ")+" 2>/dev/null | sort || true")
	return res.Stdout
}

func quoteAll(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, itest.ShellQuote(p))
	}
	return out
}

func taskLeaves(results []taskpkg.Result) []taskpkg.Result {
	var out []taskpkg.Result
	for _, r := range results {
		if r.Block {
			out = append(out, taskLeaves(r.Results)...)
			continue
		}
		out = append(out, r)
	}
	return out
}
