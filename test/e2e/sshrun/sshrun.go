//go:build e2e

package sshrun

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Host struct {
	Name    string
	Addr    string
	Port    int
	User    string
	Key     string
	HostKey string
	Arch    string
}

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

var (
	ErrUnreachable    = errors.New("could not reach the machine over ssh")
	ErrHostKeyChanged = errors.New("the host key of the machine changed")
)

const (
	ExitUnreachable  = 255
	ControlPersist   = "60s"
	ConnectTimeout   = "15"
	AliveInterval    = "15"
	AliveCountMax    = "4"
	maxControlPath   = 80
	knownHostsPrefix = "known_hosts."
)

func (h Host) port() int {
	if h.Port == 0 {
		return 22
	}
	return h.Port
}

func (h Host) label() string {
	if h.Name != "" {
		return h.Name
	}
	return h.Addr
}

func (h Host) Destination() string {
	if h.User == "" {
		return h.Addr
	}
	return h.User + "@" + h.Addr
}

func (h Host) validate() error {
	if h.Addr == "" {
		return errors.New("sshrun: the host has no address")
	}
	if h.User == "" {
		return fmt.Errorf("sshrun: the host %s has no user", h.label())
	}
	return nil
}

func (h Host) knownHostsEntry() string {
	name := h.Addr
	if h.port() != 22 {
		name = fmt.Sprintf("[%s]:%d", h.Addr, h.port())
	}
	return name + " " + strings.TrimSpace(h.HostKey) + "\n"
}

func (h Host) fingerprint() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s@%s:%d", h.User, h.Addr, h.port())))
	return hex.EncodeToString(sum[:])[:10]
}

func KnownHostsPath(dir string, h Host) string {
	return filepath.Join(dir, knownHostsPrefix+h.fingerprint())
}

func ControlPath(dir string, h Host) string {
	path := filepath.Join(dir, "cm-"+h.fingerprint())
	if len(path) <= maxControlPath {
		return path
	}
	return filepath.Join(os.TempDir(), "caramelo-e2e-cm-"+h.fingerprint())
}

func WriteKnownHosts(dir string, h Host) (string, error) {
	path := KnownHostsPath(dir, h)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("sshrun: make the run directory %s: %w", dir, err)
	}
	body := ""
	if strings.TrimSpace(h.HostKey) != "" {
		body = h.knownHostsEntry()
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", fmt.Errorf("sshrun: write the known_hosts of %s: %w", h.label(), err)
	}
	return path, nil
}

func ForgetHostKey(dir string, h Host) error {
	path := KnownHostsPath(dir, h)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return fmt.Errorf("sshrun: forget the host key of %s: %w", h.label(), err)
	}
	return nil
}

type optionSet struct {
	control bool
	port    bool
}

func baseOptions(dir string, h Host, set optionSet) ([]string, error) {
	if err := h.validate(); err != nil {
		return nil, err
	}
	known, err := WriteKnownHosts(dir, h)
	if err != nil {
		return nil, err
	}
	strict := "accept-new"
	if strings.TrimSpace(h.HostKey) != "" {
		strict = "yes"
	}
	args := []string{}
	if set.control {
		args = append(args,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+ControlPath(dir, h),
			"-o", "ControlPersist="+ControlPersist,
		)
	} else {
		args = append(args,
			"-o", "ControlMaster=no",
			"-o", "ControlPath=none",
		)
	}
	args = append(args,
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking="+strict,
		"-o", "UserKnownHostsFile="+known,
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout="+ConnectTimeout,
		"-o", "ServerAliveInterval="+AliveInterval,
		"-o", "ServerAliveCountMax="+AliveCountMax,
		"-o", "LogLevel=ERROR",
	)
	if h.Key != "" {
		args = append(args, "-i", h.Key)
	}
	return args, nil
}

func Options(dir string, h Host) ([]string, error) {
	args, err := baseOptions(dir, h, optionSet{control: true})
	if err != nil {
		return nil, err
	}
	return append(args, "-p", fmt.Sprint(h.port())), nil
}

func OptionString(dir string, h Host) (string, error) {
	args, err := Options(dir, h)
	if err != nil {
		return "", err
	}
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		if strings.ContainsAny(a, " \t'\"$*?") {
			quoted = append(quoted, Quote(a))
			continue
		}
		quoted = append(quoted, a)
	}
	return strings.Join(quoted, " "), nil
}

func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func ShellCommand(cmd string) string {
	return "sh -c " + Quote(cmd)
}

func SudoCommand(user, cmd string) string {
	if user == "" || user == "root" {
		return "sudo -n " + ShellCommand(cmd)
	}
	return "sudo -n -u " + Quote(user) + " " + ShellCommand(cmd)
}

func HostKeyChanged(stderr string) bool {
	for _, marker := range []string{
		"REMOTE HOST IDENTIFICATION HAS CHANGED",
		"Host key verification failed",
	} {
		if strings.Contains(stderr, marker) {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "no output"
	}
	if at := strings.IndexByte(s, '\n'); at >= 0 {
		return strings.TrimSpace(s[:at])
	}
	return s
}
