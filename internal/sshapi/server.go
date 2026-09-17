package sshapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/ssh"
	gossh "golang.org/x/crypto/ssh"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/serverconfig"
)

const (
	TransportSocket = "socket"
	TransportSSH    = "ssh"

	TransportTunnel = "tunnel"
)

const (
	handshakeTimeout  = 15 * time.Second
	idleTimeout       = 10 * time.Minute
	maxTimeout        = 24 * time.Hour
	shutdownTimeout   = 10 * time.Second
	keepaliveInterval = 2 * time.Minute
)

const noShellUsage = "caramelo: no interactive shell here. Usage: ssh -p PORT caramelo@host <command> [args]"

type Command struct {
	Args    []string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Service api.Service
	Session api.Session
}

type Exec func(ctx context.Context, c Command) int

type Server struct {
	Config  serverconfig.Config
	Service api.Service
	Version string

	Exec Exec

	HostKey gossh.Signer

	Log io.Writer

	Hostname string

	VPNListener net.Listener

	PeerLookup PeerLookup

	Machines MachineLookup

	IdleTimeout time.Duration
	MaxTimeout  time.Duration
	KeepAlive   time.Duration

	mu        sync.Mutex
	tcpLn     net.Listener
	listeners []apiListener
	listened  bool
}

type apiListener struct {
	transport string
	srv       *ssh.Server
	ln        net.Listener
}

func (s *Server) Listen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listened {
		return nil
	}
	if s.Exec == nil {
		return errors.New("sshapi: Server.Exec is nil")
	}
	if s.VPNListener != nil && s.PeerLookup == nil {
		return errors.New("sshapi: Server.VPNListener without Server.PeerLookup: a session inside the tunnel must have an identity")
	}
	if s.HostKey == nil {
		signer, err := EnsureHostKey(s.Config.HostKeyPath())
		if err != nil {
			return err
		}
		s.HostKey = signer
	}
	if s.Hostname == "" {
		s.Hostname, _ = os.Hostname()
	}

	unixLn, err := s.listenUnix()
	if err != nil {
		return err
	}
	s.listeners = append(s.listeners, apiListener{TransportSocket, s.newSSHServer(TransportSocket), unixLn})

	if s.Config.APIListensPublic() {
		tcpLn, err := s.listenTCP()
		if err != nil {
			s.closeListenersLocked()
			return err
		}
		auth := &Authorizer{Path: s.Config.AuthorizedKeysPath(), User: s.Config.User, Log: s.Log}
		srv := s.newSSHServer(TransportSSH)
		srv.PublicKeyHandler = auth.Authorize
		s.tcpLn = tcpLn
		s.listeners = append(s.listeners, apiListener{TransportSSH, srv, tcpLn})
	}

	if s.VPNListener != nil && s.Config.APIListensVPN() {

		srv := s.newSSHServer(TransportTunnel)
		srv.ConnCallback = s.tunnelConn
		s.listeners = append(s.listeners, apiListener{TransportTunnel, srv, s.VPNListener})
	}

	s.listened = true
	return nil
}

func (s *Server) listenTCP() (net.Listener, error) {

	network := "tcp"
	if ip := net.ParseIP(s.Config.Bind); ip != nil && ip.To4() != nil {
		network = "tcp4"
	}
	addr := net.JoinHostPort(s.Config.Bind, strconv.Itoa(s.Config.SSHPort))
	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return ln, nil
}

func (s *Server) closeListenersLocked() {
	for _, l := range s.listeners {
		_ = l.ln.Close()
	}
	s.listeners, s.tcpLn = nil, nil
}

func (s *Server) listenUnix() (net.Listener, error) {
	path := s.Config.SocketPath()
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	if fi, err := os.Stat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
		}
	}

	if len(path) >= 104 {
		return nil, fmt.Errorf("socket path %s is %d bytes, over the unix socket limit; shorten run_dir in config.yaml", path, len(path))
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}
	return ln, nil
}

