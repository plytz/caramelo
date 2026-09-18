package setup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup/testutil"
)

func edgeEnv(t *testing.T, run *testutil.FakeRunner) (*Env, *strings.Builder) {
	t.Helper()
	env, _ := testEnv(t, run)
	log := &strings.Builder{}
	env.Log = log
	env.Config.Edge = true
	env.Config.ACMEEmail = "ops@example.com"
	return env, log
}

func edgeInstalled(t *testing.T, env *Env, run *testutil.FakeRunner) {
	t.Helper()
	cfg := env.Config
	for path, content := range map[string]string{
		EdgeSocketUnitPath:  EdgeSocketUnitContent(cfg, false),
		EdgeServiceUnitPath: EdgeServiceUnitContent(cfg, env.ConfigDir),
		EdgeSysctlPath:      EdgeSysctlContent,
	} {
		run.Stdout("stat -c %U:%G:%a:%F -- "+path, "root:root:644:regular file\n")
		run.Stdout("cat -- "+path, content)
	}
	run.Stdout("stat -c %U:%G:%a:%F -- "+cfg.EdgeDir(), cfg.User+":"+cfg.Group+":750:directory\n")
	run.Stdout("stat -c %U:%G:%a:%F -- "+cfg.EdgeCertsDir(), cfg.User+":"+cfg.Group+":700:directory\n")
	run.Stdout("getent passwd "+cfg.User, cfg.User+":x:999:999::/var/lib/caramelo:/usr/sbin/nologin\n")
	run.Respond("systemctl is-active --quiet "+EdgeSocketUnit, runner.Result{})
	run.Respond("systemctl is-active --quiet "+EdgeServiceUnit, runner.Result{})
}

func TestEdgeStepSkipsWithoutTheFlag(t *testing.T) {
	run := testutil.New()
	run.ExitPrefix("stat -c %U:%G:%a:%F -- /etc/", 1)
	env, _ := testEnv(t, run)

	_, _, err := NewEdgeStep().Check(context.Background(), env)
	var skip Skip
	if !errors.As(err, &skip) || !strings.Contains(skip.Reason, "--edge") {
		t.Fatalf("Check = %v, want a Skip naming --edge", err)
	}
}

func TestEdgeStepIsDoneWhenTheMachineMatches(t *testing.T) {
	run := testutil.New()
	run.Strict = true
	env, _ := edgeEnv(t, run)
	edgeInstalled(t, env, run)

	done, detail, err := NewEdgeStep().Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check: %v\n%s", err, run.Transcript())
	}
	if !done {
		t.Fatalf("Check said not done: %s", detail)
	}
	for _, want := range []string{EdgeSocketUnit, "443/udp", env.Config.EdgeCertsDir()} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not mention %q", detail, want)
		}
	}
}

func TestEdgeStepReportsWhatIsMissing(t *testing.T) {
	run := testutil.New()
	run.ExitPrefix("stat -c %U:%G:%a:%F --", 1)
	run.Stdout("getent passwd caramelo", "caramelo:x:999:999::/var/lib/caramelo:/usr/sbin/nologin\n")
	run.Exit("systemctl is-active --quiet "+EdgeSocketUnit, 3)
	run.Exit("systemctl is-active --quiet "+EdgeServiceUnit, 3)
	env, _ := edgeEnv(t, run)

	done, detail, err := NewEdgeStep().Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("Check said done on a machine with no edge at all")
	}
	for _, want := range []string{
		EdgeSocketUnitPath, EdgeServiceUnitPath, EdgeSysctlPath,
		env.Config.EdgeCertsDir(), EdgeSocketUnit + " not active",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not mention %q", detail, want)
		}
	}
}

func TestEdgeStepSaysWhenTheUserIsNotThereYet(t *testing.T) {
	run := testutil.New()
	run.ExitPrefix("stat -c %U:%G:%a:%F --", 1)
	run.Exit("getent passwd caramelo", 2)
	env, _ := edgeEnv(t, run)

	_, detail, err := NewEdgeStep().Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !strings.Contains(detail, "does not exist yet") {
		t.Errorf("detail = %q, want it to name the missing user", detail)
	}
	if strings.Contains(detail, "not active") {
		t.Errorf("detail = %q, want no systemd question on a box with no user", detail)
	}
}

