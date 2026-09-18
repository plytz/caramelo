package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/bootstrap"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup"
	"github.com/plytz/caramelo/internal/vpnclient"
)

type scriptedShell struct {
	target remote.Target
	report setup.Report
	ran    []string
}

func (s *scriptedShell) Run(_ context.Context, c Cmd) (int, error) {
	s.ran = append(s.ran, c.Line)
	if c.Stdin != nil {
		_, _ = io.Copy(io.Discard, c.Stdin)
	}
	switch {
	case strings.HasPrefix(c.Line, "uname -sm"):
		arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
		if arch == "" {
			arch = runtime.GOARCH
		}
		_, _ = io.WriteString(c.Stdout, "Linux "+arch+"\n1000\nsudo\nkeys\n")
	case strings.HasPrefix(c.Line, "d=$(mktemp"):
		_, _ = io.WriteString(c.Stdout, "/tmp/caramelo-setup.test\n")
	case strings.Contains(c.Line, "hub setup"):
		_, _ = io.WriteString(c.Stderr, "[changed] user: caramelo (uid 999)\n")
		b, _ := json.Marshal(s.report)
		_, _ = c.Stdout.Write(append(b, '\n'))
		if s.report.Failed > 0 {
			return ExitError, nil
		}
	}
	return 0, nil
}

type Cmd = bootstrap.Cmd

func useScriptedTarget(t *testing.T, report setup.Report, verify func(address string) (json.RawMessage, error)) *scriptedShell {
	t.Helper()
	initializedCommander(t)
	placeholderLinuxSibling(t)
	sh := &scriptedShell{report: report}
	prevShell, prevVerify, prevJoin := newBootstrapShell, verifyMachine, joinMachine
	newBootstrapShell = func(target remote.Target) (bootstrap.Shell, error) {
		sh.target = target
		return sh, nil
	}
	verifyMachine = func(_ context.Context, _ *app, address string) (json.RawMessage, error) {
		return verify(address)
	}

	joinMachine = func(context.Context, string, vpnclient.Control) (*vpnclient.State, error) {
		return &vpnclient.State{Machine: "box", Endpoint: "10.0.0.5:4021", LastHandshake: time.Now()}, nil
	}
	t.Cleanup(func() { newBootstrapShell, verifyMachine, joinMachine = prevShell, prevVerify, prevJoin })
	return sh
}

func okVerify(address string) (json.RawMessage, error) {
	return json.RawMessage(`{"transport":"ssh","machine":"` + address + `"}`), nil
}

func greenReport() setup.Report {
	return setup.Report{RunID: "20260908T000000.000Z", Changed: 8, Results: []setup.Result{
		{Step: "preflight", Status: setup.StatusChanged}, {Step: "summary", Status: setup.StatusOK, Detail: "ssh -p 4022 caramelo@10.0.0.5"},
	}}
}

func TestSetupTargetBootstrapsRecordsAndVerifies(t *testing.T) {
	sh := useScriptedTarget(t, greenReport(), okVerify)

	code, stdout, stderr := run(t, "hub", "setup", "--target", "admin@box.example", "--yes", "--json",
		"--data-dir", "/mnt/big", "--force", "--name", "prod")
	if code != ExitOK {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	if sh.target.User != "admin" || sh.target.Host != "box.example" || sh.target.Port != 22 {
		t.Errorf("target = %+v, want admin@box.example:22", sh.target)
	}
	setupLine := ""
	for _, l := range sh.ran {
		if strings.Contains(l, "hub setup") {
			setupLine = l
		}
	}
	for _, want := range []string{"sudo -n ", "--yes --json", "--data-dir=/mnt/big", "--force", "--name=prod"} {
		if !strings.Contains(setupLine, want) {
			t.Errorf("setup line %q lacks %q", setupLine, want)
		}
	}
	for _, unwanted := range []string{"--target", "--json --json"} {
		if strings.Contains(setupLine, unwanted) {
			t.Errorf("setup line %q forwards %q", setupLine, unwanted)
		}
	}
	if !strings.Contains(stderr, "[changed] user") {
		t.Errorf("remote step lines did not stream through: %q", stderr)
	}

	var res bootstrapResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("stdout is not a bootstrap result: %v\n%s", err, stdout)
	}
	if res.Target != "admin@box.example:22" || res.Setup.Changed != 8 || !res.Verified {
		t.Errorf("result = %+v", res)
	}
	if res.Machine == nil || res.Machine.Name != "prod" || res.Machine.Address != "caramelo@box.example:4022" || !res.Machine.Default {
		t.Errorf("machine = %+v", res.Machine)
	}
	if !strings.Contains(string(res.Status), `"transport":"ssh"`) {
		t.Errorf("status = %s", res.Status)
	}

	cfg, err := remote.LoadCommanderConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Commander.DefaultMachine != "prod" || cfg.Commander.Machines["prod"] != "caramelo@box.example:4022" {
		t.Errorf("commander config = %+v", cfg)
	}
}

