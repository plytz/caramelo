//go:build e2e

package sshrun

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fakeSSH = `#!/bin/sh
if [ -n "$SSHREC" ]; then
	for a in "$@"; do printf '%s\n' "$a" >> "$SSHREC"; done
	printf 'ARGS-END\n' >> "$SSHREC"
fi
if [ -n "$SSH_FAKE_COUNT" ]; then
	n=$(cat "$SSH_FAKE_COUNT" 2>/dev/null || echo 0)
	n=$((n+1))
	echo "$n" > "$SSH_FAKE_COUNT"
	if [ -n "$SSH_FAKE_FAIL_UNTIL" ] && [ "$n" -le "$SSH_FAKE_FAIL_UNTIL" ]; then
		echo 'ssh: connect to host port 22: Connection refused' >&2
		exit 255
	fi
fi
if [ -n "$SSH_FAKE_STDERR" ]; then printf '%s\n' "$SSH_FAKE_STDERR" >&2; fi
if [ -n "$SSH_FAKE_EXIT" ]; then exit "$SSH_FAKE_EXIT"; fi
dest=
cmd=
for a in "$@"; do
	if [ -z "$dest" ]; then
		case "$a" in *@*) dest=$a;; esac
	elif [ -z "$cmd" ]; then
		cmd=$a
	else
		cmd="$cmd $a"
	fi
done
if [ -z "$cmd" ]; then exit 0; fi
/bin/sh -c "$cmd"
`

const fakeSCP = `#!/bin/sh
if [ -n "$SCPREC" ]; then
	for a in "$@"; do printf '%s\n' "$a" >> "$SCPREC"; done
	printf 'ARGS-END\n' >> "$SCPREC"
fi
if [ -n "$SCP_FAKE_EXIT" ]; then exit "$SCP_FAKE_EXIT"; fi
n=$#
i=0
src=
dst=
for a in "$@"; do
	i=$((i+1))
	if [ "$i" -eq "$((n-1))" ]; then src=$a; fi
	if [ "$i" -eq "$n" ]; then dst=$a; fi
done
case "$src" in *:*) src=${src#*:};; esac
case "$dst" in *:*) dst=${dst#*:};; esac
cp "$src" "$dst"
`

const fakeSudo = `#!/bin/sh
while [ $# -gt 0 ]; do
	case "$1" in
		-n) shift;;
		-u) shift 2;;
		*) break;;
	esac
done
exec "$@"
`

type fakes struct {
	dir    string
	sshRec string
	scpRec string
}

func newFakes(t *testing.T) *fakes {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("make the fake bin: %v", err)
	}
	for name, body := range map[string]string{"ssh": fakeSSH, "scp": fakeSCP, "sudo": fakeSudo} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write the fake %s: %v", name, err)
		}
	}
	f := &fakes{dir: dir, sshRec: filepath.Join(dir, "ssh.args"), scpRec: filepath.Join(dir, "scp.args")}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin:/bin")
	t.Setenv("SSHREC", f.sshRec)
	t.Setenv("SCPREC", f.scpRec)
	return f
}

func (f *fakes) calls(t *testing.T, rec string) [][]string {
	t.Helper()
	body, err := os.ReadFile(rec)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read %s: %v", rec, err)
	}
	var out [][]string
	var call []string
	for _, line := range strings.Split(string(body), "\n") {
		if line == "ARGS-END" {
			out = append(out, call)
			call = nil
			continue
		}
		if line == "" {
			continue
		}
		call = append(call, line)
	}
	return out
}

func (f *fakes) sshCalls(t *testing.T) [][]string { return f.calls(t, f.sshRec) }

func (f *fakes) scpCalls(t *testing.T) [][]string { return f.calls(t, f.scpRec) }

func lastRemote(t *testing.T, calls [][]string) string {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("the fake ssh was never called")
	}
	call := calls[len(calls)-1]
	return call[len(call)-1]
}

