//go:build integration

package edge

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	capi "github.com/plytz/caramelo/internal/api"
	cedge "github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/test/integration/itest"
)

func TestExposeAndUnexpose(t *testing.T) {
	m := begin(t)
	needRepo(t)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(5*time.Minute))
	defer cancel()

	before, err := internet.Certificate(ctx, hostX)
	if err != nil {
		t.Fatalf("read the certificate before unexposing: %v", err)
	}
	serial := itest.SerialOf(before)
	t.Logf("%s is served with certificate %s", hostX, serial)

	detail := showEnv(t, envX)
	web := serviceOf(t, "env show "+envX, detail.Services, webService)
	onBoxURL := web.URL

	res := mustInRepo(t, "env", "unexpose", envX, "--json")
	out := decode[capi.ExposeResult](t, "env unexpose "+envX, res.Stdout)
	if !contains(out.Changed, hostX) {
		t.Errorf("unexpose changed %v, want it to name %s", out.Changed, hostX)
	}
	if len(out.Routes) != 0 {
		t.Errorf("the environment still has routes after unexpose: %+v", out.Routes)
	}

	t.Run("the name stops being served at once", func(t *testing.T) {
		deadline := time.Now().Add(itest.Scale(2 * time.Second))
		var lastErr error
		for {
			if lastErr = internet.Handshake(ctx, hostX); lastErr != nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s still completes a TLS handshake 2s after unexpose", hostX)
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("%s: %v", hostX, lastErr)
		res, err := internet.Get(ctx, "http://"+hostX+"/")
		if err != nil {
			t.Fatalf("GET http://%s/: %v", hostX, err)
		}
		if res.Status != http.StatusNotFound {
			t.Errorf("HTTP %d on port 80 for an unexposed name, want 404", res.Status)
		}
	})

	t.Run("the environment is still running", func(t *testing.T) {
		if onBoxURL == "" {
			t.Fatalf("env show %s reported no address for %s: a running service has a port on the machine, "+
				"and without it this case proves nothing", envX, webService)
		}
		got := onBox(t, m, "curl -fsS --max-time 10 "+itest.ShellQuote(onBoxURL+"/"))
		if got.ExitCode != 0 {
			t.Fatalf("the service does not answer on the box at %s: exit %d\n%s",
				onBoxURL, got.ExitCode, got.Stderr)
		}
		if !strings.Contains(got.Stdout, envX) {
			t.Errorf("the service answered %q, want it to name the environment", got.Stdout)
		}
	})

	t.Run("unexposing again is not an error", func(t *testing.T) {
		if res := inRepo(t, "env", "unexpose", envX); res.ExitCode != 0 {
			t.Errorf("a second unexpose: exit %d, want 0\nstderr:\n%s", res.ExitCode, res.Stderr)
		}
	})

	t.Run("exposing it again reuses the certificate", func(t *testing.T) {
		res := mustInRepo(t, "env", "expose", envX, "--json")
		out := decode[capi.ExposeResult](t, "env expose "+envX, res.Stdout)
		if out.URL != urlX {
			t.Errorf("expose url = %q, want %q", out.URL, urlX)
		}
		routeOf(t, "env expose "+envX, out.Routes, hostX)

		got, err := internet.GetWithin(ctx, urlX+"/", itest.Scale(30*time.Second))
		if err != nil {
			t.Fatalf("%v", err)
		}
		if now := got.Serial(); now != serial {
			t.Errorf("the certificate changed from %s to %s: exposing a name again must not order a new one",
				serial, now)
		}
	})

	t.Run("--host routes a second name at the same environment", func(t *testing.T) {
		res := mustInRepo(t, "env", "expose", envX, "--host", hostSecond, "--json")
		out := decode[capi.ExposeResult](t, "env expose --host", res.Stdout)
		if !contains(out.Changed, hostSecond) {
			t.Errorf("expose --host changed %v, want it to name %s", out.Changed, hostSecond)
		}
		hosts := make([]string, 0, len(out.Routes))
		for _, r := range out.Routes {
			hosts = append(hosts, r.Host)
		}
		if !contains(hosts, hostX) || !contains(hosts, hostSecond) {
			t.Errorf("the environment's routes are %v, want both %s and %s", hosts, hostX, hostSecond)
		}

		got, err := internet.GetWithin(ctx, "https://"+hostSecond+"/", itest.Scale(90*time.Second))
		if err != nil {
			t.Fatalf("%v", err)
		}
		if !strings.Contains(got.Body, envX) {
			t.Errorf("%s answered %q, want the same environment", hostSecond, got.Body)
		}
		if got.Cert == nil || got.Cert.VerifyHostname(hostSecond) != nil {
			t.Errorf("%s was served a certificate that is not for it", hostSecond)
		}
		if got.Serial() == serial {
			t.Errorf("%s and %s share certificate %s; each name gets its own", hostX, hostSecond, serial)
		}
	})
}

func TestFourEnvironmentsAtOnce(t *testing.T) {
	begin(t)
	needRepo(t)

	names := []string{"par-a", "par-b", "par-c", "par-d"}
	t.Cleanup(func() {
		for _, name := range names {
			destroyEnv(t, name, "--delete-branch")
		}
	})
	for _, name := range names {
		createEnv(t, name, "--from", branch, "--no-push")
	}

	var wg sync.WaitGroup
	results := make([]itest.Result, len(names))
	errs := make([]error, len(names))
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			results[i], errs[i] = commanderExec(commanderOpts{Dir: repo, Timeout: itest.Scale(12 * time.Minute)},
				"up", name, "--no-push", "--json")
		}(i, name)
	}
	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(5*time.Minute))
	defer cancel()
	for i, name := range names {
		res := results[i]
		if errs[i] != nil {
			t.Errorf("up %s: %v\nstderr:\n%s", name, errs[i], res.Stderr)
			continue
		}
		if res.ExitCode != 0 {
			t.Errorf("up %s: exit %d\nstderr:\n%s", name, res.ExitCode, res.Stderr)
			continue
		}
		out := decode[capi.UpResult](t, "up "+name, res.Stdout)
		want := fmt.Sprintf("https://%s.%s", name, domain)
		if out.URL != want {
			t.Errorf("up %s: url %q, want %q", name, out.URL, want)
			continue
		}
		got, err := internet.GetWithin(ctx, want+"/", itest.Scale(90*time.Second))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !strings.Contains(got.Body, name) {
			t.Errorf("%s answered %q, want it to name %s: two names must never share a pool", want, got.Body, name)
		}
	}

	t.Run("the machine reports every route", func(t *testing.T) {
		st := edgeStatus(t)
		hosts := map[string]cedge.Route{}
		for _, r := range st.Routes {
			hosts[r.Host] = r
		}
		for _, name := range names {
			host := name + "." + domain
			if _, ok := hosts[host]; !ok {
				t.Errorf("edge status does not list %s: it has %v", host, st.Routes)
			}
		}
	})
}
