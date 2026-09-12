//go:build integration

package vpn

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	capi "github.com/plytz/caramelo/internal/api"
	cenv "github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vpn"
	"github.com/plytz/caramelo/internal/vpnclient"
	"github.com/plytz/caramelo/test/integration/itest"
)

func TestVPN(t *testing.T) {
	s := start(t)
	for _, step := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"VPNUp", s.vpnUp},
		{"TunnelIsTheOnlyWayIn", s.tunnelIsTheOnlyWayIn},
		{"EnvGetsAnAddress", s.envGetsAnAddress},
		{"ConnectOpensLocalPorts", s.connectOpensLocalPorts},
		{"GitPushOverTheTunnel", s.gitPushOverTheTunnel},
		{"PeerAddAndRemoveAnotherIdentity", s.peerAddAndRemoveAnotherIdentity},
		{"VPNConfigRendersWgQuick", s.vpnConfigRendersWgQuick},
		{"TransparentModeOnTheClient", s.transparentModeOnTheClient},
		{"ConcurrentUseOfTheTunnel", s.concurrentUseOfTheTunnel},
		{"EnvDestroyFreesTheAddress", s.envDestroyFreesTheAddress},
		{"PeerRemoveSilencesTheClient", s.peerRemoveSilencesTheClient},
		{"ZZFinalState", s.zzFinalState},
		{"ZZTunnelSurvivesAPowerCycle", s.zzTunnelSurvivesAPowerCycle},
	} {
		t.Run(step.name, step.run)
	}
}

func (s *suite) vpnUp(t *testing.T) {
	up := fmt.Sprintf("vpn up --machine %s --name %s --json", s.boxIP, clientPeer)
	res := s.clientCaramelo(t, up)
	if res.ExitCode != 0 {
		t.Fatalf("vpn up on %s: exit %d\nstdout:%s\nstderr:%s",
			s.client.Alias, res.ExitCode, res.Stdout, res.Stderr)
	}
	st := decode[vpnclient.State](t, "vpn up", res.Stdout)

	if st.Mode != vpnclient.ModeUserspace {
		t.Errorf("mode = %q, want %q: nothing may be installed for the default mode",
			st.Mode, vpnclient.ModeUserspace)
	}
	if st.PeerName != clientPeer {
		t.Errorf("peer_name = %q, want %q", st.PeerName, clientPeer)
	}
	if st.PublicKey == "" {
		t.Error("vpn up reported no public key")
	}
	inSubnet(t, "vpn up ip", st.IP, vpn.KindPeer)
	if want := machineAddress(t).String() + ":" + fmt.Sprint(vpn.ResolverPort); st.Resolver != want {
		t.Errorf("resolver = %q, want %q", st.Resolver, want)
	}
	if st.LastHandshake.IsZero() {
		t.Error("vpn up returned with no handshake: it must verify a session through the tunnel")
	}

	key := s.clientKeyPath()
	perm := s.runOnClient(t, "stat -c %a "+itest.ShellQuote(key), itest.Scale(time.Minute))
	if perm.ExitCode != 0 {
		t.Fatalf("no private key at %s on %s: exit %d: %s",
			key, s.client.Alias, perm.ExitCode, strings.TrimSpace(perm.Stderr))
	}
	if got := strings.TrimSpace(perm.Stdout); got != "600" {
		t.Errorf("%s mode = %s, want 600", key, got)
	}
	if st.PublicKey != "" {
		leak := s.runOnClient(t, fmt.Sprintf("grep -qF %s %s",
			itest.ShellQuote(st.PublicKey), itest.ShellQuote(key)), itest.Scale(time.Minute))
		if leak.ExitCode == 0 {
			t.Error("the private key file contains the public key verbatim; keys must not be confused")
		}
	}

	peers := s.listPeers(t, s.withSSH())
	p, ok := peers[clientPeer]
	if !ok {
		t.Fatalf("peer %q not in peer list: %v", clientPeer, keysOf(peers))
	}
	if p.IP != st.IP.String() {
		t.Errorf("peer %s ip = %q, the client says %s", clientPeer, p.IP, st.IP)
	}
	if p.PublicKey != st.PublicKey {
		t.Errorf("peer %s public key = %q, the client says %q", clientPeer, p.PublicKey, st.PublicKey)
	}

	again := s.clientCaramelo(t, up)
	if again.ExitCode != 0 {
		t.Fatalf("a second vpn up on %s: exit %d\nstderr:%s", s.client.Alias, again.ExitCode, again.Stderr)
	}
	if st2 := decode[vpnclient.State](t, "vpn up (again)", again.Stdout); st2.IP != st.IP {
		t.Errorf("a second vpn up moved the address: %s -> %s", st.IP, st2.IP)
	}
}

