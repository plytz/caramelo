package setup

import (
	"context"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup/testutil"
)

func TestSummaryTellsTheOperatorHowToConnect(t *testing.T) {
	run := testutil.New()
	run.Stdout("cat -- /var/lib/caramelo/ssh/host_ed25519.pub", namelessKey+" caramelod\n")
	run.Stdout("cat -- /var/lib/caramelo/ssh/authorized_keys", aliceKey+"\n")
	run.Stdout("ip -j route get 1.1.1.1", `[{"dst":"1.1.1.1","gateway":"192.168.121.1","dev":"eth0","prefsrc":"192.168.121.135"}]`)
	env, log := testEnv(t, run)
	env.Config.APIListen = serverconfig.APIListenBoth

	done, detail, err := (&SummaryStep{}).Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %q, %v; want done", done, detail, err)
	}
	out := log.String()
	if !strings.Contains(out, "host key: SHA256:") {
		t.Errorf("log %q does not print the host key fingerprint", out)
	}
	if !strings.Contains(out, "authorized keys: alice@laptop") {
		t.Errorf("log %q does not list the seeded keys", out)
	}
	if !strings.Contains(out, "try: ssh -p 4022 caramelo@192.168.121.135 status") {
		t.Errorf("log %q does not print the command to try", out)
	}
	if !strings.Contains(detail, "caramelo@192.168.121.135") {
		t.Errorf("detail %q does not name the machine", detail)
	}
}

func TestSummarySurvivesAMachineWithoutAKeyOrARoute(t *testing.T) {
	run := testutil.New()
	run.ExitPrefix("cat -- ", 1)
	run.Exit("ip -j route get 1.1.1.1", 2)
	run.Exit("hostname -I", 1)
	env, log := testEnv(t, run)

	done, _, err := (&SummaryStep{}).Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %v; want the summary to report what it can", done, err)
	}
	out := log.String()
	if !strings.Contains(out, "authorized keys: none yet") {
		t.Errorf("log %q does not say there are no keys", out)
	}
	if !strings.Contains(out, "no host key") {
		t.Errorf("log %q does not warn about the missing host key", out)
	}
}

func TestSummaryFallsBackToHostnameForTheIP(t *testing.T) {
	run := testutil.New()
	run.ExitPrefix("cat -- ", 1)
	run.Exit("ip -j route get 1.1.1.1", 2)
	run.Stdout("hostname -I", "192.168.56.12 10.0.0.4\n")
	env, log := testEnv(t, run)

	if _, _, err := (&SummaryStep{}).Check(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "192.168.56.12") {
		t.Errorf("log %q did not fall back to the first local address", log.String())
	}
}

func checkedFirewall(t *testing.T, env *Env, run *testutil.FakeRunner) {
	t.Helper()
	asRoot(t)
	noFirewall(run)
	if _, _, err := (&FirewallStep{}).Check(context.Background(), env); err != nil {
		t.Fatalf("the firewall step: %v", err)
	}
}

func TestSummaryNamesThePortsThatMustBeReachable(t *testing.T) {
	run := testutil.New()
	env, log := testEnv(t, run)
	env.Config.APIListen = serverconfig.APIListenBoth
	checkedFirewall(t, env, run)

	if _, _, err := (&SummaryStep{}).Check(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"udp 4021", "tcp 4022", "10.86.0.0/16",
		"this machine is not blocking it", "checked on this machine only"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the summary does not mention %q:\n%s", want, log.String())
		}
	}
	if strings.Contains(log.String(), "make sure these ports reach this machine") {
		t.Errorf("the summary still hands out advice nobody checked:\n%s", log.String())
	}
}

func TestSummaryNeverSaysAPortIsReachableFromALocalReading(t *testing.T) {
	run := testutil.New()
	env, log := testEnv(t, run)
	env.Config.APIListen = serverconfig.APIListenBoth
	env.Config.Edge = true
	checkedFirewall(t, env, run)

	if _, _, err := (&SummaryStep{}).Check(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"reachable", "reached"} {
		if strings.Contains(strings.ToLower(log.String()), forbidden) {
			t.Errorf("the summary claims %q from a local reading:\n%s", forbidden, log.String())
		}
	}
}

