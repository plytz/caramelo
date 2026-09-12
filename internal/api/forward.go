package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/remote"
)

const IdentityEnv = remote.IdentityEnv

const ForwardUser = "caramelo"

type TunnelForwarder struct {
	Dialer remote.Dialer

	User string

	Timeout time.Duration

	Machine string
}

var _ Forwarder = (*TunnelForwarder)(nil)

func (f *TunnelForwarder) Forward(ctx context.Context, loc Location, argv []string,
	stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	switch {
	case loc.Local:
		return 1, fmt.Errorf("forward %v: the location is this machine, which is a bug in the resolver", argv)
	case !loc.Reachable:
		return 1, Unreachable(loc)
	case loc.Address == "":
		return 1, NoAddress(loc)
	case f == nil || f.Dialer == nil:
		return 1, fmt.Errorf("this machine's tunnel is not up, so machine %q cannot be reached: "+
			"`caramelo status` says what the network is doing", loc.Machine)
	}
	user := f.User
	if user == "" {
		user = ForwardUser
	}
	t := &remote.ConnTransport{
		KindName: remote.KindTunnel,
		Dialer:   f.Dialer,
		Network:  "tcp",
		Address:  loc.Address,
		User:     user,
		Label:    loc.Machine + " (" + loc.Address + ")",
		Timeout:  f.Timeout,

		OnBehalfOf: env.IdentityFrom(ctx),
	}

	out := &MachineStamp{W: stdout, Machine: loc.Machine}
	errw := &MachineStamp{W: stderr, Machine: loc.Machine}
	code, err := t.Run(ctx, argv, remote.Streams{Stdin: stdin, Stdout: out, Stderr: errw})
	if err != nil {
		return code, fmt.Errorf("run %q on machine %q: %w", argv[0], loc.Machine, err)
	}
	if ferr := out.Flush(); ferr != nil {
		return code, ferr
	}
	if ferr := errw.Flush(); ferr != nil {
		return code, ferr
	}
	return code, nil
}

type MachineStamp struct {
	W io.Writer

	Machine string

	mu  sync.Mutex
	buf bytes.Buffer
}

var _ io.Writer = (*MachineStamp)(nil)

func (s *MachineStamp) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Write(p)
	for {
		line, err := s.buf.ReadBytes('\n')
		if err != nil {

			s.buf.Write(line)
			break
		}
		if err := s.write(line); err != nil {
			return len(p), err
		}
	}
	if err := s.passPartial(); err != nil {
		return len(p), err
	}
	return len(p), nil
}

const partialLimit = 64 << 10

func (s *MachineStamp) passPartial() error {
	rest := s.buf.Bytes()
	trimmed := bytes.TrimLeft(rest, " \t\r\n")
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '{' && s.buf.Len() <= partialLimit {
		return nil
	}
	out := append([]byte(nil), rest...)
	s.buf.Reset()
	if s.W == nil {
		return nil
	}

	if _, err := s.W.Write(out); err != nil {
		return fmt.Errorf("write a forwarded machine's output: %w", err)
	}
	return nil
}

func (s *MachineStamp) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf.Len() == 0 {
		return nil
	}
	rest := s.buf.Bytes()
	s.buf.Reset()
	return s.write(rest)
}

func (s *MachineStamp) write(line []byte) error {
	if s.W == nil {
		return nil
	}
	out := line
	if stamped, ok := stamp(line, s.Machine); ok {
		out = stamped
	}
	if _, err := s.W.Write(out); err != nil {
		return fmt.Errorf("write a forwarded machine's output: %w", err)
	}
	return nil
}

func stamp(line []byte, machine string) ([]byte, bool) {
	if machine == "" {
		return nil, false
	}
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return nil, false
	}

	if _, ok := raw["action"]; !ok {
		return nil, false
	}
	if _, ok := raw["status"]; !ok {
		return nil, false
	}
	if m, ok := raw["machine"]; ok && len(bytes.Trim(m, `"`)) > 0 {
		return nil, false
	}
	var e progress.Event
	if err := json.Unmarshal(trimmed, &e); err != nil {
		return nil, false
	}
	e.Machine = machine
	encoded, err := json.Marshal(e)
	if err != nil {
		return nil, false
	}
	return append(encoded, '\n'), true
}
