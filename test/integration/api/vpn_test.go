//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	capi "github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/vpn"
	"github.com/plytz/caramelo/internal/vpnclient"
	"github.com/plytz/caramelo/test/integration/itest"
)

const apiPeer = "api-itest-peer"

func tunnelBox(t *testing.T) (*itest.Machine, *itest.Machine, itest.SSHAPIOptions) {
	t.Helper()
	lab := itest.New(t, itest.Options{
		Suite:      suite,
		State:      itest.StateProvisioned,
		Roles:      []string{itest.RoleHub},
		Commanders: []string{itest.RoleCommander},
	})
	m, o := ready(t, lab.Machine(itest.RoleHub))
	commander := lab.Commander(itest.RoleCommander)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
	defer cancel()
	if err := itest.InstallBinaryOn(ctx, commander); err != nil {
		t.Fatalf("install the commander on %s: %v", commander.Alias, err)
	}
	mustOnCommander(t, commander, nil, itest.Scale(time.Minute), "commander", "init", "--json")
	return m, commander, o
}

func onCommander(t *testing.T, l *itest.Machine, env []string, timeout time.Duration, args ...string) itest.Result {
	t.Helper()
	line := "env"
	for _, kv := range env {
		line += " " + itest.ShellQuote(kv)
	}
	line += " " + itest.CarameloBinary
	for _, a := range args {
		line += " " + itest.ShellQuote(a)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res, err := l.Run(ctx, line)
	if err != nil {
		t.Fatalf("[%s] %s: %v\nstdout:%s\nstderr:%s", l.Alias, line, err, res.Stdout, res.Stderr)
	}
	return res
}

func mustOnCommander(t *testing.T, l *itest.Machine, env []string, timeout time.Duration, args ...string) itest.Result {
	t.Helper()
	res := onCommander(t, l, env, timeout, args...)
	if res.ExitCode != 0 {
		t.Fatalf("[%s] caramelo %s: exit %d\nstdout:%s\nstderr:%s",
			l.Alias, strings.Join(args, " "), res.ExitCode, res.Stdout, res.Stderr)
	}
	return res
}

func TestTunnelSessionAndIdentity(t *testing.T) {
	m, commander, o := tunnelBox(t)
	target := itest.CarameloUser + "@" + m.MustAddress(t)
	env := []string{"CARAMELO_MACHINE=" + target}

	t.Cleanup(func() {
		itest.SSHAPIRun(t, o, "peer", "remove", apiPeer)
	})

	up := mustOnCommander(t, commander, env, itest.Scale(3*time.Minute), "vpn", "up", "--name", apiPeer, "--json")
	st := decodeJSON[vpnclient.State](t, "vpn up", up.Stdout)
	if st.PeerName != apiPeer || !st.IP.IsValid() {
		t.Fatalf("vpn up = %+v, want the peer %q with an address", st, apiPeer)
	}

	commander.MustRun(t, "rm -rf "+commander.Home()+"/.ssh")
	commander.MustRun(t, "sudo -n rm -f /usr/bin/ssh /bin/ssh /usr/local/bin/ssh")
	if res := commander.MustRun(t, "command -v ssh || true"); strings.TrimSpace(res.Stdout) != "" {
		t.Fatalf("ssh is still on %s at %q; the tunnel would not be the only way in",
			commander.Alias, strings.TrimSpace(res.Stdout))
	}

	out := mustOnCommander(t, commander, env, itest.Scale(2*time.Minute), "status", "--json")
	status := decodeJSON[capi.Status](t, "status", out.Stdout)
	if status.Transport != remote.KindTunnel {
		t.Errorf("transport = %q, want %q", status.Transport, remote.KindTunnel)
	}
	if status.Identity != apiPeer {
		t.Errorf("identity = %q, want the peer name %q", status.Identity, apiPeer)
	}

	own := onCommander(t, commander, env, itest.Scale(time.Minute), "peer", "remove", apiPeer)
	if own.ExitCode == 0 {
		t.Errorf("a peer revoked its own identity through its own tunnel and got an answer:\n%s", own.Stdout)
	}
	if !strings.Contains(own.Stderr, "--force") {
		t.Errorf("the refusal does not say how to do it anyway: %q", own.Stderr)
	}

	if res := itest.SSHAPIRun(t, o, "peer", "remove", apiPeer); res.ExitCode != 0 {
		t.Fatalf("revoking %s from the machine: exit %d\nstderr:%s", apiPeer, res.ExitCode, res.Stderr)
	}
	started := time.Now()
	gone := onCommander(t, commander, env, itest.Scale(3*time.Minute), "status", "--json")
	switch gone.ExitCode {
	case 0:
		t.Errorf("the machine still answered %s after the peer was revoked:\n%s",
			time.Since(started).Round(time.Second), gone.Stdout)
	case 1:
		t.Logf("gave up after %s: %s", time.Since(started).Round(time.Second), strings.TrimSpace(gone.Stderr))
		if !strings.Contains(strings.ToLower(gone.Stderr), "handshake") {
			t.Errorf("the error does not explain the silence (no handshake): %q", gone.Stderr)
		}
	default:
		t.Errorf("exit = %d, want 1 for a machine that cannot be reached through the tunnel\nstderr:%s",
			gone.ExitCode, gone.Stderr)
	}
}

func TestTunnelSessionFromTheHost(t *testing.T) {
	m, o := box(t)
	home := itest.CommanderHome(t, m)
	res := itest.CommanderOK(t, itest.CommanderOptions{Env: itest.CommanderEnv(home)},
		"--machine", m.TunnelTarget(t), "status", "--json")
	st := decodeStatus(t, res.Stdout)
	if st.Transport != remote.KindTunnel {
		t.Errorf("transport = %q, want %q", st.Transport, remote.KindTunnel)
	}
	if st.Identity != itest.LabPeerName {
		t.Errorf("identity = %q, want the lab peer %q", st.Identity, itest.LabPeerName)
	}

	overSSH := itest.SSHAPIRun(t, o, "status", "--json")
	if overSSH.ExitCode != 0 {
		t.Fatalf("status over ssh: exit %d\nstderr:%s", overSSH.ExitCode, overSSH.Stderr)
	}
	if got := decodeStatus(t, overSSH.Stdout); got.HostKeyFingerprint != st.HostKeyFingerprint {
		t.Errorf("the two transports reached different daemons: host key %q over the tunnel, %q over ssh",
			st.HostKeyFingerprint, got.HostKeyFingerprint)
	}
}

func TestVPNStatusOfTheMachine(t *testing.T) {
	_, o := box(t)
	res := itest.SSHAPIRun(t, o, "status", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("status: exit %d\nstderr:%s", res.ExitCode, res.Stderr)
	}
	st := decodeStatus(t, res.Stdout)
	if st.VPN == nil {
		t.Fatal("status carries no tunnel section")
	}
	if !st.VPN.Enabled {
		t.Fatalf("the machine's network is not up: %+v", st.VPN)
	}
	if st.VPN.PublicKey == "" {
		t.Error("the machine reports no public key; a peer cannot configure itself without it")
	}
	if want := fmt.Sprintf(":%d", vpn.DefaultListenPort); !strings.HasSuffix(st.VPN.Listen, want) {
		t.Errorf("listen = %q, want it to end in %q", st.VPN.Listen, want)
	}
	if st.VPN.Subnet != vpn.DefaultSubnet {
		t.Errorf("subnet = %q, want %q", st.VPN.Subnet, vpn.DefaultSubnet)
	}
	if st.VPN.Resolver == "" {
		t.Error("no resolver address: .internal has to be answered somewhere")
	}
	if !slices.Contains(serverconfig.VPNModeValues, st.VPN.Mode) {
		t.Errorf("mode = %q, want one of %s", st.VPN.Mode, strings.Join(serverconfig.VPNModeValues, ", "))
	}
	switch st.VPN.APIListen {
	case serverconfig.APIListenVPN, serverconfig.APIListenPublic, serverconfig.APIListenBoth:
	default:
		t.Errorf("api_listen = %q, want one of vpn, public, both", st.VPN.APIListen)
	}
}

func TestPublicListenerMatchesAPIListen(t *testing.T) {
	m, o := box(t)
	opts := o
	opts.Timeout = 20 * time.Second
	opts.AllowTimeout = true
	res := itest.SSHAPIRun(t, opts, "status", "--json")

	local, err := m.Run(context.Background(), itest.CarameloBinary+" status --json")
	if err != nil {
		t.Fatalf("status on the box: %v", err)
	}
	st := decodeStatus(t, local.Stdout)
	if st.VPN == nil {
		t.Fatal("status carries no tunnel section")
	}

	if st.VPN.APIListen == serverconfig.APIListenVPN {
		if res.ExitCode == 0 {
			t.Errorf("api_listen=vpn but the public listener answered:\n%s", res.Stdout)
		}
		return
	}
	if res.ExitCode != 0 {
		t.Errorf("api_listen=%q but the public listener did not answer: exit %d\nstderr:%s",
			st.VPN.APIListen, res.ExitCode, res.Stderr)
	}
	t.Logf("api_listen is %q; the flip to %q is the milestone's last step",
		st.VPN.APIListen, serverconfig.APIListenVPN)
}

func decodeJSON[T any](t *testing.T, what, stdout string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &v); err != nil {
		t.Fatalf("%s --json: %v\nstdout: %q", what, err, stdout)
	}
	return v
}
