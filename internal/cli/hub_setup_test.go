package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup"
)

func TestServerSetupIsRegisteredWithItsFlags(t *testing.T) {
	code, stdout, _ := run(t, "hub", "setup", "--help")
	if code != ExitOK {
		t.Fatalf("exit code = %d", code)
	}
	for _, flag := range []string{
		"--config-dir", "--state-dir", "--data-dir", "--user", "--group", "--ssh-port",
		"--bind", "--authorized-keys", "--yes", "--force", "--low-ports", "--dry-run", "--no-packages",
		"--target", "--name", "--binary", "--release",
	} {
		if !strings.Contains(stdout, flag) {
			t.Errorf("hub setup has no %s flag:\n%s", flag, stdout)
		}
	}
}

func TestServerSetupRefusesWithoutAnAnswer(t *testing.T) {

	code, _, stderr := run(t, "hub", "setup", "--config-dir", t.TempDir())
	if code != ExitError {
		t.Fatalf("exit code = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Errorf("stderr %q does not say how to confirm", stderr)
	}
}

func TestResolveSetupConfigLayersDefaultsFileAndFlags(t *testing.T) {
	dir := t.TempDir()
	saved := serverconfig.Default()
	saved.Name, saved.Hub.Fleet = "box", "home"
	saved.SSHPort = 5022
	saved.DataDir = "/srv/caramelo"
	if err := serverconfig.Save(dir, saved, 0o640); err != nil {
		t.Fatal(err)
	}

	a := &app{}
	cmd := a.hubSetupCmd()
	if err := cmd.Flags().Parse([]string{"--data-dir", "/mnt/big"}); err != nil {
		t.Fatal(err)
	}
	flags := serverconfig.Default()
	flags.DataDir = "/mnt/big"

	flags.Name = "renamed-by-the-hostname"
	got, err := resolveSetupConfig(cmd, dir, flags, "")
	if err != nil {
		t.Fatalf("resolveSetupConfig() error: %v", err)
	}
	if got.Name != "box" || got.Hub.Fleet != "home" {
		t.Errorf("machine %q of fleet %q, want box of home: a second setup renames nothing",
			got.Name, got.Hub.Fleet)
	}
	if got.SSHPort != 5022 {
		t.Errorf("ssh port = %d, want the one the machine already uses", got.SSHPort)
	}
	if got.DataDir != "/mnt/big" {
		t.Errorf("data dir = %q, want the flag to win", got.DataDir)
	}
}

func TestResolveSetupConfigRejectsABadPort(t *testing.T) {
	a := &app{}
	cmd := a.hubSetupCmd()
	if err := cmd.Flags().Parse([]string{"--ssh-port", "99999"}); err != nil {
		t.Fatal(err)
	}
	flags := serverconfig.Default()
	flags.SSHPort = 99999
	flags.Name = "box"
	if _, err := resolveSetupConfig(cmd, t.TempDir(), flags, ""); err == nil {
		t.Fatal("resolveSetupConfig() accepted a port out of range")
	}
}

func TestSetupPlanNamesWhatWillChange(t *testing.T) {
	env := &setup.Env{
		Config: namedHub("box"), ConfigDir: serverconfig.DefaultConfigDir,
		Opts: setup.Options{InstallPackages: true, AuthorizedKeysFile: "/root/keys.pub"},
	}
	plan := setupPlan(env)
	for _, want := range []string{"/etc/caramelo", "/var/lib/caramelo", "/mnt/caramelo", "caramelo:caramelo", "0.0.0.0:4022", "/root/keys.pub", "docker-ce"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan does not mention %q:\n%s", want, plan)
		}
	}
	for _, want := range []string{"4.0 GiB at /var/lib/caramelo.swapfile", "vm.swappiness 10"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan does not say what swap it will allocate (%q):\n%s", want, plan)
		}
	}
	env.Config.Swap = serverconfig.Swap{Backend: serverconfig.SwapOff}
	if !strings.Contains(setupPlan(env), "none (swap: off)") {
		t.Errorf("plan does not say that swap is off:\n%s", setupPlan(env))
	}
	env.Config = serverconfig.Default()

	env.Opts.InstallPackages = false
	if !strings.Contains(setupPlan(env), "--no-packages") {
		t.Errorf("plan does not say that packages are skipped:\n%s", setupPlan(env))
	}
}

