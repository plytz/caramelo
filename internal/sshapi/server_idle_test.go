package sshapi_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/plytz/caramelo/internal/testutil"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/sshapi"
)

func startQuiet(t *testing.T, quiet, idle, keepalive time.Duration) (*sshapi.Server, gossh.Signer) {
	t.Helper()
	dir := t.TempDir()
	cfg := serverconfig.Default()
	cfg.APIListen = serverconfig.APIListenPublic
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.RunDir = filepath.Join(testutil.ShortDir(t), "run")
	cfg.Bind = "127.0.0.1"
	cfg.SSHPort = 0

	clientSigner, _ := newClientKey(t, dir)
	line := strings.TrimSpace(string(gossh.MarshalAuthorizedKey(clientSigner.PublicKey())))
	if _, err := sshapi.AppendKey(cfg.AuthorizedKeysPath(), line, "test-key"); err != nil {
		t.Fatal(err)
	}

	srv := &sshapi.Server{
		Config:      cfg,
		Service:     &fakeService{},
		Version:     "test",
		Hostname:    "testbox",
		Log:         &testWriter{t},
		IdleTimeout: idle,
		KeepAlive:   keepalive,
		Exec: func(ctx context.Context, c sshapi.Command) int {
			select {
			case <-ctx.Done():
				return 1
			case <-time.After(quiet):
			}
			fmt.Fprintln(c.Stdout, "done")
			return 0
		},
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not stop within 10s")
		}
	})
	return srv, clientSigner
}

func runQuiet(t *testing.T, srv *sshapi.Server, signer gossh.Signer, args ...string) (code int, stdout string, connErr error) {
	t.Helper()
	client, err := gossh.Dial("tcp", srv.Addr().String(), &gossh.ClientConfig{
		User:            serverconfig.DefaultUser,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.FixedHostKey(srv.HostKey.PublicKey()),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	var out strings.Builder
	sess.Stdout = &out
	err = sess.Run(remote.JoinArgs(args))
	var ee *gossh.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitStatus()
	default:
		connErr = err
	}
	return code, out.String(), connErr
}

func TestQuietCommandSurvivesTheIdleTimeout(t *testing.T) {

	srv, signer := startQuiet(t, 900*time.Millisecond, 300*time.Millisecond, 50*time.Millisecond)
	code, stdout, connErr := runQuiet(t, srv, signer, "status")
	if connErr != nil {
		t.Fatalf("the session was dropped while the command was running: %v", connErr)
	}
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.TrimSpace(stdout) != "done" {
		t.Errorf("stdout = %q, want the command's own output", stdout)
	}
}

func TestWithoutKeepalivesAQuietCommandIsDropped(t *testing.T) {

	srv, signer := startQuiet(t, 2*time.Second, 300*time.Millisecond, -1)
	_, _, connErr := runQuiet(t, srv, signer, "status")
	if connErr == nil {
		t.Fatal("want the idle timeout to drop a quiet session when nothing keeps it alive")
	}
}
