package setup

import (
	"context"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/firewall"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup/testutil"
)

const ufwDenyingTheTunnel = `Status: active
Logging: on (low)
Default: deny (incoming), allow (outgoing), disabled (routed)

To                         Action      From
--                         ------      ----
22/tcp                     ALLOW IN    Anywhere
4021/udp                   DENY IN     Anywhere
`

func noFirewall(run *testutil.FakeRunner) *testutil.FakeRunner {
	for _, tool := range []string{"ufw", "firewall-cmd", "nft", "iptables"} {
		run.Exit("sh -c command -v "+tool, 1)
	}
	return run
}

func ufwBox(status string) *testutil.FakeRunner { return withUFW(testutil.New(), status) }

func withUFW(run *testutil.FakeRunner, status string) *testutil.FakeRunner {
	noFirewall(run)
	run.Stdout("sh -c command -v ufw", "/usr/sbin/ufw\n")
	return run.Stdout("ufw status verbose", status)
}

func TestSetupStopsWhenTheFirewallDeniesTheTunnelPort(t *testing.T) {
	noUser(t)
	asRoot(t)
	env, log := testEnv(t, withUFW(healthyBox(), ufwDenyingTheTunnel))

	before, _ := HostSteps()
	rep := Execute(context.Background(), before, env, nil)

	if rep.Failed != 1 {
		t.Fatalf("%d failed step(s), want 1; a denied tunnel port must stop the run:\n%s", rep.Failed, log.String())
	}
	last := rep.Results[len(rep.Results)-1]
	if last.Step != "firewall" || last.Status != StatusFailed {
		t.Fatalf("the run stopped at %q (%s), want the firewall step", last.Step, last.Status)
	}
	for _, want := range []string{"ufw is active and denies udp 4021", "4021/udp DENY IN Anywhere",
		"ufw allow 4021/udp", forceHint} {
		if !strings.Contains(last.Error, want) {
			t.Errorf("the failure %q does not mention %q", last.Error, want)
		}
	}
	for _, r := range rep.Results {
		if r.Step == "vpn" || r.Step == "host-config" {
			t.Errorf("the run reached %q: a blocked tunnel port must stop setup before anything is configured", r.Step)
		}
	}
}

func TestFirewallStepWarnsAndCarriesOnWithForce(t *testing.T) {
	asRoot(t)
	env, log := testEnv(t, ufwBox(ufwDenyingTheTunnel))
	env.Opts.Force = true

	done, detail, err := (&FirewallStep{}).Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %q, %v; want --force to get past it", done, detail, err)
	}
	if !strings.Contains(log.String(), "warning: ufw is active and denies udp 4021") {
		t.Errorf("--force said nothing about the blocked port:\n%s", log.String())
	}
}

func TestFirewallStepOnlyFailsOnTheTunnelPort(t *testing.T) {
	asRoot(t)
	const denyingTheEdge = `Status: active
Default: deny (incoming), allow (outgoing)

To                         Action      From
--                         ------      ----
4021/udp                   ALLOW IN    Anywhere
`
	env, log := testEnv(t, ufwBox(denyingTheEdge))
	env.Config.Edge = true

	done, _, err := (&FirewallStep{}).Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %v; a blocked edge port warns, it does not stop setup", done, err)
	}
	if !strings.Contains(log.String(), "denies tcp 443") {
		t.Errorf("the blocked edge port was not reported:\n%s", log.String())
	}
}

func TestFirewallStepNeverFailsOnAMachineThatDialsOut(t *testing.T) {
	asRoot(t)
	cases := map[string]func(env *Env){
		"a member config": func(env *Env) { env.Config.Fleet.Role = serverconfig.RoleMember },
		"--join-token":    func(env *Env) { env.Opts.Join = JoinSpec{Token: "a-ticket"} },
		"--private": func(env *Env) {
			env.Config.Fleet.Role, env.Config.Fleet.Private = serverconfig.RoleMember, true
			env.Config.Edge = true
		},
	}
	for name, apply := range cases {
		t.Run(name, func(t *testing.T) {
			env, log := testEnv(t, ufwBox(ufwDenyingTheTunnel))
			apply(env)

			done, detail, err := (&FirewallStep{}).Check(context.Background(), env)
			if err != nil || !done {
				t.Fatalf("Check() = %v, %q, %v; %s never needs udp 4021 open", done, detail, err, name)
			}
			if strings.Contains(log.String(), "denies udp 4021") {
				t.Errorf("%s was asked for an inbound tunnel port:\n%s", name, log.String())
			}
		})
	}
}

func TestFirewallStepReportsABoxWithNoFirewallInAHandfulOfCommands(t *testing.T) {
	asRoot(t)
	run := noFirewall(testutil.New())
	env, log := testEnv(t, run)

	done, detail, err := (&FirewallStep{}).Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %q, %v; want a clean report", done, detail, err)
	}
	if !strings.Contains(detail, "no firewall manager is in charge here") {
		t.Errorf("detail = %q, want it to say nothing is in charge", detail)
	}
	want := "this machine is not blocking udp 4021 (" + LocalOnlyCaveat + ")"
	if !strings.Contains(log.String(), want) {
		t.Errorf("log does not carry %q:\n%s", want, log.String())
	}
	for _, forbidden := range []string{"reachable", "reached"} {
		if strings.Contains(strings.ToLower(log.String()), forbidden) {
			t.Errorf("a local reading claims %q:\n%s", forbidden, log.String())
		}
	}
	if n := len(run.Calls()); n > 8 {
		t.Errorf("%d commands to find out there is no firewall:\n%s", n, run.Transcript())
	}
}

