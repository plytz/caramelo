//go:build integration

package edge

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	cedge "github.com/plytz/caramelo/internal/edge"
	ccerts "github.com/plytz/caramelo/internal/edge/certs"
	"github.com/plytz/caramelo/internal/serverconfig"
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
	var back sync.Once
	restore := func() { back.Do(func() { restoreACME(t, m) }) }
	t.Cleanup(restore)
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

	restore()
	assertTheStoreKeepsWhatTheMachineNoLongerUses(t, ctx, m)
}

func assertTheStoreKeepsWhatTheMachineNoLongerUses(t *testing.T, ctx context.Context, m *itest.Machine) {
	t.Helper()

	live, err := internet.Certificate(ctx, hostX)
	if err != nil {
		t.Fatalf("read the certificate of %s after the machine went back to ACME: %v", hostX, err)
	}
	serial := itest.SerialOf(live)

	st := edgeStatus(t)
	stale := staleCertificate(t, st, hostInternal)
	t.Logf("%s is kept as stale under issuer key %q", stale.Host, stale.IssuerKey)
	if stale.IssuerKey == "" {
		t.Error("a stale certificate names no issuer key, so nothing says which tree it is in")
	}
	if stale.Managed {
		t.Error("a stale certificate is reported as managed; nothing renews an issuer the machine dropped")
	}
	served := liveCertificate(t, st, hostX)
	if served.IssuerKey == "" || served.IssuerKey == stale.IssuerKey {
		t.Errorf("%s is live under %q and the stale one under %q; a machine that changed issuer "+
			"has two trees", hostX, served.IssuerKey, stale.IssuerKey)
	}

	res := mustInRepo(t, "edge", "prune", "--older-than", "0", "--json")
	pruned := decode[ccerts.PruneResult](t, "edge prune", res.Stdout)
	if !namesHost(pruned.Removed, hostInternal) {
		t.Fatalf("edge prune removed %+v, want the certificate of %s among them", pruned.Removed, hostInternal)
	}
	for _, c := range pruned.Removed {
		if c.Host == hostX && c.IssuerKey == served.IssuerKey {
			t.Errorf("the prune removed the configured issuer's certificate for %s", hostX)
		}
	}

	after := edgeStatus(t)
	for _, c := range after.Certificates {
		if c.State == ccerts.Stale {
			t.Errorf("%s (%s) is still listed as stale after a prune with no retention", c.Host, c.IssuerKey)
		}
	}
	kept := liveCertificate(t, after, hostX)
	if kept.Serial != served.Serial {
		t.Errorf("%s is now certificate %s, was %s: a prune must not touch what is served",
			hostX, kept.Serial, served.Serial)
	}

	now, err := internet.GetWithin(ctx, urlX+"/", itest.Scale(60*time.Second))
	if err != nil {
		t.Fatalf("%s does not answer after a prune: %v", hostX, err)
	}
	if now.Cert == nil || itest.SerialOf(now.Cert) != serial {
		t.Errorf("%s is served with a different certificate after the prune", hostX)
	}

	root := serverconfig.Default().EdgeCertsDir() + "/caramelo/internal_ca/root.crt"
	if res := onBox(t, m, "sudo test -f "+root); res.ExitCode != 0 {
		t.Errorf("%s is gone: the internal CA's root is not certificate material of a previous "+
			"issuer and a prune must keep it", root)
	}
}

func staleCertificate(t *testing.T, st cedge.Status, host string) ccerts.Certificate {
	t.Helper()
	for _, c := range st.Certificates {
		if c.Host == host && c.State == ccerts.Stale {
			return c
		}
	}
	t.Fatalf("edge status lists no stale certificate for %s: %+v", host, st.Certificates)
	return ccerts.Certificate{}
}

func liveCertificate(t *testing.T, st cedge.Status, host string) ccerts.Certificate {
	t.Helper()
	for _, c := range st.Certificates {
		if c.Host == host && c.State == ccerts.Live {
			return c
		}
	}
	t.Fatalf("edge status lists no live certificate for %s: %+v", host, st.Certificates)
	return ccerts.Certificate{}
}

func namesHost(list []ccerts.Certificate, host string) bool {
	for _, c := range list {
		if c.Host == host {
			return true
		}
	}
	return false
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
