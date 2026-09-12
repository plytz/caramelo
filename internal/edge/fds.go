package edge

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

const (
	listenFDsStart = 3

	envListenPID     = "LISTEN_PID"
	envListenFDs     = "LISTEN_FDS"
	envListenFDNames = "LISTEN_FDNAMES"
)

const (
	DefaultHTTPAddr  = "127.0.0.1:8080"
	DefaultHTTPSAddr = "127.0.0.1:8443"
)

type Listeners struct {
	HTTP net.Listener

	HTTPS net.Listener

	QUIC net.PacketConn

	Inherited bool
}

func (l *Listeners) Names() []string {
	if l == nil {
		return nil
	}
	var out []string
	if l.HTTP != nil {
		out = append(out, "tcp "+l.HTTP.Addr().String())
	}
	if l.HTTPS != nil {
		out = append(out, "tcp "+l.HTTPS.Addr().String())
	}
	if l.QUIC != nil {
		out = append(out, "udp "+l.QUIC.LocalAddr().String())
	}
	return out
}

func (l *Listeners) Close() error {
	if l == nil {
		return nil
	}
	var err error
	if l.HTTP != nil {
		if e := l.HTTP.Close(); e != nil && err == nil {
			err = e
		}
	}
	if l.HTTPS != nil {
		if e := l.HTTPS.Close(); e != nil && err == nil {
			err = e
		}
	}
	if l.QUIC != nil {
		if e := l.QUIC.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

func Inherit() (*Listeners, error) {
	pid := os.Getenv(envListenPID)
	fds := os.Getenv(envListenFDs)
	if pid == "" && fds == "" {
		return nil, nil
	}
	defer func() {

		os.Unsetenv(envListenPID)
		os.Unsetenv(envListenFDs)
		os.Unsetenv(envListenFDNames)
	}()
	if pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return nil, fmt.Errorf("edge: %s is %s but this process is %d: the sockets are not ours",
			envListenPID, pid, os.Getpid())
	}
	n, err := strconv.Atoi(strings.TrimSpace(fds))
	if err != nil {
		return nil, fmt.Errorf("edge: %s %q: %w", envListenFDs, fds, err)
	}
	if n <= 0 {
		return nil, nil
	}
	nums := make([]int, 0, n)
	for i := range n {
		nums = append(nums, listenFDsStart+i)
	}
	return adopt(nums)
}

func adopt(fds []int) (*Listeners, error) {
	out := &Listeners{Inherited: true}
	var streams []net.Listener
	for _, fd := range fds {
		closeOnExec(fd)
		kind, port, err := sockInfo(fd)
		if err != nil {
			out.Close()
			return nil, fmt.Errorf("edge: inherited fd %d: %w", fd, err)
		}
		f := os.NewFile(uintptr(fd), fmt.Sprintf("caramelo-edge-%d", fd))
		if f == nil {
			out.Close()
			return nil, fmt.Errorf("edge: inherited fd %d is not open", fd)
		}
		switch kind {
		case sockStream:
			ln, err := net.FileListener(f)
			f.Close()
			if err != nil {
				out.Close()
				return nil, fmt.Errorf("edge: inherited fd %d (tcp): %w", fd, err)
			}
			switch port {
			case HTTPPort:
				out.HTTP = ln
			case HTTPSPort:
				out.HTTPS = ln
			default:
				streams = append(streams, ln)
			}
		case sockDatagram:
			pc, err := net.FilePacketConn(f)
			f.Close()
			if err != nil {
				out.Close()
				return nil, fmt.Errorf("edge: inherited fd %d (udp): %w", fd, err)
			}
			if out.QUIC != nil {
				pc.Close()
				out.Close()
				return nil, errors.New("edge: more than one datagram socket was passed; only udp/443 is served")
			}
			out.QUIC = pc
		default:
			f.Close()
			out.Close()
			return nil, fmt.Errorf("edge: inherited fd %d: socket type %d is neither stream nor datagram", fd, kind)
		}
	}

	for _, ln := range streams {
		switch {
		case out.HTTP == nil:
			out.HTTP = ln
		case out.HTTPS == nil:
			out.HTTPS = ln
		default:
			ln.Close()
			out.Close()
			return nil, errors.New("edge: more than two stream sockets were passed; only 80 and 443 are served")
		}
	}
	if out.HTTPS == nil {
		out.Close()
		return nil, errors.New("edge: no TLS socket was passed: the edge needs 443")
	}
	return out, nil
}

type BindOptions struct {
	HTTP  string
	HTTPS string

	QUIC bool
}

func Bind(o BindOptions) (*Listeners, error) {
	if o.HTTP == "" {
		o.HTTP = DefaultHTTPAddr
	}
	if o.HTTPS == "" {
		o.HTTPS = DefaultHTTPSAddr
	}
	out := &Listeners{}
	var err error
	if out.HTTP, err = net.Listen("tcp", o.HTTP); err != nil {
		return nil, fmt.Errorf("edge: listen %s: %w", o.HTTP, err)
	}
	if out.HTTPS, err = net.Listen("tcp", o.HTTPS); err != nil {
		out.Close()
		return nil, fmt.Errorf("edge: listen %s: %w", o.HTTPS, err)
	}
	if o.QUIC {

		for attempt := 0; ; attempt++ {
			addr := out.HTTPS.Addr().String()
			pc, err := net.ListenPacket("udp", addr)
			if err == nil {
				out.QUIC = pc
				break
			}
			if !anyPort(o.HTTPS) || attempt >= quicRebindAttempts {
				out.Close()
				return nil, fmt.Errorf("edge: listen udp %s: %w", addr, err)
			}
			if err := out.HTTPS.Close(); err != nil {
				out.Close()
				return nil, fmt.Errorf("edge: close %s to try another port: %w", addr, err)
			}
			if out.HTTPS, err = net.Listen("tcp", o.HTTPS); err != nil {
				out.Close()
				return nil, fmt.Errorf("edge: listen %s: %w", o.HTTPS, err)
			}
		}
	}
	return out, nil
}

const quicRebindAttempts = 8

func anyPort(addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	return err == nil && (port == "0" || port == "")
}