func TestSetupTargetHumanOutputAndCustomPort(t *testing.T) {
	sh := useScriptedTarget(t, greenReport(), okVerify)

	if err := remote.SaveCommanderConfig(remote.CommanderConfig{Commander: remote.Commander{DefaultMachine: "first", Machines: map[string]string{"first": "caramelo@a:4022"}}}); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := run(t, "hub", "setup", "--target", "10.0.0.5:2222", "--yes", "--ssh-port", "5022")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if sh.target.Port != 2222 || sh.target.User != localUser() {
		t.Errorf("target = %+v", sh.target)
	}
	for _, want := range []string{"2 steps, 1 changed, 0 failed", "machine 10.0.0.5 (caramelo@10.0.0.5:5022) recorded in", "try: caramelo --machine 10.0.0.5 status"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	cfg, _ := remote.LoadCommanderConfig()
	if cfg.Commander.DefaultMachine != "first" || cfg.Commander.Machines["10.0.0.5"] != "caramelo@10.0.0.5:5022" {
		t.Errorf("commander config = %+v", cfg)
	}
}

func TestSetupTargetDryRunRecordsNothing(t *testing.T) {
	rep := greenReport()
	rep.DryRun = true
	sh := useScriptedTarget(t, rep, func(string) (json.RawMessage, error) {
		t.Error("verify must not run on a dry run")
		return nil, nil
	})
	code, stdout, _ := run(t, "hub", "setup", "--target", "root@box", "--dry-run")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout, "would change") {
		t.Errorf("stdout = %q", stdout)
	}
	joined := strings.Join(sh.ran, "\n")
	if !strings.Contains(joined, "--dry-run") {
		t.Errorf("--dry-run not forwarded:\n%s", joined)
	}
	cfg, _ := remote.LoadCommanderConfig()
	if len(cfg.Commander.Machines) != 0 {
		t.Errorf("dry run recorded a machine: %+v", cfg)
	}
}

func TestSetupTargetFailedStepExitsOne(t *testing.T) {
	rep := setup.Report{RunID: "x", Failed: 1, Results: []setup.Result{{Step: "preflight", Status: setup.StatusFailed, Error: "too small"}}}
	useScriptedTarget(t, rep, func(string) (json.RawMessage, error) {
		t.Error("verify must not run after a failed setup")
		return nil, nil
	})
	code, stdout, _ := run(t, "hub", "setup", "--target", "root@box", "--yes", "--json")
	if code != ExitError {
		t.Fatalf("exit %d, want %d", code, ExitError)
	}
	var res bootstrapResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("stdout: %v", err)
	}
	if res.Setup.Failed != 1 || res.Machine != nil || res.Verified {
		t.Errorf("result = %+v", res)
	}
}

func TestSetupTargetUnreachableAPIIsAnError(t *testing.T) {
	useScriptedTarget(t, greenReport(), func(string) (json.RawMessage, error) {
		return nil, context.DeadlineExceeded
	})
	code, _, stderr := run(t, "hub", "setup", "--target", "root@box", "--yes")
	if code != ExitError {
		t.Fatalf("exit %d, want %d", code, ExitError)
	}
	for _, want := range []string{"recorded as machine \"box\"", "port 4022", "caramelo --machine box status"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q: %s", want, stderr)
		}
	}

	cfg, _ := remote.LoadCommanderConfig()
	if cfg.Commander.Machines["box"] == "" {
		t.Errorf("machine not recorded: %+v", cfg)
	}
}

func TestSetupTargetNeedsYesWithoutATerminal(t *testing.T) {
	sh := useScriptedTarget(t, greenReport(), okVerify)
	code, _, stderr := run(t, "hub", "setup", "--target", "root@box")
	if code != ExitError || !strings.Contains(stderr, "--yes") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if len(sh.ran) != 0 {
		t.Errorf("the target was touched without confirmation: %q", sh.ran)
	}
}

