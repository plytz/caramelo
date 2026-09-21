package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/setup"
)

type Options struct {
	Binary string

	Release string

	Version string

	AuthorizedKeys string

	SetupArgs []string

	JoinToken string

	Peer setup.PeerSpec

	Log          io.Writer
	RemoteStderr io.Writer
}

type Probe struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
	UID  int    `json:"uid"`

	Privilege string `json:"privilege"`

	HasAuthorizedKeys bool `json:"has_authorized_keys"`
}

type Outcome struct {
	Probe Probe `json:"probe"`

	Binary string `json:"binary"`

	Dir string `json:"dir"`

	Report setup.Report `json:"setup"`

	ExitCode int `json:"exit_code"`
}

const (
	PrivilegeRoot         = "root"
	PrivilegeSudo         = "sudo"
	PrivilegeSudoPassword = "sudo-password"
	PrivilegeNoSudo       = "no-sudo"
)

func Run(ctx context.Context, sh Shell, o Options) (Outcome, error) {
	if o.Log == nil {
		o.Log = io.Discard
	}
	if o.RemoteStderr == nil {
		o.RemoteStderr = io.Discard
	}
	var out Outcome

	probe, err := probeTarget(ctx, sh)
	if err != nil {
		return out, err
	}
	out.Probe = probe
	if err := checkProbe(probe); err != nil {
		return out, err
	}
	binary, err := ResolveBinary(ctx, o, probe)
	if err != nil {
		return out, err
	}
	o.Binary = binary
	out.Binary = binary
	fmt.Fprintf(o.Log, "[bootstrap] target: %s/%s, %s\n", probe.OS, probe.Arch, privilegeText(probe.Privilege))

	dir, err := ship(ctx, sh, o)
	if err != nil {
		return out, err
	}
	out.Dir = dir
	defer func() {

		if code, err := sh.Run(ctx, Cmd{Line: "rm -rf " + remote.Quote(dir)}); err != nil || code != 0 {
			fmt.Fprintf(o.Log, "[bootstrap] warning: could not remove %s on the target\n", dir)
		}
	}()

	if !o.Peer.Empty() {
		if err := o.Peer.Validate(); err != nil {
			return out, err
		}
	}

	report, code, err := runSetup(ctx, sh, o, probe, dir)
	if err != nil {
		return out, err
	}
	out.Report, out.ExitCode = report, code
	return out, nil
}

const probeScript = `uname -sm; id -u; ` +
	`if [ "$(id -u)" = 0 ]; then echo root; ` +
	`elif ! command -v sudo >/dev/null 2>&1; then echo no-sudo; ` +
	`elif sudo -n true >/dev/null 2>&1; then echo sudo; ` +
	`else echo sudo-password; fi; ` +
	`if [ -s "$HOME/.ssh/authorized_keys" ]; then echo keys; else echo no-keys; fi`

