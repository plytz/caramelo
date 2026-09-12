package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/setup"
)

type fakeShell struct {
	t       *testing.T
	answers []answer
	ran     []string
	stdin   map[string]string
}

type answer struct {
	prefix string
	stdout string
	stderr string
	code   int
	err    error
}

func (f *fakeShell) Run(_ context.Context, c Cmd) (int, error) {
	f.ran = append(f.ran, c.Line)
	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		if f.stdin == nil {
			f.stdin = map[string]string{}
		}
		f.stdin[c.Line] = string(b)
	}
	for _, a := range f.answers {
		if strings.HasPrefix(c.Line, a.prefix) {
			if c.Stdout != nil {
				_, _ = io.WriteString(c.Stdout, a.stdout)
			}
			if c.Stderr != nil {
				_, _ = io.WriteString(c.Stderr, a.stderr)
			}
			return a.code, a.err
		}
	}
	f.t.Fatalf("unexpected command line %q", c.Line)
	return 0, nil
}

func probeAnswer(privilege string, keys bool) string {
	k := "no-keys"
	if keys {
		k = "keys"
	}
	return fmt.Sprintf("Linux %s\n1000\n%s\n%s\n", unameArch(), privilege, k)
}

func unameArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return runtime.GOARCH
}

func fakeBinary(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "caramelo")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho fake\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func reportJSON(t *testing.T, r setup.Report) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func TestRunShipsAndRunsSetupWithSudo(t *testing.T) {
	bin := fakeBinary(t)
	want := setup.Report{RunID: "r1", Changed: 3, Results: []setup.Result{{Step: "summary", Status: setup.StatusOK}}}
	sh := &fakeShell{t: t, answers: []answer{
		{prefix: "uname -sm", stdout: probeAnswer(PrivilegeSudo, true)},
		{prefix: "d=$(mktemp", stdout: "/tmp/caramelo-setup.abc123\n"},
		{prefix: "sudo -n /tmp/caramelo-setup.abc123/caramelo server setup", stdout: reportJSON(t, want), stderr: "[changed] user\n"},
		{prefix: "rm -rf", code: 0},
	}}
	var log, remoteErr bytes.Buffer
	out, err := Run(context.Background(), sh, Options{
		Binary: bin, SetupArgs: []string{"--data-dir=/mnt/x", "--force"}, Log: &log, RemoteStderr: &remoteErr,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Report.RunID != want.RunID || out.Report.Changed != 3 {
		t.Errorf("report = %+v, want %+v", out.Report, want)
	}
	if out.Dir != "/tmp/caramelo-setup.abc123" {
		t.Errorf("dir = %q", out.Dir)
	}
	if out.Probe.Privilege != PrivilegeSudo || !out.Probe.HasAuthorizedKeys {
		t.Errorf("probe = %+v", out.Probe)
	}

	if got := sh.stdin[shipScript]; got != "#!/bin/sh\necho fake\n" {
		t.Errorf("shipped content = %q", got)
	}

	setupLine := sh.ran[2]
	for _, want := range []string{"--yes --json", "--data-dir=/mnt/x", "--force", `--authorized-keys "$HOME/.ssh/authorized_keys"`} {
		if !strings.Contains(setupLine, want) {
			t.Errorf("setup line %q lacks %q", setupLine, want)
		}
	}
	if sh.ran[3] != "rm -rf /tmp/caramelo-setup.abc123" {
		t.Errorf("cleanup line = %q", sh.ran[3])
	}
	if !strings.Contains(remoteErr.String(), "[changed] user") {
		t.Errorf("remote stderr not passed through: %q", remoteErr.String())
	}
	if !strings.Contains(log.String(), "passwordless sudo") {
		t.Errorf("log = %q", log.String())
	}
}

func TestRunAsRootWithLocalKeys(t *testing.T) {
	bin := fakeBinary(t)
	keys := filepath.Join(t.TempDir(), "keys.pub")
	if err := os.WriteFile(keys, []byte("ssh-ed25519 AAAA test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sh := &fakeShell{t: t, answers: []answer{
		{prefix: "uname -sm", stdout: probeAnswer(PrivilegeRoot, false)},
		{prefix: "d=$(mktemp", stdout: "/tmp/caramelo-setup.root1\n"},
		{prefix: "cat >/tmp/caramelo-setup.root1/authorized_keys"},
		{prefix: "/tmp/caramelo-setup.root1/caramelo server setup", stdout: reportJSON(t, setup.Report{RunID: "r2"})},
		{prefix: "rm -rf"},
	}}
	out, err := Run(context.Background(), sh, Options{Binary: bin, AuthorizedKeys: keys})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Report.RunID != "r2" {
		t.Errorf("report = %+v", out.Report)
	}
	if got := sh.stdin["cat >/tmp/caramelo-setup.root1/authorized_keys"]; got != "ssh-ed25519 AAAA test\n" {
		t.Errorf("shipped keys = %q", got)
	}
	line := sh.ran[3]
	if strings.HasPrefix(line, "sudo") {
		t.Errorf("root should not use sudo: %q", line)
	}
	if !strings.Contains(line, "--authorized-keys /tmp/caramelo-setup.root1/authorized_keys") {
		t.Errorf("setup line %q does not use the shipped keys", line)
	}
}

func TestRunRefusesBeforeShipping(t *testing.T) {
	bin := fakeBinary(t)
	cases := map[string]struct {
		probe string
		want  string
	}{
		"sudo needs a password":      {probeAnswer(PrivilegeSudoPassword, true), "password for sudo"},
		"no sudo":                    {probeAnswer(PrivilegeNoSudo, true), "no sudo"},
		"not a shell":                {"garbage", "unexpected answer"},
		"a path in the uname answer": {"X/../../../../../../etc/os release\n0\nroot\nno-keys\n", "unexpected uname output"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			sh := &fakeShell{t: t, answers: []answer{{prefix: "uname -sm", stdout: c.probe}}}
			_, err := Run(context.Background(), sh, Options{Binary: bin})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to mention %q", err, c.want)
			}
			if len(sh.ran) != 1 {
				t.Errorf("ran %d commands, want only the probe: %q", len(sh.ran), sh.ran)
			}
		})
	}
}