func TestWriteSetupReportSummarises(t *testing.T) {
	report := setup.Report{
		Results: []setup.Result{
			{Step: "preflight", Status: setup.StatusOK},
			{Step: "user", Status: setup.StatusChanged},
			{Step: "summary", Status: setup.StatusOK, Detail: "ssh -p 4022 caramelo@10.0.0.2"},
		},
		Changed: 1,
	}
	var b strings.Builder
	if err := writeSetupReport(&b, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "3 steps, 1 changed, 0 failed") {
		t.Errorf("summary = %q", b.String())
	}
	if !strings.Contains(b.String(), "try: ssh -p 4022 caramelo@10.0.0.2 status") {
		t.Errorf("summary does not repeat how to connect: %q", b.String())
	}
}

func TestServerStatusOnAMachineWithoutCaramelo(t *testing.T) {
	dir := t.TempDir()
	st := hubStatusOf(context.Background(), stubRunner{}, dir)
	if st.Installed {
		t.Errorf("an empty directory was reported as an installation")
	}
	if st.User.Exists {
		t.Errorf("the caramelo user was reported as existing: %+v", st.User)
	}
	if st.Caramelod.Active || st.Caramelod.State != "unknown" {
		t.Errorf("caramelod = %+v, want unknown", st.Caramelod)
	}
	var b strings.Builder
	if err := writeHubStatus(&b, st); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "not set up") {
		t.Errorf("status does not say the machine is not set up:\n%s", b.String())
	}
}

func TestServerStatusJSONAndExitCode(t *testing.T) {
	code, stdout, _ := run(t, "hub", "status", "--config-dir", t.TempDir(), "--json")
	if code != ExitError {
		t.Errorf("exit code = %d, want %d when caramelod is not running", code, ExitError)
	}
	var st hubStatus
	if err := json.Unmarshal([]byte(stdout), &st); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if st.Port.Port != serverconfig.DefaultSSHPort {
		t.Errorf("port = %d, want the default", st.Port.Port)
	}
}

func TestUninstallReportsWhatItDidWithoutPurge(t *testing.T) {
	stub := &scriptedRunner{results: map[string]runner.Result{}}
	u := &uninstaller{
		cfg: serverconfig.Default(), configDir: t.TempDir(),
		exec: stub,
	}
	report := u.run(context.Background())
	if report.Failed != 0 {
		t.Fatalf("uninstall failed: %+v", report.Actions)
	}
	var kept bool
	for _, act := range report.Actions {
		if act.Action == "purge" && act.Status == "skipped" {
			kept = true
		}
	}
	if !kept {
		t.Errorf("uninstall did not say what it kept: %+v", report.Actions)
	}
	if !stub.ran("systemctl daemon-reload") {
		t.Errorf("systemd was not reloaded: %v", stub.lines)
	}
	if stub.ran("userdel") || stub.ran("rm -rf -- /mnt/caramelo") {
		t.Errorf("uninstall removed data without --purge: %v", stub.lines)
	}
}

func TestUninstallDryRunChangesNothing(t *testing.T) {
	stub := &scriptedRunner{results: map[string]runner.Result{
		"getent passwd caramelo": {Stdout: "caramelo:x:999:989::/var/lib/caramelo:/bin/sh\n"},
	}}
	u := &uninstaller{cfg: serverconfig.Default(), configDir: t.TempDir(), purge: true, dryRun: true, exec: stub}
	report := u.run(context.Background())
	for _, act := range report.Actions {
		if act.Status == "done" {
			t.Errorf("a dry run did something: %+v", act)
		}
	}
	for _, line := range stub.lines {
		readOnly := strings.HasPrefix(line, "getent ") ||
			strings.HasPrefix(line, "dpkg-query ") ||
			strings.HasPrefix(line, "systemd-escape ")
		if !readOnly {
			t.Errorf("a dry run ran %q, want only read-only commands: %v", line, stub.lines)
		}
	}
}

