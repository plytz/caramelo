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
	if first.Machine == nil {
		t.Fatalf("no machine recorded\noutput:\n%s", firstRaw)
	}
	if first.Machine.Name != machineName || !first.Machine.Default ||
		!strings.HasPrefix(first.Machine.Address, itest.CarameloUser+"@") ||
		!strings.HasSuffix(first.Machine.Address, fmt.Sprintf(":%d", itest.CarameloSSHPort)) {
		t.Errorf("machine = %+v", first.Machine)
	}
	if !first.Verified {
		t.Errorf("the API was not verified from the laptop\noutput:\n%s", firstRaw)
	}
	var st capi.Status
	if err := json.Unmarshal(first.Status, &st); err != nil || st.Transport != remote.KindTunnel {
		t.Errorf("status = %s (%v), want transport %s", first.Status, err, remote.KindTunnel)
	}

	cfg := clientConfig(t)
	if cfg.DefaultMachine != machineName || cfg.Machines[machineName] != first.Machine.Address {
		t.Errorf("client config = %+v, want default %s -> %s", cfg, machineName, first.Machine.Address)
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

func TestClientUsesTheNewDefaultMachine(t *testing.T) {
	begin(t)
	if firstErr != nil {
		t.Skip("bootstrap failed")
	}
	st := laptopStatus(t)
	if st.Transport != remote.KindTunnel {
		t.Errorf("transport = %q, want %q: a bootstrapped laptop holds a peer key, "+
			"which the client prefers over the system ssh", st.Transport, remote.KindTunnel)
	}
}

func TestBootstrapJoinedTheNetwork(t *testing.T) {
	begin(t)
	if firstErr != nil {
		t.Skip("bootstrap failed")
	}

	key := peerKeyDir + "/" + vpnclient.KeyFileName(machineName)
	mode := strings.TrimSpace(laptop.MustRun(t, "stat -c %a "+itest.ShellQuote(key)).Stdout)
	if mode != "600" {
		t.Errorf("%s mode = %s, want 600", key, mode)
	}

	peers := laptopPeers(t)
	if len(peers) == 0 {
		t.Fatal("the machine has no peers after a bootstrap; the laptop was never admitted")
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

	joined := laptopVPNState(t)
	if joined.PeerName == "" || !joined.IP.IsValid() {
		t.Errorf("vpn status = %+v, want the identity and address the bootstrap registered", joined)
	}
}

func TestTheMachineAdmittedOnlyTheLaptop(t *testing.T) {
	begin(t)
	if firstErr != nil {
		t.Skip("bootstrap failed")
	}
	joined := laptopVPNState(t)
	peers := laptopPeers(t)
	if len(peers) != 1 {
		t.Fatalf("the machine has %d peers after one bootstrap, want exactly the laptop: %+v", len(peers), peers)
	}
	if peers[0].PublicKey != joined.PublicKey {
		t.Errorf("the admitted peer holds %q, and the laptop holds %q: the key the bootstrap passed "+
			"as --peer is not this computer's", peers[0].PublicKey, joined.PublicKey)
	}
	if peers[0].Name != joined.PeerName {
		t.Errorf("the admitted peer is named %q and the laptop calls itself %q", peers[0].Name, joined.PeerName)
	}
}

func TestThePrivateKeyStayedOnTheLaptop(t *testing.T) {
	m := begin(t)
	if firstErr != nil {
		t.Skip("bootstrap failed")
	}
	key := peerKeyDir + "/" + vpnclient.KeyFileName(machineName)
	private := strings.TrimSpace(laptop.MustRun(t, "cat "+itest.ShellQuote(key)).Stdout)
	if private == "" {
		t.Fatalf("no private key at %s on %s", key, laptop.Alias)
	}
	res := m.MustRun(t, "sudo -n grep -rlF "+itest.ShellQuote(private)+" /var/lib/caramelo /etc/caramelo /tmp; true")
	if found := strings.TrimSpace(res.Stdout); found != "" {
		t.Errorf("the laptop's private key is on the machine, in %s", strings.Join(strings.Fields(found), " "))
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
	if !res.Verified || res.Machine == nil || !res.Machine.Default {
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
		t.Fatalf("%s came back at a new address (%s, was %s); the client config the bootstrap "+
			"wrote names the old one", m.Alias, after, before)
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.Budget().For(3*time.Minute))
	defer cancel()
	if err := itest.WaitForCaramelod(ctx, m); err != nil {
		t.Fatalf("caramelod did not come back after the power cycle: %v", err)
	}
	st := laptopStatus(t)
	if st.Transport != remote.KindTunnel {
		t.Errorf("transport after the power cycle = %q, want %q", st.Transport, remote.KindTunnel)
	}
	if st.VPN == nil || !st.VPN.Enabled {
		t.Errorf("the machine reports no working network after the power cycle: %+v", st.VPN)
	}
}

func clientConfig(t *testing.T) remote.ClientConfig {
	t.Helper()
	local := filepath.Join(t.TempDir(), "config.yaml")
	ctx, cancel := context.WithTimeout(context.Background(), laptop.Budget().For(time.Minute))
	defer cancel()
	if err := laptop.Fetch(ctx, clientConfigPath, local); err != nil {
		t.Fatalf("read the client config from %s: %v", laptop.Alias, err)
	}
	if info, err := os.Stat(local); err == nil && info.IsDir() {
		local = filepath.Join(local, "config.yaml")
	}
	cfg, err := remote.LoadClientConfigFrom(local)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func laptopClient(t *testing.T, v any, args ...string) {
	t.Helper()
	res := laptop.MustRun(t, laptopBin+" "+strings.Join(args, " "))
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), v); err != nil {
		t.Fatalf("caramelo %s: %v\nstdout: %q\nstderr: %q", strings.Join(args, " "), err, res.Stdout, res.Stderr)
	}
}

func laptopStatus(t *testing.T) capi.Status {
	t.Helper()
	var st capi.Status
	laptopClient(t, &st, "status", "--json")
	return st
}

func laptopPeers(t *testing.T) []state.Peer {
	t.Helper()
	var peers []state.Peer
	laptopClient(t, &peers, "peer", "list", "--json")
	return peers
}

func laptopVPNState(t *testing.T) vpnclient.State {
	t.Helper()
	var v vpnclient.State
	laptopClient(t, &v, "vpn", "status", "--json")
	return v
}

func explainNotIdempotent(t *testing.T, m *itest.Machine) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), m.Budget().For(2*time.Minute))
	defer cancel()
	res, err := m.Run(ctx, "sudo -n "+itest.CarameloBinary+" server setup --yes --json --dry-run")
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
