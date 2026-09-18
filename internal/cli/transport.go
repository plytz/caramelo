package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/plytz/caramelo/internal/userdir"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/serverconfig"
)

func init() { forward = forwardImpl }

const dialTimeout = 10 * time.Second

type transport struct {
	kind       string
	socketPath string
	user       string
	target     remote.Target

	dialer remote.Dialer

	why string
}

const (
	kindSocket = remote.KindSocket
	kindSSH    = remote.KindSSH
	kindTunnel = remote.KindTunnel
)

func (t transport) String() string {
	switch t.kind {
	case kindSocket:
		return "socket " + t.socketPath
	case kindTunnel:
		return "tunnel " + t.target.String()
	default:
		return "ssh " + t.target.String()
	}
}

func forwardImpl(ctx context.Context, a *app) (int, error) {
	t, err := resolveTransport(ctx, a)
	if err != nil {
		return ExitError, err
	}
	if os.Getenv("CARAMELO_DEBUG") != "" {
		fmt.Fprintf(a.stderr, "transport: %s (%s)\n", t, t.why)
	}
	switch t.kind {
	case kindSocket:
		return forwardSocket(ctx, a, t)
	case kindTunnel:
		return forwardTunnel(ctx, a, t)
	default:
		return forwardSSH(ctx, a, t)
	}
}

func resolveTransport(ctx context.Context, a *app) (transport, error) {
	socketPath, user := daemonSocket()
	commander, err := remote.LoadCommanderConfig()
	if err != nil {
		return transport{}, err
	}
	c, err := remote.Select(ctx, remote.Selection{
		Machine:      a.machine,
		Config:       commander,
		SocketPath:   socketPath,
		SocketUser:   user,
		SocketExists: socketExists,
	})
	if errors.Is(err, remote.ErrNoMachine) {
		path, _ := remote.CommanderConfigPath()
		return transport{}, fmt.Errorf(
			"no local caramelod (no socket at %s) and no machine configured; "+
				"run 'sudo caramelo hub setup' on this machine, or point at one with "+
				"--machine <user@host> (or CARAMELO_MACHINE, or commander.default_machine in %s)",
			socketPath, path)
	}
	if err != nil {
		return transport{}, err
	}
	return transport{
		kind: c.Kind, socketPath: c.SocketPath, user: c.User,
		target: c.Target, dialer: c.Dialer, why: c.Why,
	}, nil
}

func daemonSocket() (path, user string) {
	def := serverconfig.Default()
	cfg, err := serverconfig.Load(serverconfig.ConfigDir())
	if err != nil {
		return def.SocketPath(), def.User
	}
	return cfg.SocketPath(), cfg.User
}

func socketExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

func forwardSocket(ctx context.Context, a *app, t transport) (int, error) {
	conn, err := net.DialTimeout("unix", t.socketPath, dialTimeout)
	if err != nil {
		return ExitError, socketDialError(t.socketPath, err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	cconn, chans, reqs, err := gossh.NewClientConn(conn, t.socketPath, &gossh.ClientConfig{
		User: t.user,

		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         dialTimeout,
	})
	if err != nil {
		return ExitError, fmt.Errorf("connect to caramelod at %s: %w", t.socketPath, err)
	}
	_ = conn.SetDeadline(time.Time{})
	client := gossh.NewClient(cconn, chans, reqs)
	defer func() { _ = client.Close() }()

	sess, err := client.NewSession()
	if err != nil {
		return ExitError, fmt.Errorf("open session on %s: %w", t.socketPath, err)
	}
	defer func() { _ = sess.Close() }()
	if in := a.stdinFor(); in != nil {
		sess.Stdin = in
	}
	sess.Stdout = a.stdout
	sess.Stderr = a.stderr

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = client.Close()
		case <-done:
		}
	}()

	err = sess.Run(remote.JoinArgs(a.args))
	return exitCodeFromSSH(err)
}

func socketDialError(path string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no caramelod socket at %s; run 'sudo caramelo hub setup' on this machine, or use --machine <user@host>", path)
	}
	return fmt.Errorf("caramelod socket exists at %s but refused the connection; is caramelod running? (systemctl --user -M caramelo@ status caramelod): %w", path, err)
}

func exitCodeFromSSH(err error) (int, error) {
	if err == nil {
		return ExitOK, nil
	}
	var ee *gossh.ExitError
	if errors.As(err, &ee) {
		code := ee.ExitStatus()
		if code == 255 {

			code = ExitError
		}
		return code, nil
	}
	var missing *gossh.ExitMissingError
	if errors.As(err, &missing) {
		return ExitError, errors.New("caramelod closed the session without an exit status")
	}
	return ExitError, fmt.Errorf("running command on caramelod: %w", err)
}

func forwardStdin() io.Reader {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	return os.Stdin
}

func (a *app) stdinFor() io.Reader {
	if a.stdin != nil {
		return a.stdin
	}
	return forwardStdin()
}

func forwardTunnel(ctx context.Context, a *app, t transport) (int, error) {
	tr := remote.TunnelTransport(t.dialer, t.target)
	return tr.Run(ctx, a.args, remote.Streams{
		Stdin:  a.stdinFor(),
		Stdout: a.stdout,
		Stderr: a.stderr,
	})
}

func forwardSSH(ctx context.Context, a *app, t transport) (int, error) {
	sshBin, args, err := sshInvocation()
	if err != nil {
		return ExitError, err
	}
	args = append(args, "-p", strconv.Itoa(t.target.Port), t.target.Destination(), "--")
	args = append(args, remote.QuoteArgs(a.args)...)

	cmd := exec.CommandContext(ctx, sshBin, args...)

	cmd.Stdin = os.Stdin
	if a.stdin != nil {
		cmd.Stdin = a.stdin
	}
	cmd.Stdout = a.stdout
	cmd.Stderr = a.stderr
	err = cmd.Run()
	if err == nil {
		return ExitOK, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code != 255 {
			return code, nil
		}
		return ExitError, nil
	}
	return ExitError, fmt.Errorf("running ssh to %s: %w", t.target, err)
}

func sshInvocation() (bin string, args []string, err error) {
	args = []string{
		"-o", "BatchMode=yes",

		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ServerAliveInterval=15",
	}
	if cp, err := controlPath(); err == nil {
		args = append(args,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+cp,
			"-o", "ControlPersist=60s",
		)
	}

	bin, extra, err := remote.SSHClient()
	if err != nil {
		return "", nil, err
	}
	return bin, append(args, extra...), nil
}

const maxControlPath = 107

func controlPath() (string, error) {
	dir, err := userdir.Cache()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "caramelo")

	if len(dir)+len("/cm-")+40+len(".XXXXXXXXXXXXXXXX") > maxControlPath {
		return "", fmt.Errorf("control path under %s would exceed the unix socket length limit", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "cm-%C"), nil
}