func openFake(t *testing.T, f *fakes) *Session {
	t.Helper()
	s, err := Open(context.Background(), f.dir, testHost(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenChecksTheConnectionOnce(t *testing.T) {
	f := newFakes(t)
	openFake(t, f)
	calls := f.sshCalls(t)
	if len(calls) != 1 {
		t.Fatalf("Open made %d calls, want 1", len(calls))
	}
	if got := lastRemote(t, calls); got != "sh -c 'true'" {
		t.Errorf("Open ran %q", got)
	}
}

func TestOpenFailsWhenTheMachineIsUnreachable(t *testing.T) {
	f := newFakes(t)
	t.Setenv("SSH_FAKE_EXIT", "255")
	_, err := Open(context.Background(), f.dir, testHost(t))
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Open returned %v, want ErrUnreachable", err)
	}
}

func TestRunGoesThroughSHDashC(t *testing.T) {
	f := newFakes(t)
	s := openFake(t, f)
	res, err := s.Run(context.Background(), "echo hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stdout != "hello\n" || res.ExitCode != 0 {
		t.Fatalf("Run gave %+v", res)
	}
	if got := lastRemote(t, f.sshCalls(t)); got != "sh -c 'echo hello'" {
		t.Errorf("the remote command was %q", got)
	}
}

func TestRunSurvivesTheShellHop(t *testing.T) {
	f := newFakes(t)
	s := openFake(t, f)
	work := t.TempDir()
	for _, name := range []string{"one.txt", "two.txt"} {
		if err := os.WriteFile(filepath.Join(work, name), nil, 0o644); err != nil {
			t.Fatalf("write the fixture: %v", err)
		}
	}
	for _, cmd := range []string{
		`printf '%s\n' "a  b"`,
		`echo "it's here"`,
		`V=set; echo "$V"`,
		`echo $HOME`,
		`cd ` + work + ` && echo *.txt`,
		`cd ` + work + ` && ls | wc -l`,
		`echo 'a|b;c&d' | tr 'a-z' 'A-Z'`,
		`printf '%s\n' "$(echo nested)"`,
	} {
		want, err := exec.Command("/bin/sh", "-c", cmd).Output()
		if err != nil {
			t.Fatalf("local sh with %q: %v", cmd, err)
		}
		res, err := s.Run(context.Background(), cmd)
		if err != nil {
			t.Fatalf("Run %q: %v", cmd, err)
		}
		if res.Stdout != string(want) {
			t.Errorf("Run %q gave %q, local sh gave %q", cmd, res.Stdout, string(want))
		}
	}
}

func TestRunReturnsTheRemoteExitCode(t *testing.T) {
	f := newFakes(t)
	s := openFake(t, f)
	res, err := s.Run(context.Background(), "exit 7")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("the exit code is %d, want 7", res.ExitCode)
	}
}

func TestExit255IsUnreachableAndNotAnAnswer(t *testing.T) {
	f := newFakes(t)
	s := openFake(t, f)
	t.Setenv("SSH_FAKE_EXIT", "255")
	t.Setenv("SSH_FAKE_STDERR", "ssh: connect to host port 22: Connection refused")
	res, err := s.Run(context.Background(), "true")
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Run returned %v, want ErrUnreachable", err)
	}
	if errors.Is(err, ErrHostKeyChanged) {
		t.Error("a refused connection was reported as a changed host key")
	}
	if res.ExitCode != ExitUnreachable {
		t.Errorf("the exit code is %d", res.ExitCode)
	}
}

func TestAChangedHostKeyIsItsOwnError(t *testing.T) {
	f := newFakes(t)
	s := openFake(t, f)
	t.Setenv("SSH_FAKE_EXIT", "255")
	t.Setenv("SSH_FAKE_STDERR", "WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!")
	_, err := s.Run(context.Background(), "true")
	if !errors.Is(err, ErrHostKeyChanged) || !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Run returned %v, want both ErrHostKeyChanged and ErrUnreachable", err)
	}
}

