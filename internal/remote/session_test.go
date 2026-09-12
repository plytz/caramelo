package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/ssh"
)

func echoServer(t *testing.T) (addr string, argv *[]string) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	got := []string{}
	srv := &ssh.Server{Handler: func(sess ssh.Session) {
		mu.Lock()
		got = append([]string(nil), sess.Command()...)
		mu.Unlock()
		fmt.Fprintf(sess, "argv=%s\n", strings.Join(sess.Command(), "|"))
		fmt.Fprintln(sess.Stderr(), "on stderr")
		code := 0
		switch sess.Command()[0] {
		case "fail":
			code = 2
		case "transport":
			code = 255
		}
		_ = sess.Exit(code)
	}}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String(), &got
}

type recordingDialer struct {
	network string
	address string
	err     error
}

func (d *recordingDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d.network, d.address = network, address
	if d.err != nil {
		return nil, d.err
	}
	var std net.Dialer
	return std.DialContext(ctx, network, address)
}

func TestConnTransportRun(t *testing.T) {
	addr, argv := echoServer(t)
	dialer := &recordingDialer{}
	tr := &ConnTransport{
		KindName: KindTunnel, Dialer: dialer, Network: "tcp", Address: addr,
		User: "caramelo", Label: "box (10.86.0.1:4022)",
	}

	var stdout, stderr strings.Builder
	code, err := tr.Run(context.Background(), []string{"env", "list", "--app", "my app"}, Streams{Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	if want := "argv=env|list|--app|my app\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	if !strings.Contains(stderr.String(), "on stderr") {
		t.Errorf("stderr = %q, want the command's own stderr", stderr.String())
	}

	if got := *argv; len(got) != 4 || got[3] != "my app" {
		t.Errorf("the server saw %q, want the argument vector back", got)
	}
	if dialer.network != "tcp" || dialer.address != addr {
		t.Errorf("dialled %s %s, want tcp %s", dialer.network, dialer.address, addr)
	}
	if tr.Kind() != KindTunnel {
		t.Errorf("Kind() = %q, want %q", tr.Kind(), KindTunnel)
	}
	if tr.String() != "tunnel box (10.86.0.1:4022)" {
		t.Errorf("String() = %q", tr.String())
	}
}

func TestConnTransportExitCodes(t *testing.T) {
	addr, _ := echoServer(t)
	tr := &ConnTransport{KindName: KindTunnel, Network: "tcp", Address: addr, User: "caramelo"}

	for _, tc := range []struct {
		arg  string
		want int
	}{
		{"ok", 0},
		{"fail", 2},

		{"transport", 1},
	} {
		code, err := tr.Run(context.Background(), []string{tc.arg}, Streams{})
		if err != nil {
			t.Fatalf("%s: %v", tc.arg, err)
		}
		if code != tc.want {
			t.Errorf("%s: exit %d, want %d", tc.arg, code, tc.want)
		}
	}
}

func TestConnTransportDialFailure(t *testing.T) {
	boom := errors.New("no route to host")
	tr := &ConnTransport{
		KindName: KindTunnel,
		Dialer:   &recordingDialer{err: boom},
		Network:  "tcp", Address: "10.86.0.1:4022", User: "caramelo", Label: "box",
		Timeout: time.Second,
	}
	code, err := tr.Run(context.Background(), []string{"status"}, Streams{})
	if err == nil {
		t.Fatal("a dial failure was not reported")
	}
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "box") {
		t.Errorf("err = %v, want the dialer's error and the endpoint", err)
	}
}

func TestConnTransportHandshakeTimeout(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			t.Cleanup(func() { _ = c.Close() })
		}
	}()

	tr := &ConnTransport{
		KindName: KindTunnel, Network: "tcp", Address: ln.Addr().String(),
		User: "caramelo", Timeout: 300 * time.Millisecond,
	}
	done := make(chan error, 1)
	go func() {
		code, err := tr.Run(context.Background(), []string{"status"}, Streams{})
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a silent endpoint was reported as success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run hung on a silent endpoint")
	}
}
