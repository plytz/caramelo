package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/api"
)

func vpnStatusFixture() *api.VPNStatus {
	return &api.VPNStatus{
		Enabled:   true,
		PublicKey: "Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE=",
		Listen:    "0.0.0.0:4021",
		Subnet:    "10.86.0.0/16",
		Address:   "10.86.0.1",
		Resolver:  "10.86.0.1:53",
		APIListen: "both",
		Peers:     2,
		Envs:      3,
		Routes:    7,
	}
}

func TestStatusShowsTheTunnel(t *testing.T) {
	st := statusFixture()
	st.VPN = vpnStatusFixture()
	code, stdout, _ := runWithService(t, &fakeAPI{status: st}, "status")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{
		"tunnel", "10.86.0.1 on 0.0.0.0:4021 (udp)", "subnet 10.86.0.0/16", "resolver 10.86.0.1:53",
		"2, 3 environment(s) addressed, 7 relayed port(s)",

		"Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE=",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestStatusSaysWhereTheAPIListens(t *testing.T) {
	for _, tc := range []struct{ listen, want string }{
		{"vpn", "4022 (inside the tunnel only)"},
		{"both", "4022 (public and inside the tunnel)"},

		{"public", "4022 (public only)"},
	} {
		st := statusFixture()
		st.VPN = vpnStatusFixture()
		st.VPN.APIListen = tc.listen
		_, stdout, _ := runWithService(t, &fakeAPI{status: st}, "status")
		if !strings.Contains(stdout, tc.want) {
			t.Errorf("api_listen %q: stdout missing %q:\n%s", tc.listen, tc.want, stdout)
		}
	}
}

func TestStatusOnAMachineWithoutANetwork(t *testing.T) {
	st := statusFixture()
	st.VPN = &api.VPNStatus{Error: "setup predates M5"}
	_, stdout, _ := runWithService(t, &fakeAPI{status: st}, "status")
	if !strings.Contains(stdout, "not running: setup predates M5") {
		t.Errorf("stdout = %s", stdout)
	}

	st.VPN = nil
	_, stdout, _ = runWithService(t, &fakeAPI{status: st}, "status")
	if strings.Contains(stdout, "tunnel") {
		t.Errorf("a machine with no network reported one:\n%s", stdout)
	}
}

func TestStatusJSONCarriesTheTunnel(t *testing.T) {
	st := statusFixture()
	st.VPN = vpnStatusFixture()
	_, stdout, _ := runWithService(t, &fakeAPI{status: st}, "status", "--json")
	var got api.Status
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout = %q: %v", stdout, err)
	}
	if got.VPN == nil || got.VPN.Address != "10.86.0.1" || got.VPN.PublicKey == "" || !got.VPN.Enabled {
		t.Fatalf("vpn = %+v", got.VPN)
	}
	if got.VPN.Subnet != "10.86.0.0/16" || got.VPN.Listen != "0.0.0.0:4021" {
		t.Errorf("vpn = %+v", got.VPN)
	}
}