func TestRunAsUsesSudo(t *testing.T) {
	f := newFakes(t)
	s := openFake(t, f)
	if _, err := s.RunAsRoot(context.Background(), "id -u"); err != nil {
		t.Fatalf("RunAsRoot: %v", err)
	}
	if got := lastRemote(t, f.sshCalls(t)); got != "sudo -n sh -c 'id -u'" {
		t.Errorf("the root command was %q", got)
	}
	if _, err := s.RunAs(context.Background(), "caramelo", "id -un"); err != nil {
		t.Fatalf("RunAs: %v", err)
	}
	if got := lastRemote(t, f.sshCalls(t)); got != "sudo -n -u 'caramelo' sh -c 'id -un'" {
		t.Errorf("the user command was %q", got)
	}
	if _, err := s.RunAs(context.Background(), "debian", "id -un"); err != nil {
		t.Fatalf("RunAs the login user: %v", err)
	}
	if got := lastRemote(t, f.sshCalls(t)); got != "sh -c 'id -un'" {
		t.Errorf("the login user went through sudo: %q", got)
	}
}

func TestRunTTYAsksForATerminalAndSetsItsSize(t *testing.T) {
	f := newFakes(t)
	s := openFake(t, f)
	s.Rows, s.Cols = 24, 100
	if _, err := s.RunTTY(context.Background(), []string{"CARAMELO_HOME=/home/it's"}, "tput cols"); err != nil {
		t.Fatalf("RunTTY: %v", err)
	}
	call := f.sshCalls(t)
	last := call[len(call)-1]
	if !contains(last, "-tt") {
		t.Errorf("ssh was not asked for a terminal: %v", last)
	}
	remote := last[len(last)-1]
	for _, want := range []string{"stty rows 24 cols 100", `export CARAMELO_HOME=`, "tput cols"} {
		if !strings.Contains(remote, want) {
			t.Errorf("the remote command %q is missing %q", remote, want)
		}
	}
}

func TestRunTTYRefusesAMalformedEnvironment(t *testing.T) {
	f := newFakes(t)
	s := openFake(t, f)
	if _, err := s.RunTTY(context.Background(), []string{"NOTKEYVALUE"}, "true"); err == nil {
		t.Error("a malformed environment entry was accepted")
	}
}

func TestCopyPreservesTheLocalMode(t *testing.T) {
	f := newFakes(t)
	s := openFake(t, f)
	local := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(local, []byte("payload\n"), 0o741); err != nil {
		t.Fatalf("write the payload: %v", err)
	}
	remote := filepath.Join(t.TempDir(), "landed")
	if err := os.WriteFile(remote, nil, 0o600); err != nil {
		t.Fatalf("write the destination: %v", err)
	}
	if err := s.Copy(context.Background(), local, remote); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	info, err := os.Stat(remote)
	if err != nil {
		t.Fatalf("stat the destination: %v", err)
	}
	if info.Mode().Perm() != 0o741 {
		t.Errorf("the mode is %o, want 741", info.Mode().Perm())
	}
	body, err := os.ReadFile(remote)
	if err != nil || string(body) != "payload\n" {
		t.Fatalf("the destination holds %q (%v)", string(body), err)
	}
	calls := f.scpCalls(t)
	if len(calls) != 1 {
		t.Fatalf("scp ran %d times", len(calls))
	}
	last := calls[0]
	if last[len(last)-1] != "debian@198.51.100.7:"+remote {
		t.Errorf("scp wrote to %q", last[len(last)-1])
	}
	if !contains(last, "-P") {
		t.Errorf("scp was not given a port: %v", last)
	}
}