func (s *Server) newSSHServer(transport string) *ssh.Server {
	srv := &ssh.Server{
		Version:          "caramelod_" + sanitizeVersion(s.Version),
		Handler:          s.handler(transport),
		HostSigners:      []ssh.Signer{s.HostKey},
		HandshakeTimeout: handshakeTimeout,
		IdleTimeout:      durOr(s.IdleTimeout, idleTimeout),
		MaxTimeout:       durOr(s.MaxTimeout, maxTimeout),

		PtyCallback: func(ssh.Context, ssh.Pty) bool { return true },
		PtyHandler: func(ssh.Context, ssh.Session, ssh.Pty) (func() error, error) {
			return func() error { return nil }, nil
		},

		ChannelHandlers:   map[string]ssh.ChannelHandler{"session": ssh.DefaultSessionHandler},
		RequestHandlers:   map[string]ssh.RequestHandler{},
		SubsystemHandlers: map[string]ssh.SubsystemHandler{},
	}
	return srv
}

func sanitizeVersion(v string) string {
	if v == "" {
		return "dev"
	}
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r < 0x21 || r > 0x7e {
			return '_'
		}
		return r
	}, v)
}

func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tcpLn == nil {
		return nil
	}
	return s.tcpLn.Addr()
}

func (s *Server) SocketPath() string { return s.Config.SocketPath() }