func (s *suite) tunnelIsTheOnlyWayIn(t *testing.T) {
	if err := os.RemoveAll(s.home + "/.ssh"); err != nil {
		t.Fatalf("removing the laptop's ssh key: %v", err)
	}
	if p, ok := itest.LookPathIn(s.noSSHPath, "ssh"); ok {
		t.Fatalf("the test PATH still has ssh at %s", p)
	}
	t.Logf("the laptop now has no ssh key and no ssh binary; PATH=%s", s.noSSHPath)

	res := itest.ClientOK(t, s.opts(s.clientEnv()), "status", "--json")
	st := decode[capi.Status](t, "status", res.Stdout)
	if st.Transport != remote.KindTunnel {
		t.Errorf("transport = %q, want %q", st.Transport, remote.KindTunnel)
	}
	if st.Identity != hostPeer {
		t.Errorf("identity = %q, want the peer name %q: inside the tunnel the network stack is the identity",
			st.Identity, hostPeer)
	}
	if st.VPN == nil || !st.VPN.Enabled {
		t.Fatalf("status carries no working tunnel section: %+v", st.VPN)
	}
	if st.VPN.Subnet != vpn.DefaultSubnet {
		t.Errorf("subnet = %q, want %q", st.VPN.Subnet, vpn.DefaultSubnet)
	}
	if st.VPN.Peers < 1 {
		t.Errorf("peers = %d, want at least this one", st.VPN.Peers)
	}

	itest.ClientOK(t, s.opts(s.clientEnv()), "env", "list", "--json")
}

func (s *suite) envGetsAnAddress(t *testing.T) {
	s.pushSample(t, defaultBranch)

	res := itest.ClientOK(t, s.opts(s.clientEnv()), "env", "create", envName, "--from", defaultBranch, "--json")
	e := decode[envRecord](t, "env create", res.Stdout)
	if e.Status != cenv.StatusReady {
		t.Fatalf("env create: status %q, want %q", e.Status, cenv.StatusReady)
	}
	ip := addr(t, "env vpn_ip", e.VPNIP)
	inSubnet(t, "env vpn_ip", ip, vpn.KindEnv)
	s.envAddress = ip.String()
	t.Logf("%s.%s.%s is %s", envName, appName, vpn.Domain, s.envAddress)

	detail := s.showEnv(t, envName)
	if detail.Env.VPNIP != s.envAddress {
		t.Errorf("env show vpn_ip = %q, create said %q", detail.Env.VPNIP, s.envAddress)
	}
	created := false
	for _, ev := range detail.Events {
		if ev.Action == "create" {
			created = true
			if ev.Identity != hostPeer {
				t.Errorf("create event identity = %q, want the peer name %q", ev.Identity, hostPeer)
			}
		}
	}
	if !created {
		t.Errorf("no create event on %s: %+v", envName, detail.Events)
	}

	s.upEnv(t, envName)

	t.Run("env url carries the address on the machine's network", func(t *testing.T) {
		urls := s.envURLs(t, envName)
		for _, c := range []struct {
			name   string
			port   int
			scheme string
		}{
			{"web", 0, "http://"},
			{"echo", 9001, "udp://"},
			{"db", 5432, "tcp://"},
			{"cache", 6379, "tcp://"},
		} {
			u, ok := urls[c.name]
			if !ok {
				t.Errorf("env url %s lists nothing called %q", envName, c.name)
				continue
			}
			want := c.port
			if want == 0 {
				want = u.Port
			}
			host := vpn.ServiceHost(appName, envName, c.name)
			if u.InternalHost != host || u.InternalPort != want {
				t.Errorf("%s = %+v, want %s on %d", c.name, u, host, want)
			}
			if got := fmt.Sprintf("%s%s:%d", c.scheme, host, want); u.InternalURL != got {
				t.Errorf("%s internal url = %q, want %q", c.name, u.InternalURL, got)
			}
			if got := fmt.Sprintf("%s:%d", s.envAddress, want); u.InternalAddress != got {
				t.Errorf("%s internal address = %q, want %q", c.name, u.InternalAddress, got)
			}
		}
	})
}