func TestFirewallStepDegradesToCannotTellWithoutRoot(t *testing.T) {
	prev := geteuid
	geteuid = func() int { return 1000 }
	t.Cleanup(func() { geteuid = prev })

	env, log := testEnv(t, ufwBox(ufwDenyingTheTunnel))
	done, detail, err := (&FirewallStep{}).Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %q, %v; an unprivileged read reports what it can", done, detail, err)
	}
	if !strings.Contains(detail, firewall.NotRoot) {
		t.Errorf("detail = %q, want %q", detail, firewall.NotRoot)
	}
	if !strings.Contains(log.String(), "cannot be worked out") {
		t.Errorf("an unprivileged read did not say it could not tell:\n%s", log.String())
	}
}

func TestTheExistingFixturesStillReportNoFirewall(t *testing.T) {
	asRoot(t)
	env, _ := testEnv(t, testutil.New())

	done, detail, err := (&FirewallStep{}).Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %q, %v; a runner that answers nothing must not stop a setup", done, detail, err)
	}
	if env.Firewall.Manager != firewall.ManagerNone {
		t.Errorf("manager = %q, want none: empty command output is not a firewall", env.Firewall.Manager)
	}
	if got := env.Firewall.Says(firewall.Tunnel(env.Config)); got != firewall.VerdictUnknown {
		t.Errorf("udp 4021 = %q, want %q", got, firewall.VerdictUnknown)
	}
}

func TestFirewallStepReportsLLMNRAndChangesNothing(t *testing.T) {
	asRoot(t)
	run := noFirewall(testutil.New())
	run.Stdout("ss -lun", "State  Recv-Q Send-Q Local Address:Port  Peer Address:Port\nUNCONN 0      0      0.0.0.0:5355        0.0.0.0:*\n")
	env, log := testEnv(t, run)

	if _, _, err := (&FirewallStep{}).Check(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "LLMNR on udp 5355") {
		t.Errorf("the exposed LLMNR listener was not reported:\n%s", log.String())
	}
	for _, line := range run.Lines() {
		if strings.HasPrefix(line, "systemctl") || strings.HasPrefix(line, "ufw") {
			t.Errorf("the step changed something: %q", line)
		}
	}
}

func TestFirewallApplyRefusesWithoutOpenPorts(t *testing.T) {
	asRoot(t)
	run := ufwBox(ufwDenyingTheTunnel)
	env, _ := testEnv(t, run)

	if _, _, err := (&FirewallStep{}).Check(context.Background(), env); err == nil {
		t.Fatal("Check must refuse a denied tunnel port")
	}
	run.Reset()
	if err := (&FirewallStep{}).Apply(context.Background(), env); err == nil {
		t.Fatal("Apply = nil, want a refusal without --open-ports")
	}
	if len(run.Calls()) != 0 {
		t.Errorf("Apply ran commands without --open-ports:\n%s", run.Transcript())
	}
}

func TestOpenPortsOpensTheBlockedPortAndNothingElse(t *testing.T) {
	asRoot(t)
	run := ufwBox(ufwDenyingTheTunnel)
	env, log := testEnv(t, run)
	env.Opts.OpenPorts = true

	done, _, err := (&FirewallStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("Check() said there was nothing to do with --open-ports and a blocked port")
	}
	if err := (&FirewallStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply: %v\n%s", err, run.Transcript())
	}
	if !run.Ran("ufw allow 4021/udp") {
		t.Errorf("the blocked port was not opened:\n%s", run.Transcript())
	}
	if run.Ran("ufw allow 4022/tcp") || run.Ran("ufw enable") {
		t.Errorf("--open-ports touched more than the ports this machine needs:\n%s", run.Transcript())
	}
	if !strings.Contains(log.String(), "opened udp 4021 in ufw") {
		t.Errorf("the change was not reported:\n%s", log.String())
	}
}

func TestOpenPortsRefusesToWriteIntoANftablesRuleset(t *testing.T) {
	asRoot(t)
	run := noFirewall(testutil.New())
	run.Stdout("sh -c command -v nft", "/usr/sbin/nft\n")
	run.Stdout("nft list ruleset", "table inet filter {\n\tchain input {\n\t\ttype filter hook input priority 0; policy drop;\n\t}\n}\n")
	env, _ := testEnv(t, run)
	env.Opts.OpenPorts = true

	if _, _, err := (&FirewallStep{}).Check(context.Background(), env); err == nil {
		t.Fatal("a ruleset caramelo will not write into must still stop setup")
	}
	err := (&FirewallStep{}).Apply(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "nft add rule") {
		t.Fatalf("Apply = %v, want a refusal carrying the rule to add by hand", err)
	}
	for _, line := range run.Lines() {
		if strings.HasPrefix(line, "nft add") {
			t.Errorf("a rule was written blind into an nftables ruleset: %q", line)
		}
	}
}

func TestDryRunWithOpenPortsChangesNothing(t *testing.T) {
	noUser(t)
	asRoot(t)
	run := ufwBox(ufwDenyingTheTunnel)
	env, _ := testEnv(t, run)
	env.Opts.OpenPorts, env.DryRun = true, true

	rep := Execute(context.Background(), []Step{NewFirewallStep()}, env, nil)
	if got := rep.Results[0].Status; got != StatusWouldChange {
		t.Fatalf("status = %q, want %q", got, StatusWouldChange)
	}
	for _, line := range run.Lines() {
		if strings.HasPrefix(line, "ufw allow") {
			t.Errorf("--dry-run opened a port: %q", line)
		}
	}
}
