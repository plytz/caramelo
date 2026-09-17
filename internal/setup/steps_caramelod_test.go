package setup

import (
	"context"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup/testutil"
)

const (
	aliceKey    = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAkbSsUU/u3DW1WRsxNrOZ9qV0FQ/O+2yamfunzisob+ alice@laptop"
	namelessKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHRKo3e6Vjt8Xm9ZEJ9fH72HXV+ZBB69yDGoda/YZSs0"
)

func sudoUser(t *testing.T, name string) {
	t.Helper()
	prev := getenv
	getenv = func(k string) string {
		if k == "SUDO_USER" {
			return name
		}
		return ""
	}
	t.Cleanup(func() { getenv = prev })
}

func installerBox() *testutil.FakeRunner {
	run := testutil.New()
	run.ExitPrefix("stat -c %U:%G:%a:%F -- ", 1)
	run.ExitPrefix("cat -- ", 1)
	run.Exit("sha256sum -- "+serverconfig.BinaryPath, 1)
	run.Stdout("sha256sum -- /tmp/caramelo", "abc123  /tmp/caramelo\n")
	run.Stdout("getent passwd alice", "alice:x:1000:1000::/home/alice:/bin/bash\n")
	run.Stdout("cat -- /home/alice/.ssh/authorized_keys", aliceKey+"\n")
	run.Stdout("hostname", "box\n")
	run.Stdout("getent passwd caramelo", "caramelo:x:999:989::/var/lib/caramelo:/bin/sh\n")
	run.Exit("systemctl --user is-active --quiet caramelod.service", 3)
	return run
}

func running(run *testutil.FakeRunner) *testutil.FakeRunner {
	run.Respond("test -e /run/caramelo/caramelod.sock", runner.Result{})
	run.Stdout("ss -ltn", "State  Recv-Q Send-Q Local Address:Port Peer Address:Port\nLISTEN 0 128 0.0.0.0:4022 0.0.0.0:*\n")
	run.Stdout("ss -lun", "State  Recv-Q Send-Q Local Address:Port Peer Address:Port\nUNCONN 0 0 0.0.0.0:4021 0.0.0.0:*\n")
	run.Respond(serverconfig.BinaryPath+" status --json", runner.Result{Stdout: "{}\n"})
	return run
}

func TestCaramelodCheckOnABareBox(t *testing.T) {
	sudoUser(t, "alice")
	env, _ := testEnv(t, installerBox())
	done, detail, err := (&CaramelodStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if done {
		t.Fatal("Check() said done on a box without caramelod")
	}
	for _, want := range []string{serverconfig.BinaryPath + " differs", "caramelod.service missing", "1 key(s) not authorized", "not active", "caramelod.sock missing"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not mention %q", detail, want)
		}
	}
}

func TestCaramelodCheckBeforeTheUserExists(t *testing.T) {
	sudoUser(t, "alice")
	run := installerBox()
	run.Exit("getent passwd caramelo", 2)
	env, _ := testEnv(t, run)

	done, detail, err := (&CaramelodStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check() error on a box the earlier steps have not touched: %v", err)
	}
	if done {
		t.Fatal("Check() said done without a user")
	}
	if !strings.Contains(detail, "user caramelo does not exist yet") {
		t.Errorf("detail %q does not explain what is missing", detail)
	}
	if run.Ran("systemctl --user") {
		t.Errorf("the step asked systemd about a user that does not exist:\n%s", run.Transcript())
	}
}

func TestCaramelodApply(t *testing.T) {
	sudoUser(t, "alice")
	run := running(installerBox())
	env, log := testEnv(t, run)

	if err := (&CaramelodStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v\n%s", err, run.Transcript())
	}

	if !run.Ran("install -m 0755 -o root -g root /tmp/caramelo " + serverconfig.BinaryPath) {
		t.Errorf("the binary was not installed:\n%s", run.Transcript())
	}

	keys, found := run.Find("tee -- /var/lib/caramelo/ssh/authorized_keys")
	if !found {
		t.Fatalf("no key was seeded:\n%s", run.Transcript())
	}
	if keys.Cmd.User != "caramelo" {
		t.Errorf("authorized_keys written as %q, want the caramelo user", keys.Cmd.User)
	}
	if !strings.Contains(keys.Stdin, aliceKey) {
		t.Errorf("authorized_keys = %q, want alice's key", keys.Stdin)
	}
	if !run.Ran("chmod 0600 -- /var/lib/caramelo/ssh/authorized_keys") {
		t.Errorf("authorized_keys was not locked down:\n%s", run.Transcript())
	}
	if !strings.Contains(log.String(), "seeded key ssh-ed25519") || !strings.Contains(log.String(), "alice@laptop") {
		t.Errorf("log %q does not name the seeded key", log.String())
	}

	unit, found := run.Find("tee -- " + UserUnitPath("/var/lib/caramelo"))
	if !found {
		t.Fatalf("no unit was written:\n%s", run.Transcript())
	}
	if unit.Cmd.User != "caramelo" {
		t.Errorf("unit written as %q, want the caramelo user", unit.Cmd.User)
	}
	for _, want := range []string{"After=docker.service", "Wants=docker.service", "ExecStart=" + serverconfig.BinaryPath + " hub run", "Restart=always", "WantedBy=default.target"} {
		if !strings.Contains(unit.Stdin, want) {
			t.Errorf("unit %q does not contain %q", unit.Stdin, want)
		}
	}

	for _, want := range []string{
		"mkdir -p -- /var/lib/caramelo/.config/systemd/user",
		"systemctl --user daemon-reload",
		"systemctl --user enable --now caramelod.service",
		serverconfig.BinaryPath + " status --json",
	} {
		call, ran := run.Find(want)
		if !ran {
			t.Errorf("Apply did not run %q:\n%s", want, run.Transcript())
			continue
		}
		if call.Cmd.User != "caramelo" {
			t.Errorf("%q ran as %q, want the caramelo user", want, call.Cmd.User)
		}
	}

	if !run.Ran("ss -lun") {
		t.Errorf("Apply never checked that the tunnel port is listening:\n%s", run.Transcript())
	}
	if run.Ran("systemctl --user restart") {
		t.Errorf("a unit that was not running was restarted:\n%s", run.Transcript())
	}
}

