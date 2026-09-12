//go:build unix

package edge

import (
	"net"
	"os"
	"strconv"
	"syscall"
	"testing"
)

func TestTheEdgeTakesTheSocketsItIsHanded(t *testing.T) {
	plain := listenTCP(t)
	tls := listenTCP(t)
	quic := listenUDP(t)

	ln, err := adopt([]int{fdOf(t, plain), fdOf(t, tls), fdOf(t, quic)})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	defer ln.Close()

	if !ln.Inherited {
		t.Error("Inherited = false for sockets that were handed over")
	}
	if ln.HTTP == nil || ln.HTTPS == nil || ln.QUIC == nil {
		t.Fatalf("adopted %+v, want all three", ln)
	}

	if ln.HTTP.Addr().String() != plain.Addr().String() {
		t.Errorf("HTTP = %s, want %s", ln.HTTP.Addr(), plain.Addr())
	}
	if ln.HTTPS.Addr().String() != tls.Addr().String() {
		t.Errorf("HTTPS = %s, want %s", ln.HTTPS.Addr(), tls.Addr())
	}
	if ln.QUIC.LocalAddr().String() != quic.LocalAddr().String() {
		t.Errorf("QUIC = %s, want %s", ln.QUIC.LocalAddr(), quic.LocalAddr())
	}
	names := ln.Names()
	if len(names) != 3 || names[2][:3] != "udp" {
		t.Errorf("Names() = %v", names)
	}

	conn, err := net.Dial("tcp", ln.HTTPS.Addr().String())
	if err != nil {
		t.Fatalf("dial the adopted socket: %v", err)
	}
	conn.Close()
}

func TestAHandOverWithoutATLSSocketIsRefused(t *testing.T) {
	only := listenTCP(t)

	if _, err := adopt([]int{fdOf(t, only)}); err == nil {
		t.Error("adopt with no 443 socket = nil, want an error: the edge needs 443")
	}
}

func TestInheritSaysNothingWhenSystemdIsNotThere(t *testing.T) {
	t.Setenv(envListenPID, "")
	t.Setenv(envListenFDs, "")
	os.Unsetenv(envListenPID)
	os.Unsetenv(envListenFDs)
	ln, err := Inherit()
	if err != nil || ln != nil {
		t.Fatalf("Inherit outside systemd = %+v, %v, want nil, nil so the process binds its own", ln, err)
	}

	t.Setenv(envListenPID, strconv.Itoa(os.Getpid()+1))
	t.Setenv(envListenFDs, "3")
	if _, err := Inherit(); err == nil {
		t.Error("Inherit with someone else's LISTEN_PID = nil, want an error")
	}

	t.Setenv(envListenPID, strconv.Itoa(os.Getpid()))
	t.Setenv(envListenFDs, "0")
	ln, err = Inherit()
	if err != nil || ln != nil {
		t.Errorf("Inherit with LISTEN_FDS=0 = %+v, %v", ln, err)
	}
}

func TestBindOpensLoopbackSocketsWhenThereIsNoSystemd(t *testing.T) {
	ln, err := Bind(BindOptions{HTTP: "127.0.0.1:0", HTTPS: "127.0.0.1:0", QUIC: true})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	defer ln.Close()
	if ln.Inherited {
		t.Error("Inherited = true for sockets the process opened itself")
	}

	if _, tcpPort, err := net.SplitHostPort(ln.HTTPS.Addr().String()); err != nil {
		t.Fatal(err)
	} else if _, udpPort, err := net.SplitHostPort(ln.QUIC.LocalAddr().String()); err != nil {
		t.Fatal(err)
	} else if tcpPort != udpPort {
		t.Errorf("tcp %s and udp %s: Alt-Svc would send clients to a port nothing answers on", tcpPort, udpPort)
	}
	if err := ln.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func listenTCP(t *testing.T) *net.TCPListener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l.(*net.TCPListener)
}

func listenUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c.(*net.UDPConn)
}

func fdOf(t *testing.T, s interface{ File() (*os.File, error) }) int {
	t.Helper()
	f, err := s.File()
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	return fd
}
