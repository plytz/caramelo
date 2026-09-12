package setup

import (
	"context"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/setup/testutil"
)

func healthyBox() *testutil.FakeRunner {
	run := testutil.New()
	run.Stdout("cat -- /etc/os-release", "PRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nID=debian\nVERSION_ID=\"13\"\nVERSION_CODENAME=trixie\n")
	run.Respond("test -d /run/systemd/system", runner.Result{})
	run.Stdout("stat -fc %T /sys/fs/cgroup", "cgroup2fs\n")
	run.Stdout("uname -r", "6.12.86+deb13-amd64\n")
	run.Respond("sh -c command -v apt-get", runner.Result{Stdout: "/usr/bin/apt-get\n"})
	run.Exit("systemctl is-active --quiet docker.service", 3)
	return run
}

func asRoot(t *testing.T) {
	t.Helper()
	prev := geteuid
	geteuid = func() int { return 0 }
	t.Cleanup(func() { geteuid = prev })
}

func TestPreflightAcceptsASupportedBox(t *testing.T) {
	asRoot(t)
	env, log := testEnv(t, healthyBox())
	done, detail, err := (&PreflightStep{}).Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %q, %v; want done", done, detail, err)
	}
	for _, want := range []string{"debian 13", "cgroup2", "kernel 6.12.86"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not mention %q", detail, want)
		}
	}
	if strings.Contains(log.String(), "warning") {
		t.Errorf("a supported box warned: %q", log.String())
	}
}

func TestPreflightRefusesNonRoot(t *testing.T) {
	prev := geteuid
	geteuid = func() int { return 1000 }
	t.Cleanup(func() { geteuid = prev })

	env, _ := testEnv(t, healthyBox())
	_, _, err := (&PreflightStep{}).Check(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("err = %v, want an error naming sudo", err)
	}
}

func TestPreflightRejections(t *testing.T) {
	tests := []struct {
		name    string
		script  func(*testutil.FakeRunner)
		force   bool
		wantErr string
	}{
		{
			name:    "another distribution",
			script:  func(r *testutil.FakeRunner) { r.Stdout("cat -- /etc/os-release", "ID=alpine\nVERSION_ID=3.20\n") },
			wantErr: "unsupported distribution",
		},
		{
			name:   "another distribution with --force",
			script: func(r *testutil.FakeRunner) { r.Stdout("cat -- /etc/os-release", "ID=alpine\nVERSION_ID=3.20\n") },
			force:  true,
		},
		{
			name:    "cgroup v1",
			script:  func(r *testutil.FakeRunner) { r.Stdout("stat -fc %T /sys/fs/cgroup", "tmpfs\n") },
			wantErr: "cgroup v2",
		},
		{
			name:    "no systemd",
			script:  func(r *testutil.FakeRunner) { r.Exit("test -d /run/systemd/system", 1) },
			wantErr: "systemd is not running",
		},
		{
			name:    "old kernel",
			script:  func(r *testutil.FakeRunner) { r.Stdout("uname -r", "4.19.0-21-amd64\n") },
			wantErr: "too old",
		},
		{
			name:    "no apt",
			script:  func(r *testutil.FakeRunner) { r.Exit("sh -c command -v apt-get", 127) },
			wantErr: "apt-get not found",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			asRoot(t)
			run := healthyBox()
			tc.script(run)
			env, _ := testEnv(t, run)
			env.Opts.Force = tc.force
			done, _, err := (&PreflightStep{}).Check(context.Background(), env)
			switch {
			case tc.wantErr == "":
				if err != nil || !done {
					t.Fatalf("Check() = %v, %v; want done with no error", done, err)
				}
			case err == nil:
				t.Fatalf("Check() succeeded; want an error mentioning %q", tc.wantErr)
			case !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestPreflightWarnsAboutUntestedVersionAndRootfulDocker(t *testing.T) {
	asRoot(t)
	run := healthyBox()
	run.Stdout("cat -- /etc/os-release", "ID=ubuntu\nVERSION_ID=\"20.04\"\n")
	run.Respond("systemctl is-active --quiet docker.service", runner.Result{})
	env, log := testEnv(t, run)

	done, detail, err := (&PreflightStep{}).Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %v; want a warning, not a failure", done, err)
	}
	if !strings.Contains(log.String(), "not a tested version") {
		t.Errorf("log %q does not warn about the version", log.String())
	}
	if !strings.Contains(log.String(), "rootful docker.service is running") {
		t.Errorf("log %q does not warn about the running daemon", log.String())
	}
	if !strings.Contains(detail, "2 warning") {
		t.Errorf("detail %q does not count the warnings", detail)
	}
}

func TestPreflightSkipsAptWithoutPackages(t *testing.T) {
	asRoot(t)
	run := healthyBox()
	run.Exit("sh -c command -v apt-get", 127)
	env, log := testEnv(t, run)
	env.Opts.InstallPackages = false

	done, _, err := (&PreflightStep{}).Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %v; want done when packages are not ours to install", done, err)
	}
	if !strings.Contains(log.String(), "apt-get not found") {
		t.Errorf("log %q does not mention the missing apt", log.String())
	}
}

func TestParseKernelVersion(t *testing.T) {
	tests := []struct {
		in           string
		major, minor int
		wantErr      bool
	}{
		{in: "6.12.86+deb13-amd64", major: 6, minor: 12},
		{in: "5.15.0-105-generic\n", major: 5, minor: 15},
		{in: "6.1", major: 6, minor: 1},
		{in: "5.10.0-rc1", major: 5, minor: 10},
		{in: "weird", wantErr: true},
	}
	for _, tc := range tests {
		major, minor, err := parseKernelVersion(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseKernelVersion(%q) = %d.%d, want an error", tc.in, major, minor)
			}
			continue
		}
		if err != nil || major != tc.major || minor != tc.minor {
			t.Errorf("parseKernelVersion(%q) = %d.%d, %v; want %d.%d", tc.in, major, minor, err, tc.major, tc.minor)
		}
	}
}

func TestParseOSRelease(t *testing.T) {
	got := parseOSRelease("# comment\nID=debian\nVERSION_ID=\"13\"\nPRETTY_NAME='Debian 13'\nbroken\n")
	for k, want := range map[string]string{"ID": "debian", "VERSION_ID": "13", "PRETTY_NAME": "Debian 13"} {
		if got[k] != want {
			t.Errorf("parseOSRelease()[%q] = %q, want %q", k, got[k], want)
		}
	}
}