type stubRunner struct{}

func (stubRunner) Run(context.Context, runner.Cmd) (runner.Result, error) {
	return runner.Result{}, context.Canceled
}

type scriptedRunner struct {
	results map[string]runner.Result
	lines   []string
}

func (s *scriptedRunner) Run(_ context.Context, c runner.Cmd) (runner.Result, error) {
	line := strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " "))
	s.lines = append(s.lines, line)
	if res, ok := s.results[line]; ok {
		return res, nil
	}
	return runner.Result{}, nil
}

func (s *scriptedRunner) ran(prefix string) bool {
	for _, l := range s.lines {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func TestSetupStepsAreTheWholePlanInOrder(t *testing.T) {
	var names []string
	for _, s := range setupSteps() {
		names = append(names, s.Name())
	}
	want := []string{"preflight", "firewall", "gauge", "swap", "user", "dirs", "host-config", "docker-packages",
		"docker-rootless", "vpn", "vault", "caramelod", "edge", "peer", "join", "summary"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("steps = %v, want %v", names, want)
	}
}

func TestResolveSetupConfigKeepsTheNetworkAMachineAlreadyHas(t *testing.T) {
	dir := t.TempDir()
	existing := namedHub("box")
	existing.VPNSubnet = "10.99.0.0/16"
	existing.VPNListen = "0.0.0.0:4123"
	existing.APIListen = serverconfig.APIListenVPN
	if err := serverconfig.Save(dir, existing, 0o640); err != nil {
		t.Fatal(err)
	}

	a := &app{}
	cmd := a.hubSetupCmd()
	if err := cmd.Flags().Parse([]string{"--api-listen", "both"}); err != nil {
		t.Fatal(err)
	}
	flags := serverconfig.Default()
	flags.APIListen = serverconfig.APIListenBoth

	got, err := resolveSetupConfig(cmd, dir, flags, "")
	if err != nil {
		t.Fatalf("resolveSetupConfig() error: %v", err)
	}

	if got.VPNSubnet != "10.99.0.0/16" || got.VPNListen != "0.0.0.0:4123" {
		t.Errorf("network = %s on %s, want the one the machine already uses", got.VPNSubnet, got.VPNListen)
	}
	if got.APIListen != serverconfig.APIListenBoth {
		t.Errorf("api_listen = %q, want the flag to win", got.APIListen)
	}
}

func TestResolveSetupConfigKeepsTheSwapAMachineAlreadyHas(t *testing.T) {
	dir := t.TempDir()
	existing := namedHub("box")
	existing.Swap = serverconfig.Swap{Backend: serverconfig.SwapOff, Swappiness: 10}
	if err := serverconfig.Save(dir, existing, 0o640); err != nil {
		t.Fatal(err)
	}

	a := &app{}
	cmd := a.hubSetupCmd()
	if err := cmd.Flags().Parse([]string{"--api-listen", "both"}); err != nil {
		t.Fatal(err)
	}
	flags := serverconfig.Default()
	flags.APIListen = serverconfig.APIListenBoth

	got, err := resolveSetupConfig(cmd, dir, flags, "")
	if err != nil {
		t.Fatalf("resolveSetupConfig() error: %v", err)
	}
	if got.Swap != existing.Swap {
		t.Errorf("swap = %+v, want the one the machine already has (%+v)", got.Swap, existing.Swap)
	}
}

func TestParseSwapFlag(t *testing.T) {
	tests := []struct {
		arg     string
		want    serverconfig.Swap
		wantErr bool
	}{
		{arg: "4G", want: serverconfig.Swap{Backend: serverconfig.SwapFile, SizeBytes: 4 << 30, Swappiness: serverconfig.DefaultSwappiness}},
		{arg: "512m", want: serverconfig.Swap{Backend: serverconfig.SwapFile, SizeBytes: 512 << 20, Swappiness: serverconfig.DefaultSwappiness}},
		{arg: "off", want: serverconfig.Swap{Backend: serverconfig.SwapOff, Swappiness: serverconfig.DefaultSwappiness}},
		{arg: "OFF", want: serverconfig.Swap{Backend: serverconfig.SwapOff, Swappiness: serverconfig.DefaultSwappiness}},
		{arg: "0", want: serverconfig.Swap{Backend: serverconfig.SwapOff, Swappiness: serverconfig.DefaultSwappiness}},
		{arg: "none", want: serverconfig.Swap{Backend: serverconfig.SwapOff, Swappiness: serverconfig.DefaultSwappiness}},
		{arg: "zram", want: serverconfig.Swap{Backend: serverconfig.SwapZram, Swappiness: serverconfig.DefaultSwappiness}},
		{arg: "plenty", wantErr: true},
		{arg: "", wantErr: true},
	}
	for _, tc := range tests {
		got, err := parseSwapFlag(tc.arg)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseSwapFlag(%q) = %+v, want an error", tc.arg, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSwapFlag(%q) error: %v", tc.arg, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseSwapFlag(%q) = %+v, want %+v", tc.arg, got, tc.want)
		}
	}
}

func TestSetupRejectsSwapValuesTheMachineCannotHave(t *testing.T) {
	for _, arg := range []string{"zram", "plenty", "64m"} {
		code, _, stderr := run(t, "hub", "setup", "--yes", "--swap", arg, "--config-dir", t.TempDir())
		if code == ExitOK {
			t.Errorf("--swap %s was accepted", arg)
		}
		if !strings.Contains(stderr, "swap") {
			t.Errorf("--swap %s: stderr does not say what is wrong: %q", arg, stderr)
		}
	}
}

func TestResolveSetupConfigRejectsABadNetwork(t *testing.T) {
	for _, args := range [][]string{
		{"--vpn-subnet", "10.86.0.0/28"},
		{"--vpn-subnet", "not-a-subnet"},
		{"--vpn-listen", "4021"},
		{"--api-listen", "sometimes"},
	} {
		a := &app{}
		cmd := a.hubSetupCmd()
		if err := cmd.Flags().Parse(args); err != nil {
			t.Fatal(err)
		}
		flags := serverconfig.Default()
		switch args[0] {
		case "--vpn-subnet":
			flags.VPNSubnet = args[1]
		case "--vpn-listen":
			flags.VPNListen = args[1]
		case "--api-listen":
			flags.APIListen = args[1]
		}
		flags.Name = "box"
		if _, err := resolveSetupConfig(cmd, t.TempDir(), flags, ""); err == nil {
			t.Errorf("resolveSetupConfig accepted %v", args)
		}
	}
}

func TestSetupRefusesAnUnusablePeerBeforeTouchingTheMachine(t *testing.T) {
	noCommanderConfig(t)
	code, _, stderr := run(t, "hub", "setup", "--yes", "--peer", "laptop nonsense")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "--peer") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestSetupPlanNamesTheNetworkAndThePeer(t *testing.T) {
	env := &setup.Env{
		Config: serverconfig.Default(), ConfigDir: serverconfig.DefaultConfigDir,
		Opts: setup.Options{
			InstallPackages: true,
			Peer:            setup.PeerSpec{Name: "alex-laptop", PublicKey: testPeerKey},
		},
	}
	plan := setupPlan(env)
	for _, want := range []string{"10.86.0.0/16", "0.0.0.0:4021", "alex-laptop"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan does not mention %q:\n%s", want, plan)
		}
	}

	if strings.Contains(plan, testPeerKey) {
		t.Errorf("the plan prints the whole key:\n%s", plan)
	}
}