func TestCopyAsRootStagesAndInstalls(t *testing.T) {
	f := newFakes(t)
	s := openFake(t, f)
	local := filepath.Join(t.TempDir(), "caramelo")
	if err := os.WriteFile(local, []byte("binary\n"), 0o755); err != nil {
		t.Fatalf("write the payload: %v", err)
	}
	installed := filepath.Join(t.TempDir(), "caramelo")
	if err := s.CopyAsRoot(context.Background(), local, installed); err != nil {
		t.Fatalf("CopyAsRoot: %v", err)
	}
	info, err := os.Stat(installed)
	if err != nil {
		t.Fatalf("stat the installed file: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("the installed mode is %o, want 755", info.Mode().Perm())
	}
	calls := f.scpCalls(t)
	if len(calls) != 1 {
		t.Fatalf("scp ran %d times", len(calls))
	}
	dst := calls[0][len(calls[0])-1]
	if !strings.HasPrefix(dst, "debian@198.51.100.7:"+StagingDir+"/") {
		t.Fatalf("scp staged at %q", dst)
	}
	remote := lastRemote(t, f.sshCalls(t))
	for _, want := range []string{"sudo -n sh -c", "install -m 0755", installed, "rm -f"} {
		if !strings.Contains(remote, want) {
			t.Errorf("the install command %q is missing %q", remote, want)
		}
	}
}

func TestFetchBringsTheFileBack(t *testing.T) {
	f := newFakes(t)
	s := openFake(t, f)
	remote := filepath.Join(t.TempDir(), "far")
	if err := os.WriteFile(remote, []byte("far\n"), 0o644); err != nil {
		t.Fatalf("write the far file: %v", err)
	}
	local := filepath.Join(t.TempDir(), "near")
	if err := s.Fetch(context.Background(), remote, local); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	body, err := os.ReadFile(local)
	if err != nil || string(body) != "far\n" {
		t.Fatalf("the local copy holds %q (%v)", string(body), err)
	}
	calls := f.scpCalls(t)
	if calls[0][len(calls[0])-2] != "debian@198.51.100.7:"+remote {
		t.Errorf("scp read from %q", calls[0][len(calls[0])-2])
	}
}

func TestScpFailureIsReported(t *testing.T) {
	f := newFakes(t)
	s := openFake(t, f)
	t.Setenv("SCP_FAKE_EXIT", "1")
	local := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(local, nil, 0o644); err != nil {
		t.Fatalf("write the payload: %v", err)
	}
	if err := s.Copy(context.Background(), local, "/var/tmp/payload"); err == nil {
		t.Error("a failed scp was swallowed")
	}
}

func TestWaitForSSHRetriesWhileTheMachineBoots(t *testing.T) {
	f := newFakes(t)
	t.Setenv("SSH_FAKE_COUNT", filepath.Join(f.dir, "count"))
	t.Setenv("SSH_FAKE_FAIL_UNTIL", "3")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := WaitForSSH(ctx, f.dir, testHost(t), 10*time.Millisecond); err != nil {
		t.Fatalf("WaitForSSH: %v", err)
	}
	if n := len(f.sshCalls(t)); n != 4 {
		t.Errorf("WaitForSSH made %d attempts, want 4", n)
	}
	call := f.sshCalls(t)[0]
	if optionValue(call, "ControlPath") != "none" || optionValue(call, "ControlMaster") != "no" {
		t.Errorf("the probe reused a control socket: %v", call)
	}
}

func TestWaitForSSHStopsOnAChangedHostKey(t *testing.T) {
	f := newFakes(t)
	t.Setenv("SSH_FAKE_EXIT", "255")
	t.Setenv("SSH_FAKE_STDERR", "WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := WaitForSSH(ctx, f.dir, testHost(t), 10*time.Millisecond)
	if !errors.Is(err, ErrHostKeyChanged) {
		t.Fatalf("WaitForSSH returned %v, want ErrHostKeyChanged", err)
	}
	if n := len(f.sshCalls(t)); n != 1 {
		t.Errorf("WaitForSSH kept waiting: %d attempts", n)
	}
}

func TestWaitForSSHGivesUpWithTheLastAttempt(t *testing.T) {
	f := newFakes(t)
	t.Setenv("SSH_FAKE_EXIT", "255")
	t.Setenv("SSH_FAKE_STDERR", "ssh: connect to host port 22: Connection refused")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := WaitForSSH(ctx, f.dir, testHost(t), 10*time.Millisecond)
	if err == nil {
		t.Fatal("WaitForSSH came back green")
	}
	if !strings.Contains(err.Error(), "Connection refused") || !strings.Contains(err.Error(), "m1") {
		t.Errorf("the error does not say what happened: %v", err)
	}
}

func TestRunCarriesTheCallersDeadline(t *testing.T) {
	f := newFakes(t)
	s := openFake(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.Run(ctx, "sleep 30")
	if err == nil {
		t.Fatal("a command past the deadline came back green")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the deadline took %s to bite", time.Since(start))
	}
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