func (s *suite) connectOpensLocalPorts(t *testing.T) {
	s.needEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
	defer cancel()
	session, err := itest.Connect(ctx, s.opts(s.clientEnv()), envName)
	if err != nil {
		t.Fatalf("caramelo connect %s: %v", envName, err)
	}
	defer session.Close()

	for _, l := range session.Listeners {
		t.Logf("connect: %-6s %-4s %s -> %s (%s)", l.Name, l.Protocol, l.Local, l.Remote, l.Host)
	}
	for _, dep := range []struct {
		name string
		port int
	}{{"db", 5432}, {"cache", 6379}} {
		l, ok := listenerFor(session, dep.name, vpn.TCP)
		if !ok {
			t.Errorf("connect opened no tcp port for %q", dep.name)
			continue
		}
		if want := vpn.ServiceHost(appName, envName, dep.name); l.Host != want {
			t.Errorf("%s host = %q, want %q", dep.name, l.Host, want)
		}
		if want := fmt.Sprintf("%s:%d", s.envAddress, dep.port); l.Remote != want {
			t.Errorf("%s remote = %q, want %q: an env answers on its native ports",
				dep.name, l.Remote, want)
		}
	}

	if local, ok := session.Local("cache", vpn.TCP); ok {
		if got := redisPing(t, local); got != "+PONG" {
			t.Errorf("redis through the tunnel answered %q, want +PONG", got)
		}
	}
	if local, ok := session.Local("db", vpn.TCP); ok {
		postgresHandshake(t, local)
	}

	t.Run("a udp datagram comes back", func(t *testing.T) {
		l, ok := listenerFor(session, "echo", vpn.UDP)
		if !ok {
			t.Fatalf("connect opened no udp port for the echo service: %+v", session.Listeners)
		}
		if want := fmt.Sprintf("%s:%d", s.envAddress, 9001); l.Remote != want {
			t.Errorf("echo remote = %q, want %q: a service answers on its own port", l.Remote, want)
		}
		const payload = "caramelo through the tunnel"
		if got := udpEcho(t, l.Local, payload); got != payload {
			t.Errorf("the echo service returned %q, wanted %q back", got, payload)
		}
	})

	t.Run("http on the service's own port", func(t *testing.T) {
		local, ok := session.Local("web", vpn.TCP)
		if !ok {
			t.Fatalf("connect opened no tcp port for the web service: %+v", session.Listeners)
		}
		body := httpGet(t, "http://"+local+"/")
		if !strings.Contains(body, envName) {
			t.Errorf("GET / through the tunnel = %q, want it to name the environment (%s)", body, envName)
		}
		if deps := httpGet(t, "http://"+local+"/deps"); !strings.Contains(deps, "db ok") ||
			!strings.Contains(deps, "cache ok") {
			t.Errorf("GET /deps = %q, want both dependencies reachable", deps)
		}
	})
}

