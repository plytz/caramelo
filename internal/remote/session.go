package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

const (
	exitOK    = 0
	exitError = 1
)

const DialTimeout = 10 * time.Second

type ConnTransport struct {
	KindName string

	Dialer Dialer

	Network string
	Address string

	User string

	Label string

	Timeout time.Duration

	HostKey gossh.HostKeyCallback

	OnBehalfOf string
}

const IdentityEnv = "CARAMELO_IDENTITY"

var _ Transport = (*ConnTransport)(nil)

func (t *ConnTransport) Kind() string { return t.KindName }

func (t *ConnTransport) String() string {
	label := t.Label
	if label == "" {
		label = t.Address
	}
	return t.KindName + " " + label
}

func (t *ConnTransport) Run(ctx context.Context, argv []string, s Streams) (int, error) {
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = DialTimeout
	}
	dialer := t.Dialer
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, timeout)
	conn, err := dialer.DialContext(dialCtx, t.Network, t.Address)
	cancelDial()
	if err != nil {
		return exitError, fmt.Errorf("connect to caramelod at %s: %w", t.String(), err)
	}
	defer func() { _ = conn.Close() }()

	hostKey := t.HostKey
	if hostKey == nil {
		hostKey = gossh.InsecureIgnoreHostKey()
	}

	_ = conn.SetDeadline(time.Now().Add(timeout))
	cc, chans, reqs, err := gossh.NewClientConn(conn, t.Address, &gossh.ClientConfig{
		User:            t.User,
		HostKeyCallback: hostKey,
		Timeout:         timeout,
	})
	if err != nil {
		return exitError, fmt.Errorf("connect to caramelod at %s: %w", t.String(), err)
	}
	_ = conn.SetDeadline(time.Time{})
	client := gossh.NewClient(cc, chans, reqs)
	defer func() { _ = client.Close() }()

	sess, err := client.NewSession()
	if err != nil {
		return exitError, fmt.Errorf("open session on %s: %w", t.String(), err)
	}
	defer func() { _ = sess.Close() }()
	if t.OnBehalfOf != "" {
		if err := sess.Setenv(IdentityEnv, t.OnBehalfOf); err != nil {
			return exitError, fmt.Errorf("say who %s is for on %s: %w", argv[0], t.String(), err)
		}
	}
	sess.Stdin, sess.Stdout, sess.Stderr = s.Stdin, s.Stdout, s.Stderr

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = client.Close()
		case <-done:
		}
	}()

	return ExitCode(sess.Run(JoinArgs(argv)))
}

func ExitCode(err error) (int, error) {
	if err == nil {
		return exitOK, nil
	}
	var ee *gossh.ExitError
	if errors.As(err, &ee) {
		code := ee.ExitStatus()
		if code == 255 {

			code = exitError
		}
		return code, nil
	}
	var missing *gossh.ExitMissingError
	if errors.As(err, &missing) {
		return exitError, errors.New("caramelod closed the session without an exit status")
	}
	return exitError, fmt.Errorf("running command on caramelod: %w", err)
}
