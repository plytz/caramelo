//go:build integration

package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	capi "github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/remote"
	setuppkg "github.com/plytz/caramelo/internal/setup"
	"github.com/plytz/caramelo/internal/state"
	taskpkg "github.com/plytz/caramelo/internal/task"
	"github.com/plytz/caramelo/internal/vpn"
	"github.com/plytz/caramelo/internal/vpnclient"
	"github.com/plytz/caramelo/test/integration/itest"
)

func TestBootstrap(t *testing.T) {
	begin(t)
	if firstErr != nil {
		t.Fatalf("bootstrap failed: %v\noutput:\n%s", firstErr, firstRaw)
	}
	for _, r := range first.Setup.Results {
		t.Logf("step %-18s %-12s %s%s", r.Step, r.Status, r.Detail, r.Error)
	}
	if first.Probe.OS != "linux" || first.Probe.Arch != boxArch || first.Probe.Privilege != "sudo" {
		t.Errorf("probe = %+v, want linux/%s with sudo", first.Probe, boxArch)
	}
	if first.Setup.Failed != 0 || first.Setup.Changed == 0 {
		t.Errorf("setup report: %d failed, %d changed\noutput:\n%s", first.Setup.Failed, first.Setup.Changed, firstRaw)
	}
	if first.Fleet == nil {
		t.Fatalf("no fleet recorded\noutput:\n%s", firstRaw)
	}
	if first.Fleet.Name != machineName || !first.Fleet.Default ||
		!strings.HasPrefix(first.Fleet.Hub, itest.CarameloUser+"@") ||
		!strings.HasSuffix(first.Fleet.Hub, fmt.Sprintf(":%d", itest.CarameloSSHPort)) {
		t.Errorf("fleet = %+v", first.Fleet)
	}
	if !first.Verified {
		t.Errorf("the API was not verified from the commander\noutput:\n%s", firstRaw)
	}
	var st capi.Status
	if err := json.Unmarshal(first.Status, &st); err != nil || st.Transport != remote.KindTunnel {
		t.Errorf("status = %s (%v), want transport %s", first.Status, err, remote.KindTunnel)
	}

	cfg := commanderConfig(t)
	if cfg.Commander.DefaultFleet != machineName || cfg.Commander.Fleets[machineName].Hub != first.Fleet.Hub {
		t.Errorf("commander config = %+v, want default %s -> %s", cfg, machineName, first.Fleet.Hub)
	}
	if key := cfg.Commander.Fleets[machineName].PublicKey; key == "" {
		t.Errorf("commander config = %+v, want the hub's key pinned for the fleet on first contact", cfg)
	}
}

func TestGossSetup(t *testing.T) {
	m := begin(t)
	if firstErr != nil {
		t.Skip("bootstrap failed; the machine state is not worth checking")
	}
	itest.RunGoss(t, m, itest.MustGossSpec(t, "setup.yaml"))
}

func TestNothingLeftInTmp(t *testing.T) {
	m := begin(t)
	res := m.MustRun(t, "ls -d /tmp/caramelo-setup.* 2>/dev/null || true")
	if strings.TrimSpace(res.Stdout) != "" {
		t.Errorf("bootstrap left %s on the box", strings.TrimSpace(res.Stdout))
	}
}

func TestCommanderUsesTheNewDefaultFleet(t *testing.T) {
	begin(t)
	if firstErr != nil {
		t.Skip("bootstrap failed")
	}
	st := commanderStatus(t)
	if st.Transport != remote.KindTunnel {
		t.Errorf("transport = %q, want %q: a bootstrapped commander holds a peer key, "+
			"which the commander prefers over the system ssh", st.Transport, remote.KindTunnel)
	}
}

func TestBootstrapJoinedTheNetwork(t *testing.T) {
	begin(t)
	if firstErr != nil {
		t.Skip("bootstrap failed")
	}

	key := peerKeyDir + "/" + vpnclient.KeyFileName(machineName)
	mode := strings.TrimSpace(commander.MustRun(t, "stat -c %a "+itest.ShellQuote(key)).Stdout)
	if mode != "600" {
		t.Errorf("%s mode = %s, want 600", key, mode)
	}

	peers := commanderPeers(t)
	if len(peers) == 0 {
		t.Fatal("the machine has no peers after a bootstrap; the commander was never admitted")
	}
	prefix, err := netip.ParsePrefix(vpn.DefaultSubnet)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range peers {
		t.Logf("peer %s %s added_by=%s", p.Name, p.IP, p.AddedBy)
		ip, err := netip.ParseAddr(p.IP)
		if err != nil {
			t.Errorf("peer %s has %q as an address: %v", p.Name, p.IP, err)
			continue
		}
		if !prefix.Contains(ip) {
			t.Errorf("peer %s address %s is outside %s", p.Name, ip, prefix)
		}
	}

	joined := commanderVPNState(t)
	if joined.PeerName == "" || !joined.IP.IsValid() {
		t.Errorf("vpn status = %+v, want the identity and address the bootstrap registered", joined)
	}
}