func TestEdgeStepApplyInstallsAndStarts(t *testing.T) {
	run := testutil.New()
	run.ExitPrefix("stat -c %U:%G:%a:%F --", 1)
	run.ExitPrefix("systemctl is-active", 3)
	env, log := edgeEnv(t, run)

	if err := NewEdgeStep().Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply: %v\n%s", err, run.Transcript())
	}

	wrote := map[string]string{}
	var order []string
	for _, c := range run.Calls() {
		switch {
		case c.Cmd.Name == "tee":
			wrote[c.Cmd.Args[len(c.Cmd.Args)-1]] = c.Stdin
		case c.Cmd.Name == "systemctl":
			order = append(order, strings.Join(c.Cmd.Args, " "))
		}
	}
	for _, path := range []string{EdgeSocketUnitPath, EdgeServiceUnitPath, EdgeSysctlPath} {
		if _, ok := wrote[path]; !ok {
			t.Errorf("%s was not written:\n%s", path, run.Transcript())
		}
	}

	socket := wrote[EdgeSocketUnitPath]
	for _, want := range []string{"ListenStream=80", "ListenStream=443", "ListenDatagram=443",
		"Service=" + EdgeServiceUnit, "TriggerLimitIntervalSec=0"} {
		if !strings.Contains(socket, want) {
			t.Errorf("the socket unit does not have %q:\n%s", want, socket)
		}
	}
	for _, never := range []string{"PartOf=", "BindsTo=", "Requires="} {
		if strings.Contains(socket, never) {
			t.Errorf("the socket unit has %q, which makes a service restart close the ports:\n%s", never, socket)
		}
	}

	service := wrote[EdgeServiceUnitPath]
	for _, want := range []string{
		"User=" + env.Config.User, "ExecStart=" + serverconfig.BinaryPath + " edge",
		"Requires=" + EdgeSocketUnit, "StartLimitIntervalSec=0", "NoNewPrivileges=true",
		"ReadWritePaths=" + env.Config.EdgeDir() + " " + env.Config.RunDir,
	} {
		if !strings.Contains(service, want) {
			t.Errorf("the service unit does not have %q:\n%s", want, service)
		}
	}

	if strings.Contains(service, "ReadWritePaths="+env.Config.StateDir+" ") {
		t.Errorf("the service unit makes the whole state dir writable by the edge:\n%s", service)
	}

	wantOrder := []string{"daemon-reload", "is-active --quiet " + EdgeSocketUnit,
		"enable --now " + EdgeSocketUnit, "is-active --quiet " + EdgeServiceUnit,
		"enable --now " + EdgeServiceUnit}
	if strings.Join(order, "|") != strings.Join(wantOrder, "|") {
		t.Errorf("systemctl calls = %q, want %q", order, wantOrder)
	}

	if !strings.Contains(run.Transcript(), "sysctl --quiet --load="+EdgeSysctlPath) {
		t.Errorf("the sysctl drop-in was written but never loaded:\n%s", run.Transcript())
	}

	if !strings.Contains(log.String(), "the edge answers tcp 80, tcp 443, udp 443") {
		t.Errorf("log = %q, want the edge to state what it binds", log.String())
	}
	if strings.Contains(log.String(), "make sure these ports reach this machine") {
		t.Errorf("the edge still hands out advice nobody checked: %q", log.String())
	}
}

func TestEdgeSocketUnitWithoutHTTP3(t *testing.T) {
	cfg := serverconfig.Default()
	cfg.Edge, cfg.HTTP3 = true, false
	unit := EdgeSocketUnitContent(cfg, false)
	if strings.Contains(unit, "ListenDatagram") {
		t.Errorf("--no-http3 still binds udp 443:\n%s", unit)
	}
	if !strings.Contains(unit, "ListenStream=443") {
		t.Errorf("--no-http3 dropped tcp 443 too:\n%s", unit)
	}
}

func TestEdgeServiceUnitCarriesTheConfigDir(t *testing.T) {
	cfg := serverconfig.Default()
	if got := EdgeServiceUnitContent(cfg, serverconfig.DefaultConfigDir); strings.Contains(got, "--config-dir") {
		t.Errorf("the default config dir was spelled out:\n%s", got)
	}
	got := EdgeServiceUnitContent(cfg, "/opt/caramelo/etc")
	if !strings.Contains(got, "ExecStart="+serverconfig.BinaryPath+" edge --config-dir /opt/caramelo/etc") {
		t.Errorf("the config dir did not reach ExecStart:\n%s", got)
	}
}

