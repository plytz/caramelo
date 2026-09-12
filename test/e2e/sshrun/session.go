//go:build e2e

package sshrun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	DefaultRows = 40
	DefaultCols = 120
	StagingDir  = "/var/tmp"

	closeTimeout = 30 * time.Second
	killDelay    = 2 * time.Second
)

type Session struct {
	Rows int
	Cols int

	host   Host
	dir    string
	opts   []string
	mu     sync.Mutex
	closed bool
}

func Open(ctx context.Context, dir string, h Host) (*Session, error) {
	opts, err := Options(dir, h)
	if err != nil {
		return nil, err
	}
	s := &Session{Rows: DefaultRows, Cols: DefaultCols, host: h, dir: dir, opts: opts}
	if _, err := s.Run(ctx, "true"); err != nil {
		return nil, fmt.Errorf("sshrun: open a session to %s: %w", h.label(), err)
	}
	return s, nil
}

func (s *Session) Host() Host { return s.host }

func (s *Session) Dir() string { return s.dir }

func (s *Session) Options() []string { return append([]string(nil), s.opts...) }

func (s *Session) KnownHostsPath() string { return KnownHostsPath(s.dir, s.host) }

func (s *Session) Run(ctx context.Context, cmd string) (Result, error) {
	return s.run(ctx, ShellCommand(cmd))
}

func (s *Session) RunAs(ctx context.Context, user, cmd string) (Result, error) {
	if user == "" || user == s.host.User {
		return s.Run(ctx, cmd)
	}
	return s.run(ctx, SudoCommand(user, cmd))
}

func (s *Session) RunAsRoot(ctx context.Context, cmd string) (Result, error) {
	return s.RunAs(ctx, "root", cmd)
}

func (s *Session) run(ctx context.Context, remote string) (Result, error) {
	args := append(s.Options(), s.host.Destination(), remote)
	return s.exec(ctx, "ssh", args)
}

func (s *Session) RunTTY(ctx context.Context, env []string, cmd string) (Result, error) {
	rows, cols := s.Rows, s.Cols
	if rows <= 0 {
		rows = DefaultRows
	}
	if cols <= 0 {
		cols = DefaultCols
	}
	var script strings.Builder
	script.WriteString("stty rows " + strconv.Itoa(rows) + " cols " + strconv.Itoa(cols) + " 2>/dev/null; ")
	for _, kv := range env {
		key, value, found := strings.Cut(kv, "=")
		if !found {
			return Result{}, fmt.Errorf("sshrun: %q is not KEY=VALUE", kv)
		}
		script.WriteString("export " + key + "=" + Quote(value) + "; ")
	}
	script.WriteString(cmd)
	args := s.Options()
	args = append(args, "-tt", s.host.Destination(), ShellCommand(script.String()))
	return s.execTTY(ctx, args)
}

func (s *Session) Copy(ctx context.Context, local, remote string) error {
	mode, err := fileMode(local)
	if err != nil {
		return err
	}
	if err := s.scp(ctx, local, s.host.Destination()+":"+remote); err != nil {
		return fmt.Errorf("sshrun: copy %s to %s on %s: %w", local, remote, s.host.label(), err)
	}
	res, err := s.Run(ctx, "chmod "+mode+" "+Quote(remote))
	if err != nil {
		return fmt.Errorf("sshrun: set the mode of %s on %s: %w", remote, s.host.label(), err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("sshrun: set the mode of %s on %s: exit %d: %s", remote, s.host.label(), res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

func (s *Session) CopyAsRoot(ctx context.Context, local, remote string) error {
	mode, err := fileMode(local)
	if err != nil {
		return err
	}
	staged := filepath.Join(StagingDir, "sshrun-"+stageID(local+remote)+"-"+filepath.Base(local))
	if err := s.scp(ctx, local, s.host.Destination()+":"+staged); err != nil {
		return fmt.Errorf("sshrun: stage %s on %s: %w", local, s.host.label(), err)
	}
	install := "install -m " + mode + " " + Quote(staged) + " " + Quote(remote) + "; rc=$?; rm -f " + Quote(staged) + "; exit $rc"
	res, err := s.run(ctx, SudoCommand("root", install))
	if err != nil {
		return fmt.Errorf("sshrun: install %s on %s: %w", remote, s.host.label(), err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("sshrun: install %s on %s: exit %d: %s", remote, s.host.label(), res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

func (s *Session) Fetch(ctx context.Context, remote, local string) error {
	if err := s.scp(ctx, s.host.Destination()+":"+remote, local); err != nil {
		return fmt.Errorf("sshrun: fetch %s from %s: %w", remote, s.host.label(), err)
	}
	return nil
}

func (s *Session) scp(ctx context.Context, from, to string) error {
	args, err := baseOptions(s.dir, s.host, optionSet{control: true})
	if err != nil {
		return err
	}
	args = append(args, "-P", fmt.Sprint(s.host.port()), from, to)
	res, err := s.exec(ctx, "scp", args)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("scp exit %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	path := ControlPath(s.dir, s.host)
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	args := append(s.Options(), "-O", "exit", s.host.Destination())
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	res, err := s.exec(ctx, "ssh", args)
	if _, statErr := os.Stat(path); statErr != nil {
		return nil
	}
	if err := os.Remove(path); err == nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sshrun: close the session to %s: %w", s.host.label(), err)
	}
	return fmt.Errorf("sshrun: close the session to %s: exit %d: %s", s.host.label(), res.ExitCode, firstLine(res.Stderr))
}

func (s *Session) exec(ctx context.Context, bin string, args []string) (Result, error) {
	return runProcess(ctx, s.host, bin, args, false)
}

func (s *Session) execTTY(ctx context.Context, args []string) (Result, error) {
	return runProcess(ctx, s.host, "ssh", args, true)
}

func runProcess(ctx context.Context, h Host, bin string, args []string, terminal bool) (Result, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = killDelay
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	var out, errbuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errbuf
	if terminal {
		cmd.Stderr = io.MultiWriter(&out, &errbuf)
	}
	err := cmd.Run()
	res := Result{Stdout: out.String(), Stderr: errbuf.String()}
	code, err := exitCode(ctx, err)
	if err != nil {
		return res, fmt.Errorf("%s %s: %w", bin, h.label(), err)
	}
	res.ExitCode = code
	return res, classify(h, res)
}

func exitCode(ctx context.Context, err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code, nil
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return -1, fmt.Errorf("%w: %w", ctxErr, err)
	}
	return -1, err
}

func classify(h Host, res Result) error {
	if res.ExitCode != ExitUnreachable {
		return nil
	}
	if HostKeyChanged(res.Stderr) {
		return fmt.Errorf("%s: %w: %w: %s", h.label(), ErrUnreachable, ErrHostKeyChanged, firstLine(res.Stderr))
	}
	return fmt.Errorf("%s: %w: %s", h.label(), ErrUnreachable, firstLine(res.Stderr))
}

func stageID(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

func fileMode(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("sshrun: read the mode of %s: %w", path, err)
	}
	return "0" + strconv.FormatUint(uint64(info.Mode().Perm()), 8), nil
}
