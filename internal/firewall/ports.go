package firewall

import (
	"net"
	"strconv"
	"strings"

	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/vpn"
)

const (
	WhyTunnel = "the tunnel everything runs through"
	WhyAPI    = "the SSH API"
	WhyEdge   = "the edge"
	WhyHTTP3  = "HTTP/3"
)

func Required(cfg serverconfig.Config, wantsInbound, private bool) []PortSpec {
	var want []PortSpec
	if wantsInbound {
		want = append(want, PortSpec{Proto: "udp", Port: VPNPort(cfg.VPNListen), Why: WhyTunnel})
	}
	if cfg.APIListensPublic() {
		want = append(want, PortSpec{Proto: "tcp", Port: cfg.SSHPort, Why: WhyAPI + " (api_listen: " + cfg.APIListen + ")"})
	}
	return append(want, EdgePorts(cfg, private)...)
}

func EdgePorts(cfg serverconfig.Config, private bool) []PortSpec {
	if !cfg.Edge || private {
		return nil
	}
	out := []PortSpec{
		{Proto: "tcp", Port: 80, Why: WhyEdge},
		{Proto: "tcp", Port: 443, Why: WhyEdge},
	}
	if cfg.HTTP3 {
		out = append(out, PortSpec{Proto: "udp", Port: 443, Why: WhyHTTP3})
	}
	return out
}

func List(specs []PortSpec) string {
	var out []string
	for _, s := range specs {
		out = append(out, s.String())
	}
	return strings.Join(out, ", ")
}

func VPNPort(listen string) int {
	if _, port, err := net.SplitHostPort(listen); err == nil {
		if n, err := strconv.Atoi(port); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return vpn.DefaultListenPort
}

func Tunnel(cfg serverconfig.Config) PortSpec {
	return PortSpec{Proto: "udp", Port: VPNPort(cfg.VPNListen), Why: WhyTunnel}
}