func TestSetupTargetUsageErrors(t *testing.T) {
	useScriptedTarget(t, greenReport(), okVerify)
	cases := [][]string{
		{"--target", "@box", "--yes"},
		{"--target", "box", "--yes", "--ssh-port", "70000"},
		{"--target", "box", "--yes", "--authorized-keys", filepath.Join(t.TempDir(), "missing.pub")},
	}
	for _, args := range cases {
		code, _, _ := run(t, append([]string{"hub", "setup"}, args...)...)
		if code != ExitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, ExitUsage)
		}
	}
}

func TestBootstrapPlanNamesTheTarget(t *testing.T) {
	target, _ := remote.ParseTargetWith("admin@box", "me", 22)
	f := bootstrapFlags{cfg: serverconfig.Default(), configDir: "/etc/caramelo", authorizedKeys: "/home/me/keys.pub"}
	plan := bootstrapPlan(target, f)
	for _, want := range []string{"admin@box:22", "/etc/caramelo", "/home/me/keys.pub (local file)", `"box"`, "0.0.0.0:4022"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan lacks %q:\n%s", want, plan)
		}
	}
	f.authorizedKeys = ""
	if !strings.Contains(bootstrapPlan(target, f), "already authorized for admin on box") {
		t.Errorf("plan does not explain the key default:\n%s", bootstrapPlan(target, f))
	}
}

func TestSetupTargetReportNamesThePlatform(t *testing.T) {
	useScriptedTarget(t, greenReport(), okVerify)
	code, stdout, stderr := run(t, "hub", "setup", "--target", "10.0.0.5", "--yes")
	if code != ExitOK {
		t.Fatalf("exit %d (%s)", code, stderr)
	}
	want := "10.0.0.5:22 is linux/" + runtime.GOARCH
	if !strings.Contains(stdout, want) {
		t.Errorf("the report does not say %q:\n%s", want, stdout)
	}

	target, _ := remote.ParseTargetWith("admin@box", "me", 22)
	plan := bootstrapPlan(target, bootstrapFlags{cfg: serverconfig.Default(), configDir: "/etc/caramelo"})
	if !strings.Contains(plan, "caramelo-<os>-<arch>") {
		t.Errorf("the plan does not say a sibling binary may be shipped:\n%s", plan)
	}
}

func TestSetupTargetAdmitsTheCommanderAsAPeer(t *testing.T) {
	sh := useScriptedTarget(t, greenReport(), okVerify)
	code, stdout, stderr := run(t, "hub", "setup", "--target", "root@box", "--yes", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	setupLine := lastSetupLine(sh)
	if !strings.Contains(setupLine, "--peer ") {
		t.Fatalf("the setup line does not admit the commander: %q", setupLine)
	}

	var res bootstrapResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatal(err)
	}
	if res.Peer == nil || res.Peer.Name == "" || res.Peer.PublicKey == "" {
		t.Fatalf("peer = %+v", res.Peer)
	}

	if !strings.Contains(setupLine, res.Peer.PublicKey) {
		t.Errorf("the setup line does not carry the public key: %q", setupLine)
	}
	if err := res.Peer.Validate(); err != nil {
		t.Errorf("the peer this run registered is not usable: %v", err)
	}

	path, err := vpnclient.KeyPath("box")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("no key for the machine: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode = %o, want 600", fi.Mode().Perm())
	}
	key, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(setupLine, strings.TrimSpace(string(key))) {
		t.Error("the private key was sent to the target")
	}

	again, err := ensurePeerKey("box")
	if err != nil || again.PublicKey != res.Peer.PublicKey {
		t.Errorf("a second run made a new key: %+v, %v", again, err)
	}
}

func TestSetupTargetForwardsAnExplicitPeer(t *testing.T) {
	sh := useScriptedTarget(t, greenReport(), okVerify)
	code, stdout, _ := run(t, "hub", "setup", "--target", "root@box", "--yes", "--json",
		"--peer", "agent-7 "+testPeerKey)
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	setupLine := lastSetupLine(sh)
	if !strings.Contains(setupLine, "--peer 'agent-7 "+testPeerKey+"'") {
		t.Errorf("setup line = %q", setupLine)
	}
	var res bootstrapResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatal(err)
	}
	if res.Peer == nil || res.Peer.Name != "agent-7" {
		t.Errorf("peer = %+v", res.Peer)
	}
	if _, err := os.Stat(mustKeyPath(t, "box")); err == nil {
		t.Error("a key was generated although an identity was given")
	}
}

