package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	gossh "golang.org/x/crypto/ssh"

	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/vpnclient"
)

func init() {
	register(func(a *app) *cobra.Command {
		cmd := &cobra.Command{
			Use:   "git-ssh [ssh options] [user@]host command",
			Short: "git's SSH transport, carried by the tunnel (used via GIT_SSH_COMMAND)",
			Long: `Not meant to be run by hand. Caramelo sets
GIT_SSH_COMMAND='caramelo git-ssh' so that git push and git fetch travel
through the machine's private network instead of a system ssh client.`,
			Hidden: true,

			Args:               minArgs(1),
			DisableFlagParsing: true,
			RunE: func(cmd *cobra.Command, args []string) error {
				inv, err := parseSSHArgs(args)
				if err != nil {
					return &usageError{err}
				}
				code, err := a.runGitSSH(cmd.Context(), inv)
				if err != nil {
					return err
				}
				if code != ExitOK {
					return &exitError{code}
				}
				return nil
			},
		}
		return available(cmd, onCommander)
	})
}

type sshInvocationArgs struct {
	User    string
	Host    string
	Port    int
	Command string
}

func parseSSHArgs(args []string) (sshInvocationArgs, error) {
	inv := sshInvocationArgs{User: serverconfig.DefaultUser, Port: serverconfig.DefaultSSHPort}

	valued := map[byte]bool{
		'p': true, 'o': true, 'i': true, 'l': true, 'F': true, 'c': true,
		'm': true, 'b': true, 'e': true, 'D': true, 'L': true, 'R': true, 'W': true,
	}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(rest) == 0 && strings.HasPrefix(a, "-") && a != "-" {
			flag := a[1]
			value := a[2:]
			if valued[flag] {
				if value == "" {
					i++
					if i >= len(args) {
						return inv, fmt.Errorf("ssh option -%c needs a value", flag)
					}
					value = args[i]
				}
				switch flag {
				case 'p':
					port, err := strconv.Atoi(value)
					if err != nil || port < 1 || port > 65535 {
						return inv, fmt.Errorf("bad port %q", value)
					}
					inv.Port = port
				case 'l':
					inv.User = value
				}
			}
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) == 0 {
		return inv, errors.New("no host")
	}
	dest := rest[0]
	if i := strings.LastIndex(dest, "@"); i >= 0 {
		if i == 0 {
			return inv, fmt.Errorf("no user before @ in %q", rest[0])
		}
		inv.User, dest = dest[:i], dest[i+1:]
	}
	if dest == "" {
		return inv, errors.New("no host")
	}
	inv.Host = dest

	inv.Command = strings.Join(rest[1:], " ")
	if strings.TrimSpace(inv.Command) == "" {
		return inv, errors.New("no command: git-ssh carries a git command, it is not a shell")
	}
	return inv, nil
}

func (a *app) runGitSSH(ctx context.Context, inv sshInvocationArgs) (int, error) {
	target, err := vpnclient.DialTarget(ctx, inv.Host, inv.Port, vpnclient.TunnelOptions{Log: a.stderr})
	if err != nil {
		if errors.Is(err, vpnclient.ErrNoKey) {
			return ExitError, fmt.Errorf(
				"git-ssh cannot reach %s: the commander has not joined its network (run 'caramelo vpn up'): %w",
				inv.Host, err)
		}
		return ExitError, err
	}
	defer func() { _ = target.Dialer.Close() }()

	conn, err := target.Dialer.DialContext(ctx, "tcp", target.Address)
	if err != nil {
		return ExitError, err
	}

	cconn, chans, reqs, err := gossh.NewClientConn(conn, target.Address, &gossh.ClientConfig{
		User:            inv.User,
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         dialTimeout,
	})
	if err != nil {
		_ = conn.Close()
		return ExitError, fmt.Errorf("connect to %s inside the tunnel to %s: %w",
			target.Address, target.Machine, err)
	}
	client := gossh.NewClient(cconn, chans, reqs)
	defer func() { _ = client.Close() }()

	sess, err := client.NewSession()
	if err != nil {
		return ExitError, fmt.Errorf("open a session on %s: %w", target.Machine, err)
	}
	defer func() { _ = sess.Close() }()

	sess.Stdin = stdinFrom(ctx)
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

	return exitCodeFromSSH(sess.Run(inv.Command))
}

func gitSSHCommandTunnel(binary string) string {
	return remote.Quote(binary) + " git-ssh"
}

const gitSSHVariant = "GIT_SSH_VARIANT=ssh"

func gitSSHEnvTunnel(binary string) []string {
	return []string{"GIT_SSH_COMMAND=" + gitSSHCommandTunnel(binary), gitSSHVariant}
}

func thisBinary() string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return "caramelo"
	}
	return exe
}
