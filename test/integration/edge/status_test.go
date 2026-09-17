//go:build integration

package edge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	cedge "github.com/plytz/caramelo/internal/edge"
	ccerts "github.com/plytz/caramelo/internal/edge/certs"
	"github.com/plytz/caramelo/internal/serverconfig"
	csetup "github.com/plytz/caramelo/internal/setup"
	"github.com/plytz/caramelo/test/integration/itest"
)

var tlsModeACME = serverconfig.DefaultTLS

func TestEdgeStatusAndAccessLog(t *testing.T) {
	begin(t)
	needRepo(t)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
	defer cancel()

	nonce := fmt.Sprintf("status-probe-%d", time.Now().UnixNano())
	internet.GetOK(t, ctx, urlX+"/?"+nonce)

	st := edgeStatus(t)
	t.Logf("edge status: %d route(s), %d certificate(s), http3=%v, tls=%s, ca=%s",
		len(st.Routes), len(st.Certificates), st.HTTP3, st.TLS, st.ACMEDirectory)

	t.Run("the machine reports what it is serving", func(t *testing.T) {
		if !st.Running {
			t.Fatalf("edge status says the edge is not running: %s", st.Error)
		}
		if !st.HTTP3 {
			t.Errorf("http3 = false, want true: it is on by default and the suite just used it")
		}
		if string(st.TLS) != tlsModeACME {
			t.Errorf("tls = %q, want %q: this machine issues from a CA over ACME", st.TLS, tlsModeACME)
		}
		if st.ACMEDirectory != itest.PebbleDirectory {
			t.Errorf("acme directory = %q, want the lab's %q", st.ACMEDirectory, itest.PebbleDirectory)
		}
		if !anyContains(st.Listeners, "443") {
			t.Errorf("listeners = %v, want the sockets systemd handed over to include 443", st.Listeners)
		}
	})

	t.Run("both routes, with their targets", func(t *testing.T) {
		for _, host := range []string{hostX, hostSecond} {
			route := routeOf(t, "edge status", st.Routes, host)
			if len(route.Targets) == 0 {
				t.Errorf("route %s has no targets", host)
				continue
			}
			for _, target := range route.Targets {
				if target.Port == 0 {
					t.Errorf("route %s target %d has no port", host, target.Replica)
				}
				if !target.State.Valid() {
					t.Errorf("route %s target %d is in state %q", host, target.Replica, target.State)
				}
			}
			if len(route.Active()) == 0 {
				t.Errorf("route %s has no active target: %+v", host, route.Targets)
			}
		}
	})

	t.Run("both certificates, with their expiries", func(t *testing.T) {
		byHost := map[string]bool{}
		for _, c := range st.Certificates {
			if c.State != ccerts.Live {
				continue
			}
			byHost[c.Host] = true
			if c.NotAfter.IsZero() || c.NotAfter.Before(time.Now()) {
				t.Errorf("certificate for %s expires at %v", c.Host, c.NotAfter)
			}
			if c.Serial == "" {
				t.Errorf("certificate for %s has no serial: a renewal is a different serial for the same host", c.Host)
			}
		}
		for _, host := range []string{hostX, hostSecond} {
			if !byHost[host] {
				t.Errorf("no live certificate for %s in edge status: %+v", host, st.Certificates)
			}
		}
	})

	t.Run("a machine that never changed issuer has nothing stale", func(t *testing.T) {
		for _, c := range st.Certificates {
			if c.State != ccerts.Live {
				t.Errorf("certificate for %s is %q under issuer key %q; this machine has only ever "+
					"used one certificate authority", c.Host, c.State, c.IssuerKey)
			}
			if c.IssuerKey == "" {
				t.Errorf("certificate for %s names no issuer key, so nothing says which tree holds it", c.Host)
			}
			if !c.Managed {
				t.Errorf("certificate for %s is not managed, so nothing would renew it", c.Host)
			}
		}
	})

	t.Run("the access log has the requests, with the target that answered", func(t *testing.T) {
		res := mustInRepo(t, "logs", envX, "--edge", "--tail", "500", "--json")
		var found bool
		var lines int
		for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var entry cedge.AccessLog
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("logs --edge --json: %v in %q", err, line)
			}
			lines++
			if entry.Host != hostX || !strings.Contains(entry.Path, nonce) {
				continue
			}
			found = true
			if entry.Status != 200 {
				t.Errorf("the access log says HTTP %d for a request that answered 200", entry.Status)
			}
			if entry.Target == "" || entry.Replica == 0 {
				t.Errorf("the access log line names no target: %+v", entry)
			}
			if entry.Env != envX || entry.Service != webService {
				t.Errorf("the access log line says %s/%s, want %s/%s", entry.Env, entry.Service, envX, webService)
			}
		}
		if !found {
			t.Errorf("no access-log line for GET %s/?%s among %d lines", urlX, nonce, lines)
		}
	})
}