func probeTarget(ctx context.Context, sh Shell) (Probe, error) {
	var stdout, stderr bytes.Buffer
	code, err := sh.Run(ctx, Cmd{Line: probeScript, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		return Probe{}, err
	}
	if code != 0 {
		return Probe{}, fmt.Errorf("probing the target: exit %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return parseProbe(stdout.String())
}

func parseProbe(s string) (Probe, error) {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) != 4 {
		return Probe{}, fmt.Errorf("probing the target: unexpected answer %q (is the login shell a POSIX sh?)", strings.TrimSpace(s))
	}
	var p Probe
	fields := strings.Fields(lines[0])
	if len(fields) != 2 {
		return Probe{}, fmt.Errorf("probing the target: unexpected uname output %q", lines[0])
	}
	p.OS, p.Arch = strings.ToLower(fields[0]), goArch(fields[1])
	if !probeWord.MatchString(p.OS) || !probeWord.MatchString(p.Arch) {
		return Probe{}, fmt.Errorf("probing the target: unexpected uname output %q: the system and machine names must be plain words of letters, digits and underscore, so this answer names no platform caramelo can ship a binary to; run `uname -sm` on the target to see what it answers, and ship a binary of your own with --binary <path> if the target is sound", lines[0])
	}
	uid, err := strconv.Atoi(strings.TrimSpace(lines[1]))
	if err != nil {
		return Probe{}, fmt.Errorf("probing the target: unexpected uid %q", lines[1])
	}
	p.UID = uid
	p.Privilege = strings.TrimSpace(lines[2])
	p.HasAuthorizedKeys = strings.TrimSpace(lines[3]) == "keys"
	return p, nil
}

var probeWord = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func goArch(m string) string {
	switch m {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	return m
}

var executablePath = os.Executable

var downloadRelease = Download

func SiblingName(os, arch string) string {
	return fmt.Sprintf("caramelo-%s-%s", os, arch)
}

func ResolveBinary(ctx context.Context, o Options, p Probe) (string, error) {
	log := o.Log
	if log == nil {
		log = io.Discard
	}
	if o.Binary != "" {
		if _, err := os.Stat(o.Binary); err != nil {
			return "", fmt.Errorf("--binary %s: %w", o.Binary, err)
		}
		fmt.Fprintf(log, "[bootstrap] shipping the binary given with --binary: %s\n", o.Binary)
		return o.Binary, nil
	}
	if o.Release != "" {
		if !IsReleaseTag(o.Release) {
			return "", fmt.Errorf("--release %s: not a release tag; give one of the form v1.2.3 as published at https://github.com/%s/releases", o.Release, ReleaseRepository)
		}
		if !Supported(p.OS, p.Arch) {
			return "", unsupportedPlatformError(p)
		}
		return shipRelease(ctx, o.Release, p, log)
	}
	self, err := executablePath()
	if err != nil {
		return "", fmt.Errorf("locate this binary: %w", err)
	}
	if p.OS == runtime.GOOS && p.Arch == runtime.GOARCH {
		fmt.Fprintf(log, "[bootstrap] shipping this binary (same platform)\n")
		return self, nil
	}
	sibling := filepath.Join(filepath.Dir(self), SiblingName(p.OS, p.Arch))
	if _, err := os.Stat(sibling); err == nil {
		fmt.Fprintf(log, "[bootstrap] shipping the sibling %s\n", sibling)
		return sibling, nil
	}
	if !Supported(p.OS, p.Arch) {
		return "", unsupportedPlatformError(p)
	}
	if IsReleaseTag(o.Version) {
		return shipRelease(ctx, o.Version, p, log)
	}
	return "", fmt.Errorf("the target is %s/%s, this binary is %s/%s and its version %s is not a release, so there is nothing to ship; build one for the target and pass it with --binary <path>, put it beside this binary as %s, or download a published one with --release <tag>",
		p.OS, p.Arch, runtime.GOOS, runtime.GOARCH, versionText(o.Version), sibling)
}

func versionText(v string) string {
	if v == "" {
		return "dev"
	}
	return v
}

func unsupportedPlatformError(p Probe) error {
	return fmt.Errorf("the target is %s/%s and caramelo publishes no release for it; the released platforms are %s. Build a binary for the target yourself and ship it with --binary <path>, or put it beside this binary as %s",
		p.OS, p.Arch, strings.Join(SupportedPlatforms, ", "), SiblingName(p.OS, p.Arch))
}

func shipRelease(ctx context.Context, tag string, p Probe, log io.Writer) (string, error) {
	fmt.Fprintf(log, "[bootstrap] shipping release %s for %s/%s\n", tag, p.OS, p.Arch)
	path, err := downloadRelease(ctx, Release{Tag: tag, OS: p.OS, Arch: p.Arch, Log: log})
	if err != nil {
		return "", fmt.Errorf("getting the %s release of caramelo for %s/%s: %w", tag, p.OS, p.Arch, err)
	}
	return path, nil
}

func checkProbe(p Probe) error {
	switch p.Privilege {
	case PrivilegeRoot, PrivilegeSudo:
		return nil
	case PrivilegeSudoPassword:
		return fmt.Errorf("the target user needs a password for sudo; setup runs over a pipe and cannot answer prompts. Log in as root (--target root@host) or allow passwordless sudo for this user (echo '%s' | sudo tee /etc/sudoers.d/caramelo-setup)", "USER ALL=(ALL) NOPASSWD: ALL")
	case PrivilegeNoSudo:
		return fmt.Errorf("the target has no sudo and the login user is not root; log in as root (--target root@host) or install sudo")
	}
	return fmt.Errorf("probing the target: unexpected privilege answer %q", p.Privilege)
}

func privilegeText(p string) string {
	if p == PrivilegeRoot {
		return "logged in as root"
	}
	return "passwordless sudo"
}

const shipScript = `d=$(mktemp -d /tmp/caramelo-setup.XXXXXX) && cat >"$d/caramelo" && chmod 0755 "$d/caramelo" && printf '%s\n' "$d"`

func ship(ctx context.Context, sh Shell, o Options) (string, error) {
	f, err := os.Open(o.Binary)
	if err != nil {
		return "", fmt.Errorf("open the binary to ship: %w", err)
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err == nil {
		fmt.Fprintf(o.Log, "[bootstrap] shipping %s (%d MB)\n", o.Binary, (fi.Size()+512*1024)/(1024*1024))
	}
	var stdout, stderr bytes.Buffer
	code, err := sh.Run(ctx, Cmd{Line: shipScript, Stdin: f, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(stdout.String())
	if code != 0 || !strings.HasPrefix(dir, "/tmp/caramelo-setup.") {
		return "", fmt.Errorf("copying the binary to the target: exit %d: %s", code, strings.TrimSpace(stderr.String()))
	}

	if o.AuthorizedKeys != "" {
		keys, err := os.Open(o.AuthorizedKeys)
		if err != nil {
			return dir, fmt.Errorf("open the authorized keys file: %w", err)
		}
		defer func() { _ = keys.Close() }()
		stderr.Reset()
		line := "cat >" + remote.Quote(dir+"/authorized_keys")
		code, err := sh.Run(ctx, Cmd{Line: line, Stdin: keys, Stderr: &stderr})
		if err != nil {
			return dir, err
		}
		if code != 0 {
			return dir, fmt.Errorf("copying the authorized keys to the target: exit %d: %s", code, strings.TrimSpace(stderr.String()))
		}
	}
	return dir, nil
}

func setupLine(o Options, p Probe, dir string) string {
	var b strings.Builder
	if p.Privilege != PrivilegeRoot {
		b.WriteString("sudo -n ")
	}
	b.WriteString(remote.Quote(dir + "/caramelo"))
	b.WriteString(" fleet setup --yes --json")
	for _, a := range o.SetupArgs {
		b.WriteString(" " + remote.Quote(a))
	}
	if o.JoinToken != "" {
		b.WriteString(" --join-token -")
	}
	if !o.Peer.Empty() {
		b.WriteString(" --peer " + remote.Quote(o.Peer.String()))
	}
	switch {
	case o.AuthorizedKeys != "":
		b.WriteString(" --authorized-keys " + remote.Quote(dir+"/authorized_keys"))
	case p.HasAuthorizedKeys:
		b.WriteString(` --authorized-keys "$HOME/.ssh/authorized_keys"`)
	}
	return b.String()
}

func runSetup(ctx context.Context, sh Shell, o Options, p Probe, dir string) (setup.Report, int, error) {
	fmt.Fprintf(o.Log, "[bootstrap] running fleet setup on the target\n")
	var stdout bytes.Buffer
	var stdin io.Reader
	if o.JoinToken != "" {
		stdin = strings.NewReader(o.JoinToken)
	}
	code, err := sh.Run(ctx, Cmd{
		Line: setupLine(o, p, dir), Stdin: stdin, Stdout: &stdout, Stderr: o.RemoteStderr,
	})
	if err != nil {
		return setup.Report{}, code, err
	}
	var report setup.Report
	raw := strings.TrimSpace(stdout.String())
	if raw == "" || json.Unmarshal([]byte(raw), &report) != nil {
		if len(raw) > 512 {
			raw = raw[len(raw)-512:]
		}
		return setup.Report{}, code, fmt.Errorf("fleet setup on the target exited %d without a report (stdout: %q)", code, raw)
	}
	return report, code, nil
}