func TestCaramelodApplyRestartsOnANewBinary(t *testing.T) {
	sudoUser(t, "alice")
	run := running(installerBox())
	run.Stdout("sha256sum -- "+serverconfig.BinaryPath, "old  "+serverconfig.BinaryPath+"\n")
	run.Respond("systemctl --user is-active --quiet caramelod.service", runner.Result{})

	env, _ := testEnv(t, run)
	unit := (&CaramelodStep{}).unitSpec(env)
	run.Stdout("stat -c %U:%G:%a:%F -- "+unit.Path, "caramelo:caramelo:644:regular file\n")
	run.Stdout("cat -- "+unit.Path, unit.Content)

	if err := (&CaramelodStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v\n%s", err, run.Transcript())
	}
	if !run.Ran("systemctl --user restart caramelod.service") {
		t.Errorf("a new binary did not restart the running daemon:\n%s", run.Transcript())
	}
}

func TestCaramelodApplyFailsWhenTheSmokeTestFails(t *testing.T) {
	sudoUser(t, "alice")
	run := running(installerBox())
	run.Respond(serverconfig.BinaryPath+" status --json", runner.Result{ExitCode: 1, Stderr: "caramelo: no daemon\n"})
	env, _ := testEnv(t, run)

	err := (&CaramelodStep{}).Apply(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "smoke test") {
		t.Fatalf("err = %v, want the smoke test to fail the step", err)
	}
}

func TestCaramelodSeedsFromPubFilesAndNamesNamelessKeys(t *testing.T) {
	sudoUser(t, "alice")
	run := installerBox()
	run.Exit("cat -- /home/alice/.ssh/authorized_keys", 1)
	run.Stdout("find /home/alice/.ssh -maxdepth 1 -type f -name *.pub", "/home/alice/.ssh/id_ed25519.pub\n")
	run.Stdout("cat -- /home/alice/.ssh/id_ed25519.pub", namelessKey+"\n")
	env, _ := testEnv(t, run)

	if err := (&CaramelodStep{}).seedKeys(context.Background(), env); err != nil {
		t.Fatalf("seedKeys() error: %v\n%s", err, run.Transcript())
	}
	keys, found := run.Find("tee -- /var/lib/caramelo/ssh/authorized_keys")
	if !found {
		t.Fatalf("no key was seeded:\n%s", run.Transcript())
	}
	if !strings.Contains(keys.Stdin, namelessKey+" alice@box") {
		t.Errorf("authorized_keys = %q, want the key named after the installer and the box", keys.Stdin)
	}
}