func (s *suite) gitPushOverTheTunnel(t *testing.T) {
	remoteURL := s.internalRemote(t)
	env := s.gitOverTunnelEnv(t)

	before := itest.GitRefs(t, s.repo, env, remoteURL)
	if _, ok := before["refs/heads/"+defaultBranch]; !ok {
		t.Fatalf("ls-remote through the tunnel does not list %s: %v", defaultBranch, before)
	}

	if err := os.WriteFile(s.repo+"/note.txt", []byte("through the tunnel\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit := itest.GitCommitAll(t, s.repo, env, "sampleapp: a commit pushed through the tunnel")
	itest.Git(t, s.repo, env, "push", remoteURL, defaultBranch)

	after := itest.GitRefs(t, s.repo, env, remoteURL)
	if after["refs/heads/"+defaultBranch] != commit {
		t.Errorf("%s on the box = %q, want the commit just pushed %q",
			defaultBranch, after["refs/heads/"+defaultBranch], commit)
	}

	t.Run("caramelo up pushes the checkout through the tunnel", func(t *testing.T) {
		s.needEnv(t)
		message := "hello from the tunnel " + time.Now().Format("150405")
		replaceLine(t, s.repo, "greeting.py", `MESSAGE = "hello from"`,
			fmt.Sprintf("MESSAGE = %q", message))
		itest.GitCommitAll(t, s.repo, s.gitOverTunnelEnv(t), "sampleapp: a new greeting")

		s.upEnv(t, envName)

		ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
		defer cancel()
		session, err := itest.Connect(ctx, s.opts(s.clientEnv()), envName)
		if err != nil {
			t.Fatalf("caramelo connect %s: %v", envName, err)
		}
		defer session.Close()
		local, ok := session.Local("web", vpn.TCP)
		if !ok {
			t.Fatalf("no local port for the web service: %+v", session.Listeners)
		}
		if body := httpGet(t, "http://"+local+"/"); !strings.Contains(body, message) {
			t.Errorf("GET / = %q, want the greeting pushed through the tunnel (%q)", body, message)
		}
	})
}

func (s *suite) peerAddAndRemoveAnotherIdentity(t *testing.T) {
	pub := generatePublicKey(t)
	t.Cleanup(func() {
		_, _ = itest.RunClient(context.Background(), s.opts(s.clientEnv()), "peer", "remove", agentPeer)
	})

	res := itest.ClientOK(t, s.opts(s.clientEnv()), "peer", "add", agentPeer, pub, "--json")
	added := decode[state.Peer](t, "peer add", res.Stdout)
	if added.Name != agentPeer || added.PublicKey != pub {
		t.Errorf("peer add returned %+v, want %s with the key it was given", added, agentPeer)
	}
	inSubnet(t, "peer add ip", addr(t, "peer add ip", added.IP), vpn.KindPeer)

	peers := s.listPeers(t, s.clientEnv())
	if _, ok := peers[agentPeer]; !ok {
		t.Fatalf("peer %q not listed after peer add: %v", agentPeer, keysOf(peers))
	}
	if peers[agentPeer].IP == peers[hostPeer].IP {
		t.Errorf("two peers share the address %s", peers[agentPeer].IP)
	}

	itest.ClientOK(t, s.opts(s.clientEnv()), "peer", "add", agentPeer, pub, "--json")
	if again := s.listPeers(t, s.clientEnv()); again[agentPeer].IP != peers[agentPeer].IP {
		t.Errorf("re-adding %s moved its address: %s -> %s",
			agentPeer, peers[agentPeer].IP, again[agentPeer].IP)
	}

	itest.ClientOK(t, s.opts(s.clientEnv()), "peer", "remove", agentPeer)
	if left := s.listPeers(t, s.clientEnv()); left[agentPeer].Name != "" {
		t.Errorf("peer %q is still listed after peer remove", agentPeer)
	}
}

func (s *suite) vpnConfigRendersWgQuick(t *testing.T) {
	res := s.clientCaramelo(t, "vpn config --machine "+s.boxIP)
	if res.ExitCode != 0 {
		t.Fatalf("vpn config on %s: exit %d\nstderr:%s", s.client.Alias, res.ExitCode, res.Stderr)
	}
	for _, want := range []string{"[Interface]", "PrivateKey", "Address", "[Peer]", "PublicKey", "Endpoint", "AllowedIPs"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("vpn config output has no %q:\n%s", want, res.Stdout)
		}
	}
	if !strings.Contains(res.Stdout, fmt.Sprint(vpn.DefaultListenPort)) {
		t.Errorf("vpn config does not point at udp %d:\n%s", vpn.DefaultListenPort, res.Stdout)
	}
}

func (s *suite) transparentModeOnTheClient(t *testing.T) {
	s.needEnv(t)

	s.joinClient(t)
	s.writeClientConfig(t, s.boxIP)

	if res := s.sudoClientCaramelo(t, "vpn install --machine "+s.boxIP+" --json"); res.ExitCode != 0 {
		t.Fatalf("sudo caramelo vpn install on %s: exit %d\nstdout:%s\nstderr:%s",
			s.client.Alias, res.ExitCode, res.Stdout, res.Stderr)
	}
	if res := s.clientCaramelo(t, "vpn up --transparent --machine "+s.boxIP+" --json"); res.ExitCode != 0 {
		t.Fatalf("vpn up --transparent on %s: exit %d\nstderr:%s", s.client.Alias, res.ExitCode, res.Stderr)
	}

	host := vpn.ServiceHost(appName, envName, "db")
	res := s.runOnClient(t, "getent hosts "+host, itest.Scale(time.Minute))
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, s.envAddress) {
		t.Errorf("getent hosts %s on %s: exit %d, stdout %q, want %s",
			host, s.client.Alias, res.ExitCode, res.Stdout, s.envAddress)
	}
	if res := s.runOnClient(t, "getent hosts nope."+appName+"."+vpn.Domain, itest.Scale(time.Minute)); res.ExitCode == 0 {
		t.Errorf("an unknown .internal name resolved on %s: %q", s.client.Alias, res.Stdout)
	}

	script := fmt.Sprintf("python3 -c 'import socket; s=socket.create_connection((\"%s\", 5432), %d); "+
		"print(\"connected\", s.getpeername()); s.close()'", host, int(itest.Scale(10*time.Second).Seconds()))
	if res := s.runOnClient(t, script, itest.Scale(time.Minute)); res.ExitCode != 0 ||
		!strings.Contains(res.Stdout, "connected") {
		t.Errorf("tcp to %s:5432 from %s: exit %d\nstdout:%s\nstderr:%s",
			host, s.client.Alias, res.ExitCode, res.Stdout, res.Stderr)
	}

	itest.RunGoss(t, s.client, itest.MustGossSpec(t, "vpn.yaml"))

	t.Run("http and udp to the environment's services", func(t *testing.T) {
		urls := s.envURLs(t, envName)
		web, ok := urls["web"]
		if !ok {
			t.Fatalf("env url %s lists no web service: %+v", envName, urls)
		}
		itest.EnsureCurl(t, s.client)
		res := s.runOnClient(t, fmt.Sprintf("curl -sS --max-time %d %s/",
			int(itest.Scale(20*time.Second).Seconds()), web.InternalURL), itest.Scale(2*time.Minute))
		if res.ExitCode != 0 || !strings.Contains(res.Stdout, envName) {
			t.Errorf("curl %s/ on %s: exit %d\nstdout:%s\nstderr:%s",
				web.InternalURL, s.client.Alias, res.ExitCode, res.Stdout, res.Stderr)
		}
		echoHost := vpn.ServiceHost(appName, envName, "echo")
		script := fmt.Sprintf(`python3 -c 'import socket, sys
for _ in range(5):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.settimeout(4)
    try:
        s.sendto(b"caramelo", ("%s", 9001))
        print(s.recv(64).decode())
        sys.exit(0)
    except OSError as e:
        last = e
    finally:
        s.close()
print("no answer:", last, file=sys.stderr)
sys.exit(1)'`, echoHost)
		if res := s.runOnClient(t, script, itest.Scale(time.Minute)); res.ExitCode != 0 ||
			!strings.Contains(res.Stdout, "caramelo") {
			t.Errorf("udp to %s:9001 from %s: exit %d\nstdout:%s\nstderr:%s",
				echoHost, s.client.Alias, res.ExitCode, res.Stdout, res.Stderr)
		}
	})
}

