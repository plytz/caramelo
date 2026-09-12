package remote

import (
	"context"
	"errors"
	"io"
	"net"
)

const (
	KindSocket = "socket"

	KindSSH = "ssh"

	KindTunnel = "tunnel"
)

type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type DialerFunc func(ctx context.Context, network, address string) (net.Conn, error)

func (f DialerFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

type Streams struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type Transport interface {
	Kind() string

	String() string

	Run(ctx context.Context, argv []string, s Streams) (int, error)
}

var TunnelDialer func(ctx context.Context, t Target) (Dialer, error)

var ErrNoTunnel = errors.New("no tunnel for this machine")

func Tunnel(ctx context.Context, t Target) (Dialer, error) {
	if TunnelDialer == nil {
		return nil, ErrNoTunnel
	}
	d, err := TunnelDialer(ctx, t)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, ErrNoTunnel
	}
	return d, nil
}