func TestSetupPeerFlagForms(t *testing.T) {
	for _, args := range [][]string{
		{"hub", "setup", "--dry-run", "--peer", "agent-7", testPeerKey},
		{"hub", "setup", "--dry-run", "--peer", "agent-7 " + testPeerKey},
		{"hub", "setup", "--dry-run", "--peer", "agent-7=" + testPeerKey},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(append(args, "--json"), &stdout, &stderr); code == ExitUsage {
			t.Errorf("caramelo %v: usage error: %s", args, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"hub", "setup", "--dry-run", "stray"}, &stdout, &stderr); code != ExitUsage {
		t.Errorf("a stray argument exited %d, want %d", code, ExitUsage)
	}
}

func namedHub(name string) serverconfig.Config {
	c := serverconfig.Default()
	c.Name, c.Hub.Fleet = name, name
	return c
}

func aMemberConfig(t *testing.T, dir string) serverconfig.Config {
	t.Helper()
	c := serverconfig.Default()
	c.VPNSubnet = "10.87.0.0/16"
	c.Name, c.Role, c.Hub = "m1", serverconfig.RoleMember, serverconfig.Hub{}
	c.Member = serverconfig.Member{
		Fleet: "home", Subnet: "10.87.0.0/16",
		Hub: serverconfig.MemberHub{
			Endpoint: "hub.example.com:4021", Address: "10.86.0.1",
			PublicKey: "0000000000000000000000000000000000000000000=",
		},
	}
	if err := serverconfig.Save(dir, c, 0o640); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return c
}

func TestASecondSetupKeepsTheMemberBlock(t *testing.T) {
	dir := t.TempDir()
	existing := aMemberConfig(t, dir)
	cmd := (&app{}).hubSetupCmd()
	if err := cmd.Flags().Set("data-dir", "/mnt/other"); err != nil {
		t.Fatal(err)
	}
	got, err := resolveSetupConfig(cmd, dir, serverconfig.Config{DataDir: "/mnt/other"}, "")
	if err != nil {
		t.Fatalf("resolveSetupConfig: %v", err)
	}
	if got.Member != existing.Member || got.Role != existing.Role || !got.Hub.Empty() {
		t.Errorf("the member became %+v (role %q, hub %+v), want %+v",
			got.Member, got.Role, got.Hub, existing.Member)
	}
	if got.DataDir != "/mnt/other" {
		t.Errorf("data-dir = %q, want the flag's value", got.DataDir)
	}
}

func TestSetupRefusesToRenameTheFleetOfAMember(t *testing.T) {
	dir := t.TempDir()
	aMemberConfig(t, dir)
	cmd := (&app{}).hubSetupCmd()
	if err := cmd.Flags().Set("fleet", "work"); err != nil {
		t.Fatal(err)
	}
	_, err := resolveSetupConfig(cmd, dir, serverconfig.Config{}, "work")
	if err == nil {
		t.Fatal("--fleet renamed the fleet a member belongs to")
	}
	for _, want := range []string{"home", "member leave"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q, want it to mention %q", err, want)
		}
	}
}