func TestTheMachineAdmittedOnlyTheCommander(t *testing.T) {
	begin(t)
	if firstErr != nil {
		t.Skip("bootstrap failed")
	}
	joined := commanderVPNState(t)
	peers := commanderPeers(t)
	if len(peers) != 1 {
		t.Fatalf("the machine has %d peers after one bootstrap, want exactly the commander: %+v", len(peers), peers)
	}
	if peers[0].PublicKey != joined.PublicKey {
		t.Errorf("the admitted peer holds %q, and the commander holds %q: the key the bootstrap passed "+
			"as --peer is not the commander's", peers[0].PublicKey, joined.PublicKey)
	}
	if peers[0].Name != joined.PeerName {
		t.Errorf("the admitted peer is named %q and the commander calls itself %q", peers[0].Name, joined.PeerName)
	}
}

func TestThePrivateKeyStayedOnTheCommander(t *testing.T) {
	m := begin(t)
	if firstErr != nil {
		t.Skip("bootstrap failed")
	}
	key := peerKeyDir + "/" + vpnclient.KeyFileName(machineName)
	private := strings.TrimSpace(commander.MustRun(t, "cat "+itest.ShellQuote(key)).Stdout)
	if private == "" {
		t.Fatalf("no private key at %s on %s", key, commander.Alias)
	}
	res := m.MustRun(t, "sudo -n grep -rlF "+itest.ShellQuote(private)+" /var/lib/caramelo /etc/caramelo /tmp; true")
	if found := strings.TrimSpace(res.Stdout); found != "" {
		t.Errorf("the commander's private key is on the machine, in %s", strings.Join(strings.Fields(found), " "))
	}
}

func TestIdempotent(t *testing.T) {
	m := begin(t)
	if firstErr != nil {
		t.Skip("first run failed; a second run proves nothing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.Budget().For(10*time.Minute))
	defer cancel()
	res, raw, err := bootstrap(ctx, "--name", machineName)
	if err != nil {
		t.Fatalf("second bootstrap failed: %v\noutput:\n%s", err, raw)
	}
	for _, r := range res.Setup.Results {
		t.Logf("step %-18s %-12s %s", r.Step, r.Status, r.Detail)
	}
	if res.Setup.Changed != 0 || res.Setup.Failed != 0 {
		t.Errorf("second run: %d changed, %d failed; want 0 and 0", res.Setup.Changed, res.Setup.Failed)
		explainNotIdempotent(t, m)
	}
	if !res.Verified || res.Fleet == nil || !res.Fleet.Default {
		t.Errorf("second run result = %+v", res)
	}
}

func TestTheMachineComesBackAfterAPowerCycle(t *testing.T) {
	m := begin(t)
	if firstErr != nil {
		t.Skip("bootstrap failed")
	}
	before := m.MustAddress(t)
	itest.MustRestart(t, m)
	if after := m.MustAddress(t); after != before {
		t.Fatalf("%s came back at a new address (%s, was %s); the commander config the bootstrap "+
			"wrote names the old one", m.Alias, after, before)
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.Budget().For(3*time.Minute))
	defer cancel()
	if err := itest.WaitForCaramelod(ctx, m); err != nil {
		t.Fatalf("caramelod did not come back after the power cycle: %v", err)
	}
	st := commanderStatus(t)
	if st.Transport != remote.KindTunnel {
		t.Errorf("transport after the power cycle = %q, want %q", st.Transport, remote.KindTunnel)
	}
	if st.VPN == nil || !st.VPN.Enabled {
		t.Errorf("the machine reports no working network after the power cycle: %+v", st.VPN)
	}
}

func commanderConfig(t *testing.T) remote.CommanderConfig {
	t.Helper()
	local := filepath.Join(t.TempDir(), "config.yaml")
	ctx, cancel := context.WithTimeout(context.Background(), commander.Budget().For(time.Minute))
	defer cancel()
	if err := commander.Fetch(ctx, commanderConfigPath, local); err != nil {
		t.Fatalf("read the commander config from %s: %v", commander.Alias, err)
	}
	if info, err := os.Stat(local); err == nil && info.IsDir() {
		local = filepath.Join(local, "config.yaml")
	}
	cfg, err := remote.LoadCommanderConfigFrom(local)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func commanderJSON(t *testing.T, v any, args ...string) {
	t.Helper()
	res := commander.MustRun(t, commanderBin+" "+strings.Join(args, " "))
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), v); err != nil {
		t.Fatalf("caramelo %s: %v\nstdout: %q\nstderr: %q", strings.Join(args, " "), err, res.Stdout, res.Stderr)
	}
}

func commanderStatus(t *testing.T) capi.Status {
	t.Helper()
	var st capi.Status
	commanderJSON(t, &st, "status", "--json")
	return st
}

func commanderPeers(t *testing.T) []state.Peer {
	t.Helper()
	var peers []state.Peer
	commanderJSON(t, &peers, "peer", "list", "--json")
	return peers
}

func commanderVPNState(t *testing.T) vpnclient.State {
	t.Helper()
	var v vpnclient.State
	commanderJSON(t, &v, "vpn", "status", "--json")
	return v
}

func explainNotIdempotent(t *testing.T, m *itest.Machine) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), m.Budget().For(2*time.Minute))
	defer cancel()
	res, err := m.Run(ctx, "sudo -n "+itest.CarameloBinary+" fleet setup --yes --json --dry-run")
	if err != nil {
		t.Logf("dry run to explain the difference: %v", err)
		return
	}
	var report setuppkg.Report
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &report); err != nil {
		t.Logf("dry run to explain the difference: %v\n%s", err, res.Stdout)
		return
	}
	for _, r := range report.Results {
		if r.Status == setuppkg.StatusWouldChange {
			t.Logf("still not done: step %s: %s", r.Step, r.Detail)
		}
	}
}