func (s *suite) joinClient(t *testing.T) {
	t.Helper()
	up := "vpn up --machine " + s.boxIP + " --name " + clientPeer + " --json"
	res := s.clientCaramelo(t, up)
	if res.ExitCode == 0 {
		t.Logf("%s joined the network on its own", s.client.Alias)
		return
	}
	t.Logf("%s could not register itself (exit %d: %s); admitting it from the host",
		s.client.Alias, res.ExitCode, strings.TrimSpace(res.Stderr))

	st := decode[vpnclient.State](t, "vpn status on "+s.client.Alias,
		s.clientCaramelo(t, "vpn status --machine "+s.boxIP+" --json").Stdout)
	if st.PublicKey == "" {
		t.Fatalf("%s has no key to admit: `vpn up` failed and `vpn status` reports none. "+
			"A client that cannot reach the API must still generate its key so an existing "+
			"peer can add it.", s.client.Alias)
	}
	itest.ClientOK(t, s.opts(s.clientEnv()), "peer", "add", clientPeer, st.PublicKey, "--json")
	if res := s.clientCaramelo(t, up); res.ExitCode != 0 {
		t.Fatalf("vpn up on %s after peer add: exit %d\nstdout:%s\nstderr:%s",
			s.client.Alias, res.ExitCode, res.Stdout, res.Stderr)
	}
}

