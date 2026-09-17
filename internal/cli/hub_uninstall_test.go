package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/plytz/caramelo/internal/testutil"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
)

func tempServerConfig(t *testing.T) (string, serverconfig.Config) {
	t.Helper()
	dir := t.TempDir()
	cfg := serverconfig.Default()
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.RunDir = filepath.Join(testutil.ShortDir(t), "run")
	configDir := filepath.Join(dir, "etc")
	if err := os.MkdirAll(configDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := serverconfig.Save(configDir, cfg, 0o640); err != nil {
		t.Fatal(err)
	}
	return configDir, cfg
}

func TestUninstallDryRunReportsThePlanWithoutTouchingTheMachine(t *testing.T) {
	configDir, cfg := tempServerConfig(t)

	code, stdout, stderr := run(t, "hub", "uninstall", "--config-dir", configDir, "--dry-run", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
	}
	var report uninstallReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout is not a report: %v\n%s", err, stdout)
	}
	if !report.DryRun || report.Purge {
		t.Errorf("report = %+v, want a dry run without purge", report)
	}
	if report.Failed != 0 {
		t.Errorf("a dry run reported %d failures: %+v", report.Failed, report.Actions)
	}
	var sawPlan, sawKept bool
	for _, act := range report.Actions {
		if act.Status == "done" {
			t.Errorf("a dry run did %+v", act)
		}
		if act.Status == "would-change" {
			sawPlan = true
		}
		if act.Action == "purge" && strings.Contains(act.Detail, cfg.DataDir) {
			sawKept = true
		}
	}
	if !sawPlan {
		t.Errorf("nothing was reported as would-change: %+v", report.Actions)
	}
	if !sawKept {
		t.Errorf("the report does not say the data directory is kept: %+v", report.Actions)
	}

	if !serverconfig.Exists(configDir) {
		t.Error("a dry run deleted the configuration")
	}
}

func TestUninstallWithoutAConfigAssumesTheDefaults(t *testing.T) {
	code, stdout, stderr := run(t, "hub", "uninstall", "--config-dir", t.TempDir(), "--dry-run", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stderr, "assuming the default layout") {
		t.Errorf("stderr = %q, want it to say it fell back to the defaults", stderr)
	}
	var report uninstallReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout is not a report: %v\n%s", err, stdout)
	}
	if len(report.Actions) == 0 {
		t.Error("no actions were planned")
	}
}

func TestUninstallRejectsAnUnreadableConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(serverconfig.Path(dir), []byte("\tnot: [yaml"), 0o640); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run(t, "hub", "uninstall", "--config-dir", dir, "--dry-run")
	if code != ExitError {
		t.Fatalf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, serverconfig.Path(dir)) {
		t.Errorf("stderr = %q, want it to name the file it could not read", stderr)
	}
}

func TestUninstallUsageErrors(t *testing.T) {
	if code, _, _ := run(t, "hub", "uninstall", "extra"); code != ExitUsage {
		t.Errorf("an extra argument = %d, want %d", code, ExitUsage)
	}
}

func TestPurgeRefusesWithoutAnAnswer(t *testing.T) {
	configDir, _ := tempServerConfig(t)
	code, _, stderr := run(t, "hub", "uninstall", "--config-dir", configDir, "--purge")
	if code != ExitError {
		t.Fatalf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Errorf("stderr = %q, want it to name the flag that confirms", stderr)
	}
}

func TestConfirmPurge(t *testing.T) {
	cfg := serverconfig.Default()
	var stderr bytes.Buffer
	a := &app{stdout: &bytes.Buffer{}, stderr: &stderr}

	ctx := withStdin(context.Background(), strings.NewReader("purge\n"))
	if _, err := a.confirmPurge(ctx, cfg); !errors.Is(err, errPurgeNeedsYes) {
		t.Errorf("confirmPurge with piped input = %v, want errPurgeNeedsYes", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("it prompted anyway: %q", stderr.String())
	}

	tty, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("no %s: %v", os.DevNull, err)
	}
	defer tty.Close()
	stderr.Reset()
	ctx = withStdin(context.Background(), tty)
	proceed, err := a.confirmPurge(ctx, cfg)
	if proceed {
		t.Error("confirmPurge said yes to an empty answer")
	}
	if !errors.Is(err, errPurgeNeedsYes) {
		t.Errorf("confirmPurge at EOF = %v, want errPurgeNeedsYes", err)
	}
	for _, want := range []string{cfg.DataDir, cfg.User, "Type 'purge'"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("prompt %q does not mention %q", stderr.String(), want)
		}
	}
}

