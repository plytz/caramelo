package dockersetup

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/runner"

	dockerrt "github.com/plytz/caramelo/internal/runtime/docker"
	"github.com/plytz/caramelo/internal/setup"
)

const infoJSON = `{"Driver":"overlayfs","CgroupDriver":"systemd","CgroupVersion":"2",
  "DockerRootDir":"/mnt/caramelo/docker","ServerVersion":"29.8.0","Name":"worker1",
  "SecurityOptions":["name=seccomp,profile=builtin","name=rootless","name=cgroupns"],
  "Warnings":null}`

const versionText = `Client: Docker Engine - Community
 Version:           29.8.0
 Context:           default

Server: Docker Engine - Community
 Engine:
  Version:          29.8.0
 rootlesskit:
  Version:          3.1.0
  NetworkDriver:    slirp4netns
  PortDriver:       builtin
  StateDir:         /run/user/999/dockerd-rootless
`

func testStep() *rootlessStep {
	clock := time.Unix(0, 0)
	return &rootlessStep{
		lookupUID:  func(string) (string, error) { return "999", nil },
		lookupHome: func(string) (string, error) { return "/var/lib/caramelo", nil },
		now:        func() time.Time { return clock },

		sleep: func(_ context.Context, d time.Duration) { clock = clock.Add(d) },
	}
}

const daemonJSONPath = "/var/lib/caramelo/.config/docker/daemon.json"

func daemonJSON(t *testing.T) string {
	t.Helper()
	b, err := dockerrt.RenderDaemonJSON(dockerrt.DaemonConfig{DataRoot: "/mnt/caramelo/docker"})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func healthyRunner(t *testing.T) *fakeRunner {
	f := &fakeRunner{}
	f.on("systemctl --user is-enabled docker", ok("enabled\n"))
	f.on("cat "+daemonJSONPath, ok(daemonJSON(t)))
	f.on("docker info", ok(infoJSON))
	f.on("docker version", ok(versionText))
	f.on("test -S", ok(""))
	return f
}

func TestRootlessCheckDone(t *testing.T) {
	f := healthyRunner(t)
	done, detail, err := testStep().Check(context.Background(), testEnv(f))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !done {
		t.Fatalf("not done: %s", detail)
	}
	for _, want := range []string{"rootless 29.8.0", "overlayfs", "/mnt/caramelo/docker", "slirp4netns/builtin"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not mention %q", detail, want)
		}
	}

	for _, c := range f.calls {
		if c.User != "caramelo" {
			t.Errorf("command %q ran as root, want the caramelo session", cmdKey(c))
		}
	}
}

func TestRootlessCheckUnitMissing(t *testing.T) {
	f := healthyRunner(t)
	f.rules = append([]rule{{match: "systemctl --user is-enabled docker", res: ok("not-found\n")}}, f.rules...)
	done, detail, err := testStep().Check(context.Background(), testEnv(f))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("done with no user unit")
	}
	if !strings.Contains(detail, "not-found") {
		t.Errorf("detail = %q", detail)
	}
	if f.indexOf("docker info", 0) >= 0 {
		t.Error("ran docker info before knowing the unit exists")
	}
}

func TestRootlessCheckDaemonJSONDiffers(t *testing.T) {
	f := healthyRunner(t)
	f.rules = append([]rule{{match: "cat " + daemonJSONPath,
		res: ok(`{"data-root": "/var/lib/caramelo/.local/share/docker"}`)}}, f.rules...)
	done, detail, err := testStep().Check(context.Background(), testEnv(f))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("done with a foreign daemon.json")
	}
	if !strings.Contains(detail, "daemon.json") {
		t.Errorf("detail = %q", detail)
	}
}

func TestRootlessCheckDaemonJSONMissing(t *testing.T) {
	f := healthyRunner(t)
	f.rules = append([]rule{{match: "cat " + daemonJSONPath, res: fail(1, "No such file or directory")}}, f.rules...)
	done, _, err := testStep().Check(context.Background(), testEnv(f))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("done with no daemon.json")
	}
}