func (s *suite) concurrentUseOfTheTunnel(t *testing.T) {
	s.needEnv(t)

	t.Run("one device carries many conversations at once", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
		defer cancel()
		session, err := itest.Connect(ctx, s.opts(s.clientEnv()), envName)
		if err != nil {
			t.Fatalf("caramelo connect %s: %v", envName, err)
		}
		defer session.Close()

		cache, ok := session.Local("cache", vpn.TCP)
		if !ok {
			t.Fatalf("no tcp port for cache: %+v", session.Listeners)
		}
		web, _ := session.Local("web", vpn.TCP)
		echo, _ := session.Local("echo", vpn.UDP)

		const n = 8
		var wg sync.WaitGroup
		errs := make(chan error, 3*n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if got, err := itest.RedisPing(cache); err != nil || got != "+PONG" {
					errs <- fmt.Errorf("redis %d: %q (%v)", i, got, err)
				}
			}(i)
			if web != "" {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					res, err := (&http.Client{Timeout: itest.Scale(30 * time.Second)}).
						Get("http://" + web + "/healthz")
					if err != nil {
						errs <- fmt.Errorf("http %d: %w", i, err)
						return
					}
					_ = res.Body.Close()
					if res.StatusCode != http.StatusOK {
						errs <- fmt.Errorf("http %d: %s", i, res.Status)
					}
				}(i)
			}
			if echo != "" {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					payload := fmt.Sprintf("datagram-%d", i)
					got, err := udpRoundTrip(echo, payload)
					if err != nil || got != payload {
						errs <- fmt.Errorf("udp %d: %q (%v)", i, got, err)
					}
				}(i)
			}
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	})

	t.Run("many processes at once in transparent mode", func(t *testing.T) {
		if peers := s.listPeers(t, s.clientEnv()); peers[clientPeer].Name == "" {
			t.Skipf("%s is not a peer; transparent mode never came up", s.client.Alias)
		}
		const n = 4
		cmd := fmt.Sprintf("for i in $(seq %d); do %s env list --json > /tmp/list.$i.json 2>/tmp/list.$i.err & done; wait; "+
			"for i in $(seq %d); do python3 -c \"import json,sys; json.load(open('/tmp/list.$i.json'))\" || "+
			"{ echo \"run $i:\"; cat /tmp/list.$i.err; exit 1; }; done; echo all-ok",
			n, itest.CarameloBinary, n)
		res := s.runOnClient(t, cmd, itest.Scale(3*time.Minute))
		if res.ExitCode != 0 || !strings.Contains(res.Stdout, "all-ok") {
			t.Errorf("%d concurrent commands on %s: exit %d\nstdout:%s\nstderr:%s",
				n, s.client.Alias, res.ExitCode, res.Stdout, res.Stderr)
		}
	})
}

