//go:build integration

package edge

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	cedge "github.com/plytz/caramelo/internal/edge"
	cenv "github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/test/integration/itest"
)

const (
	appName = "sampleapp"

	branch     = "edge"
	domain     = "shop.test"
	webService = "web"

	envX  = "feat-x"
	hostX = envX + "." + domain
	urlX  = "https://" + hostX

	hostUnknown = "nope." + domain

	hostSecond = "other." + domain

	edgePort = "443"
)

func TestUpExposesTheEnvironment(t *testing.T) {
	m := begin(t)
	repo = initSampleRepo(t)

	e := createEnv(t, envX, "--from", branch)
	if e.Commit != firstCommit {
		t.Fatalf("env create %s: commit %q, want %q", envX, e.Commit, firstCommit)
	}

	out := up(t, envX)
	t.Logf("up %s: url %q, %d route(s)", envX, out.URL, len(out.Routes))

	if out.URL != urlX {
		t.Errorf("up --json url = %q, want %q: <env>.<domain> is the first exposed service's name",
			out.URL, urlX)
	}
	route := routeOf(t, "up "+envX, out.Routes, hostX)
	if route.Kind != cedge.KindHTTPS {
		t.Errorf("route %s kind = %q, want %q", hostX, route.Kind, cedge.KindHTTPS)
	}
	if route.Env != envX || route.Service != webService {
		t.Errorf("route %s points at %s/%s, want %s/%s", hostX, route.Env, route.Service, envX, webService)
	}
	if got := len(route.Active()); got != 2 {
		t.Errorf("route %s has %d active target(s), want 2: %+v", hostX, got, route.Targets)
	}

	web := serviceOf(t, "up "+envX, out.Services, webService)
	if len(web.Replicas) != 2 {
		t.Errorf("web has %d replica(s), want 2: %+v", len(web.Replicas), web.Replicas)
	}
	if web.Host != hostX || web.PublicURL != urlX {
		t.Errorf("web is published at %q (%q), want %q (%q)", web.Host, web.PublicURL, hostX, urlX)
	}
	if roll := rolloutOf(t, "up "+envX, out.Rollouts, webService); roll.Failed != nil {
		t.Errorf("the first rollout failed at %+v; steps: %v", roll.Failed, steps(roll))
	}

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
	defer cancel()

	t.Run("the first request gets a certificate", func(t *testing.T) {
		started := time.Now()
		res, err := internet.GetWithin(ctx, urlX+"/", itest.Scale(90*time.Second))
		if err != nil {
			t.Fatalf("%v", err)
		}
		if res.Cert == nil {
			t.Fatal("no certificate on an https response")
		}
		t.Logf("the first request took %s and got a certificate from %q",
			time.Since(started).Round(time.Millisecond), res.Cert.Issuer.CommonName)
		if !strings.Contains(res.Cert.Issuer.CommonName, "Pebble") {
			t.Errorf("issuer = %q, want the lab's ACME server: nothing else may have signed this",
				res.Cert.Issuer.CommonName)
		}
		if err := res.Cert.VerifyHostname(hostX); err != nil {
			t.Errorf("the certificate is not for %s: %v", hostX, err)
		}
		if !strings.Contains(res.Body, envX) {
			t.Errorf("GET %s = %q, want it to name the environment", urlX, res.Body)
		}
		if got := versionOf(res.Body); got != "v1" {
			t.Errorf("version = %q, want v1", got)
		}
	})

	t.Run("twenty requests see both replicas", func(t *testing.T) {
		client := internet.Client(0)
		defer client.CloseIdleConnections()
		seen := map[string]int{}
		for i := 0; i < 20; i++ {
			res, err := internet.Do(ctx, client, urlX+"/")
			if err != nil {
				t.Fatalf("request %d: %v", i+1, err)
			}
			if res.Status != http.StatusOK {
				t.Fatalf("request %d: HTTP %d", i+1, res.Status)
			}
			seen[replicaOf(res.Body)]++
		}
		if len(seen) != 2 {
			t.Errorf("20 requests reached %d replica(s) (%v), want both: round-robin is the point of a pool", len(seen), seen)
		}
		t.Logf("round-robin over the pool: %v", seen)
	})

	t.Run("one container per replica", func(t *testing.T) {
		names := dockerNames(t, m, "label="+cenv.LabelEnv+"="+envX, true)
		for _, index := range []int{1, 2} {
			want := cenv.ReplicaContainerName(appName, envX, webService, index)
			if !contains(names, want) {
				t.Errorf("no container %s; the environment has %v", want, names)
			}
		}
	})
}