func TestCaramelodDoesNotSeedAKeyTwice(t *testing.T) {
	sudoUser(t, "alice")
	run := installerBox()

	run.Stdout("cat -- /var/lib/caramelo/ssh/authorized_keys", strings.Replace(aliceKey, "alice@laptop", "alice-old", 1)+"\n")
	env, _ := testEnv(t, run)

	done, detail, err := (&CaramelodStep{}).keysSeeded(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("keysSeeded() = %v, %q, %v; want done", done, detail, err)
	}
	if err := (&CaramelodStep{}).seedKeys(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if run.Ran("tee -- /var/lib/caramelo/ssh/authorized_keys") {
		t.Errorf("an already authorized key was written again:\n%s", run.Transcript())
	}
}

func TestCaramelodUsesTheGivenKeysFile(t *testing.T) {
	sudoUser(t, "alice")
	run := installerBox()
	run.Stdout("cat -- /root/deploy.pub", namelessKey+" ops\n")
	env, _ := testEnv(t, run)
	env.Opts.AuthorizedKeysFile = "/root/deploy.pub"

	if err := (&CaramelodStep{}).seedKeys(context.Background(), env); err != nil {
		t.Fatalf("seedKeys() error: %v", err)
	}
	keys, _ := run.Find("tee -- /var/lib/caramelo/ssh/authorized_keys")
	if !strings.Contains(keys.Stdin, "ops") {
		t.Errorf("authorized_keys = %q, want the key from --authorized-keys", keys.Stdin)
	}
	if strings.Contains(keys.Stdin, "alice@laptop") {
		t.Errorf("--authorized-keys was given but the sudo user's keys were seeded too: %q", keys.Stdin)
	}
}

func TestCaramelodMissingKeysFileIsAnError(t *testing.T) {
	sudoUser(t, "alice")
	env, _ := testEnv(t, installerBox())
	env.Opts.AuthorizedKeysFile = "/root/nope.pub"
	if err := (&CaramelodStep{}).seedKeys(context.Background(), env); err == nil {
		t.Fatal("seedKeys() accepted a missing --authorized-keys file")
	}
}

func TestUnitContentCarriesACustomConfigDir(t *testing.T) {
	env, _ := testEnv(t, testutil.New())
	if strings.Contains(unitContent(env), "--config-dir") {
		t.Errorf("the default config directory is spelled out in the unit: %q", unitContent(env))
	}
	env.ConfigDir = "/opt/caramelo/etc"
	if !strings.Contains(unitContent(env), "hub run --config-dir /opt/caramelo/etc") {
		t.Errorf("unit = %q, want the config directory passed to the daemon", unitContent(env))
	}
}

func TestHasListener(t *testing.T) {
	out := "State  Recv-Q Send-Q Local Address:Port  Peer Address:Port\n" +
		"LISTEN 0      128          0.0.0.0:22         0.0.0.0:*\n" +
		"LISTEN 0      4096            [::]:4022          [::]:*\n"
	if !hasListener(out, 4022) {
		t.Errorf("hasListener(4022) = false, want true")
	}
	if hasListener(out, 402) {
		t.Errorf("hasListener(402) matched a suffix of another port")
	}
	if hasListener(out, 8080) {
		t.Errorf("hasListener(8080) = true, want false")
	}
}

func TestParseKeyLineIdentifiesAKeyByItsBlob(t *testing.T) {
	blob1, comment, keyType, err := parseKeyLine(aliceKey)
	if err != nil {
		t.Fatalf("parseKeyLine() error: %v", err)
	}
	if comment != "alice@laptop" || keyType != "ssh-ed25519" {
		t.Errorf("comment=%q type=%q", comment, keyType)
	}
	blob2, _, _, err := parseKeyLine(strings.Replace(aliceKey, "alice@laptop", "other", 1))
	if err != nil || blob1 != blob2 {
		t.Errorf("the same key with another comment got a different identity")
	}
	if _, _, _, err := parseKeyLine("# just a comment"); err == nil {
		t.Errorf("parseKeyLine() accepted a comment line")
	}
}

func TestCaramelodRestartsWhenTheConfigChanged(t *testing.T) {
	sudoUser(t, "alice")
	run := running(installerBox())
	run.Stdout("sha256sum -- "+serverconfig.BinaryPath, "abc123  "+serverconfig.BinaryPath+"\n")
	run.Respond("systemctl --user is-active --quiet caramelod.service", runner.Result{})
	env, _ := testEnv(t, run)
	unit := (&CaramelodStep{}).unitSpec(env)
	run.Stdout("stat -c %U:%G:%a:%F -- "+unit.Path, "caramelo:caramelo:644:regular file\n")
	run.Stdout("cat -- "+unit.Path, unit.Content)
	env.ConfigChanged = true

	done, detail, err := (&CaramelodStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if done {
		t.Fatalf("Check() said done although config.yaml moved under the daemon: %q", detail)
	}
	if !strings.Contains(detail, "changed since caramelod started") {
		t.Errorf("detail %q does not say why the step has to run", detail)
	}

	if err := (&CaramelodStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v\n%s", err, run.Transcript())
	}
	if !run.Ran("systemctl --user restart caramelod.service") {
		t.Errorf("a new configuration did not restart the running daemon:\n%s", run.Transcript())
	}
}

func TestCaramelodDoesNotRestartWhenNothingChanged(t *testing.T) {
	sudoUser(t, "alice")
	run := running(installerBox())
	run.Stdout("sha256sum -- "+serverconfig.BinaryPath, "abc123  "+serverconfig.BinaryPath+"\n")
	run.Respond("systemctl --user is-active --quiet caramelod.service", runner.Result{})
	env, _ := testEnv(t, run)
	unit := (&CaramelodStep{}).unitSpec(env)
	run.Stdout("stat -c %U:%G:%a:%F -- "+unit.Path, "caramelo:caramelo:644:regular file\n")
	run.Stdout("cat -- "+unit.Path, unit.Content)

	if err := (&CaramelodStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v\n%s", err, run.Transcript())
	}
	if run.Ran("systemctl --user restart") {
		t.Errorf("a run that changed nothing restarted the daemon:\n%s", run.Transcript())
	}
}