func TestRunReportsAnUnreachableTarget(t *testing.T) {
	sh := &fakeShell{t: t, answers: []answer{{prefix: "uname -sm", err: fmt.Errorf("ssh to x failed: Permission denied")}}}
	_, err := Run(context.Background(), sh, Options{Binary: fakeBinary(t)})
	if err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunWithoutAReportIsAnErrorAndStillCleansUp(t *testing.T) {
	sh := &fakeShell{t: t, answers: []answer{
		{prefix: "uname -sm", stdout: probeAnswer(PrivilegeSudo, false)},
		{prefix: "d=$(mktemp", stdout: "/tmp/caramelo-setup.x\n"},
		{prefix: "sudo -n", stderr: "sudo: a password is required\n", code: 1},
		{prefix: "rm -rf"},
	}}
	_, err := Run(context.Background(), sh, Options{Binary: fakeBinary(t)})
	if err == nil || !strings.Contains(err.Error(), "exited 1 without a report") {
		t.Fatalf("err = %v", err)
	}
	if last := sh.ran[len(sh.ran)-1]; last != "rm -rf /tmp/caramelo-setup.x" {
		t.Errorf("last command = %q, want the cleanup", last)
	}

	if strings.Contains(sh.ran[2], "--authorized-keys") {
		t.Errorf("setup line %q should not name a keys file", sh.ran[2])
	}
}

func TestRunFailedStepIsAReportNotAnError(t *testing.T) {
	rep := setup.Report{RunID: "r3", Failed: 1, Results: []setup.Result{{Step: "preflight", Status: setup.StatusFailed, Error: "too small"}}}
	sh := &fakeShell{t: t, answers: []answer{
		{prefix: "uname -sm", stdout: probeAnswer(PrivilegeRoot, true)},
		{prefix: "d=$(mktemp", stdout: "/tmp/caramelo-setup.y\n"},
		{prefix: "/tmp/caramelo-setup.y/caramelo server setup", stdout: reportJSON(t, rep), code: 1},
		{prefix: "rm -rf"},
	}}
	out, err := Run(context.Background(), sh, Options{Binary: fakeBinary(t)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Report.Failed != 1 || out.ExitCode != 1 {
		t.Errorf("outcome = %+v", out)
	}
}

func TestOpenSSHArgv(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\"\ncat\nif [ \"$FAKE_SSH_FAIL\" = 1 ]; then echo 'Permission denied (publickey).' >&2; exit 255; fi\n"
	fake := filepath.Join(dir, "ssh")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	target, err := remote.ParseTargetWith("admin@box.example:2222", "me", 22)
	if err != nil {
		t.Fatal(err)
	}
	sh := OpenSSH{Target: target, Bin: fake, Extra: []string{"-F", "/x/config"}}
	var stdout bytes.Buffer
	code, err := sh.Run(context.Background(), Cmd{Line: "uname -sm; id -u", Stdin: strings.NewReader("piped\n"), Stdout: &stdout})
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	got := stdout.String()
	for _, want := range []string{"BatchMode=yes", "StrictHostKeyChecking=accept-new", "-F\n/x/config\n", "-p\n2222\n", "admin@box.example\n--\nuname -sm; id -u\n", "piped\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("fake ssh saw %q, want %q in it", got, want)
		}
	}

	t.Setenv("FAKE_SSH_FAIL", "1")
	code, err = sh.Run(context.Background(), Cmd{Line: "true"})
	if code != sshFailure || err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Errorf("failed ssh: code %d err %v", code, err)
	}
}

func TestOpenSSHPassesTheCommandsExitCode(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "ssh")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sh := OpenSSH{Target: remote.Target{User: "u", Host: "h", Port: 22}, Bin: fake}
	code, err := sh.Run(context.Background(), Cmd{Line: "false"})
	if err != nil || code != 3 {
		t.Errorf("code %d err %v, want 3 and nil", code, err)
	}
}