func TestRootlessCheckRejectsWrongDaemon(t *testing.T) {
	tests := []struct {
		name, info, want string
	}{
		{"rootful", `{"ServerVersion":"29.8.0","Driver":"overlayfs","DockerRootDir":"/mnt/caramelo/docker",
			"CgroupDriver":"systemd","CgroupVersion":"2","SecurityOptions":["name=apparmor"]}`, "not rootless"},
		{"data root", `{"ServerVersion":"29.8.0","Driver":"overlayfs","DockerRootDir":"/var/lib/docker",
			"CgroupDriver":"systemd","CgroupVersion":"2","SecurityOptions":["name=rootless"]}`, "data root"},
		{"storage driver", `{"ServerVersion":"29.8.0","Driver":"vfs","DockerRootDir":"/mnt/caramelo/docker",
			"CgroupDriver":"systemd","CgroupVersion":"2","SecurityOptions":["name=rootless"]}`, "storage driver"},
		{"cgroup v1", `{"ServerVersion":"29.8.0","Driver":"overlayfs","DockerRootDir":"/mnt/caramelo/docker",
			"CgroupDriver":"cgroupfs","CgroupVersion":"1","SecurityOptions":["name=rootless"]}`, "cgroup version"},
		{"cgroup driver", `{"ServerVersion":"29.8.0","Driver":"overlayfs","DockerRootDir":"/mnt/caramelo/docker",
			"CgroupDriver":"cgroupfs","CgroupVersion":"2","SecurityOptions":["name=rootless"]}`, "cgroup driver"},
		{"delegation", `{"ServerVersion":"29.8.0","Driver":"overlayfs","DockerRootDir":"/mnt/caramelo/docker",
			"CgroupDriver":"systemd","CgroupVersion":"2","SecurityOptions":["name=rootless"],
			"Warnings":["WARNING: No cpuset support","WARNING: No io.max (rbps) support"]}`, "cgroup delegation missing"},
		{"br_netfilter", `{"ServerVersion":"29.8.0","Driver":"overlayfs","DockerRootDir":"/mnt/caramelo/docker",
			"CgroupDriver":"systemd","CgroupVersion":"2","SecurityOptions":["name=rootless"],
			"Warnings":["WARNING: bridge-nf-call-iptables is disabled, br_netfilter not loaded"]}`, "br_netfilter"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := healthyRunner(t)
			f.rules = append([]rule{{match: "docker info", res: ok(tc.info)}}, f.rules...)
			done, detail, err := testStep().Check(context.Background(), testEnv(f))
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if done {
				t.Fatalf("done for %s", tc.name)
			}
			if !strings.Contains(detail, tc.want) {
				t.Errorf("detail = %q, want it to mention %q", detail, tc.want)
			}
		})
	}
}

func TestRootlessCheckDaemonDown(t *testing.T) {
	f := healthyRunner(t)
	f.rules = append([]rule{{match: "docker info",
		res: fail(1, "Cannot connect to the Docker daemon at unix:///run/user/999/docker.sock.")}}, f.rules...)
	done, detail, err := testStep().Check(context.Background(), testEnv(f))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("done with the daemon down")
	}
	if !strings.Contains(detail, "docker info failed") {
		t.Errorf("detail = %q", detail)
	}
}

func freshRunner(t *testing.T) *fakeRunner {
	f := &fakeRunner{}
	f.on("test -S", ok(""))
	f.on("docker version", ok(versionText))
	f.on("cat "+daemonJSONPath, ok(daemonJSON(t)))
	installed := false
	f.hook = func(c runner.Cmd) (runner.Result, bool) {
		switch k := cmdKey(c); {
		case k == "systemctl --user is-enabled docker":
			if installed {
				return ok("enabled\n"), true
			}
			return ok("not-found\n"), true
		case strings.HasPrefix(k, "dockerd-rootless-setuptool.sh install"):
			installed = true
			return ok(""), true
		case strings.HasPrefix(k, "docker info"):
			if !installed {
				return fail(1, "Cannot connect to the Docker daemon"), true
			}
			return ok(infoJSON), true
		}
		return runner.Result{}, false
	}
	return f
}

