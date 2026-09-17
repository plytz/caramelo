package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/vpn"
)

func aJoinTicket(t *testing.T) (fleet.Ticket, string) {
	t.Helper()
	priv, err := vpn.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := priv.Public()
	if err != nil {
		t.Fatal(err)
	}
	tk := fleet.Ticket{
		Hub:       "nx1",
		Endpoint:  "hub.example.com:4021",
		PublicKey: pub.Base64(),
		Address:   "10.86.0.1",
		Peer:      "10.86.0.9",
		Range:     fleet.FleetRange,
		Secret:    "s3cr3t",
	}
	s, err := tk.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return tk, s
}

func TestJoinOnAMachineThatWasNeverSetUpNamesTheStepsThatMakeWhatItNeeds(t *testing.T) {
	_, token := aJoinTicket(t)
	code, stdout, stderr := run(t, "member", "join", "hub.example.com:4021", "--token", token, "--config-dir", t.TempDir())
	if code != ExitError {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitError, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	for _, want := range []string{
		"caramelo hub setup",
		"dirs",
		serverconfig.ConfigFile,
		serverconfig.DefaultUser,
		"docker-rootless",
		"WireGuard key",
		"vpn",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to name %q", stderr, want)
		}
	}
	if strings.Contains(stderr, "no such file") {
		t.Errorf("stderr = %q, still names a missing file instead of the step that makes it", stderr)
	}
}

func TestJoinRefusalLeadsWithALineThatStandsAlone(t *testing.T) {
	_, token := aJoinTicket(t)
	_, _, stderr := run(t, "member", "join", "hub.example.com:4021", "--token", token, "--config-dir", t.TempDir())
	first := strings.SplitN(strings.TrimSpace(stderr), "\n", 2)[0]
	for _, want := range []string{"has not been set up", "caramelo hub setup"} {
		if !strings.Contains(first, want) {
			t.Errorf("first line = %q, want it alone to say %q", first, want)
		}
	}
}

func TestJoinPreflightOnANeverSetUpMachineListsEveryPreconditionInSetupOrder(t *testing.T) {
	cfg := serverconfig.Default()
	cfg.User = "caramelo-nobody-for-tests"
	cfg.StateDir = t.TempDir()
	dir := t.TempDir()

	pre, err := joinPreflightFor(dir, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pre.missing) != 4 {
		t.Fatalf("missing = %q, want one line for each of the user, the configuration, rootless Docker and the key", pre.missing)
	}
	for i, want := range []string{"user", "dirs", "docker-rootless", "vpn"} {
		if !strings.Contains(pre.missing[i], want) {
			t.Errorf("missing[%d] = %q, want it to name setup's %q step", i, pre.missing[i], want)
		}
		if !strings.Contains(pre.missing[i], "caramelo hub setup") {
			t.Errorf("missing[%d] = %q, want it to name `caramelo hub setup`", i, pre.missing[i])
		}
	}
	if !strings.Contains(pre.missing[0], cfg.User) {
		t.Errorf("the user line %q does not name the user %s", pre.missing[0], cfg.User)
	}
	if !strings.Contains(pre.missing[1], serverconfig.Path(dir)) {
		t.Errorf("the configuration line %q does not name %s", pre.missing[1], serverconfig.Path(dir))
	}
	if !strings.Contains(pre.missing[3], cfg.VPNKeyPath()) {
		t.Errorf("the key line %q does not name %s", pre.missing[3], cfg.VPNKeyPath())
	}
	if err := pre.err(); err == nil {
		t.Fatal("a machine missing all four preconditions passed the preflight")
	} else if first := strings.SplitN(err.Error(), "\n", 2)[0]; !strings.Contains(first, "sudo caramelo hub setup") {
		t.Errorf("first line = %q, want it to name `sudo caramelo hub setup`", first)
	}
}

func TestJoinOnAMachineWithAConfigAndNoKeyNamesTheKey(t *testing.T) {
	configDir, cfg := tempServerConfig(t)
	_, token := aJoinTicket(t)

	code, stdout, stderr := run(t, "member", "join", "hub.example.com:4021", "--token", token, "--config-dir", configDir)
	if code != ExitError {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitError, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	for _, want := range []string{cfg.VPNKeyPath(), "caramelo hub setup"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to name %q", stderr, want)
		}
	}
	if strings.Contains(stderr, serverconfig.Path(configDir)) {
		t.Errorf("stderr = %q, names the configuration that is there", stderr)
	}
}

func TestJoinOnAMachineWhoseConfigurationWillNotParseDoesNotCallItUnsetUp(t *testing.T) {
	configDir := t.TempDir()
	if err := os.WriteFile(serverconfig.Path(configDir), []byte("user: [caramelo\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	_, token := aJoinTicket(t)

	code, _, stderr := run(t, "member", "join", "hub.example.com:4021", "--token", token, "--config-dir", configDir)
	if code != ExitError {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitError, stderr)
	}
	if !strings.Contains(stderr, "read this machine's configuration") {
		t.Errorf("stderr = %q, want it to say the configuration could not be read", stderr)
	}
	if strings.Contains(stderr, "has not been set up") {
		t.Errorf("stderr = %q, reports a machine with a broken configuration as one that was never set up", stderr)
	}
}

func TestJoiningTheHubThisMachineIsAlreadyInStillReportsNoChange(t *testing.T) {
	configDir, cfg := tempServerConfig(t)
	ticket, token := aJoinTicket(t)
	cfg.VPNSubnet = "10.87.0.0/16"
	cfg.Fleet = serverconfig.Fleet{
		Role:   serverconfig.RoleMember,
		Name:   "m1",
		Subnet: "10.87.0.0/16",
		Hub: serverconfig.FleetHub{
			Name: ticket.Hub, Endpoint: ticket.Endpoint, Address: ticket.Address, PublicKey: ticket.PublicKey,
		},
	}
	if err := serverconfig.Save(configDir, cfg, 0o640); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := run(t, "member", "join", ticket.Endpoint, "--token", token, "--config-dir", configDir)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitOK, stderr)
	}
	if !strings.Contains(stdout, ticket.Hub) {
		t.Errorf("stdout = %q, want it to name the hub it is already a member of", stdout)
	}
}

func TestJoinHelpSaysItRunsAfterServerSetup(t *testing.T) {
	root := manualRoot(t)
	cmd, _, err := root.Find([]string{"member", "join"})
	if err != nil {
		t.Fatal(err)
	}
	help := cmd.Short + "\n" + cmd.Long
	for _, want := range []string{"caramelo hub setup", "caramelo member add"} {
		if !strings.Contains(help, want) {
			t.Errorf("the help of %q does not name %q", cmd.CommandPath(), want)
		}
	}
}
