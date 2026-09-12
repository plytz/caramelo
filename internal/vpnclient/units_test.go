package vpnclient

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemdUnit(t *testing.T) {
	unit := SystemdUnit("/usr/local/bin/caramelo", "worker1", "caramelo0")
	for _, want := range []string{
		"[Unit]", "[Service]", "[Install]",
		"Description=Caramelo tunnel to worker1",
		"ExecStart=/usr/local/bin/caramelo vpn service --machine worker1",

		"Restart=on-failure",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit is missing %q:\n%s", want, unit)
		}
	}

	if got := SystemdUnit("/bin/caramelo", "worker1", "wg7"); !strings.Contains(got, "--interface wg7") {
		t.Errorf("a named interface is missing from the unit:\n%s", got)
	}

	if got := SystemdUnit("/opt/my tools/caramelo", "worker1", ""); !strings.Contains(got, "'/opt/my tools/caramelo'") {
		t.Errorf("the binary path was not quoted:\n%s", got)
	}
}

func TestLaunchdPlist(t *testing.T) {
	plist := LaunchdPlist("/usr/local/bin/caramelo", "worker1", "")
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		"<key>Label</key>",
		"<string>" + LaunchdLabel + "</string>",
		"<string>/usr/local/bin/caramelo</string>",
		"<string>vpn</string>",
		"<string>service</string>",
		"<string>--machine</string>",
		"<string>worker1</string>",
		"<key>RunAtLoad</key>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("the plist is missing %q:\n%s", want, plist)
		}
	}

	if got := LaunchdPlist("/opt/a&b/caramelo", "worker1", ""); !strings.Contains(got, "a&amp;b") {
		t.Errorf("the path was not escaped:\n%s", got)
	}
}

func TestResolverEntryIsSplitDNS(t *testing.T) {
	entry := ResolverEntry("10.86.0.1:53")
	if !strings.Contains(entry, "domain internal\n") {
		t.Errorf("the resolver entry does not name the domain:\n%s", entry)
	}
	if !strings.Contains(entry, "nameserver 10.86.0.1\n") {
		t.Errorf("the resolver entry has no nameserver:\n%s", entry)
	}
	if strings.Contains(entry, "port 53") {
		t.Errorf("port 53 is the default and must not be written:\n%s", entry)
	}

	if strings.Contains(entry, "search") || strings.Contains(entry, "domain .\n") {
		t.Errorf("the resolver entry captures more than .internal:\n%s", entry)
	}
	if got := ResolverEntry("10.86.0.1:5353"); !strings.Contains(got, "port 5353") {
		t.Errorf("a non-standard port is missing:\n%s", got)
	}
}