func TestSetupTargetDryRunGeneratesNoKey(t *testing.T) {
	rep := greenReport()
	rep.DryRun = true
	sh := useScriptedTarget(t, rep, func(string) (json.RawMessage, error) { return nil, nil })
	if code, _, _ := run(t, "hub", "setup", "--target", "root@box", "--dry-run"); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(lastSetupLine(sh), "--peer") {
		t.Error("a dry run admitted a peer")
	}
	if _, err := os.Stat(mustKeyPath(t, "box")); err == nil {
		t.Error("a dry run generated a key")
	}
}

func TestSetupTargetForwardsTheNetworkFlags(t *testing.T) {
	sh := useScriptedTarget(t, greenReport(), okVerify)
	code, _, _ := run(t, "hub", "setup", "--target", "root@box", "--yes",
		"--vpn-subnet", "10.99.0.0/16", "--vpn-listen", "0.0.0.0:4123", "--api-listen", "vpn")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"--vpn-subnet=10.99.0.0/16", "--vpn-listen=0.0.0.0:4123", "--api-listen=vpn"} {
		if !strings.Contains(lastSetupLine(sh), want) {
			t.Errorf("setup line %q lacks %q", lastSetupLine(sh), want)
		}
	}
}

func TestSetupTargetForwardsTheEdgeFlags(t *testing.T) {
	sh := useScriptedTarget(t, greenReport(), okVerify)
	code, _, _ := run(t, "hub", "setup", "--target", "root@box", "--yes",
		"--edge", "--acme-email", "me@example.com", "--acme-ca", "https://pebble:14000/dir",
		"--tls", "acme", "--no-http3")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"--edge", "--acme-email=me@example.com",
		"--acme-ca=https://pebble:14000/dir", "--tls=acme", "--no-http3"} {
		if !strings.Contains(lastSetupLine(sh), want) {
			t.Errorf("setup line %q lacks %q", lastSetupLine(sh), want)
		}
	}
}

func TestBootstrapPlanNamesTheEdge(t *testing.T) {
	target, _ := remote.ParseTargetWith("admin@box", "me", 22)
	cfg := serverconfig.Default()
	cfg.Edge, cfg.ACMECA = true, "https://pebble:14000/dir"
	plan := bootstrapPlan(target, bootstrapFlags{cfg: cfg, configDir: "/etc/caramelo"})
	for _, want := range []string{"ports 80 and 443", "https://pebble:14000/dir"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan lacks %q:\n%s", want, plan)
		}
	}
	cfg.TLS = "internal"
	if plan := bootstrapPlan(target, bootstrapFlags{cfg: cfg, configDir: "/etc/caramelo"}); !strings.Contains(plan, "internal CA") {
		t.Errorf("plan does not say where an internal machine's certificates come from:\n%s", plan)
	}
	off := serverconfig.Default()
	if plan := bootstrapPlan(target, bootstrapFlags{cfg: off, configDir: "/etc/caramelo"}); strings.Contains(plan, "ports 80 and 443") {
		t.Errorf("a machine set up without --edge should not promise an edge:\n%s", plan)
	}
}

func TestSetupTargetReportsHowItVerified(t *testing.T) {
	useScriptedTarget(t, greenReport(), func(machine string) (json.RawMessage, error) {
		return json.RawMessage(`{"transport":"tunnel","machine":"` + machine + `"}`), nil
	})
	joined := ""
	prev := joinMachine
	joinMachine = func(_ context.Context, machine string, _ vpnclient.Control) (*vpnclient.State, error) {
		joined = machine
		return &vpnclient.State{Machine: machine, Endpoint: "10.0.0.5:4021", LastHandshake: time.Now()}, nil
	}
	t.Cleanup(func() { joinMachine = prev })

	code, stdout, _ := run(t, "hub", "setup", "--target", "root@box", "--yes", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if joined != "box" {
		t.Errorf("the commander joined %q, want the machine it just set up", joined)
	}
	var res bootstrapResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatal(err)
	}
	if res.Transport != "tunnel" {
		t.Errorf("transport = %q, want tunnel", res.Transport)
	}
}

