package remote

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestTunnel(t *testing.T) {
	ctx := context.Background()
	target := Target{User: "caramelo", Host: "box", Port: 4022}

	t.Run("nothing installed", func(t *testing.T) {
		TunnelDialer = nil
		if _, err := Tunnel(ctx, target); !errors.Is(err, ErrNoTunnel) {
			t.Errorf("err = %v, want ErrNoTunnel", err)
		}
	})

	t.Run("installed but no tunnel for this machine", func(t *testing.T) {
		TunnelDialer = func(context.Context, Target) (Dialer, error) { return nil, nil }
		t.Cleanup(func() { TunnelDialer = nil })
		if _, err := Tunnel(ctx, target); !errors.Is(err, ErrNoTunnel) {
			t.Errorf("a nil dialer with no error must read as ErrNoTunnel, got %v", err)
		}
	})

	t.Run("a real failure is not a fall-through", func(t *testing.T) {
		boom := errors.New("handshake timed out")
		TunnelDialer = func(context.Context, Target) (Dialer, error) { return nil, boom }
		t.Cleanup(func() { TunnelDialer = nil })
		if _, err := Tunnel(ctx, target); !errors.Is(err, boom) {
			t.Errorf("err = %v, want the dialer's own error", err)
		}
	})

	t.Run("installed", func(t *testing.T) {
		want := DialerFunc(func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("unused")
		})
		var got Target
		TunnelDialer = func(_ context.Context, tt Target) (Dialer, error) { got = tt; return want, nil }
		t.Cleanup(func() { TunnelDialer = nil })
		d, err := Tunnel(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		if d == nil {
			t.Fatal("Tunnel returned no dialer")
		}
		if got != target {
			t.Errorf("dialer asked for %v, want %v", got, target)
		}
	})
}