func TestResolvectlCommandsRouteOnlyInternal(t *testing.T) {
	cmds := ResolvectlCommands("caramelo0", "10.86.0.1:53")
	if len(cmds) != 2 {
		t.Fatalf("got %d commands, want 2: %v", len(cmds), cmds)
	}
	if got, want := strings.Join(cmds[0], " "), "resolvectl dns caramelo0 10.86.0.1"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	if got, want := strings.Join(cmds[1], " "), "resolvectl domain caramelo0 ~internal"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

type recordingRun struct {
	calls []string
	fail  map[string]error
}

func (r *recordingRun) run(ctx context.Context, name string, args ...string) error {
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	if err, ok := r.fail[name]; ok {
		return err
	}
	return nil
}

func (r *recordingRun) joined() string { return strings.Join(r.calls, "\n") }

func TestInstallLinux(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	run := &recordingRun{}
	inst := &SystemInstaller{GOOS: "linux", Root: root, Home: home, Run: run.run}

	if installed, err := inst.Installed(context.Background()); err != nil || installed {
		t.Fatalf("Installed before installing = %v, %v", installed, err)
	}
	err := inst.Install(context.Background(), InstallOptions{
		Machine: "worker1", Binary: "/usr/local/bin/caramelo",
	})
	if err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(root, strings.TrimPrefix(SystemdUnitPath(home), "/"))
	b, err := os.ReadFile(unit)
	if err != nil {
		t.Fatalf("the unit was not written: %v", err)
	}
	if !strings.Contains(string(b), "--machine worker1") {
		t.Errorf("the unit does not carry the machine:\n%s", b)
	}

	if !strings.Contains(string(b), "ExecStart="+PrivilegedBinary) {
		t.Errorf("the unit does not run %s:\n%s", PrivilegedBinary, b)
	}

	for _, want := range []string{
		"install -D -m " + PrivilegedMode + " -o root -g ",
		"setcap cap_net_admin+eip " + filepath.Join(root, strings.TrimPrefix(PrivilegedBinary, "/")),
		"systemctl --user daemon-reload",
		"systemctl --user enable --now " + UnitName,
	} {
		if !strings.Contains(run.joined(), want) {
			t.Errorf("install did not run %q; it ran:\n%s", want, run.joined())
		}
	}
	if strings.Contains(run.joined(), "setcap cap_net_admin+eip /usr/local/bin/caramelo") {
		t.Errorf("the capability was granted to the shared binary; every local user "+
			"could then create interfaces and rewrite this computer's routes:\n%s", run.joined())
	}
	if installed, err := inst.Installed(context.Background()); err != nil || !installed {
		t.Fatalf("Installed after installing = %v, %v", installed, err)
	}

	if err := inst.Install(context.Background(), InstallOptions{Machine: "worker1", Binary: "/usr/local/bin/caramelo"}); err != nil {
		t.Fatalf("installing twice: %v", err)
	}

	run.calls = nil
	if err := inst.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Errorf("the unit survived uninstall: %v", err)
	}
	if !strings.Contains(run.joined(), "systemctl --user disable --now "+UnitName) {
		t.Errorf("uninstall did not stop the service; it ran:\n%s", run.joined())
	}
	if _, err := os.Stat(filepath.Join(root, strings.TrimPrefix(PrivilegedBinary, "/"))); !os.IsNotExist(err) {
		t.Errorf("a binary holding CAP_NET_ADMIN survived uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, strings.TrimPrefix(PolkitRuleFile, "/"))); !os.IsNotExist(err) {
		t.Errorf("the polkit rule survived uninstall: %v", err)
	}
	if err := inst.Uninstall(context.Background()); err != nil {
		t.Fatalf("uninstalling twice: %v", err)
	}
}

func TestInstallDarwin(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	run := &recordingRun{}
	inst := &SystemInstaller{GOOS: "darwin", Root: root, Home: home, Run: run.run}
	err := inst.Install(context.Background(), InstallOptions{Machine: "worker1", Binary: "/usr/local/bin/caramelo"})
	if err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(root, strings.TrimPrefix(LaunchdPlistPath(home), "/"))
	if _, err := os.Stat(plist); err != nil {
		t.Fatalf("the launchd agent was not written: %v", err)
	}
	resolver := filepath.Join(root, strings.TrimPrefix(ResolverFile, "/"))
	b, err := os.ReadFile(resolver)
	if err != nil {
		t.Fatalf("the resolver entry was not written: %v", err)
	}
	if !strings.Contains(string(b), "domain internal") {
		t.Errorf("the resolver entry is wrong:\n%s", b)
	}
	if !strings.Contains(run.joined(), "launchctl bootstrap") {
		t.Errorf("the agent was not started; the installer ran:\n%s", run.joined())
	}
	if err := inst.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(resolver); !os.IsNotExist(err) {
		t.Error("the resolver entry survived uninstall")
	}
}

func TestInstallUnsupportedPlatform(t *testing.T) {
	inst := &SystemInstaller{GOOS: "windows", Root: t.TempDir(), Home: t.TempDir(),
		Run: (&recordingRun{}).run}
	err := inst.Install(context.Background(), InstallOptions{Machine: "worker1", Binary: "c:/caramelo.exe"})
	if err == nil || !strings.Contains(err.Error(), ErrUnsupported.Error()) {
		t.Fatalf("err = %v, want %v", err, ErrUnsupported)
	}
	if installed, err := inst.Installed(context.Background()); err != nil || installed {
		t.Fatalf("Installed = %v, %v, want false and no error", installed, err)
	}
}

func TestInstallNeedsAMachine(t *testing.T) {
	inst := &SystemInstaller{GOOS: "linux", Root: t.TempDir(), Home: t.TempDir(),
		Run: (&recordingRun{}).run}
	if err := inst.Install(context.Background(), InstallOptions{Binary: "/bin/caramelo"}); err == nil {
		t.Fatal("installing with no machine succeeded")
	}
}

func TestInstallUnderSudoInstallsForTheInvoker(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	run := &recordingRun{}
	inst := &SystemInstaller{
		GOOS: "linux", Root: root, Home: home, Run: run.run,
		User: "alex", UID: "1000", GID: "100",
	}
	err := inst.Install(context.Background(), InstallOptions{
		Machine: "worker1", Binary: "/usr/local/bin/caramelo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, strings.TrimPrefix(SystemdUnitPath(home), "/"))); err != nil {
		t.Fatalf("the unit was not written into the invoker's home: %v", err)
	}
	want := "runuser -u alex -- env HOME=" + home + " XDG_RUNTIME_DIR=/run/user/1000 systemctl --user enable --now " + UnitName
	if !strings.Contains(run.joined(), want) {
		t.Errorf("the service was not started as the invoker; the installer ran:\n%s", run.joined())
	}

	priv := filepath.Join(root, strings.TrimPrefix(PrivilegedBinary, "/"))
	if !strings.Contains(run.joined(), "setcap cap_net_admin+eip "+priv) {
		t.Errorf("setcap was not run as root on the private copy:\n%s", run.joined())
	}
	copied := "install -D -m " + PrivilegedMode + " -o root -g 100 /usr/local/bin/caramelo " + priv
	if !strings.Contains(run.joined(), copied) {
		t.Errorf("missing %q; the installer ran:\n%s", copied, run.joined())
	}

	unitDir := filepath.Dir(filepath.Join(root, strings.TrimPrefix(SystemdUnitPath(home), "/")))
	if !strings.Contains(run.joined(), "chown 1000:100 "+filepath.Dir(unitDir)+" "+unitDir+" ") {
		t.Errorf("the unit was left owned by root:\n%s", run.joined())
	}

	rule := filepath.Join(root, strings.TrimPrefix(PolkitRuleFile, "/"))
	b2, err := os.ReadFile(rule)
	if err != nil {
		t.Fatalf("the polkit rule was not written: %v", err)
	}
	if !strings.Contains(string(b2), `subject.user == "alex"`) ||
		!strings.Contains(string(b2), "org.freedesktop.resolve1.set-dns-servers") {
		t.Errorf("the polkit rule does not let the invoker configure resolved:\n%s", b2)
	}

	if got := PolkitRule(`a" || true; //`); !strings.Contains(got, `"a\" || true; //"`) {
		t.Errorf("the user name was not escaped:\n%s", got)
	}

	if !strings.Contains(run.joined(), "loginctl enable-linger alex") {
		t.Errorf("lingering was not enabled for the invoker:\n%s", run.joined())
	}
	run.calls = nil
	if err := inst.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(run.joined(), "runuser -u alex") {
		t.Errorf("uninstall did not stop the invoker's service:\n%s", run.joined())
	}

	run.calls = nil
	if err := inst.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := inst.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"runuser -u alex -- env HOME=" + home + " XDG_RUNTIME_DIR=/run/user/1000 systemctl --user start " + UnitName,
		"runuser -u alex -- env HOME=" + home + " XDG_RUNTIME_DIR=/run/user/1000 systemctl --user stop " + UnitName,
	} {
		if !strings.Contains(run.joined(), want) {
			t.Errorf("missing %q; the installer ran:\n%s", want, run.joined())
		}
	}
}

func TestInstallDarwinUsesTheInvokersSession(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	run := &recordingRun{}
	inst := &SystemInstaller{GOOS: "darwin", Root: root, Home: home, Run: run.run,
		User: "alex", UID: "501", GID: "20"}
	if err := inst.Install(context.Background(), InstallOptions{Machine: "worker1", Binary: "/usr/local/bin/caramelo"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(run.joined(), "launchctl bootstrap gui/501") {
		t.Errorf("the agent was not bootstrapped into the invoker's session:\n%s", run.joined())
	}

	if !strings.Contains(run.joined(), "chown 501:20 ") {
		t.Errorf("the agent was left owned by root:\n%s", run.joined())
	}
}

func TestInstallRefusesANosuidBinary(t *testing.T) {
	dir := t.TempDir()
	mounts := filepath.Join(dir, "mounts")
	content := "/dev/sda1 / ext4 rw,relatime 0 0\n" +
		"tmpfs /tmp tmpfs rw,nosuid,nodev 0 0\n"
	if err := os.WriteFile(mounts, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := mountsFile
	mountsFile = mounts
	t.Cleanup(func() { mountsFile = old })

	if got := nosuidMount("/tmp/caramelo"); got != "/tmp" {
		t.Fatalf("nosuidMount(/tmp/caramelo) = %q, want /tmp", got)
	}
	if got := nosuidMount("/usr/local/bin/caramelo"); got != "" {
		t.Fatalf("nosuidMount(/usr/local/bin/caramelo) = %q, want no complaint", got)
	}

	run := &recordingRun{}
	root := t.TempDir()
	inst := &SystemInstaller{Root: root, Home: t.TempDir(), Run: run.run, User: "alex", UID: "1000", GID: "100"}
	if inst.goos() != "linux" {
		t.Skip("the capability check is Linux's")
	}

	if err := inst.Install(context.Background(), InstallOptions{Machine: "worker1", Binary: "/tmp/caramelo"}); err != nil {
		t.Fatalf("installing from a nosuid mount, which is only ever the source: %v", err)
	}

	content = strings.Replace(content, "/dev/sda1 / ext4 rw,relatime 0 0\n",
		"/dev/sda1 / ext4 rw,relatime 0 0\n/dev/sda2 /usr/local ext4 rw,nosuid 0 0\n", 1)
	if err := os.WriteFile(mounts, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run.calls = nil
	inst2 := &SystemInstaller{Root: t.TempDir(), Home: t.TempDir(), Run: run.run, User: "alex", UID: "1000", GID: "100"}
	err := inst2.Install(context.Background(), InstallOptions{Machine: "worker1", Binary: "/usr/local/bin/caramelo"})
	if err == nil || !strings.Contains(err.Error(), "nosuid") {
		t.Fatalf("err = %v, want it to name the nosuid mount", err)
	}
	if strings.Contains(run.joined(), "setcap") {
		t.Error("setcap was run on a filesystem that would ignore it")
	}

	if installed, err := inst2.Installed(context.Background()); err != nil || installed {
		t.Errorf("Installed after a refused install = %v, %v, want false", installed, err)
	}
	_ = root
}
