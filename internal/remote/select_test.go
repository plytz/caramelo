package remote

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

type fakeDialer struct{ machine string }

func (fakeDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("fakeDialer does not dial")
}

func (d fakeDialer) MachineAddr() string { return d.machine }

func selection() Selection {
	return Selection{
		SocketPath:   "/run/caramelo/caramelod.sock",
		SocketUser:   "caramelo",
		SocketExists: func(string) bool { return false },
		Tunnel:       func(context.Context, Target) (Dialer, error) { return nil, ErrNoTunnel },
	}
}

func TestSelectOrder(t *testing.T) {
	ctx := context.Background()
	box := Target{User: "caramelo", Host: "box", Port: 4022}

	t.Run("machine local is the socket", func(t *testing.T) {
		s := selection()
		s.Machine = "local"
		s.SocketExists = func(string) bool { return true }
		got, err := Select(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != KindSocket || got.SocketPath != s.SocketPath || got.User != s.SocketUser {
			t.Fatalf("got %+v, want the local socket", got)
		}
	})

	t.Run("an explicit machine beats the local socket", func(t *testing.T) {
		s := selection()
		s.Machine = "box"
		s.SocketExists = func(string) bool { return true }
		got, err := Select(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != KindSSH || got.Target != box {
			t.Fatalf("got %+v, want ssh to %v", got, box)
		}
	})

	t.Run("a local socket beats default_machine", func(t *testing.T) {
		s := selection()
		s.SocketExists = func(string) bool { return true }
		s.Config.DefaultMachine = "box"
		got, err := Select(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != KindSocket {
			t.Fatalf("got %+v, want the local socket", got)
		}
	})

	t.Run("default_machine when there is no socket", func(t *testing.T) {
		s := selection()
		s.Config = ClientConfig{DefaultMachine: "box", Machines: map[string]string{"box": "alex@10.0.0.5:4023"}}
		got, err := Select(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		want := Target{User: "alex", Host: "10.0.0.5", Port: 4023}
		if got.Kind != KindSSH || got.Target != want {
			t.Fatalf("got %+v, want ssh to %v", got, want)
		}
		if !strings.Contains(got.Why, "default_machine") {
			t.Errorf("why = %q, want it to name the rule", got.Why)
		}
	})

	t.Run("nothing configured", func(t *testing.T) {
		if _, err := Select(ctx, selection()); !errors.Is(err, ErrNoMachine) {
			t.Fatalf("err = %v, want ErrNoMachine", err)
		}
	})

	t.Run("an unparseable machine is an error", func(t *testing.T) {
		s := selection()
		s.Machine = "alex@box:notaport"
		if _, err := Select(ctx, s); err == nil {
			t.Fatal("Select accepted a machine with a bad port")
		}
	})
}

func TestSelectPrefersTheTunnel(t *testing.T) {
	ctx := context.Background()
	dialer := fakeDialer{}

	for _, tc := range []struct {
		name string
		mut  func(*Selection)
	}{
		{"--machine", func(s *Selection) { s.Machine = "box" }},
		{"default_machine", func(s *Selection) { s.Config.DefaultMachine = "box" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := selection()
			var asked Target
			s.Tunnel = func(_ context.Context, target Target) (Dialer, error) {
				asked = target
				return dialer, nil
			}
			tc.mut(&s)
			got, err := Select(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != KindTunnel {
				t.Fatalf("kind = %q, want %q", got.Kind, KindTunnel)
			}
			if got.Dialer != Dialer(dialer) {
				t.Error("the choice did not carry the tunnel's dialer")
			}
			if asked.Host != "box" {
				t.Errorf("the tunnel was asked for %v, want the machine", asked)
			}
			if !strings.Contains(got.Why, "peer key") {
				t.Errorf("why = %q, want it to say a peer key picked the tunnel", got.Why)
			}
		})
	}

	t.Run("the socket never asks for a tunnel", func(t *testing.T) {
		s := selection()
		s.SocketExists = func(string) bool { return true }
		s.Tunnel = func(context.Context, Target) (Dialer, error) {
			t.Error("the local socket asked for a tunnel")
			return nil, ErrNoTunnel
		}
		got, err := Select(ctx, s)
		if err != nil || got.Kind != KindSocket {
			t.Fatalf("got %+v, %v; want the local socket", got, err)
		}
	})

	t.Run("no tunnel falls through to ssh", func(t *testing.T) {
		s := selection()
		s.Machine = "box"

		s.Tunnel = func(context.Context, Target) (Dialer, error) { return nil, nil }
		got, err := Select(ctx, s)
		if err != nil || got.Kind != KindSSH {
			t.Fatalf("got %+v, %v; want ssh", got, err)
		}
	})

	t.Run("a broken tunnel is an error, not a fall-through", func(t *testing.T) {
		boom := errors.New("no handshake with box after 10s")
		s := selection()
		s.Machine = "box"
		s.Tunnel = func(context.Context, Target) (Dialer, error) { return nil, boom }
		_, err := Select(ctx, s)
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the tunnel's own error", err)
		}
		if !strings.Contains(err.Error(), "box") {
			t.Errorf("err = %v, want it to name the machine", err)
		}
	})
}

func TestTunnelTransportAddress(t *testing.T) {
	target := Target{User: "caramelo", Host: "box.example.com", Port: 4022}

	tr := TunnelTransport(fakeDialer{}, target)
	if tr.Kind() != KindTunnel {
		t.Errorf("Kind() = %q, want %q", tr.Kind(), KindTunnel)
	}
	if tr.Address != "10.86.0.1:4022" {
		t.Errorf("Address = %q, want the machine's address inside the default subnet", tr.Address)
	}
	if tr.User != target.User {
		t.Errorf("User = %q, want %q", tr.User, target.User)
	}
	if !strings.Contains(tr.String(), "box.example.com") {
		t.Errorf("String() = %q, want it to name the machine", tr.String())
	}

	tr = TunnelTransport(fakeDialer{machine: "10.99.0.1"}, Target{User: "caramelo", Host: "box", Port: 2222})
	if tr.Address != "10.99.0.1:2222" {
		t.Errorf("Address = %q, want the dialer's machine address and the target's port", tr.Address)
	}
	tr = TunnelTransport(fakeDialer{machine: "box.internal"}, target)
	if tr.Address != "box.internal:4022" {
		t.Errorf("Address = %q, want the name the dialer resolves", tr.Address)
	}
}