func TestCommanderInitWroteEveryFileACommanderNeeds(t *testing.T) {
	begin(t)
	if initErr != nil {
		t.Fatalf("commander init failed: %v\noutput:\n%s", initErr, initRaw)
	}
	dir := commanderHome + "/.config/caramelo"
	for _, want := range []struct{ path, mode string }{
		{dir, "700"},
		{dir + "/config.yaml", "600"},
		{dir + "/" + vpnclient.IdentityKeyFile, "600"},
		{peerKeyDir, "700"},
		{commanderHome + "/.cache/caramelo", "700"},
	} {
		res, err := commander.Run(context.Background(), "stat -c %a "+itest.ShellQuote(want.path))
		if err != nil || res.ExitCode != 0 {
			t.Errorf("%s: %v (exit %d) %s", want.path, err, res.ExitCode, res.Stderr)
			continue
		}
		if got := strings.TrimSpace(res.Stdout); got != want.mode {
			t.Errorf("%s mode = %s, want %s", want.path, got, want.mode)
		}
	}
	if cfg := commanderConfig(t); cfg.Name != commanderName || cfg.Role != remote.RoleCommander {
		t.Errorf("commander config = %+v, want %s as a %s", cfg, commanderName, remote.RoleCommander)
	}

	res := commander.MustRun(t, commanderBin+" commander init --json")
	var again struct {
		Name    string `json:"name"`
		Changed bool   `json:"changed"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &again); err != nil {
		t.Fatalf("commander init --json: %v\nstdout: %q", err, res.Stdout)
	}
	if again.Changed || again.Name != commanderName {
		t.Errorf("a second commander init reported %+v, want %s unchanged", again, commanderName)
	}

	run := commander.MustRun(t, commanderBin+" task run commander-setup --json")
	var report taskpkg.Report
	if err := json.Unmarshal([]byte(strings.TrimSpace(run.Stdout)), &report); err != nil {
		t.Fatalf("task run commander-setup --json: %v\nstdout: %q", err, run.Stdout)
	}
	if report.Changed != 0 || report.Failed != 0 {
		t.Errorf("the task behind commander init reported %+v on a commander, want it to change nothing", report)
	}
	for _, r := range taskLeaves(report.Results) {
		if r.Status != taskpkg.StatusOK {
			t.Errorf("item %s reported %q, want ok on a box commander init already named", r.Name, r.Status)
		}
	}
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

func TestAFreshBoxRefusesToSetUpAnotherMachine(t *testing.T) {
	begin(t)
	target, err := itest.SSHTarget(context.Background(), machine)
	if err != nil {
		t.Fatal(err)
	}
	fresh := "/tmp/fresh-box"
	commander.MustRun(t, "rm -rf "+fresh+" && mkdir -p "+fresh)
	cmd := fmt.Sprintf("env HOME=%s XDG_CONFIG_HOME=%s/.config %s fleet setup --target %s --yes",
		fresh, fresh, commanderBin, target)
	res, err := commander.Run(context.Background(), cmd)
	if err != nil && res.ExitCode == 0 {
		t.Fatalf("%s: %v", cmd, err)
	}
	if res.ExitCode != 2 {
		t.Errorf("exit = %d, want 2\nstdout:\n%sstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "caramelo commander init") {
		t.Errorf("stderr = %q, want it to name 'caramelo commander init'", res.Stderr)
	}
}
