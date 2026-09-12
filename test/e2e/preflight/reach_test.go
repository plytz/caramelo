//go:build e2e

package preflight

import (
	"context"
	"strings"
	"testing"

	"github.com/plytz/caramelo/test/e2e/inventory"
	"github.com/plytz/caramelo/test/e2e/sshrun"
)

func TestMachinesReachEachOther(t *testing.T) {
	if inv.Count() < 2 {
		t.Skipf("the inventory %s holds %d machine, and a machine reaching another at its inventory host needs at least 2", invPath, inv.Count())
	}
	for _, from := range inv.Machines {
		from := from
		for _, to := range inv.Machines {
			to := to
			if from.Name == to.Name {
				continue
			}
			t.Run(from.Name+"_to_"+to.Name, func(t *testing.T) {
				session := openSession(t, from)
				addr, err := targetAddress(to.Host)
				if err != nil {
					t.Fatalf("machine %s: %v", to.Name, err)
				}
				if addr != to.Host {
					t.Logf("the inventory host of %s is the name %s, resolved to %s by the machine running the tests so no name resolved on %s can answer instead", to.Name, to.Host, addr, from.Name)
				}
				t.Run("host_tcp", func(t *testing.T) { checkTCP(t, session, from, to, addr) })
				t.Run("host_icmp", func(t *testing.T) { checkPing(t, session, from, to, addr) })
			})
		}
	}
}

func checkTCP(t *testing.T, s *sshrun.Session, from, to inventory.Machine, addr string) {
	ctx, cancel := context.WithTimeout(context.Background(), reachTimeout)
	defer cancel()
	probe, err := s.Run(ctx, hasCommand("nc")+" || "+hasCommand("bash"))
	if err != nil {
		t.Fatalf("machine %s: looking for nc and bash: %v", from.Name, err)
	}
	if probe.ExitCode != 0 {
		t.Skipf("machine %s has neither nc nor bash, and preflight installs nothing", from.Name)
	}
	res, err := s.Run(ctx, reachCommand(addr, to.Port))
	if err != nil {
		t.Fatalf("machine %s: connecting to %s: %v", from.Name, addr, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("machine %s cannot open tcp %d to %s, the host and port of machine %s in the inventory %s: exit %d: %s", from.Name, to.Port, addr, to.Name, invPath, res.ExitCode, strings.TrimSpace(res.Stderr+res.Stdout))
	}
}

func checkPing(t *testing.T, s *sshrun.Session, from, to inventory.Machine, addr string) {
	ctx, cancel := context.WithTimeout(context.Background(), reachTimeout)
	defer cancel()
	probe, err := s.Run(ctx, hasCommand("ping"))
	if err != nil {
		t.Fatalf("machine %s: looking for ping: %v", from.Name, err)
	}
	if probe.ExitCode != 0 {
		t.Skipf("machine %s has no ping, and preflight installs nothing", from.Name)
	}
	res, err := s.Run(ctx, pingCommand(addr))
	if err != nil {
		t.Fatalf("machine %s: pinging %s: %v", from.Name, addr, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("machine %s cannot ping %s, the host of machine %s in the inventory %s: exit %d: %s", from.Name, addr, to.Name, invPath, res.ExitCode, strings.TrimSpace(res.Stderr+res.Stdout))
	}
}