func (s *suite) envDestroyFreesTheAddress(t *testing.T) {
	s.needEnv(t)
	freed := s.envAddress

	itest.ClientOK(t, s.opts(s.clientEnv()), "env", "destroy", envName, "--yes")
	s.envAddress = ""

	script := fmt.Sprintf("python3 -c 'import socket;\ns=socket.socket()\ns.settimeout(5)\nimport sys\n"+
		"try:\n    s.connect((\"%s\", 5432))\nexcept OSError as e:\n    print(\"refused\", e); sys.exit(0)\n"+
		"print(\"connected\"); sys.exit(1)'", freed)
	if res := s.runOnClient(t, script, itest.Scale(time.Minute)); res.ExitCode != 0 {
		t.Errorf("%s:5432 still answered after destroy: exit %d\nstdout:%s\nstderr:%s",
			freed, res.ExitCode, res.Stdout, res.Stderr)
	}

	host := vpn.ServiceHost(appName, envName, "db")
	allowed := time.Duration(vpn.TTL)*time.Second + itest.Scale(20*time.Second)
	deadline := time.Now().Add(allowed)
	var last itest.Result
	for {
		last = s.runOnClient(t, "getent hosts "+host, itest.Scale(time.Minute))
		if last.ExitCode != 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if last.ExitCode == 0 {
		t.Errorf("%s still resolves %s after destroy, longer than the %ds the resolver allows an "+
			"answer to be cached: %q", host, allowed, vpn.TTL, last.Stdout)
	}

	s.pushSample(t, defaultBranch)
	res := itest.ClientOK(t, s.opts(s.clientEnv()), "env", "create", nextEnvName, "--from", defaultBranch, "--json")
	e := decode[envRecord](t, "env create "+nextEnvName, res.Stdout)
	t.Cleanup(func() {
		_, _ = itest.RunClient(context.Background(), s.opts(s.clientEnv()), "env", "destroy", nextEnvName, "--yes")
	})
	if e.VPNIP != freed {
		t.Errorf("%s got %s; %s freed %s and it is the lowest free address", nextEnvName, e.VPNIP, envName, freed)
	}
}

func (s *suite) peerRemoveSilencesTheClient(t *testing.T) {
	if peers := s.listPeers(t, s.clientEnv()); peers[clientPeer].Name == "" {
		t.Skipf("%s never joined the network; nothing to revoke", s.client.Alias)
	}
	itest.ClientOK(t, s.opts(s.clientEnv()), "peer", "remove", clientPeer)

	started := time.Now()
	res := s.clientCaramelo(t, "status --json --machine "+s.boxIP)
	switch {
	case res.ExitCode == 0:
		t.Errorf("%s still reached the machine %s after its peer was removed:\n%s",
			s.client.Alias, time.Since(started).Round(time.Second), res.Stdout)
	case res.ExitCode == 1:
		t.Logf("%s gave up after %s: %s", s.client.Alias, time.Since(started).Round(time.Second),
			strings.TrimSpace(res.Stderr))
	default:
		t.Errorf("exit = %d after %s, want 1: a revoked peer is an error, never a usage error and never 255\nstderr:%s",
			res.ExitCode, time.Since(started).Round(time.Second), res.Stderr)
	}
	if !strings.Contains(strings.ToLower(res.Stderr), "handshake") {
		t.Errorf("the error does not explain the silence (no handshake): %q", res.Stderr)
	}

	st := decode[vpnclient.State](t, "vpn status",
		s.clientCaramelo(t, "vpn status --json --machine "+s.boxIP).Stdout)
	if !st.LastHandshake.IsZero() && st.LastHandshake.After(started) {
		t.Errorf("vpn status reports a handshake at %s, after the peer was removed at %s",
			st.LastHandshake, started)
	}
}

func (s *suite) zzFinalState(t *testing.T) {
	res := itest.ClientOK(t, s.opts(s.clientEnv()), "status", "--json")
	st := decode[capi.Status](t, "status", res.Stdout)
	if st.VPN == nil {
		t.Fatal("status carries no tunnel section")
	}
	before := st.VPN.APIListen
	t.Logf("api_listen = %q", before)

	if before != serverconfig.APIListenVPN {
		t.Cleanup(func() { s.setAPIListen(t, before) })
		s.setAPIListen(t, serverconfig.APIListenVPN)
	}

	opts := itest.SSHAPIFor(t, s.box)
	opts.Timeout = itest.Scale(20 * time.Second)
	opts.AllowTimeout = true
	public := itest.SSHAPIRun(t, opts, "status", "--json")
	if public.ExitCode == 0 {
		t.Errorf("the public listener still answers with api_listen=vpn:\n%s", public.Stdout)
	} else {
		t.Logf("the public port is closed (exit %d): %s", public.ExitCode, strings.TrimSpace(public.Stderr))
	}

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(time.Minute))
	defer cancel()
	if res, err := s.box.Run(ctx,
		fmt.Sprintf("ss -H -ltn | grep -q ':%d '", itest.CarameloSSHPort)); err != nil {
		t.Fatalf("ss on %s: %v", s.box.Alias, err)
	} else if res.ExitCode == 0 {
		t.Errorf("something is still listening on tcp:%d with api_listen=vpn", itest.CarameloSSHPort)
	}
	if res, err := s.box.Run(ctx,
		fmt.Sprintf("ss -H -uln | grep -q ':%d '", vpn.DefaultListenPort)); err != nil {
		t.Fatalf("ss on %s: %v", s.box.Alias, err)
	} else if res.ExitCode != 0 {
		t.Errorf("nothing is listening on udp:%d; the machine is unreachable", vpn.DefaultListenPort)
	}

	now := decode[capi.Status](t, "status",
		itest.ClientOK(t, s.opts(s.clientEnv()), "status", "--json").Stdout)
	if now.VPN == nil || now.VPN.APIListen != serverconfig.APIListenVPN {
		t.Errorf("status through the tunnel says api_listen %+v", now.VPN)
	}
	if now.Transport != remote.KindTunnel {
		t.Errorf("transport = %q, want %q", now.Transport, remote.KindTunnel)
	}
	itest.ClientOK(t, s.opts(s.clientEnv()), "env", "list", "--json")
	itest.ClientOK(t, s.opts(s.clientEnv()), "peer", "list", "--json")
}