func (s *Server) Serve(ctx context.Context) error {
	if err := s.Listen(); err != nil {
		return err
	}
	s.mu.Lock()
	lns := append([]apiListener(nil), s.listeners...)
	s.mu.Unlock()

	where := make([]string, 0, len(lns))
	for _, l := range lns {
		where = append(where, fmt.Sprintf("%s (%s)", l.ln.Addr(), l.transport))
	}
	s.logf("listening on %s", strings.Join(where, ", "))
	if s.Config.APIListensVPN() && !hasTransport(lns, TransportTunnel) {

		s.logf("warning: api_listen is %q but no tunnel listener was supplied; nothing answers inside the tunnel", s.Config.APIListen)
	}

	errs := make(chan error, len(lns))
	var wg sync.WaitGroup
	for _, l := range lns {
		wg.Add(1)
		go func(l apiListener) { defer wg.Done(); errs <- l.srv.Serve(l.ln) }(l)
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errs:
	}

	for _, l := range lns {
		_ = l.ln.Close()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	for _, l := range lns {
		if err := l.srv.Shutdown(shutdownCtx); err != nil {
			_ = l.srv.Close()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if serveErr == nil && err != nil && !errors.Is(err, ssh.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			serveErr = err
		}
	}
	_ = os.Remove(s.Config.SocketPath())
	if serveErr != nil && !errors.Is(serveErr, ssh.ErrServerClosed) {
		return fmt.Errorf("serve: %w", serveErr)
	}
	return nil
}

func hasTransport(lns []apiListener, transport string) bool {
	for _, l := range lns {
		if l.transport == transport {
			return true
		}
	}
	return false
}

func refusedOverAPI(args []string) (why string, refused bool) {
	word, rest := commandWord(args)
	switch word {
	case "hub":
		return "hub commands are not available over the API", true
	case "edge":

		for _, a := range rest {
			if EdgeSubcommands[a] {
				return "", false
			}
		}
		return "the edge is a process this machine runs, not a command it answers " +
			"(try 'caramelo edge status')", true
	}
	return "", false
}

func loggedArgs(args []string) string {
	word, _ := commandWord(args)
	if word != "secrets" {
		return strings.Join(args, " ")
	}
	out := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			out[i] = a
			continue
		}
		if name, _, ok := strings.Cut(a, "="); ok {
			out[i] = name + "=***"
			continue
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}

func commandWord(args []string) (word string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {

			return "", nil
		}
		if len(a) > 1 && a[0] == '-' {
			name := strings.TrimLeft(a, "-")
			if strings.ContainsRune(name, '=') {

				continue
			}
			if globalFlagValues[name] && i+1 < len(args) {
				i++
			}
			continue
		}
		return a, args[i+1:]
	}
	return "", nil
}

var globalFlagValues = map[string]bool{"machine": true}

var EdgeSubcommands = map[string]bool{
	"status": true, "enable": true, "disable": true, "ca": true, "counts": true, "prune": true,
	"help": true,
}

func (s *Server) handler(transport string) ssh.Handler {
	return func(sess ssh.Session) {
		args := sess.Command()

		if len(args) > 0 && args[0] == "caramelo" {
			args = args[1:]
		}
		identity := identityOf(sess.Context())

		if len(args) == 0 {
			fmt.Fprintln(sess.Stderr(), noShellUsage)
			s.logf("%s %s: no command", transport, identityOr(identity))
			_ = sess.Exit(2)
			return
		}
		if req, kind := parseGit(args); kind != notGit {
			s.serveGit(sess, transport, identity, req, kind)
			return
		}
		if why, refused := refusedOverAPI(args); refused {
			fmt.Fprintln(sess.Stderr(), "caramelo: "+why)
			s.logf("%s %s: refused %q", transport, identityOr(identity), strings.Join(args, " "))
			_ = sess.Exit(2)
			return
		}

		apiSess := api.Session{
			Transport: transport, Identity: identity, Machine: s.Hostname,
			Args:   args,
			Stdin:  sess,
			Stdout: sess,
			Stderr: sess.Stderr(),
			Peer:   s.machinePeer(sess.Context(), identity),
		}

		if apiSess.Peer != "" {
			apiSess.OnBehalfOf = envValue(sess.Environ(), api.IdentityEnv)
		}
		ctx := WithSession(sess.Context(), apiSess)
		start := time.Now()
		stopKeepalive := s.keepalive(sess)
		code := s.Exec(ctx, Command{
			Args:    args,
			Stdin:   sess,
			Stdout:  sess,
			Stderr:  sess.Stderr(),
			Service: s.Service,
			Session: apiSess,
		})
		stopKeepalive()

		if code == 255 {
			code = 1
		}
		s.logf("%s %s: %q exit %d in %s", transport, identityOr(identity), loggedArgs(args), code, time.Since(start).Round(time.Millisecond))
		_ = sess.Exit(code)
	}
}

func envValue(environ []string, key string) string {
	prefix := key + "="
	for _, kv := range environ {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}

func (s *Server) serveGit(sess ssh.Session, transport, identity string, req gitRequest, kind gitKind) {
	if kind == gitRefused {
		fmt.Fprintln(sess.Stderr(), "caramelo: "+req.Refused)
		s.logf("%s %s: refused git %s", transport, identityOr(identity), req.Verb)
		_ = sess.Exit(2)
		return
	}
	git, ok := s.Service.(GitService)
	if !ok {
		fmt.Fprintln(sess.Stderr(), "caramelo: this daemon does not serve git")
		s.logf("%s %s: git %s but no git service", transport, identityOr(identity), req.Verb)
		_ = sess.Exit(2)
		return
	}
	ctx := WithSession(sess.Context(), api.Session{Transport: transport, Identity: identity, Machine: s.Hostname})
	start := time.Now()
	stopKeepalive := s.keepalive(sess)
	code := git.ServeGit(ctx, GitRequest{
		Verb:   req.Verb,
		Path:   req.Path,
		Stdin:  sess,
		Stdout: sess,
		Stderr: sess.Stderr(),
	})
	stopKeepalive()
	if code == 255 {
		code = 1
	}
	s.logf("%s %s: git %s %q exit %d in %s", transport, identityOr(identity), req.Verb, req.Path, code,
		time.Since(start).Round(time.Millisecond))
	_ = sess.Exit(code)
}

func (s *Server) keepalive(sess ssh.Session) func() {
	every := s.KeepAlive
	if every == 0 {
		every = keepaliveInterval
	}
	if every < 0 {
		return func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:

				_, _ = sess.SendRequest("keepalive@caramelo", false, nil)
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

func durOr(d, fallback time.Duration) time.Duration {
	if d != 0 {
		return d
	}
	return fallback
}

func identityOr(identity string) string {
	if identity == "" {
		return "-"
	}
	return identity
}

func (s *Server) logf(format string, args ...any) {
	if s.Log == nil {
		return
	}
	fmt.Fprintf(s.Log, "%s caramelod: "+format+"\n", append([]any{time.Now().UTC().Format(time.RFC3339)}, args...)...)
}

type sessionKey struct{}

func WithSession(ctx context.Context, s api.Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, s)
}

func SessionFrom(ctx context.Context) (api.Session, bool) {
	s, ok := ctx.Value(sessionKey{}).(api.Session)
	return s, ok
}