func TestUninstallReportsAFailedCommand(t *testing.T) {
	stub := &scriptedRunner{results: map[string]runner.Result{
		"systemctl daemon-reload": {ExitCode: 1, Stderr: "Failed to reload: not authorised\n"},
	}}
	u := &uninstaller{cfg: serverconfig.Default(), configDir: t.TempDir(), exec: stub}
	report := u.run(context.Background())

	if report.Failed != 1 {
		t.Fatalf("failed = %d, want 1: %+v", report.Failed, report.Actions)
	}
	var found bool
	for _, act := range report.Actions {
		if act.Action != "daemon-reload" {
			continue
		}
		found = true
		if act.Status != "failed" {
			t.Errorf("daemon-reload = %+v, want status failed", act)
		}
		if !strings.Contains(act.Error, "exit 1") || !strings.Contains(act.Error, "not authorised") {
			t.Errorf("daemon-reload error = %q, want the code and what the command said", act.Error)
		}
		if act.Detail != "" {
			t.Errorf("a failed action carried a detail as well: %+v", act)
		}
	}
	if !found {
		t.Fatalf("daemon-reload was never attempted: %+v", report.Actions)
	}

	if !stub.ran("rm -rf") && len(report.Actions) < 3 {
		t.Errorf("the run stopped at the failure: %+v", report.Actions)
	}
}

func TestUninstallFallsBackToStdoutForTheReason(t *testing.T) {
	stub := &scriptedRunner{results: map[string]runner.Result{
		"systemctl daemon-reload": {ExitCode: 2, Stdout: "System has not been booted with systemd\n"},
	}}
	u := &uninstaller{cfg: serverconfig.Default(), configDir: t.TempDir(), exec: stub}
	report := u.run(context.Background())
	for _, act := range report.Actions {
		if act.Action == "daemon-reload" && !strings.Contains(act.Error, "not been booted") {
			t.Errorf("daemon-reload error = %q, want the stdout message", act.Error)
		}
	}
	if got := firstNonEmpty("", "  ", "third"); got != "third" {
		t.Errorf("firstNonEmpty skipped past the blanks to %q", got)
	}
	if got := firstNonEmpty("", " \n "); got != "" {
		t.Errorf("firstNonEmpty of nothing = %q, want empty", got)
	}
}