func TestRunAdmitsAPeer(t *testing.T) {
	bin := fakeBinary(t)
	peer := setup.PeerSpec{Name: "alex-laptop", PublicKey: "Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE="}
	sh := &fakeShell{t: t, answers: []answer{
		{prefix: "uname -sm", stdout: probeAnswer(PrivilegeRoot, true)},
		{prefix: "d=$(mktemp", stdout: "/tmp/caramelo-setup.abc123\n"},
		{prefix: "/tmp/caramelo-setup.abc123/caramelo server setup", stdout: reportJSON(t, setup.Report{RunID: "r1"})},
		{prefix: "rm -rf", code: 0},
	}}
	if _, err := Run(context.Background(), sh, Options{Binary: bin, Peer: peer}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	setupLine := sh.ran[2]
	if !strings.Contains(setupLine, "--peer 'alex-laptop Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE='") {
		t.Errorf("setup line = %q", setupLine)
	}
}

func TestRunRefusesAnUnusablePeer(t *testing.T) {
	bin := fakeBinary(t)
	sh := &fakeShell{t: t, answers: []answer{
		{prefix: "uname -sm", stdout: probeAnswer(PrivilegeRoot, true)},
		{prefix: "d=$(mktemp", stdout: "/tmp/caramelo-setup.abc123\n"},
		{prefix: "rm -rf", code: 0},
	}}
	_, err := Run(context.Background(), sh, Options{Binary: bin, Peer: setup.PeerSpec{Name: "laptop", PublicKey: "nonsense"}})
	if err == nil {
		t.Fatal("a peer whose key is not a key was accepted")
	}
	for _, line := range sh.ran {
		if strings.Contains(line, "server setup") {
			t.Errorf("setup ran anyway: %q", line)
		}
	}
}

type fakeDownloader struct {
	asked []Release
	path  string
	err   error
}

func (d *fakeDownloader) install(t *testing.T) *fakeDownloader {
	t.Helper()
	if d.path == "" && d.err == nil {
		d.path = filepath.Join(t.TempDir(), "downloaded")
		if err := os.WriteFile(d.path, []byte("release"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	downloadRelease = func(_ context.Context, r Release) (string, error) {
		d.asked = append(d.asked, r)
		return d.path, d.err
	}
	t.Cleanup(func() { downloadRelease = Download })
	return d
}

func selfBinary(t *testing.T) (dir string, self string) {
	t.Helper()
	dir = t.TempDir()
	self = filepath.Join(dir, "caramelo")
	if err := os.WriteFile(self, []byte("self"), 0o755); err != nil {
		t.Fatal(err)
	}
	executablePath = func() (string, error) { return self, nil }
	t.Cleanup(func() { executablePath = os.Executable })
	return dir, self
}

func anotherPlatform() Probe {
	if runtime.GOOS == "linux" && runtime.GOARCH == "arm64" {
		return Probe{OS: "linux", Arch: "amd64"}
	}
	return Probe{OS: "linux", Arch: "arm64"}
}

func writeSibling(t *testing.T, dir string, p Probe) string {
	t.Helper()
	sibling := filepath.Join(dir, SiblingName(p.OS, p.Arch))
	if err := os.WriteFile(sibling, []byte("cross"), 0o755); err != nil {
		t.Fatal(err)
	}
	return sibling
}

func TestResolveBinaryForAnotherArchitecture(t *testing.T) {
	dir, self := selfBinary(t)
	d := (&fakeDownloader{}).install(t)
	other := anotherPlatform()

	_, err := ResolveBinary(context.Background(), Options{}, other)
	if err == nil {
		t.Fatal("a target with no binary for it must be refused")
	}
	for _, want := range []string{"--binary", SiblingName(other.OS, other.Arch)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %v, want it to mention %q", err, want)
		}
	}

	sibling := writeSibling(t, dir, other)
	if got, err := ResolveBinary(context.Background(), Options{}, other); err != nil || got != sibling {
		t.Errorf("ResolveBinary = %q, %v; want the sibling %s", got, err, sibling)
	}

	same := Probe{OS: runtime.GOOS, Arch: runtime.GOARCH}
	if got, err := ResolveBinary(context.Background(), Options{}, same); err != nil || got != self {
		t.Errorf("ResolveBinary for the same platform = %q, %v; want %s", got, err, self)
	}

	explicit := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(explicit, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveBinary(context.Background(), Options{Binary: explicit}, other); err != nil || got != explicit {
		t.Errorf("ResolveBinary(--binary) = %q, %v", got, err)
	}
	if _, err := ResolveBinary(context.Background(), Options{Binary: filepath.Join(dir, "nope")}, other); err == nil || !strings.Contains(err.Error(), "--binary") {
		t.Errorf("a --binary that is not there must be refused naming the flag, got %v", err)
	}

	if len(d.asked) != 0 {
		t.Errorf("nothing here should have been downloaded, got %+v", d.asked)
	}
}

func TestResolveBinaryReleaseFlagOutranksThisBinary(t *testing.T) {
	selfBinary(t)
	d := (&fakeDownloader{}).install(t)
	same := Probe{OS: runtime.GOOS, Arch: runtime.GOARCH}
	if !Supported(same.OS, same.Arch) {
		t.Skipf("this platform %s/%s publishes no release", same.OS, same.Arch)
	}

	var log bytes.Buffer
	got, err := ResolveBinary(context.Background(), Options{Release: "v0.0.1", Version: "v9.9.9", Log: &log}, same)
	if err != nil || got != d.path {
		t.Fatalf("ResolveBinary(--release) = %q, %v; want %s", got, err, d.path)
	}
	if len(d.asked) != 1 {
		t.Fatalf("downloads asked for = %+v, want one", d.asked)
	}
	r := d.asked[0]
	if r.Tag != "v0.0.1" || r.OS != same.OS || r.Arch != same.Arch {
		t.Errorf("downloaded %+v, want v0.0.1 for %s/%s", r, same.OS, same.Arch)
	}
	if !strings.Contains(log.String(), "shipping release v0.0.1 for "+same.OS+"/"+same.Arch) {
		t.Errorf("log = %q", log.String())
	}
}

func TestResolveBinaryRefusesAReleaseThatIsNotATag(t *testing.T) {
	selfBinary(t)
	d := (&fakeDownloader{}).install(t)
	same := Probe{OS: runtime.GOOS, Arch: runtime.GOARCH}

	for _, tag := range []string{"latest", "0.0.1", "v1.2", "v1.2.3.4"} {
		_, err := ResolveBinary(context.Background(), Options{Release: tag}, same)
		if err == nil || !strings.Contains(err.Error(), tag) {
			t.Errorf("--release %s: err = %v, want a refusal naming the tag", tag, err)
		}
	}
	if len(d.asked) != 0 {
		t.Errorf("a bad tag must not reach the download, got %+v", d.asked)
	}
}

func TestResolveBinaryDownloadsItsOwnVersionForAnotherPlatform(t *testing.T) {
	selfBinary(t)
	d := (&fakeDownloader{}).install(t)
	other := anotherPlatform()

	var log bytes.Buffer
	got, err := ResolveBinary(context.Background(), Options{Version: "v0.4.2", Log: &log}, other)
	if err != nil || got != d.path {
		t.Fatalf("ResolveBinary = %q, %v; want the download %s", got, err, d.path)
	}
	if len(d.asked) != 1 || d.asked[0].Tag != "v0.4.2" || d.asked[0].OS != other.OS || d.asked[0].Arch != other.Arch {
		t.Fatalf("downloads asked for = %+v, want v0.4.2 for %s/%s", d.asked, other.OS, other.Arch)
	}
	if !strings.Contains(log.String(), "shipping release v0.4.2") {
		t.Errorf("log = %q", log.String())
	}
}

func TestResolveBinaryPrefersASiblingToADownload(t *testing.T) {
	dir, _ := selfBinary(t)
	d := (&fakeDownloader{}).install(t)
	other := anotherPlatform()
	sibling := writeSibling(t, dir, other)

	var log bytes.Buffer
	got, err := ResolveBinary(context.Background(), Options{Version: "v0.4.2", Log: &log}, other)
	if err != nil || got != sibling {
		t.Fatalf("ResolveBinary = %q, %v; want the sibling %s", got, err, sibling)
	}
	if len(d.asked) != 0 {
		t.Errorf("a sibling was there, nothing should have been downloaded: %+v", d.asked)
	}
	if !strings.Contains(log.String(), "shipping the sibling "+sibling) {
		t.Errorf("log = %q", log.String())
	}
}

func TestResolveBinaryRefusesADevBuildForAnotherPlatform(t *testing.T) {
	dir, _ := selfBinary(t)
	d := (&fakeDownloader{}).install(t)
	other := anotherPlatform()
	sibling := filepath.Join(dir, SiblingName(other.OS, other.Arch))

	_, err := ResolveBinary(context.Background(), Options{Version: "dev"}, other)
	if err == nil {
		t.Fatal("a dev build must not silently ship anything for another platform")
	}
	for _, want := range []string{"--binary <path>", sibling, "--release <tag>", other.OS + "/" + other.Arch, runtime.GOOS + "/" + runtime.GOARCH} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %v, want it to mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "make build") {
		t.Errorf("refusal names a Makefile target that does not exist: %v", err)
	}
	if len(d.asked) != 0 {
		t.Errorf("a dev version must not be downloaded, got %+v", d.asked)
	}
}

func TestResolveBinaryReportsAFailedDownload(t *testing.T) {
	selfBinary(t)
	(&fakeDownloader{err: errors.New("no network")}).install(t)
	other := anotherPlatform()

	_, err := ResolveBinary(context.Background(), Options{Release: "v0.4.2"}, other)
	if err == nil {
		t.Fatal("a download that failed must not resolve to a binary")
	}
	for _, want := range []string{"v0.4.2", other.OS + "/" + other.Arch, "no network"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %v, want it to mention %q", err, want)
		}
	}
}

func TestResolveBinaryRefusesAPlatformWithNoRelease(t *testing.T) {
	selfBinary(t)
	d := (&fakeDownloader{}).install(t)
	exotic := Probe{OS: "linux", Arch: "riscv64"}
	if Supported(exotic.OS, exotic.Arch) {
		t.Fatalf("%s/%s is a released platform now; pick another for this test", exotic.OS, exotic.Arch)
	}

	for name, o := range map[string]Options{
		"a released version": {Version: "v0.4.2"},
		"--release":          {Release: "v0.4.2"},
		"a dev build":        {Version: "dev"},
	} {
		_, err := ResolveBinary(context.Background(), o, exotic)
		if err == nil {
			t.Fatalf("%s: an unreleased platform with no sibling must be refused", name)
		}
		for _, want := range append([]string{"--binary"}, SupportedPlatforms...) {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: refusal = %v, want it to mention %q", name, err, want)
			}
		}
	}
	if len(d.asked) != 0 {
		t.Errorf("there is no release to download for an unreleased platform, got %+v", d.asked)
	}
}

func TestRunShipsTheBinaryForTheTarget(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "caramelo")
	if err := os.WriteFile(self, []byte("self"), 0o755); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(dir, SiblingName("linux", "riscv64"))
	if err := os.WriteFile(sibling, []byte("cross"), 0o755); err != nil {
		t.Fatal(err)
	}
	executablePath = func() (string, error) { return self, nil }
	t.Cleanup(func() { executablePath = os.Executable })

	want := setup.Report{RunID: "r1", Results: []setup.Result{{Step: "summary", Status: setup.StatusOK}}}
	sh := &fakeShell{t: t, answers: []answer{
		{prefix: "uname -sm", stdout: "Linux riscv64\n0\nroot\nkeys\n"},
		{prefix: "d=$(mktemp", stdout: "/tmp/caramelo-setup.abc\n"},
		{prefix: "/tmp/caramelo-setup.abc/caramelo server setup", stdout: reportJSON(t, want)},
		{prefix: "rm -rf", code: 0},
	}}
	out, err := Run(context.Background(), sh, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Binary != sibling {
		t.Errorf("shipped %q, want %q", out.Binary, sibling)
	}
	if out.Probe.Arch != "riscv64" {
		t.Errorf("probe arch = %q", out.Probe.Arch)
	}
}

func TestTheJoinTicketIsNotInTheRemoteCommandLine(t *testing.T) {
	const ticket = "caramelo-join-v1.c3VwZXItc2VjcmV0"
	o := Options{JoinToken: ticket, SetupArgs: []string{"--join-name=m1"}}
	line := setupLine(o, Probe{Privilege: PrivilegeSudo}, "/var/tmp/caramelo-bootstrap")
	if strings.Contains(line, ticket) {
		t.Fatalf("the setup line carries the ticket: %q", line)
	}
	if !strings.Contains(line, "--join-token -") {
		t.Fatalf("the setup line does not ask for the ticket on stdin: %q", line)
	}
}