func (s *suite) setAPIListen(t *testing.T, mode string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(10*time.Minute))
	defer cancel()
	cmd := fmt.Sprintf("sudo -n %s server setup --yes --json --api-listen %s", itest.CarameloBinary, mode)
	res, err := s.box.Run(ctx, cmd)
	if err != nil {
		t.Fatalf("api_listen=%s on %s: %v", mode, s.box.Alias, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("api_listen=%s on %s: exit %d\nstdout:%s\nstderr:%s",
			mode, s.box.Alias, res.ExitCode, res.Stdout, res.Stderr)
	}
	if mode != serverconfig.APIListenVPN {
		if err := itest.WaitForPort(ctx, s.box, itest.CarameloSSHPort); err != nil {
			t.Fatalf("the public API did not come back with api_listen=%s: %v", mode, err)
		}
	}
	t.Logf("api_listen is %q on %s", mode, s.box.Alias)
}

func (s *suite) zzTunnelSurvivesAPowerCycle(t *testing.T) {
	before := decode[capi.Status](t, "status",
		itest.ClientOK(t, s.opts(s.clientEnv()), "status", "--json").Stdout)
	if before.VPN == nil || before.VPN.PublicKey == "" {
		t.Fatalf("the machine reports no network to survive anything: %+v", before.VPN)
	}
	peersBefore := s.listPeers(t, s.clientEnv())

	itest.MustRestart(t, s.box)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(5*time.Minute))
	defer cancel()
	if err := itest.WaitForCaramelod(ctx, s.box); err != nil {
		t.Fatalf("caramelod did not come back on %s: %v", s.box.Alias, err)
	}
	if err := itest.WaitForPort(ctx, s.box, vpn.DefaultListenPort); err != nil {
		t.Fatalf("nothing listens on udp %d after the power cycle: %v", vpn.DefaultListenPort, err)
	}

	var onBox []state.Peer
	res, err := itest.RunAsUser(ctx, s.box, itest.CarameloUser, itest.CarameloBinary+" peer list --json")
	if err != nil {
		t.Fatalf("peer list on %s: %v", s.box.Alias, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("peer list on %s: exit %d: %s", s.box.Alias, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if err := decodeInto(res.Stdout, &onBox); err != nil {
		t.Fatalf("peer list --json on %s: %v\nstdout: %q", s.box.Alias, err, res.Stdout)
	}
	kept := map[string]string{}
	for _, p := range onBox {
		kept[p.Name] = p.IP
	}
	for name, p := range peersBefore {
		if kept[name] != p.IP {
			t.Errorf("peer %s was at %s before the power cycle and is %q after it: the peer table "+
				"must come back from the machine's own state", name, p.IP, kept[name])
		}
	}

	s.boxIP = s.box.MustAddress(t)
	s.tunnel = s.recordMachine(t)
	if err := itest.WriteClientHome(s.home, s.box); err != nil {
		t.Fatalf("rewrite the client home after the power cycle: %v", err)
	}

	after := decode[capi.Status](t, "status",
		itest.ClientOK(t, s.opts(s.clientEnv()), "status", "--json").Stdout)
	if after.Transport != remote.KindTunnel {
		t.Errorf("transport = %q, want %q after the power cycle", after.Transport, remote.KindTunnel)
	}
	if after.VPN == nil || !after.VPN.Enabled {
		t.Fatalf("the machine's network did not come back: %+v", after.VPN)
	}
	if after.VPN.PublicKey != before.VPN.PublicKey {
		t.Errorf("the machine's public key changed across a power cycle: %q -> %q; every peer's "+
			"configuration would be stale", before.VPN.PublicKey, after.VPN.PublicKey)
	}
	if after.VPN.Subnet != before.VPN.Subnet {
		t.Errorf("subnet = %q, want %q after the power cycle", after.VPN.Subnet, before.VPN.Subnet)
	}
	if after.Identity != hostPeer {
		t.Errorf("identity = %q, want %q: the peer is still who it was", after.Identity, hostPeer)
	}
}
