//go:build integration

package machine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/test/integration/itest"
)

func TestProvisionedMachineAnswersBothRoutes(t *testing.T) {
	lab := itest.New(t, itest.Options{Suite: "machine", State: itest.StateProvisioned})
	m := lab.Machine(itest.RoleHub)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(5*time.Minute))
	defer cancel()
	if err := itest.WaitForPort(ctx, m, itest.CarameloSSHPort); err != nil {
		t.Fatalf("caramelod on %s: %v", m.Alias, err)
	}

	sshHome := itest.CommanderHomeNoPeer(t, m)
	overSSH := itest.CommanderOK(t, itest.CommanderOptions{Env: itest.CommanderEnv(sshHome)},
		"--machine", m.CommanderTarget(t), "status", "--json")
	checkStatus(t, "ssh", overSSH.Stdout, m.Hostname)

	tunnelHome := itest.CommanderHome(t, m)
	overTunnel := itest.CommanderOK(t, itest.CommanderOptions{Env: itest.CommanderEnv(tunnelHome)},
		"--machine", m.TunnelTarget(t), "status", "--json")
	checkStatus(t, "tunnel", overTunnel.Stdout, m.Hostname)
}

func checkStatus(t *testing.T, wantTransport, stdout, wantHostname string) {
	t.Helper()
	var st struct {
		Hostname  string `json:"hostname"`
		Transport string `json:"transport"`
		Docker    struct {
			Running  bool `json:"running"`
			Rootless bool `json:"rootless"`
		} `json:"docker"`
		VPN *struct {
			Enabled bool `json:"enabled"`
			Peers   int  `json:"peers"`
		} `json:"vpn"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &st); err != nil {
		t.Fatalf("status over %s is not JSON: %v\nstdout: %q", wantTransport, err, stdout)
	}
	if st.Transport != wantTransport {
		t.Errorf("status reached the machine over %q, want %q", st.Transport, wantTransport)
	}
	if st.Hostname != wantHostname {
		t.Errorf("status hostname = %q, want %q", st.Hostname, wantHostname)
	}
	if !st.Docker.Running || !st.Docker.Rootless {
		t.Errorf("docker over %s: running=%v rootless=%v, want both true",
			wantTransport, st.Docker.Running, st.Docker.Rootless)
	}
	if st.VPN == nil || !st.VPN.Enabled {
		t.Errorf("the network is not up over %s: %+v", wantTransport, st.VPN)
	} else if st.VPN.Peers < 1 {
		t.Errorf("the machine admits %d peers over %s, want the lab identity", st.VPN.Peers, wantTransport)
	}
}
