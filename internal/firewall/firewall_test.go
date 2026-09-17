package firewall

import (
	"context"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/setup/testutil"
)

var (
	tunnel = PortSpec{Proto: "udp", Port: 4021, Why: WhyTunnel}
	api    = PortSpec{Proto: "tcp", Port: 4022, Why: WhyAPI}
	web    = PortSpec{Proto: "tcp", Port: 443, Why: WhyEdge}
)

func box(tools ...string) *testutil.FakeRunner {
	run := testutil.New()
	run.Strict = true
	for _, tool := range []string{"ufw", "firewall-cmd", "nft", "iptables"} {
		run.Exit("sh -c command -v "+tool, 1)
	}
	for _, tool := range tools {
		run.Stdout("sh -c command -v "+tool, "/usr/sbin/"+tool+"\n")
	}
	return run
}

const ufwActiveDenying = `Status: active
Logging: on (low)
Default: deny (incoming), allow (outgoing), disabled (routed)
New profiles: skip

To                         Action      From
--                         ------      ----
22/tcp                     ALLOW IN    Anywhere
4021/udp                   DENY IN     Anywhere
22/tcp (v6)                ALLOW IN    Anywhere (v6)
`

const ufwActiveAllowing = `Status: active
Default: deny (incoming), allow (outgoing), disabled (routed)

To                         Action      From
--                         ------      ----
4021/udp                   ALLOW IN    Anywhere
`

const ufwActiveScoped = `Status: active
Default: deny (incoming), allow (outgoing), disabled (routed)

To                         Action      From
--                         ------      ----
4021/udp                   ALLOW IN    10.0.0.0/8
`

const ufwActiveProfileOnly = `Status: active
Logging: on (low)
Default: deny (incoming), allow (outgoing), disabled (routed)
New profiles: skip

To                         Action      From
--                         ------      ----
OpenSSH                    ALLOW IN    Anywhere
OpenSSH (v6)               ALLOW IN    Anywhere (v6)
`

const ufwActiveSourceScopedElsewhere = `Status: active
Default: deny (incoming), allow (outgoing), disabled (routed)

To                         Action      From
--                         ------      ----
25/tcp                     DENY IN     198.51.100.7
`

const firewalldZoneWithout = `public (active)
  target: default
  icmp-block-inversion: no
  interfaces: eth0
  sources:
  services: ssh dhcpv6-client
  ports:
  protocols:
  forward: yes
  masquerade: no
  rich rules:
`

const firewalldZoneWith = `public (active)
  target: default
  services: ssh
  ports: 4021/udp 443/tcp
  rich rules:
`

const nftDropping = `table inet caramelo_itest {
	chain input {
		type filter hook input priority 0; policy accept;
		udp dport 4021 drop
	}
}
`

const nftEmptyPolicyDrop = `table inet filter {
	chain input {
		type filter hook input priority 0; policy drop;
		ct state established,related accept
		iif "lo" accept
	}
}
`

const nftAcceptUnderDropPolicy = `table inet filter {
	chain input {
		type filter hook input priority 0; policy drop;
		udp dport 4021 accept
		ct state established,related accept
	}
}
`

const nftNoInputHook = `table ip docker-bridges {
	chain filter-forward {
		type filter hook forward priority 0; policy accept;
	}
	chain nat-postrouting {
		type nat hook postrouting priority 100; policy accept;
	}
}
table ip6 docker-bridges {
	chain filter-forward {
		type filter hook forward priority 0; policy accept;
	}
}
`

const nftFiltersPrerouting = `table inet raw {
	chain pre {
		type filter hook prerouting priority -300; policy accept;
		udp dport 4021 drop
	}
}
`

const nftDockerShaped = `table ip nat {
	chain DOCKER {
		iifname "docker0" return
	}
	chain PREROUTING {
		type nat hook prerouting priority -100; policy accept;
		fib daddr type local counter jump DOCKER
	}
}
table ip filter {
	chain INPUT {
		type filter hook input priority 0; policy accept;
	}
	chain DOCKER-USER {
		counter return
	}
	chain FORWARD {
		type filter hook forward priority 0; policy drop;
		counter jump DOCKER-USER
	}
}
`

const iptablesDropPolicy = `-P INPUT DROP
-A INPUT -p tcp -m tcp --dport 22 -j ACCEPT
`

const iptablesDockerShaped = `-P INPUT ACCEPT
-A INPUT -j DOCKER-USER
`