func TestRootlessApplyFreshBox(t *testing.T) {
	f := freshRunner(t)
	if err := testStep().Apply(context.Background(), testEnv(f)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	requireOrder(t, f,
		"mkdir -p /var/lib/caramelo/.config/docker",
		"sh -c umask 022 && cat > '"+daemonJSONPath+"'",
		"dockerd-rootless-setuptool.sh install",
		"docker info",
	)

	for _, c := range f.calls {
		if c.User != "caramelo" && !strings.HasPrefix(cmdKey(c), "systemctl restart user@") &&
			cmdKey(c) != "modprobe nf_tables" {
			t.Errorf("command %q did not run as caramelo", cmdKey(c))
		}
	}
	var wrote bool
	for _, c := range f.calls {
		if !strings.HasPrefix(cmdKey(c), "sh -c umask") {
			continue
		}
		wrote = true
		if c.Stdin == nil {
			t.Fatal("daemon.json written with no content")
		}
		content, err := io.ReadAll(c.Stdin)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != daemonJSON(t) {
			t.Errorf("daemon.json content = %q, want %q", content, daemonJSON(t))
		}
	}
	if !wrote {
		t.Fatal("daemon.json never written")
	}
}

func TestRootlessApplyKeepsIptablesWhenTheNfTablesModuleIsThere(t *testing.T) {
	f := freshRunner(t)
	f.rules = append([]rule{
		{match: "test -e " + nftablesModulePath, res: ok("")},
	}, f.rules...)

	if err := testStep().Apply(context.Background(), testEnv(f)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if f.indexOf("dockerd-rootless-setuptool.sh install --skip-iptables", 0) >= 0 {
		t.Errorf("skipped iptables although %s is there; log:\n%s", nftablesModulePath, strings.Join(f.log(), "\n"))
	}
	if f.indexOf("dockerd-rootless-setuptool.sh install", 0) < 0 {
		t.Errorf("the setuptool never ran; log:\n%s", strings.Join(f.log(), "\n"))
	}
}

func TestRootlessApplySkipsIptablesWhenNfTablesIsBuiltIn(t *testing.T) {
	f := freshRunner(t)
	f.rules = append([]rule{
		{match: "test -e " + nftablesModulePath, res: fail(1, "")},
		{match: "test -e " + netfilterSysctlDir, res: ok("")},
	}, f.rules...)

	if err := testStep().Apply(context.Background(), testEnv(f)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	requireOrder(t, f,
		"modprobe nf_tables",
		"dockerd-rootless-setuptool.sh install --skip-iptables",
		"docker info",
	)
}

func TestRootlessApplySurvivesAModprobeThatCannotRun(t *testing.T) {
	f := freshRunner(t)
	f.rules = append([]rule{
		{match: "modprobe nf_tables", err: errors.New("modprobe: not found")},
		{match: "test -e " + nftablesModulePath, res: fail(1, "")},
		{match: "test -e " + netfilterSysctlDir, res: ok("")},
	}, f.rules...)

	if err := testStep().Apply(context.Background(), testEnv(f)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if f.indexOf("dockerd-rootless-setuptool.sh install --skip-iptables", 0) < 0 {
		t.Errorf("a failed modprobe stopped the install; log:\n%s", strings.Join(f.log(), "\n"))
	}
}

func TestRootlessApplyKeepsIptablesWhenNeitherNfTablesPathIsThere(t *testing.T) {
	f := freshRunner(t)
	f.rules = append([]rule{
		{match: "test -e " + nftablesModulePath, res: fail(1, "")},
		{match: "test -e " + netfilterSysctlDir, res: fail(1, "")},
	}, f.rules...)

	if err := testStep().Apply(context.Background(), testEnv(f)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if f.indexOf("dockerd-rootless-setuptool.sh install --skip-iptables", 0) >= 0 {
		t.Errorf("skipped iptables with no sign of nf_tables; log:\n%s", strings.Join(f.log(), "\n"))
	}
}

func TestRootlessApplyRestartsExistingUnit(t *testing.T) {
	f := healthyRunner(t)
	if err := testStep().Apply(context.Background(), testEnv(f)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if f.indexOf("dockerd-rootless-setuptool.sh install", 0) >= 0 {
		t.Error("ran the setuptool over an existing unit")
	}
	requireOrder(t, f,
		"sh -c umask 022 && cat > '"+daemonJSONPath+"'",
		"systemctl --user restart docker",
		"docker info",
	)
}

func TestRootlessApplyRestartsUserSessionForTheBus(t *testing.T) {
	f := healthyRunner(t)

	f.rules = append([]rule{{match: "test -S /run/user/999/bus", res: fail(1, "")}}, f.rules...)
	err := testStep().Apply(context.Background(), testEnv(f))
	if err == nil {
		t.Fatal("Apply succeeded with no user bus")
	}
	if f.indexOf("systemctl restart user@999.service", 0) < 0 {
		t.Errorf("did not restart the user session; log:\n%s", strings.Join(f.log(), "\n"))
	}
	if !strings.Contains(err.Error(), "linger") {
		t.Errorf("error %q does not mention linger", err)
	}
}

func TestRootlessApplyFailsWhenDaemonStaysUnhealthy(t *testing.T) {
	f := healthyRunner(t)
	f.rules = append([]rule{{match: "docker info", res: ok(`{"ServerVersion":"29.8.0","Driver":"vfs",
		"DockerRootDir":"/mnt/caramelo/docker","CgroupDriver":"systemd","CgroupVersion":"2",
		"SecurityOptions":["name=rootless"]}`)}}, f.rules...)
	err := testStep().Apply(context.Background(), testEnv(f))
	if err == nil {
		t.Fatal("Apply accepted a daemon with the wrong storage driver")
	}
	if !strings.Contains(err.Error(), "storage driver") {
		t.Errorf("error = %v", err)
	}
}

func TestStepNames(t *testing.T) {
	if got := Packages().Name(); got != "docker-packages" {
		t.Errorf("packages step name = %q", got)
	}
	if got := Rootless().Name(); got != "docker-rootless" {
		t.Errorf("rootless step name = %q", got)
	}
	var _ setup.Step = Packages()
	var _ setup.Step = Rootless()
}

const unitPath = "/var/lib/caramelo/.config/systemd/user/docker.service"

const unitWithoutIptables = "[Service]\nExecStart=/usr/bin/dockerd-rootless.sh \nExecReload=/bin/kill -s HUP $MAINPID\n"

func skipIptablesRunner(t *testing.T, unit string) *fakeRunner {
	f := freshRunner(t)
	f.rules = append([]rule{
		{match: "test -e " + nftablesModulePath, res: fail(1, "")},
		{match: "test -e " + netfilterSysctlDir, res: ok("")},
		{match: "cat " + unitPath, res: ok(unit)},
	}, f.rules...)
	return f
}

func writtenTo(t *testing.T, f *fakeRunner, path string) (string, bool) {
	t.Helper()
	for _, c := range f.calls {
		if c.Name != "sh" || c.Stdin == nil || !strings.Contains(cmdKey(c), path) {
			continue
		}
		b, err := io.ReadAll(c.Stdin)
		if err != nil {
			t.Fatal(err)
		}
		return string(b), true
	}
	return "", false
}

func TestRootlessApplyTakesTheIptablesFlagBackOutOfTheUnit(t *testing.T) {
	f := skipIptablesRunner(t, "[Service]\nExecStart=/usr/bin/dockerd-rootless.sh  --iptables=false\nExecReload=/bin/kill -s HUP $MAINPID\n")

	if err := testStep().Apply(context.Background(), testEnv(f)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, ok := writtenTo(t, f, unitPath)
	if !ok {
		t.Fatalf("the unit was never rewritten; log:\n%s", strings.Join(f.log(), "\n"))
	}
	if strings.Contains(got, iptablesOffFlag) {
		t.Errorf("the rewritten unit still disables iptables:\n%s", got)
	}
	if got != unitWithoutIptables {
		t.Errorf("rewritten unit = %q, want %q", got, unitWithoutIptables)
	}
	requireOrder(t, f,
		"dockerd-rootless-setuptool.sh install --skip-iptables",
		"systemctl --user daemon-reload",
		"systemctl --user restart docker",
	)
}

func TestRootlessApplyLeavesAUnitThatNeverDisabledIptables(t *testing.T) {
	f := skipIptablesRunner(t, unitWithoutIptables)

	if err := testStep().Apply(context.Background(), testEnv(f)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, ok := writtenTo(t, f, unitPath); ok {
		t.Errorf("rewrote a unit that was already right; log:\n%s", strings.Join(f.log(), "\n"))
	}
	if f.indexOf("systemctl --user restart docker", 0) >= 0 {
		t.Errorf("restarted a daemon that was fine; log:\n%s", strings.Join(f.log(), "\n"))
	}
}