const (
	renewalRatioEnv    = "CARAMELO_EDGE_RENEWAL_WINDOW_RATIO"
	renewalIntervalEnv = "CARAMELO_EDGE_RENEW_CHECK_INTERVAL"
	renewalProfileEnv  = "CARAMELO_EDGE_ACME_PROFILE"
)

const renewalDropIn = csetup.SystemUnitDir + "/" + csetup.EdgeServiceUnit + ".d/lab-renewal.conf"

func TestCertificatesAreRenewed(t *testing.T) {
	m := begin(t)
	needRepo(t)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(5*time.Minute))
	defer cancel()

	before, err := internet.Certificate(ctx, hostX)
	if err != nil {
		t.Fatalf("read the certificate: %v", err)
	}
	serial := itest.SerialOf(before)
	t.Logf("%s is served with certificate %s, valid until %s", hostX, serial, before.NotAfter)

	dropIn := "[Service]\n" +
		"Environment=" + renewalProfileEnv + "=shortlived\n" +
		"Environment=" + renewalRatioEnv + "=1.0\n" +
		"Environment=" + renewalIntervalEnv + "=5s\n"
	writeDropIn(t, m, dropIn)
	t.Cleanup(func() {
		removeDropIn(t, m)
	})
	restartEdge(t, m)

	if _, err := internet.GetWithin(ctx, urlX+"/", itest.Scale(30*time.Second)); err != nil {
		t.Fatalf("the edge does not serve after a restart: %v", err)
	}

	deadline := time.Now().Add(itest.Scale(2 * time.Minute))
	for {
		now, err := internet.Certificate(ctx, hostX)
		if err == nil && itest.SerialOf(now) != serial {
			t.Logf("renewed: %s -> %s (valid until %s)", serial, itest.SerialOf(now), now.NotAfter)
			if now.NotAfter.Before(time.Now()) {
				t.Errorf("the renewed certificate is already expired: %v", now.NotAfter)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is still served with certificate %s two minutes after the renewal window was opened; "+
				"the edge must renew on its own (the lab widens the window with %s=1.0 and %s=5s)",
				hostX, serial, renewalRatioEnv, renewalIntervalEnv)
		}
		time.Sleep(3 * time.Second)
	}
}

func writeDropIn(t *testing.T, m *itest.Machine, content string) {
	t.Helper()
	dir := csetup.SystemUnitDir + "/" + csetup.EdgeServiceUnit + ".d"
	enc := base64.StdEncoding.EncodeToString([]byte(content))
	cmd := fmt.Sprintf("sudo mkdir -p %s && printf %%s %s | base64 -d | sudo tee %s >/dev/null && sudo systemctl daemon-reload",
		dir, enc, renewalDropIn)
	if res := onBox(t, m, cmd); res.ExitCode != 0 {
		t.Fatalf("install %s: exit %d: %s", renewalDropIn, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
}

func removeDropIn(t *testing.T, m *itest.Machine) {
	t.Helper()
	cmd := fmt.Sprintf("sudo rm -f %s && sudo systemctl daemon-reload", renewalDropIn)
	if res := onBox(t, m, cmd); res.ExitCode != 0 {
		t.Errorf("remove %s: exit %d: %s", renewalDropIn, res.ExitCode, strings.TrimSpace(res.Stderr))
		return
	}
	restartEdge(t, m)
}

func anyContains(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}