func TestCheckReadsWhatIsInCharge(t *testing.T) {
	cases := []struct {
		name    string
		runner  func() *testutil.FakeRunner
		manager Manager
		want    map[PortSpec]Verdict
		detail  string
		rule    string
	}{
		{
			name: "ufw active with a deny for the tunnel port",
			runner: func() *testutil.FakeRunner {
				run := box("ufw")
				return run.Stdout("ufw status verbose", ufwActiveDenying)
			},
			manager: ManagerUFW,
			want:    map[PortSpec]Verdict{tunnel: VerdictBlocked, api: VerdictBlocked, web: VerdictBlocked},
			rule:    "4021/udp DENY IN Anywhere",
		},
		{
			name: "ufw active with an allow",
			runner: func() *testutil.FakeRunner {
				return box("ufw").Stdout("ufw status verbose", ufwActiveAllowing)
			},
			manager: ManagerUFW,
			want:    map[PortSpec]Verdict{tunnel: VerdictOpen, api: VerdictBlocked},
			rule:    "4021/udp ALLOW IN Anywhere",
		},
		{
			name: "ufw installed but inactive",
			runner: func() *testutil.FakeRunner {
				return box("ufw").Stdout("ufw status verbose", "Status: inactive\n")
			},
			manager: ManagerNone,
			want:    map[PortSpec]Verdict{tunnel: VerdictOpen},
			detail:  "inactive",
		},
		{
			name: "ufw with an allow scoped to one network",
			runner: func() *testutil.FakeRunner {
				return box("ufw").Stdout("ufw status verbose", ufwActiveScoped)
			},
			manager: ManagerUFW,
			want:    map[PortSpec]Verdict{tunnel: VerdictUnknown},
			rule:    "10.0.0.0/8",
		},
		{
			name: "ufw whose only rule names an application profile",
			runner: func() *testutil.FakeRunner {
				return box("ufw").Stdout("ufw status verbose", ufwActiveProfileOnly)
			},
			manager: ManagerUFW,
			want:    map[PortSpec]Verdict{tunnel: VerdictBlocked, api: VerdictBlocked},
			rule:    "Default: deny (incoming)",
		},
		{
			name: "ufw with a deny scoped to one source for another port",
			runner: func() *testutil.FakeRunner {
				return box("ufw").Stdout("ufw status verbose", ufwActiveSourceScopedElsewhere)
			},
			manager: ManagerUFW,
			want:    map[PortSpec]Verdict{tunnel: VerdictBlocked, api: VerdictBlocked},
			rule:    "Default: deny (incoming)",
		},
		{
			name: "firewalld running with a zone that lacks the port",
			runner: func() *testutil.FakeRunner {
				run := box("firewall-cmd").Stdout("firewall-cmd --state", "running\n")
				return run.Stdout("firewall-cmd --list-all", firewalldZoneWithout)
			},
			manager: ManagerFirewalld,
			want:    map[PortSpec]Verdict{tunnel: VerdictBlocked},
			rule:    "target: default",
		},
		{
			name: "firewalld running with the port open",
			runner: func() *testutil.FakeRunner {
				run := box("firewall-cmd").Stdout("firewall-cmd --state", "running\n")
				return run.Stdout("firewall-cmd --list-all", firewalldZoneWith)
			},
			manager: ManagerFirewalld,
			want:    map[PortSpec]Verdict{tunnel: VerdictOpen, web: VerdictOpen, api: VerdictBlocked},
		},
		{
			name: "nftables dropping the tunnel port in an inet input chain",
			runner: func() *testutil.FakeRunner {
				return box("nft").Stdout("nft list ruleset", nftDropping)
			},
			manager: ManagerNftables,
			want:    map[PortSpec]Verdict{tunnel: VerdictBlocked, api: VerdictOpen},
			rule:    "udp dport 4021 drop",
		},
		{
			name: "nftables with a drop policy and no rule for the port",
			runner: func() *testutil.FakeRunner {
				return box("nft").Stdout("nft list ruleset", nftEmptyPolicyDrop)
			},
			manager: ManagerNftables,
			want:    map[PortSpec]Verdict{tunnel: VerdictBlocked},
			rule:    "policy drop",
		},
		{
			name: "nftables with tables but no chain hooking input",
			runner: func() *testutil.FakeRunner {
				return box("nft").Stdout("nft list ruleset", nftNoInputHook)
			},
			manager: ManagerNftables,
			want:    map[PortSpec]Verdict{tunnel: VerdictOpen, api: VerdictOpen},
			rule:    "no chain hooks input in tables ip docker-bridges, ip6 docker-bridges",
			detail:  "nothing here filters inbound traffic",
		},
		{
			name: "nftables filtering on prerouting, which this reader does not follow",
			runner: func() *testutil.FakeRunner {
				return box("nft").Stdout("nft list ruleset", nftFiltersPrerouting)
			},
			manager: ManagerNftables,
			want:    map[PortSpec]Verdict{tunnel: VerdictUnknown},
			detail:  "filter on prerouting",
		},
		{
			name: "nftables installed with an empty ruleset",
			runner: func() *testutil.FakeRunner {
				return box("nft").Stdout("nft list ruleset", "")
			},
			manager: ManagerNone,
			want:    map[PortSpec]Verdict{tunnel: VerdictOpen},
			detail:  "the ruleset is empty",
		},
		{
			name: "iptables with an INPUT DROP policy",
			runner: func() *testutil.FakeRunner {
				return box("iptables").Stdout("iptables -S INPUT", iptablesDropPolicy)
			},
			manager: ManagerIptables,
			want:    map[PortSpec]Verdict{tunnel: VerdictBlocked, api: VerdictBlocked},
			rule:    "-P INPUT DROP",
		},
		{
			name:    "no firewall tool at all",
			runner:  func() *testutil.FakeRunner { return box() },
			manager: ManagerNone,
			want:    map[PortSpec]Verdict{tunnel: VerdictOpen, api: VerdictOpen, web: VerdictOpen},
			detail:  "no firewall manager is in charge here",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var want []PortSpec
			for spec := range tc.want {
				want = append(want, spec)
			}
			run := tc.runner()
			rep, err := Check(context.Background(), run, 0, want)
			if err != nil {
				t.Fatalf("Check: %v\n%s", err, run.Transcript())
			}
			if rep.Manager != tc.manager {
				t.Errorf("manager = %q, want %q (detail %q)", rep.Manager, tc.manager, rep.Detail)
			}
			if rep.Active != (tc.manager != ManagerNone) {
				t.Errorf("active = %v with manager %q", rep.Active, rep.Manager)
			}
			for spec, verdict := range tc.want {
				if got := rep.Says(spec); got != verdict {
					t.Errorf("%s = %q, want %q (detail %q)", spec, got, verdict, rep.Detail)
				}
			}
			if tc.detail != "" && !strings.Contains(rep.Detail, tc.detail) {
				t.Errorf("detail = %q, want it to mention %q", rep.Detail, tc.detail)
			}
			if tc.rule == "" {
				return
			}
			pc, ok := rep.Find(tunnel)
			if !ok || !strings.Contains(pc.Rule, tc.rule) {
				t.Errorf("the rule behind %s is %q, want it to quote %q", tunnel, pc.Rule, tc.rule)
			}
		})
	}
}

func TestADockerShapedRulesetDecidesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  *testutil.FakeRunner
	}{
		{"nftables", box("nft").Stdout("nft list ruleset", nftDockerShaped)},
		{"iptables", box("iptables").Stdout("iptables -S INPUT", iptablesDockerShaped)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep, err := Check(context.Background(), tc.run, 0, []PortSpec{tunnel, api, web})
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range rep.Ports {
				if p.Verdict != VerdictUnknown {
					t.Errorf("%s = %q, want %q: Docker's own chains say nothing about host policy",
						p.PortSpec, p.Verdict, VerdictUnknown)
				}
			}
			if len(rep.Blocked()) != 0 {
				t.Errorf("a Docker-shaped ruleset stopped a setup: %+v", rep.Blocked())
			}
		})
	}
}

func TestARulesetTheReaderCannotDecideIsNeverBlocked(t *testing.T) {
	run := box("nft").Stdout("nft list ruleset", nftAcceptUnderDropPolicy)
	rep, err := Check(context.Background(), run, 0, []PortSpec{tunnel})
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Says(tunnel); got != VerdictUnknown {
		t.Errorf("a chain that drops by default and accepts %s earlier = %q, want %q: "+
			"a false 'blocked' stops a setup that would have worked", tunnel, got, VerdictUnknown)
	}
}