func TestSetupNamesTheMachineAndItsFleet(t *testing.T) {
	dir := t.TempDir()
	cmd := (&app{}).hubSetupCmd()
	for flag, value := range map[string]string{"name": "box", "fleet": "home"} {
		if err := cmd.Flags().Set(flag, value); err != nil {
			t.Fatal(err)
		}
	}
	got, err := resolveSetupConfig(cmd, dir, serverconfig.Config{Name: "box"}, "home")
	if err != nil {
		t.Fatalf("resolveSetupConfig: %v", err)
	}
	if got.Name != "box" || got.Role != serverconfig.RoleHub || got.Hub.Fleet != "home" {
		t.Errorf("config = %q %q %q, want box, hub, home", got.Name, got.Role, got.Hub.Fleet)
	}

	plain := (&app{}).hubSetupCmd()
	if err := plain.Flags().Set("name", "box"); err != nil {
		t.Fatal(err)
	}
	fell, err := resolveSetupConfig(plain, t.TempDir(), serverconfig.Config{Name: "box"}, "")
	if err != nil {
		t.Fatalf("resolveSetupConfig: %v", err)
	}
	if fell.Hub.Fleet != "box" {
		t.Errorf("fleet = %q, want the machine's own name when --fleet is not given", fell.Hub.Fleet)
	}
}
