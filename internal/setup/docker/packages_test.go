package dockersetup

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/setup"
)

const debianOSRelease = `PRETTY_NAME="Debian GNU/Linux 13 (trixie)"
NAME="Debian GNU/Linux"
VERSION_ID="13"
VERSION="13 (trixie)"
VERSION_CODENAME=trixie
ID=debian
`

const ubuntuOSRelease = `PRETTY_NAME="Ubuntu 24.04.1 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
VERSION="24.04.1 LTS (Noble Numbat)"
VERSION_CODENAME=noble
ID=ubuntu
ID_LIKE=debian
UBUNTU_CODENAME=noble
`

func TestSourcesLine(t *testing.T) {
	tests := []struct {
		name, osRelease, arch, want string
	}{
		{
			name: "debian trixie amd64", osRelease: debianOSRelease, arch: "amd64",
			want: "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian trixie stable\n",
		},
		{
			name: "ubuntu noble arm64", osRelease: ubuntuOSRelease, arch: "arm64",
			want: "deb [arch=arm64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			osr := parseOSRelease(tc.osRelease)
			if err := osr.validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			if got := sourcesLine(osr, tc.arch); got != tc.want {
				t.Errorf("sources line:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestOSReleaseUbuntuFallsBackToUbuntuCodename(t *testing.T) {
	osr := parseOSRelease(strings.ReplaceAll(ubuntuOSRelease, "VERSION_CODENAME=noble\n", ""))
	if osr.Codename != "noble" {
		t.Errorf("codename = %q, want noble (from UBUNTU_CODENAME)", osr.Codename)
	}
	if osr.keyURL() != "https://download.docker.com/linux/ubuntu/gpg" {
		t.Errorf("key URL = %q", osr.keyURL())
	}
}

func TestOSReleaseValidateRejectsOtherDistros(t *testing.T) {
	if err := parseOSRelease("ID=fedora\nVERSION_CODENAME=x\n").validate(); err == nil {
		t.Fatal("fedora accepted")
	}
	if err := parseOSRelease("ID=debian\n").validate(); err == nil {
		t.Fatal("missing codename accepted")
	}
}

func TestParseDpkgStatus(t *testing.T) {
	tests := []struct {
		out           string
		wantInstalled bool
		wantVersion   string
	}{
		{"install ok installed 5:29.8.0-1~debian.13~trixie", true, "5:29.8.0-1~debian.13~trixie"},
		{"install ok installed 1:4.17.4-2", true, "1:4.17.4-2"},
		{"deinstall ok config-files 5:29.8.0-1~debian.13~trixie", false, ""},
		{"install ok half-configured 5:29.8.0", false, ""},
		{"", false, ""},
		{"dpkg-query: no packages found matching nosuchpkg", false, ""},
	}
	for _, tc := range tests {
		got := parseDpkgStatus("docker-ce", tc.out)
		if got.Installed != tc.wantInstalled || got.Version != tc.wantVersion {
			t.Errorf("parseDpkgStatus(%q) = %+v, want installed=%v version=%q",
				tc.out, got, tc.wantInstalled, tc.wantVersion)
		}
	}
}

func TestUpstreamVersion(t *testing.T) {
	for in, want := range map[string]string{
		"5:29.8.0-1~debian.13~trixie": "29.8.0",
		"1:4.17.4-2":                  "4.17.4",
		"29.8.0":                      "29.8.0",
		"":                            "",
	} {
		if got := upstreamVersion(in); got != want {
			t.Errorf("upstreamVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func installedRunner() *fakeRunner {
	f := &fakeRunner{}
	f.on("dpkg-query -W -f ${Status} ${Version} docker-ce", ok("install ok installed 5:29.8.0-1~debian.13~trixie"))
	f.on("dpkg-query", ok("install ok installed 1.2.3-1"))
	f.on("systemctl is-enabled", fail(1, ""))
	f.on("systemctl is-active", fail(3, ""))
	f.rules[len(f.rules)-2].res.Stdout = "disabled\n"
	f.rules[len(f.rules)-1].res.Stdout = "inactive\n"
	return f
}

func TestPackagesCheckDone(t *testing.T) {
	f := installedRunner()
	done, detail, err := Packages().Check(context.Background(), testEnv(f))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !done {
		t.Fatalf("not done: %s", detail)
	}
	if detail != "docker-ce 29.8.0" {
		t.Errorf("detail = %q, want %q", detail, "docker-ce 29.8.0")
	}
}

func TestPackagesCheckMissingPackages(t *testing.T) {
	f := installedRunner()

	f.rules = append([]rule{
		{match: "dpkg-query -W -f ${Status} ${Version} slirp4netns", res: fail(1, "no packages found")},
		{match: "dpkg-query -W -f ${Status} ${Version} uidmap", res: fail(1, "no packages found")},
	}, f.rules...)
	done, detail, err := Packages().Check(context.Background(), testEnv(f))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("reported done with packages missing")
	}
	if !strings.Contains(detail, "uidmap") || !strings.Contains(detail, "slirp4netns") {
		t.Errorf("detail = %q, want the missing packages", detail)
	}
}

func TestPackagesCheckRootfulStillActive(t *testing.T) {
	f := installedRunner()
	f.rules = append([]rule{
		{match: "systemctl is-enabled docker.socket", res: ok("enabled\n")},
		{match: "systemctl is-active containerd.service", res: ok("active\n")},
	}, f.rules...)
	done, detail, err := Packages().Check(context.Background(), testEnv(f))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("reported done with the rootful daemon running")
	}
	if !strings.Contains(detail, "docker.socket") || !strings.Contains(detail, "containerd.service") {
		t.Errorf("detail = %q, want both rootful units", detail)
	}
}

func TestPackagesCheckSkippedWhenInstallDisabled(t *testing.T) {
	f := &fakeRunner{}
	env := testEnv(f)
	env.Opts.InstallPackages = false
	_, _, err := Packages().Check(context.Background(), env)
	var skip setup.Skip
	if !errors.As(err, &skip) {
		t.Fatalf("err = %v, want setup.Skip", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %d commands while skipped", len(f.calls))
	}
}

type fakeFS struct {
	files map[string][]byte
	modes map[string]os.FileMode
	dirs  []string
}

func newFakeFS() *fakeFS {
	return &fakeFS{files: map[string][]byte{}, modes: map[string]os.FileMode{}}
}

func (fs *fakeFS) write(path string, data []byte, mode os.FileMode) error {
	fs.files[path] = data
	fs.modes[path] = mode
	return nil
}

func (fs *fakeFS) mkdir(path string, _ os.FileMode) error {
	fs.dirs = append(fs.dirs, path)
	return nil
}

func applyStep(fs *fakeFS, osRelease string, repoExists bool) *packagesStep {
	return &packagesStep{
		readFile:  func(string) ([]byte, error) { return []byte(osRelease), nil },
		writeFile: fs.write,
		mkdirAll:  fs.mkdir,
		exists:    func(string) bool { return repoExists },
		fetch: func(context.Context, string) ([]byte, error) {
			return []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----\nkey\n"), nil
		},
		osReleaseSrc: OSReleaseSrc, keyringPath: KeyringPath, sourcesPath: SourcesPath,
	}
}

func TestPackagesApplySequence(t *testing.T) {
	f := &fakeRunner{}
	f.on("dpkg --print-architecture", ok("amd64\n"))
	fs := newFakeFS()
	if err := applyStep(fs, debianOSRelease, false).Apply(context.Background(), testEnv(f)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	const wait = "apt-get -o DPkg::Lock::Timeout=120 "
	requireOrder(t, f,
		wait+"update",
		wait+"install -y --no-install-recommends ca-certificates curl gnupg",
		wait+"update",
		wait+"install -y --no-install-recommends docker-ce docker-ce-cli containerd.io",
		wait+"clean",
		"systemctl disable --now docker.service docker.socket containerd.service",
	)

	if _, ok := fs.files[KeyringPath]; !ok {
		t.Fatalf("no keyring written; files: %v", fs.files)
	}
	if fs.modes[KeyringPath] != 0o644 {
		t.Errorf("keyring mode = %v, want 0644 (apt reads it as _apt)", fs.modes[KeyringPath])
	}
	want := "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian trixie stable\n"
	if got := string(fs.files[SourcesPath]); got != want {
		t.Errorf("sources file = %q, want %q", got, want)
	}
	if len(fs.dirs) == 0 || fs.dirs[0] != KeyringDir {
		t.Errorf("keyring dir not created: %v", fs.dirs)
	}

	for _, line := range f.log() {
		if strings.Contains(line, " passt") {
			t.Errorf("passt installed: %q", line)
		}
	}

	for _, c := range f.calls {
		if c.Name != "apt-get" {
			continue
		}
		if !strings.Contains(strings.Join(c.Env, " "), "DEBIAN_FRONTEND=noninteractive") {
			t.Errorf("apt-get without DEBIAN_FRONTEND: %v", c)
		}
	}
}

func TestPackagesApplySkipsFirstUpdateWhenRepoConfigured(t *testing.T) {
	f := &fakeRunner{}
	f.on("dpkg --print-architecture", ok("amd64\n"))
	if err := applyStep(newFakeFS(), debianOSRelease, true).Apply(context.Background(), testEnv(f)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var updates int
	for _, line := range f.log() {
		if strings.HasPrefix(line, "apt-get ") && strings.HasSuffix(line, " update") {
			updates++
		}
	}
	if updates != 1 {
		t.Errorf("apt-get update ran %d times, want 1 (repo already configured)", updates)
	}
}

func TestPackagesApplyToleratesMissingRootfulUnits(t *testing.T) {
	f := &fakeRunner{}
	f.on("dpkg --print-architecture", ok("amd64\n"))
	f.on("systemctl disable", fail(1, "Failed to disable unit: Unit file containerd.service does not exist."))
	if err := applyStep(newFakeFS(), debianOSRelease, false).Apply(context.Background(), testEnv(f)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func TestPackagesApplyFailsOnRealSystemctlError(t *testing.T) {
	f := &fakeRunner{}
	f.on("dpkg --print-architecture", ok("amd64\n"))
	f.on("systemctl disable", fail(1, "Failed to connect to bus: Operation not permitted"))
	err := applyStep(newFakeFS(), debianOSRelease, false).Apply(context.Background(), testEnv(f))
	if err == nil {
		t.Fatal("systemctl failure swallowed")
	}
}

func TestPackagesApplyRejectsNonPGPKey(t *testing.T) {
	f := &fakeRunner{}
	f.on("dpkg --print-architecture", ok("amd64\n"))
	s := applyStep(newFakeFS(), debianOSRelease, false)
	s.fetch = func(context.Context, string) ([]byte, error) { return []byte("<html>captive portal</html>"), nil }
	if err := s.Apply(context.Background(), testEnv(f)); err == nil {
		t.Fatal("accepted a key that is not a PGP block")
	}
}
