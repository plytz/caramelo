package firewall

import (
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/serverconfig"
)

func names(specs []PortSpec) string {
	var out []string
	for _, s := range specs {
		out = append(out, s.String())
	}
	return strings.Join(out, ", ")
}

func TestRequiredFollowsTheConfiguration(t *testing.T) {
	cases := []struct {
		name         string
		cfg          func(serverconfig.Config) serverconfig.Config
		wantsInbound bool
		want         string
	}{
		{
			name:         "a default hub needs the tunnel only",
			cfg:          func(c serverconfig.Config) serverconfig.Config { return c },
			wantsInbound: true,
			want:         "udp 4021",
		},
		{
			name: "a public API adds its port",
			cfg: func(c serverconfig.Config) serverconfig.Config {
				c.APIListen = serverconfig.APIListenPublic
				return c
			},
			wantsInbound: true,
			want:         "udp 4021, tcp 4022",
		},
		{
			name: "api_listen both is public too",
			cfg: func(c serverconfig.Config) serverconfig.Config {
				c.APIListen = serverconfig.APIListenBoth
				return c
			},
			wantsInbound: true,
			want:         "udp 4021, tcp 4022",
		},
		{
			name: "the edge adds 80, 443 and quic",
			cfg: func(c serverconfig.Config) serverconfig.Config {
				c.Edge = true
				return c
			},
			wantsInbound: true,
			want:         "udp 4021, tcp 80, tcp 443, udp 443",
		},
		{
			name: "without http3 there is no udp 443",
			cfg: func(c serverconfig.Config) serverconfig.Config {
				c.Edge, c.HTTP3 = true, false
				return c
			},
			wantsInbound: true,
			want:         "udp 4021, tcp 80, tcp 443",
		},
		{
			name: "a private member is served through its hub and needs nothing",
			cfg: func(c serverconfig.Config) serverconfig.Config {
				c.Edge, c.Fleet.Private, c.Fleet.Role = true, true, serverconfig.RoleMember
				return c
			},
			wantsInbound: false,
			want:         "",
		},
		{
			name: "a member dials out and needs no inbound tunnel port",
			cfg: func(c serverconfig.Config) serverconfig.Config {
				c.Fleet.Role = serverconfig.RoleMember
				return c
			},
			wantsInbound: false,
			want:         "",
		},
		{
			name: "a tunnel on another port is the one that is asked for",
			cfg: func(c serverconfig.Config) serverconfig.Config {
				c.VPNListen = "0.0.0.0:51820"
				return c
			},
			wantsInbound: true,
			want:         "udp 51820",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := names(Required(tc.cfg(serverconfig.Default()), tc.wantsInbound))
			if got != tc.want {
				t.Errorf("Required = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEveryRequiredPortSaysWhyItIsWanted(t *testing.T) {
	cfg := serverconfig.Default()
	cfg.Edge, cfg.APIListen = true, serverconfig.APIListenBoth
	for _, p := range Required(cfg, true) {
		if strings.TrimSpace(p.Why) == "" {
			t.Errorf("%s is asked for without saying why", p)
		}
	}
}

func TestVPNPortFallsBackToTheDefault(t *testing.T) {
	cases := map[string]int{
		"0.0.0.0:4021":  4021,
		"[::]:51820":    51820,
		"":              4021,
		"not-a-listen":  4021,
		"0.0.0.0:99999": 4021,
	}
	for listen, want := range cases {
		if got := VPNPort(listen); got != want {
			t.Errorf("VPNPort(%q) = %d, want %d", listen, got, want)
		}
	}
}