func TestSummaryEndsWithAProbeCarryingTheMachinePublicKey(t *testing.T) {
	key := sampleKey(t)
	pub, err := key.Public()
	if err != nil {
		t.Fatal(err)
	}
	run := testutil.New()
	run.Stdout("cat -- "+keyPath, key.Base64()+"\n")
	run.Stdout("ip -j route get 1.1.1.1", `[{"prefsrc":"203.0.113.9"}]`)
	env, log := testEnv(t, run)
	checkedFirewall(t, env, run)

	if _, _, err := (&SummaryStep{}).Check(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	want := "caramelo hub probe 203.0.113.9:4021 --key " + pub.Base64()
	if !strings.Contains(log.String(), want) {
		t.Errorf("the summary does not end with %q:\n%s", want, log.String())
	}
	if !strings.Contains(log.String(), "caramelo peer add") {
		t.Errorf("the summary does not say the probing computer must be admitted:\n%s", log.String())
	}
}

func TestSummaryWorksWhenTheFirewallWasNotRead(t *testing.T) {
	run := testutil.New()
	env, log := testEnv(t, run)
	env.Config.APIListen = serverconfig.APIListenBoth

	if _, _, err := (&SummaryStep{}).Check(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "udp 4021, tcp 4022") {
		t.Errorf("the summary does not name the ports:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "the firewall was not read in this run") {
		t.Errorf("the summary invented a verdict it does not have:\n%s", log.String())
	}
}

func TestSummaryOmitsTheAPIPortWhenItIsNotPublic(t *testing.T) {
	run := testutil.New()
	env, log := testEnv(t, run)
	env.Config.APIListen = serverconfig.APIListenVPN
	checkedFirewall(t, env, run)

	if _, _, err := (&SummaryStep{}).Check(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(log.String(), "tcp 4022") {
		t.Errorf("the summary still asks for the public API port:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "udp 4021") {
		t.Errorf("the summary does not mention the tunnel's port:\n%s", log.String())
	}
}

func TestSummaryOfAMemberAsksForNoInboundPort(t *testing.T) {
	run := testutil.New()
	env, log := testEnv(t, run)
	env.Config.Role, env.Config.Member.Private = serverconfig.RoleMember, true
	env.Config.Edge = true
	checkedFirewall(t, env, run)

	if _, _, err := (&SummaryStep{}).Check(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "needs no inbound port") {
		t.Errorf("a private member was asked for a port:\n%s", log.String())
	}
}

func TestSummaryNamesTheVaultKeyWithoutReadingIt(t *testing.T) {
	run := testutil.New()
	run.Stdout("stat -c %U:%G:%a:%F -- /var/lib/caramelo/vault.key", "caramelo:caramelo:600:regular file\n")
	env, log := testEnv(t, run)

	if done, detail, err := (&SummaryStep{}).Check(context.Background(), env); err != nil || !done {
		t.Fatalf("Check() = %v, %q, %v; want done", done, detail, err)
	}
	out := log.String()
	if !strings.Contains(out, "vault: /var/lib/caramelo/vault.key (0600 caramelo:caramelo)") {
		t.Errorf("log %q does not describe the vault key", out)
	}
	if !strings.Contains(out, "caramelo secrets set") {
		t.Errorf("log %q does not say what to do with the vault", out)
	}

	for _, cmd := range run.Lines() {
		if strings.Contains(cmd, "vault.key") && !strings.HasPrefix(cmd, "stat ") {
			t.Errorf("the summary ran %q: the key is never read", cmd)
		}
	}
}

func TestSummarySaysWhenThereIsNoVaultKey(t *testing.T) {
	run := testutil.New()

	run.Exit("stat -c %U:%G:%a:%F -- /var/lib/caramelo/vault.key", 1)
	env, log := testEnv(t, run)

	if done, _, err := (&SummaryStep{}).Check(context.Background(), env); err != nil || !done {
		t.Fatalf("Check() = %v, %v; want done", done, err)
	}
	if !strings.Contains(log.String(), "cannot hold a secret") {
		t.Errorf("log %q does not say the machine has no vault", log.String())
	}
}