func TestUninstallTurnsSwapOffBeforeItRemovesTheSwapfile(t *testing.T) {
	dir := t.TempDir()
	cfg := serverconfig.Default()
	cfg.StateDir = filepath.Join(dir, "state")
	swapfile := cfg.SwapFilePath()
	if err := os.WriteFile(swapfile, []byte("swap"), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := &scriptedRunner{results: map[string]runner.Result{
		"systemd-escape -p --suffix=swap " + swapfile: {Stdout: "the-swap-unit.swap\n"},
	}}
	u := &uninstaller{cfg: cfg, configDir: dir, exec: stub}
	report := u.run(context.Background())

	if report.Failed != 0 {
		t.Fatalf("failed = %d, want 0: %+v", report.Failed, report.Actions)
	}
	var order []string
	for _, act := range report.Actions {
		switch act.Action {
		case "stop-swap", "swapoff", "remove swap-unit", "remove swap-sysctl", "remove swapfile", "daemon-reload":
			order = append(order, act.Action)
		}
	}
	want := []string{"stop-swap", "swapoff", "remove swap-unit", "remove swap-sysctl", "remove swapfile", "daemon-reload"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("actions = %v, want %v", order, want)
	}
	for _, line := range []string{
		"systemctl disable --now the-swap-unit.swap",
		"swapoff -- " + swapfile,
		"rm -rf -- " + swapfile,
	} {
		if !stub.ran(line) {
			t.Errorf("uninstall did not run %q: %v", line, stub.lines)
		}
	}
	if _, err := os.Stat(swapfile); err != nil {
		t.Errorf("the fake runner's rm actually removed the file: %v", err)
	}
}

func TestUninstallWithoutASwapUnitNameSaysSo(t *testing.T) {
	stub := &scriptedRunner{results: map[string]runner.Result{
		"systemd-escape -p --suffix=swap " + serverconfig.Default().SwapFilePath(): {ExitCode: 1},
	}}
	u := &uninstaller{cfg: serverconfig.Default(), configDir: t.TempDir(), exec: stub}
	report := u.run(context.Background())
	if report.Failed != 0 {
		t.Fatalf("failed = %d, want 0: %+v", report.Failed, report.Actions)
	}
	var found bool
	for _, act := range report.Actions {
		if act.Action == "stop-swap" {
			found = true
			if act.Status != "skipped" || act.Detail == "" {
				t.Errorf("stop-swap = %+v, want a skip that says why", act)
			}
		}
	}
	if !found {
		t.Errorf("no stop-swap action at all: %+v", report.Actions)
	}
}

func TestUninstallSurvivesTheRunnerItselfFailing(t *testing.T) {
	u := &uninstaller{cfg: serverconfig.Default(), configDir: t.TempDir(), exec: stubRunner{}}
	report := u.run(context.Background())
	if report.Failed == 0 {
		t.Fatalf("nothing was reported as failed: %+v", report.Actions)
	}
	for _, act := range report.Actions {
		if act.Status == "failed" && act.Error == "" {
			t.Errorf("%+v has no explanation", act)
		}
	}
}

func TestPurgeToleratesWhatWasNeverInstalled(t *testing.T) {
	stub := &scriptedRunner{results: map[string]runner.Result{
		"getent passwd caramelo": {Stdout: "caramelo:x:999:989::/var/lib/caramelo:/bin/sh\n"},

		"getent group caramelo":                         {ExitCode: 2},
		"systemctl --user disable --now docker.service": {ExitCode: 1, Stderr: "Failed to disable: Unit docker.service not loaded\n"},
		"loginctl disable-linger caramelo":              {ExitCode: 1, Stderr: "Failed to disable linger\n"},
		"systemctl stop user@999.service":               {ExitCode: 5, Stderr: "Unit user@999.service not loaded\n"},
	}}
	dir := t.TempDir()
	cfg := serverconfig.Default()
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.RunDir = filepath.Join(testutil.ShortDir(t), "run")
	u := &uninstaller{cfg: cfg, configDir: filepath.Join(dir, "etc"), purge: true, exec: stub}

	report := u.run(context.Background())
	if report.Failed != 0 {
		var failed []uninstallAction
		for _, act := range report.Actions {
			if act.Status == "failed" {
				failed = append(failed, act)
			}
		}
		t.Fatalf("purge failed over things that were never installed: %+v", failed)
	}
	byAction := map[string]uninstallAction{}
	for _, act := range report.Actions {
		byAction[act.Action] = act
	}
	for _, name := range []string{"stop-docker", "disable-linger", "stop-session"} {
		act, ok := byAction[name]
		if !ok {
			t.Errorf("%s was never attempted: %+v", name, report.Actions)
			continue
		}
		if act.Status != "skipped" {
			t.Errorf("%s = %+v, want it downgraded to skipped", name, act)
		}
		if act.Detail == "" {
			t.Errorf("%s was skipped without saying why: %+v", name, act)
		}
	}

	if !stub.ran("systemctl stop user@999.service") {
		t.Errorf("the uid from getent was not used: %v", stub.lines)
	}

	if act := byAction["remove user"]; act.Status != "done" {
		t.Errorf("remove user = %+v, want done", act)
	}
	if act := byAction["remove group"]; act.Status != "skipped" {
		t.Errorf("remove group = %+v, want skipped: userdel takes the private group", act)
	}
}

func TestPurgeRemovesOnlyTheDockerPackagesThatAreInstalled(t *testing.T) {
	stub := &scriptedRunner{results: map[string]runner.Result{
		"dpkg-query -W -f=${Status} docker-ce":     {Stdout: "install ok installed"},
		"dpkg-query -W -f=${Status} containerd.io": {Stdout: "install ok installed"},
		"dpkg-query -W -f=${Status} docker-ce-cli": {ExitCode: 1, Stderr: "no packages found"},
	}}
	u := &uninstaller{cfg: serverconfig.Default(), configDir: t.TempDir(), purge: true, exec: stub}
	u.purgePackages(context.Background())

	if len(u.report.Actions) != 1 {
		t.Fatalf("actions = %+v, want one", u.report.Actions)
	}
	act := u.report.Actions[0]
	if act.Status != "done" || !strings.Contains(act.Detail, "docker-ce") || !strings.Contains(act.Detail, "containerd.io") {
		t.Errorf("action = %+v, want the two installed packages", act)
	}
	if strings.Contains(act.Detail, "docker-ce-cli") {
		t.Errorf("action = %+v, want the missing package left out", act)
	}
	var purge string
	for _, line := range stub.lines {
		if strings.HasPrefix(line, "apt-get purge") {
			purge = line
		}
	}
	if purge == "" {
		t.Fatalf("apt-get was never called: %v", stub.lines)
	}
	if strings.Contains(purge, "docker-ce-cli") {
		t.Errorf("apt-get line %q names a package that is not installed", purge)
	}
}

func TestPurgeSkipsThePackagesWhenNoneAreInstalled(t *testing.T) {
	stub := &scriptedRunner{results: map[string]runner.Result{}}
	u := &uninstaller{cfg: serverconfig.Default(), configDir: t.TempDir(), purge: true, exec: stub}
	u.purgePackages(context.Background())
	if len(u.report.Actions) != 1 || u.report.Actions[0].Status != "skipped" {
		t.Errorf("actions = %+v, want one skip", u.report.Actions)
	}
	if stub.ran("apt-get") {
		t.Errorf("apt-get was called with nothing to remove: %v", stub.lines)
	}
}

func TestUninstallRemovesOnlyTheFilesThatAreThere(t *testing.T) {
	dir := t.TempDir()
	cfg := serverconfig.Default()
	cfg.StateDir = filepath.Join(dir, "state")
	stub := &scriptedRunner{results: map[string]runner.Result{}}
	u := &uninstaller{cfg: cfg, configDir: dir, exec: stub}

	u.remove(context.Background(), "unit", filepath.Join(dir, "missing"))
	if got := u.report.Actions[0]; got.Status != "skipped" || !strings.Contains(got.Detail, "is not there") {
		t.Errorf("removing a missing path = %+v, want a skip", got)
	}

	path := filepath.Join(dir, "present")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	u.remove(context.Background(), "unit", path)
	if got := u.report.Actions[1]; got.Status != "done" || !strings.Contains(got.Detail, path) {
		t.Errorf("removing an existing path = %+v, want it removed", got)
	}
	if !stub.ran("rm -rf -- " + path) {
		t.Errorf("rm was not run on %s: %v", path, stub.lines)
	}
}

func TestUninstallSaysItLeavesTheRuleSetupMayHaveOpened(t *testing.T) {
	for _, tc := range []struct {
		name   string
		listen string
		port   string
	}{
		{"the default tunnel port", serverconfig.Default().VPNListen, "4021"},
		{"a tunnel port of its own", "0.0.0.0:51820", "51820"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := serverconfig.Default()
			cfg.VPNListen = tc.listen
			stub := &scriptedRunner{}
			u := &uninstaller{cfg: cfg, configDir: t.TempDir(), exec: stub}
			report := u.run(context.Background())

			var note *uninstallAction
			for i, act := range report.Actions {
				if act.Action == "firewall" {
					note = &report.Actions[i]
				}
			}
			if note == nil {
				t.Fatalf("nothing said what happens to a rule setup may have added: %+v", report.Actions)
			}
			if note.Status != "skipped" {
				t.Errorf("firewall = %+v, want it skipped: uninstall closes nobody's port", note)
			}
			for _, want := range []string{"udp " + tc.port, "--open-ports", "ufw delete allow " + tc.port + "/udp"} {
				if !strings.Contains(note.Detail, want) {
					t.Errorf("detail = %q, want it to name %q", note.Detail, want)
				}
			}
			if stub.ran("ufw") || stub.ran("nft") || stub.ran("firewall-cmd") {
				t.Errorf("uninstall touched a ruleset: %v", stub.lines)
			}
		})
	}
}