func TestSetupTargetJoinsThroughTheBootstrapShell(t *testing.T) {
	sh := useScriptedTarget(t, greenReport(), okVerify)
	var ctl vpnclient.Control
	prev := joinMachine
	joinMachine = func(_ context.Context, _ string, c vpnclient.Control) (*vpnclient.State, error) {
		ctl = c
		return nil, nil
	}
	t.Cleanup(func() { joinMachine = prev })

	if code, _, stderr := run(t, "hub", "setup", "--target", "root@box", "--yes"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if ctl == nil {
		t.Fatal("the join was given no way to reach the machine")
	}
	var out bytes.Buffer
	if _, err := ctl.Run(context.Background(), "box", []string{"status", "--json"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	last := sh.ran[len(sh.ran)-1]
	if !strings.Contains(last, serverconfig.BinaryPath) || !strings.Contains(last, "status --json") {
		t.Errorf("the invocation the shell ran was %q, want the installed binary's `status --json`", last)
	}

	if !strings.Contains(last, "sudo -n ") {
		t.Errorf("the invocation the shell ran was %q, want it elevated", last)
	}
}

func TestSetupTargetSurvivesANetworkItCannotJoin(t *testing.T) {
	useScriptedTarget(t, greenReport(), okVerify)
	prev := joinMachine
	joinMachine = func(context.Context, string, vpnclient.Control) (*vpnclient.State, error) {
		return nil, errors.New("no handshake")
	}
	t.Cleanup(func() { joinMachine = prev })

	code, _, stderr := run(t, "hub", "setup", "--target", "root@box", "--yes")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "no handshake") {
		t.Errorf("the failure was not reported: %q", stderr)
	}
}

func lastSetupLine(sh *scriptedShell) string {
	out := ""
	for _, l := range sh.ran {
		if strings.Contains(l, "hub setup") {
			out = l
		}
	}
	return out
}

func mustKeyPath(t *testing.T, machine string) string {
	t.Helper()
	path, err := vpnclient.KeyPath(machine)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func placeholderLinuxSibling(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "linux" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(filepath.Dir(exe), bootstrap.SiblingName("linux", runtime.GOARCH))
	if _, err := os.Stat(sibling); err == nil {
		return
	}
	if err := os.WriteFile(sibling, []byte("placeholder: the scripted shell discards what is shipped\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(sibling) })
}

func TestSetupTargetRecordsWhatItsHandshakeSaw(t *testing.T) {
	useScriptedTarget(t, greenReport(), func(machine string) (json.RawMessage, error) {
		return json.RawMessage(`{"transport":"tunnel","machine":"` + machine + `"}`), nil
	})

	code, stdout, stderr := run(t, "hub", "setup", "--target", "root@box", "--yes", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	var res bootstrapResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatal(err)
	}
	if res.Reachability == nil || res.Reachability.Result != vpnclient.ProbeReached {
		t.Fatalf("reachability = %+v, want it to record the handshake it performed", res.Reachability)
	}

	code, stdout, stderr = run(t, "hub", "setup", "--target", "root@box", "--yes")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "udp 4021 reached from here") {
		t.Errorf("stdout = %q, want the one thing that proves a packet arrived", stdout)
	}
}

func TestSetupTargetClaimsNothingWhenItNeverDialled(t *testing.T) {
	useScriptedTarget(t, greenReport(), func(machine string) (json.RawMessage, error) {
		return json.RawMessage(`{"transport":"tunnel","machine":"` + machine + `"}`), nil
	})
	prev := joinMachine
	joinMachine = func(context.Context, string, vpnclient.Control) (*vpnclient.State, error) { return nil, nil }
	t.Cleanup(func() { joinMachine = prev })

	code, stdout, stderr := run(t, "hub", "setup", "--target", "root@box", "--yes", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	var res bootstrapResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatal(err)
	}
	if res.Reachability != nil {
		t.Errorf("a join that never dialled claimed %+v", res.Reachability)
	}
}

func TestSetupTargetDoesNotClaimReachedOverSSH(t *testing.T) {
	useScriptedTarget(t, greenReport(), okVerify)

	code, stdout, stderr := run(t, "hub", "setup", "--target", "root@box", "--yes", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	var res bootstrapResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatal(err)
	}
	if res.Reachability == nil || res.Reachability.Result == vpnclient.ProbeReached {
		t.Errorf("reachability = %+v, want no claim: the API answered over ssh, not through the tunnel",
			res.Reachability)
	}
}

func TestSetupTargetUnreachableAPINamesTheFirewallAndTheMachineVerdict(t *testing.T) {
	report := greenReport()
	report.Results = append(report.Results, setup.Result{
		Step: "firewall", Status: setup.StatusOK,
		Detail: "no firewall manager is in charge here (udp 4021 open)",
	})
	useScriptedTarget(t, report, func(string) (json.RawMessage, error) {
		return nil, context.DeadlineExceeded
	})
	joinWithoutAHandshake()

	code, _, stderr := run(t, "hub", "setup", "--target", "root@box", "--yes")
	if code != ExitError {
		t.Fatalf("exit %d, want %d", code, ExitError)
	}
	for _, want := range []string{"firewall", "not the one blocking the way in",
		"security group", "port 4022", "caramelo --machine box status"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

func TestSetupTargetRepeatsTheMachineOwnVerdictWhenItBlockedAPort(t *testing.T) {
	report := greenReport()
	report.Results = append(report.Results, setup.Result{
		Step: "firewall", Status: setup.StatusFailed,
		Error: "ufw is active and denies udp 4021: 4021/udp DENY IN Anywhere. Open it with 'ufw allow 4021/udp'",
	})
	useScriptedTarget(t, report, func(string) (json.RawMessage, error) {
		return nil, context.DeadlineExceeded
	})
	joinWithoutAHandshake()

	code, _, stderr := run(t, "hub", "setup", "--target", "root@box", "--yes")
	if code != ExitError {
		t.Fatalf("exit %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "ufw allow 4021/udp") {
		t.Errorf("stderr does not repeat the rule that would open the port:\n%s", stderr)
	}
}

func joinWithoutAHandshake() {
	joinMachine = func(context.Context, string, vpnclient.Control) (*vpnclient.State, error) {
		return &vpnclient.State{Machine: "box", Endpoint: "10.0.0.5:4021"}, nil
	}
}

func TestSetupTargetNeverCallsAHandshakeSilence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		verify func(string) (json.RawMessage, error)
		exit   int
	}{
		{"the API answered over ssh", okVerify, ExitOK},
		{"the API did not answer at all", func(string) (json.RawMessage, error) {
			return nil, context.DeadlineExceeded
		}, ExitError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useScriptedTarget(t, greenReport(), tc.verify)
			code, stdout, stderr := run(t, "hub", "setup", "--target", "root@box", "--yes", "--json")
			if code != tc.exit {
				t.Fatalf("exit %d, want %d\n%s", code, tc.exit, stderr)
			}
			var res bootstrapResult
			if err := json.Unmarshal([]byte(stdout), &res); err != nil {
				t.Fatalf("stdout is not a bootstrap result: %v\n%s", err, stdout)
			}
			if res.Reachability == nil || res.Reachability.Handshake.IsZero() {
				t.Fatalf("reachability = %+v, want the handshake the join performed", res.Reachability)
			}
			if res.Reachability.Result == vpnclient.ProbeNoAnswer {
				t.Errorf("result = %q with a handshake at %s: a handshake that came back is not silence",
					res.Reachability.Result, res.Reachability.Handshake)
			}
			if res.Reachability.Result == vpnclient.ProbeReached {
				t.Errorf("result = %q without a tunnel transport: 'reached' rests on both signals",
					res.Reachability.Result)
			}
		})
	}
}

func TestSetupTargetDoesNotSendTheOperatorAfterAnOpenPort(t *testing.T) {
	useScriptedTarget(t, greenReport(), func(string) (json.RawMessage, error) {
		return nil, context.DeadlineExceeded
	})

	code, stdout, stderr := run(t, "hub", "setup", "--target", "root@box", "--yes")
	if code != ExitError {
		t.Fatalf("exit %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "udp 4021 answered a handshake from here") {
		t.Errorf("stderr does not say the tunnel port answered:\n%s", stderr)
	}
	if strings.Contains(stderr, "security group or the network in front of it") {
		t.Errorf("stderr sends the operator after a port that answered:\n%s", stderr)
	}
	if strings.Contains(stdout, "did not answer from here") {
		t.Errorf("stdout = %q, want no claim of silence after a handshake came back", stdout)
	}
}

func TestSetupArgsForwardsSwap(t *testing.T) {
	sh := useScriptedTarget(t, greenReport(), okVerify)
	if code, _, stderr := run(t, "hub", "setup", "--target", "root@box", "--yes", "--swap", "8G"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(lastSetupLine(sh), "--swap=8G") {
		t.Errorf("--swap was dropped on the way to the machine: %q", lastSetupLine(sh))
	}
}

func TestSetupArgsForwardsOpenPorts(t *testing.T) {
	sh := useScriptedTarget(t, greenReport(), okVerify)
	if code, _, stderr := run(t, "hub", "setup", "--target", "root@box", "--yes", "--open-ports"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(lastSetupLine(sh), "--open-ports") {
		t.Errorf("--open-ports was dropped on the way to the machine: %q", lastSetupLine(sh))
	}
}
