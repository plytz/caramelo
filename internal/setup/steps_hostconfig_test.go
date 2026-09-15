package setup

import (
	"context"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/setup/testutil"
)

func unconfiguredHost() *testutil.FakeRunner {
	run := testutil.New()
	run.ExitPrefix("stat -c %U:%G:%a:%F -- ", 1)
	run.ExitPrefix("cat -- ", 1)
	run.Exit("test -d /sys/module/br_netfilter", 1)
	run.Exit("test -e "+bridgeNFCallPath, 1)
	return run
}

func builtinBrNetfilterHost(env *Env) *testutil.FakeRunner {
	run := configuredHost(env)
	run.Exit("test -d /sys/module/br_netfilter", 1)
	run.Respond("test -e "+bridgeNFCallPath, runner.Result{})
	return run
}

func configuredHost(env *Env) *testutil.FakeRunner {
	run := testutil.New()
	run.Respond("test -d /sys/module/br_netfilter", runner.Result{})
	run.Stdout("stat -c %U:%G:%a:%F -- /run/caramelo", "caramelo:caramelo:750:directory\n")
	run.Stdout("stat -c %U:%G:%a:%F -- /run/caramelo/secrets", "caramelo:caramelo:700:directory\n")
	for _, f := range (&HostConfigStep{}).files(env) {
		run.Stdout("stat -c %U:%G:%a:%F -- "+f.Path, "root:root:644:regular file\n")
		run.Stdout("cat -- "+f.Path, f.Content)
	}
	return run
}

func TestHostConfigCheckOnABareBox(t *testing.T) {
	env, _ := testEnv(t, unconfiguredHost())
	done, detail, err := (&HostConfigStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if done {
		t.Fatal("Check() said done on an unconfigured box")
	}
	for _, want := range []string{ModulesFile, DelegateFile, TmpfilesFile, "br_netfilter not loaded",
		"/run/caramelo missing", "/run/caramelo/secrets missing"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not mention %q", detail, want)
		}
	}
}

func TestHostConfigCheckIsDoneOnAConfiguredBox(t *testing.T) {
	env, _ := testEnv(t, nil)
	run := configuredHost(env)
	env.Run = run
	done, detail, err := (&HostConfigStep{}).Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %q, %v; want done", done, detail, err)
	}
	if !strings.Contains(detail, "br_netfilter") {
		t.Errorf("detail %q does not say what is in place", detail)
	}
}

func TestHostConfigAcceptsBrNetfilterBuiltIntoTheKernel(t *testing.T) {
	env, _ := testEnv(t, nil)
	run := builtinBrNetfilterHost(env)
	env.Run = run

	done, detail, err := (&HostConfigStep{}).Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %q, %v; want done\n%s", done, detail, err, run.Transcript())
	}
	if strings.Contains(detail, "not loaded") {
		t.Errorf("detail %q, want br_netfilter accepted through %s", detail, bridgeNFCallPath)
	}

	run.Reset()
	if err := (&HostConfigStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v\n%s", err, run.Transcript())
	}
	if run.Ran("modprobe") {
		t.Errorf("Apply ran modprobe for a built-in module:\n%s", run.Transcript())
	}
}

func TestHostConfigApplyLoadsBrNetfilterWhenNeitherProbeFindsIt(t *testing.T) {
	run := unconfiguredHost()
	run.Stdout("id -u caramelo", "999\n")
	run.Exit("systemctl is-active --quiet user@999.service", 3)
	env, _ := testEnv(t, run)

	if err := (&HostConfigStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	if !run.Ran("modprobe br_netfilter") {
		t.Errorf("Apply did not load br_netfilter:\n%s", run.Transcript())
	}
}

func TestHostConfigApply(t *testing.T) {
	run := unconfiguredHost()
	run.Stdout("id -u caramelo", "999\n")
	run.Respond("systemctl is-active --quiet user@999.service", runner.Result{})
	run.Respond("test -e /run/user/999", runner.Result{})
	env, _ := testEnv(t, run)

	if err := (&HostConfigStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v\n%s", err, run.Transcript())
	}
	for _, want := range []string{
		"mkdir -p -- /etc/systemd/system/user@.service.d",
		"tee -- " + ModulesFile,
		"tee -- " + DelegateFile,
		"tee -- " + TmpfilesFile,
		"modprobe br_netfilter",
		"systemctl daemon-reload",
		"systemd-tmpfiles --create " + TmpfilesFile,
		"systemctl restart user@999.service",
	} {
		if !run.Ran(want) {
			t.Errorf("Apply did not run %q:\n%s", want, run.Transcript())
		}
	}
	if run.Ran("tee -- " + SysctlFile) {
		t.Errorf("low ports were configured without --low-ports:\n%s", run.Transcript())
	}
	call, _ := run.Find("tee -- " + TmpfilesFile)
	runLine := strings.Index(call.Stdin, "d /run/caramelo 0750 caramelo caramelo -")
	if runLine < 0 {
		t.Errorf("tmpfiles line = %q", call.Stdin)
	}
	secretsLine := strings.Index(call.Stdin, "d /run/caramelo/secrets 0700 caramelo caramelo -")
	if secretsLine < 0 {
		t.Errorf("tmpfiles wrote no secrets directory: %q", call.Stdin)
	}
	if runLine >= 0 && secretsLine >= 0 && secretsLine < runLine {
		t.Errorf("the secrets directory is created before its parent: %q", call.Stdin)
	}
	call, _ = run.Find("tee -- " + DelegateFile)
	if !strings.Contains(call.Stdin, "Delegate=cpu cpuset io memory pids") {
		t.Errorf("delegate drop-in = %q", call.Stdin)
	}
}

func TestHostConfigApplyLowPorts(t *testing.T) {
	run := unconfiguredHost()
	run.Stdout("id -u caramelo", "999\n")
	run.Exit("systemctl is-active --quiet user@999.service", 3)
	env, _ := testEnv(t, run)
	env.Opts.LowPorts = true

	if err := (&HostConfigStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	call, found := run.Find("tee -- " + SysctlFile)
	if !found {
		t.Fatalf("--low-ports wrote no sysctl file:\n%s", run.Transcript())
	}
	if !strings.Contains(call.Stdin, "net.ipv4.ip_unprivileged_port_start=80") {
		t.Errorf("sysctl file = %q", call.Stdin)
	}
	if !run.Ran("sysctl --system") {
		t.Errorf("the sysctl was written but never applied:\n%s", run.Transcript())
	}
	if run.Ran("systemctl restart user@999.service") {
		t.Errorf("a session that is not running was restarted:\n%s", run.Transcript())
	}
}

func TestHostConfigApplyIsANoOpWhenDone(t *testing.T) {
	env, _ := testEnv(t, nil)
	run := configuredHost(env)
	env.Run = run

	if err := (&HostConfigStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	for _, forbidden := range []string{"tee", "modprobe", "systemctl daemon-reload", "systemctl restart"} {
		if run.Ran(forbidden) {
			t.Errorf("Apply ran %q on a configured box:\n%s", forbidden, run.Transcript())
		}
	}

	if !run.Ran("systemd-tmpfiles --create") {
		t.Errorf("tmpfiles was not re-created:\n%s", run.Transcript())
	}
}
