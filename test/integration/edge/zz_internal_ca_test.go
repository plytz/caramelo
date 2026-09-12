//go:build integration

package edge

import (
	"context"
	"strings"
	"testing"
	"time"

	ccerts "github.com/plytz/caramelo/internal/edge/certs"
	"github.com/plytz/caramelo/test/integration/itest"
)

const (
	internalDomain = "shop.internal"
	envInternal    = "feat-i"
	hostInternal   = envInternal + "." + internalDomain
)

func TestZZInternalCA(t *testing.T) {
	m := begin(t)
	needRepo(t)

	t.Run("a machine on ACME has nothing to trust by hand", func(t *testing.T) {
		res := inRepo(t, "edge", "ca")
		if res.ExitCode == 0 {
			t.Errorf("edge ca exited 0 on a machine issuing from a public CA; there is no root to print\nstdout:\n%s",
				res.Stdout)
		}
		if !strings.Contains(strings.ToLower(res.Stderr), "internal") {
			t.Errorf("stderr does not explain that this machine is not running tls: internal: %q", res.Stderr)
		}
	})

	cmd := "sudo " + itest.CarameloBinary + " edge enable --tls " + string(ccerts.ModeInternal)
	if res := onBox(t, m, cmd); res.ExitCode != 0 {
		t.Fatalf("edge enable --tls internal: exit %d\nstdout:%s\nstderr:%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	t.Cleanup(func() { restoreACME(t, m) })
	restartEdge(t, m)

	ca := func() ccerts.CA {
		res := mustInRepo(t, "edge", "ca", "--json")
		return decode[ccerts.CA](t, "edge ca", res.Stdout)
	}()
	if !strings.Contains(ca.PEM, "BEGIN CERTIFICATE") {
		t.Fatalf("edge ca printed no certificate: %q", ca.PEM)
	}
	t.Logf("the machine's own CA: %q (%s), expires %s", ca.Subject, ca.Fingerprint, ca.NotAfter)
	if ca.NotAfter.Before(time.Now()) {
		t.Errorf("the internal CA root expired at %v", ca.NotAfter)
	}

	createEnv(t, envInternal, "--from", branch, "--no-push")
	t.Cleanup(func() { destroyEnv(t, envInternal, "--delete-branch") })
	up(t, envInternal, "--no-push")
	mustInRepo(t, "env", "expose", envInternal, "--host", hostInternal, "--json")

	client, err := itest.NewEdgeClient(m, ca.PEM)
	if err != nil {
		t.Fatalf("a client trusting the machine's CA: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
	defer cancel()

	res, err := client.GetWithin(ctx, "https://"+hostInternal+"/", itest.Scale(90*time.Second))
	if err != nil {
		t.Fatalf("a client trusting the machine's root cannot reach %s: %v", hostInternal, err)
	}
	if !strings.Contains(res.Body, envInternal) {
		t.Errorf("%s answered %q, want it to name the environment", hostInternal, res.Body)
	}
	if res.Cert == nil {
		t.Fatalf("no certificate on the response")
	}
	if err := res.Cert.VerifyHostname(hostInternal); err != nil {
		t.Errorf("the certificate is not for %s: %v", hostInternal, err)
	}
	if strings.Contains(res.Cert.Issuer.CommonName, "Pebble") {
		t.Errorf("issuer = %q: a machine on tls: internal must not order from a CA at all",
			res.Cert.Issuer.CommonName)
	}
	t.Logf("%s is served with a certificate from %q", hostInternal, res.Cert.Issuer.CommonName)
}

func restoreACME(t *testing.T, m *itest.Machine) {
	t.Helper()
	cmd := "sudo " + itest.CarameloBinary + " edge enable --tls " + tlsModeACME
	if res := onBox(t, m, cmd); res.ExitCode != 0 {
		t.Errorf("put the machine back on tls %s: exit %d\nstdout:%s\nstderr:%s",
			tlsModeACME, res.ExitCode, res.Stdout, res.Stderr)
		return
	}
	restartEdge(t, m)
}

func TestZZZCacheImages(t *testing.T) {
	m := begin(t)
	itest.ExportImages(t, m, suiteImages...)
}
