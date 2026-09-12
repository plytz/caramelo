//go:build integration

package itest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/vpn"
	"github.com/plytz/caramelo/internal/vpnclient"
)

type ConnectSession struct {
	App       string
	Env       string
	Machine   string
	Listeners []vpnclient.Listener

	cmd    *exec.Cmd
	cancel context.CancelFunc
	stderr *bytes.Buffer
}

type connectDoc struct {
	App       string               `json:"app"`
	Env       string               `json:"env"`
	Machine   string               `json:"machine"`
	Listeners []vpnclient.Listener `json:"listeners"`
}

func Connect(ctx context.Context, o ClientOptions, args ...string) (*ConnectSession, error) {
	bin, err := clientBinary(o)
	if err != nil {
		return nil, err
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = ClientTimeout
	}
	runCtx, cancel := context.WithCancel(ctx)

	argv := append([]string{"connect"}, args...)
	argv = append(argv, "--json")
	cmd := exec.CommandContext(runCtx, bin, argv...)
	cmd.Dir = o.Dir
	cmd.Env = o.Env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("caramelo connect: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("caramelo connect: %w", err)
	}
	s := &ConnectSession{cmd: cmd, cancel: cancel, stderr: &stderr}

	type decoded struct {
		doc connectDoc
		err error
	}
	done := make(chan decoded, 1)
	go func() {
		var doc connectDoc
		err := json.NewDecoder(stdout).Decode(&doc)
		done <- decoded{doc, err}
	}()
	select {
	case d := <-done:
		if d.err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("caramelo connect %s: reading the port map: %w\nstderr:\n%s",
				strings.Join(args, " "), d.err, stderr.String())
		}
		s.App, s.Env, s.Machine = d.doc.App, d.doc.Env, d.doc.Machine
		s.Listeners = d.doc.Listeners
		return s, nil
	case <-time.After(timeout):
		_ = s.Close()
		return nil, fmt.Errorf("caramelo connect %s: no port map after %s\nstderr:\n%s",
			strings.Join(args, " "), timeout, stderr.String())
	}
}

func (s *ConnectSession) Local(name string, proto vpn.Protocol) (string, bool) {
	for _, l := range s.Listeners {
		if l.Name == name && l.Protocol == proto {
			return l.Local, true
		}
	}
	return "", false
}

func (s *ConnectSession) Listener(name string, proto vpn.Protocol) (vpnclient.Listener, bool) {
	for _, l := range s.Listeners {
		if l.Name == name && l.Protocol == proto {
			return l, true
		}
	}
	return vpnclient.Listener{}, false
}

func (s *ConnectSession) Stderr() string { return s.stderr.String() }

func (s *ConnectSession) Close() error {
	if s.cmd == nil {
		return nil
	}
	s.cancel()
	_ = s.cmd.Wait()
	s.cmd = nil
	return nil
}

func RedisPing(address string) (string, error) {
	conn, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		return "", fmt.Errorf("dial %s: %w", address, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return "", err
	}
	if _, err := conn.Write([]byte("PING\r\n")); err != nil {
		return "", fmt.Errorf("write to %s: %w", address, err)
	}
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil {
		return "", fmt.Errorf("read from %s: %w", address, err)
	}
	return strings.TrimSpace(string(buf[:n])), nil
}