func TestEdgeStepTakesTheEdgeDownWhenTheConfigSaysOff(t *testing.T) {
	run := testutil.New()
	for _, path := range []string{EdgeSocketUnitPath, EdgeServiceUnitPath, EdgeSysctlPath} {
		run.Stdout("stat -c %U:%G:%a:%F -- "+path, "root:root:644:regular file\n")
	}
	env, log := edgeEnv(t, run)
	env.Config.Edge = false

	step := NewEdgeStep()
	done, detail, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done || !strings.Contains(detail, EdgeSocketUnitPath) {
		t.Fatalf("Check = %v, %q; want the installed units reported as a difference", done, detail)
	}

	if err := step.Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply: %v\n%s", err, run.Transcript())
	}
	var order []string
	removed := map[string]bool{}
	for _, c := range run.Calls() {
		switch c.Cmd.Name {
		case "systemctl":
			order = append(order, strings.Join(c.Cmd.Args, " "))
		case "rm":
			removed[c.Cmd.Args[len(c.Cmd.Args)-1]] = true
		}
	}

	wantOrder := []string{"disable --now " + EdgeServiceUnit, "disable --now " + EdgeSocketUnit, "daemon-reload"}
	if strings.Join(order, "|") != strings.Join(wantOrder, "|") {
		t.Errorf("systemctl calls = %q, want %q", order, wantOrder)
	}
	for _, path := range []string{EdgeSocketUnitPath, EdgeServiceUnitPath, EdgeSysctlPath} {
		if !removed[path] {
			t.Errorf("%s was not removed:\n%s", path, run.Transcript())
		}
	}

	if strings.Contains(run.Transcript(), env.Config.EdgeCertsDir()) {
		t.Errorf("the certificate store was touched by a disable:\n%s", run.Transcript())
	}
	if !strings.Contains(log.String(), "certificates and routes are kept") {
		t.Errorf("log = %q, want it to say what was kept", log.String())
	}
}

func TestEdgeStepDisableIsIdempotent(t *testing.T) {
	run := testutil.New()
	run.ExitPrefix("stat -c %U:%G:%a:%F --", 1)
	env, _ := edgeEnv(t, run)
	env.Config.Edge = false

	_, _, err := NewEdgeStep().Check(context.Background(), env)
	var skip Skip
	if !errors.As(err, &skip) {
		t.Fatalf("Check = %v, want a Skip on a machine with no edge left", err)
	}
}

func TestEdgeStepApplyDoesNotRestartWhatMatches(t *testing.T) {
	run := testutil.New()
	env, _ := edgeEnv(t, run)
	edgeInstalled(t, env, run)

	if err := NewEdgeStep().Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply: %v\n%s", err, run.Transcript())
	}
	if strings.Contains(run.Transcript(), "systemctl restart") {
		t.Errorf("a no-op Apply restarted a unit:\n%s", run.Transcript())
	}
	if strings.Contains(run.Transcript(), "tee") {
		t.Errorf("a no-op Apply rewrote a file:\n%s", run.Transcript())
	}
}

func TestEdgeStepApplyRestartsWhatChanged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stale   string
		restart []string
	}{
		{"the service unit", EdgeServiceUnitPath, []string{"restart " + EdgeServiceUnit}},
		{"the socket unit", EdgeSocketUnitPath, []string{"restart " + EdgeSocketUnit, "restart " + EdgeServiceUnit}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := testutil.New()
			env, _ := edgeEnv(t, run)
			edgeInstalled(t, env, run)
			run.Stdout("cat -- "+tc.stale, "# an older build wrote this\n")

			if err := NewEdgeStep().Apply(context.Background(), env); err != nil {
				t.Fatalf("Apply: %v\n%s", err, run.Transcript())
			}
			var got []string
			for _, c := range run.Calls() {
				if c.Cmd.Name == "systemctl" && c.Cmd.Args[0] == "restart" {
					got = append(got, strings.Join(c.Cmd.Args, " "))
				}
			}
			if strings.Join(got, "|") != strings.Join(tc.restart, "|") {
				t.Errorf("restarts = %q, want %q", got, tc.restart)
			}
		})
	}
}