func TestWithoutRootEveryVerdictIsUnknown(t *testing.T) {
	run := box("ufw").Stdout("ufw status verbose", ufwActiveDenying)
	rep, err := Check(context.Background(), run, 1000, []PortSpec{tunnel, api})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Root {
		t.Error("the report claims it ran as root")
	}
	for _, p := range rep.Ports {
		if p.Verdict != VerdictUnknown {
			t.Errorf("%s = %q without root, want %q: degrade to cannot-tell, never to fine", p.PortSpec, p.Verdict, VerdictUnknown)
		}
	}
	if !strings.Contains(rep.Detail, NotRoot) {
		t.Errorf("detail = %q, want %q", rep.Detail, NotRoot)
	}
	if len(run.Calls()) != 0 {
		t.Errorf("an unprivileged read ran commands:\n%s", run.Transcript())
	}
}

func TestARunnerThatAnswersNothingDecidesNothing(t *testing.T) {
	run := testutil.New()
	rep, err := Check(context.Background(), run, 0, []PortSpec{tunnel, api})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rep.Manager != ManagerNone || rep.Active {
		t.Errorf("manager = %q active = %v, want none: empty output is not a firewall", rep.Manager, rep.Active)
	}
	for _, p := range rep.Ports {
		if p.Verdict != VerdictUnknown {
			t.Errorf("%s = %q, want %q: unreadable output is not a verdict", p.PortSpec, p.Verdict, VerdictUnknown)
		}
	}
}

func TestNothingThePackageSaysClaimsAPacketArrived(t *testing.T) {
	var said []string
	runners := []*testutil.FakeRunner{
		box(),
		testutil.New(),
		box("ufw").Stdout("ufw status verbose", ufwActiveDenying),
		box("ufw").Stdout("ufw status verbose", ufwActiveAllowing),
		box("ufw").Stdout("ufw status verbose", ufwActiveScoped),
		box("ufw").Stdout("ufw status verbose", ufwActiveProfileOnly),
		box("ufw").Stdout("ufw status verbose", ufwActiveSourceScopedElsewhere),
		box("ufw").Stdout("ufw status verbose", "Status: inactive\n"),
		box("nft").Stdout("nft list ruleset", nftDropping),
		box("nft").Stdout("nft list ruleset", nftDockerShaped),
		box("nft").Stdout("nft list ruleset", nftAcceptUnderDropPolicy),
		box("iptables").Stdout("iptables -S INPUT", iptablesDropPolicy),
	}
	for _, run := range runners {
		rep, err := Check(context.Background(), run, 0, []PortSpec{tunnel, api, web})
		if err != nil {
			t.Fatal(err)
		}
		said = append(said, rep.Detail, string(rep.Manager))
		for _, p := range rep.Ports {
			said = append(said, p.Rule, string(p.Verdict), p.Why)
		}
	}
	for _, m := range []Manager{ManagerNone, ManagerUFW, ManagerFirewalld, ManagerNftables, ManagerIptables} {
		_, printed, _ := OpenCommand(m, tunnel)
		said = append(said, printed)
	}
	said = append(said, NotRoot, WhyTunnel, WhyAPI, WhyEdge, WhyHTTP3)
	for _, s := range said {
		for _, forbidden := range []string{"reachable", "reached"} {
			if strings.Contains(strings.ToLower(s), forbidden) {
				t.Errorf("the package says %q: only a packet from outside earns %q", s, forbidden)
			}
		}
	}
}

func TestOpenCommandPrintsTheRuleAndOnlyRunsWhereItIsSafe(t *testing.T) {
	cmds, printed, supported := OpenCommand(ManagerUFW, tunnel)
	if !supported || printed != "ufw allow 4021/udp" {
		t.Errorf("ufw: %q supported=%v", printed, supported)
	}
	if len(cmds) != 1 || testutil.Key(cmds[0]) != "ufw allow 4021/udp" {
		t.Errorf("ufw commands = %v", cmds)
	}

	cmds, printed, supported = OpenCommand(ManagerFirewalld, tunnel)
	if !supported || !strings.Contains(printed, "--add-port=4021/udp") || len(cmds) != 2 {
		t.Errorf("firewalld: %q %v supported=%v", printed, cmds, supported)
	}

	for _, m := range []Manager{ManagerNftables, ManagerIptables} {
		cmds, printed, supported = OpenCommand(m, tunnel)
		if supported || len(cmds) != 0 {
			t.Errorf("%s: a rule of somebody else's is never written blind (%v)", m, cmds)
		}
		if !strings.Contains(printed, "4021") {
			t.Errorf("%s: %q does not name the port to open by hand", m, printed)
		}
	}
}

func TestCheckWithoutARunnerSaysSo(t *testing.T) {
	var none runner.Runner
	if _, err := Check(context.Background(), none, 0, []PortSpec{tunnel}); err == nil {
		t.Fatal("Check with no runner must fail")
	}
}