func altSvcPort(header, proto string) string {
	for _, entry := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok || strings.TrimSpace(key) != proto {
			continue
		}
		authority, _, _ := strings.Cut(strings.TrimSpace(value), ";")
		authority = strings.Trim(strings.TrimSpace(authority), `"`)
		if _, port, err := net.SplitHostPort(authority); err == nil {
			return port
		}
		return strings.TrimPrefix(authority, ":")
	}
	return ""
}

func TestGossEdge(t *testing.T) {
	m := begin(t)
	needRepo(t)
	itest.RunGoss(t, m, itest.MustGossSpec(t, "edge.yaml"))
}

func TestProtocols(t *testing.T) {
	begin(t)
	needRepo(t)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
	defer cancel()

	t.Run("http/2 with the alt-svc advertisement", func(t *testing.T) {
		res := internet.GetOK(t, ctx, urlX+"/")
		if res.Proto != "HTTP/2.0" {
			t.Errorf("proto = %q, want HTTP/2.0: h2 is offered in the ALPN", res.Proto)
		}
		alt := res.AltSvc()
		if !strings.Contains(alt, "h3") {
			t.Fatalf("alt-svc = %q, want it to advertise h3: that is how a browser finds HTTP/3", alt)
		}
		if got := altSvcPort(alt, "h3"); got != edgePort {
			t.Errorf("alt-svc = %q advertises h3 on port %q, want %q: the advertisement carries the udp "+
				"socket's own port, so a udp socket that did not end up on the tls listener's port sends "+
				"every browser to a port nothing answers on", alt, got, edgePort)
		}
	})

	t.Run("http/1.1 for a client that does not offer h2", func(t *testing.T) {
		res, err := internet.Get1(ctx, urlX+"/")
		if err != nil {
			t.Fatalf("%v", err)
		}
		if res.Status != http.StatusOK || res.Proto != "HTTP/1.1" {
			t.Errorf("HTTP %d %s, want 200 HTTP/1.1", res.Status, res.Proto)
		}
	})

	t.Run("http/3 on the udp socket", func(t *testing.T) {
		res, err := internet.HTTP3Get(ctx, urlX+"/")
		if err != nil {
			t.Fatalf("%v", err)
		}
		if res.Status != http.StatusOK {
			t.Fatalf("HTTP %d over quic", res.Status)
		}
		if res.Proto != "HTTP/3.0" {
			t.Errorf("proto = %q, want HTTP/3.0", res.Proto)
		}
		if !strings.Contains(res.Body, envX) {
			t.Errorf("GET over http/3 = %q, want it to name the environment", res.Body)
		}
		if res.Cert == nil || res.Cert.VerifyHostname(hostX) != nil {
			t.Errorf("http/3 served no certificate for %s", hostX)
		}
	})

	t.Run("a websocket echoes through the edge", func(t *testing.T) {
		ws, err := internet.DialWebSocket(ctx, "wss://"+hostX+"/ws")
		if err != nil {
			t.Fatalf("%v", err)
		}
		defer ws.Close()
		if err := ws.Deadline(time.Now().Add(itest.Scale(30 * time.Second))); err != nil {
			t.Fatalf("deadline: %v", err)
		}
		const msg = "hello through the edge"
		got, err := ws.Echo(msg)
		if err != nil {
			t.Fatalf("echo: %v", err)
		}
		if got != msg {
			t.Errorf("echo = %q, want %q", got, msg)
		}
	})

	t.Run("port 80 redirects a name the machine serves", func(t *testing.T) {
		res, err := internet.Get(ctx, "http://"+hostX+"/some/path")
		if err != nil {
			t.Fatalf("%v", err)
		}
		if res.Status != http.StatusMovedPermanently && res.Status != http.StatusPermanentRedirect {
			t.Errorf("HTTP %d on port 80, want a permanent redirect", res.Status)
		}
		if loc := res.Header.Get("Location"); !strings.HasPrefix(loc, "https://"+hostX) {
			t.Errorf("Location = %q, want https://%s/some/path", loc, hostX)
		}
	})

	t.Run("an unknown host gets nothing", func(t *testing.T) {
		if err := internet.Handshake(ctx, hostUnknown); err == nil {
			t.Errorf("the handshake for %s succeeded; an unknown name must not get a certificate", hostUnknown)
		} else {
			t.Logf("%s: %v", hostUnknown, err)
		}
		res, err := internet.Get(ctx, "http://"+hostUnknown+"/")
		if err != nil {
			t.Fatalf("GET http://%s/: %v", hostUnknown, err)
		}
		if res.Status != http.StatusNotFound {
			t.Errorf("HTTP %d for an unknown host on port 80, want 404", res.Status)
		}
	})
}
