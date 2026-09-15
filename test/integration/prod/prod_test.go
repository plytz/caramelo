//go:build integration

package prod

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	cedge "github.com/plytz/caramelo/internal/edge"
	cenv "github.com/plytz/caramelo/internal/env"
	cprogress "github.com/plytz/caramelo/internal/progress"
	crelease "github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/test/integration/itest"
)

const (
	appName    = "sampleapp"
	branch     = "prod"
	domain     = "shop.test"
	webService = "web"
	dbDep      = "db"

	envProd    = "production"
	envDev     = "feat-x"
	envPreview = "pr-41"
	envStaging = "staging"
	envOOM     = "oom"

	hostProd    = domain
	hostProdWWW = "www." + domain
	urlProd     = "https://" + hostProd
	urlProdWWW  = "https://" + hostProdWWW

	hostDev     = envDev + "." + domain
	hostPreview = envPreview + "." + domain
	urlDev      = "https://" + hostDev
	urlPreview  = "https://" + hostPreview

	hostStaging = envStaging + "." + domain
	urlStaging  = "https://" + hostStaging

	chosenPassword  = "chosen-by-the-operator"
	defaultPassword = "caramelo"

	appGreeting  = "hello"
	prodGreeting = "hi"
)

func TestSecretsBeforeCreate(t *testing.T) {
	m := begin(t)
	repo = initSampleRepo(t)

	set := secretsSet(t, envProd, "DB_PASSWORD="+chosenPassword)
	if !contains(set.Changed, "DB_PASSWORD") {
		t.Fatalf("secrets set wrote %v, want DB_PASSWORD", set.Changed)
	}
	for _, e := range set.Entries {
		if e.Value != "" {
			t.Errorf("secrets set handed back the value of %s", e.Name)
		}
	}
	secretsSet(t, "--app-scope", "GREETING="+appGreeting)

	t.Run("a value never reaches the machine's argv", func(t *testing.T) {
		pattern := strings.Replace(chosenPassword, "-", "[-]", 1)
		res := onBox(t, m, "sudo grep -rl "+itest.ShellQuote(pattern)+" /var/log 2>/dev/null || true")
		if out := strings.TrimSpace(res.Stdout); out != "" {
			t.Errorf("the chosen password is in %s", out)
		}
	})

	prod := createEnv(t, envProd, "--production", "--from", branch)
	if prod.Mode != cenv.ModeRelease {
		t.Errorf("env create --production: mode %q, want %q", prod.Mode, cenv.ModeRelease)
	}
	if !prod.Protected {
		t.Error("env create --production left the environment unprotected")
	}
	if prod.Commit != firstCommit {
		t.Errorf("env create %s: commit %q, want %q", envProd, prod.Commit, firstCommit)
	}

	dev := createEnv(t, envDev, "--from", branch)
	if dev.Mode != cenv.ModeDev {
		t.Errorf("env create %s: mode %q, want %q", envDev, dev.Mode, cenv.ModeDev)
	}
	if dev.Protected {
		t.Errorf("env create %s is protected; only --production and --protected do that", envDev)
	}
	createEnv(t, envPreview, "--from", branch)

	t.Run("the dependency took the password the user chose", func(t *testing.T) {
		ok := psql(t, m, envProd, chosenPassword, "select 1")
		if ok.ExitCode != 0 || !strings.Contains(ok.Stdout, "1") {
			t.Fatalf("psql with the chosen password: exit %d\nstdout: %s\nstderr: %s",
				ok.ExitCode, ok.Stdout, ok.Stderr)
		}
		bad := psql(t, m, envProd, defaultPassword, "select 1")
		if bad.ExitCode == 0 {
			t.Error("psql got in with the known-image default password: " +
				"a release environment must never run with it")
		}
	})

	t.Run("a release environment with no secret gets a generated password", func(t *testing.T) {
		st := createEnv(t, envStaging, "--release", "--from", branch)
		if st.Mode != cenv.ModeRelease {
			t.Fatalf("env create --release: mode %q, want %q", st.Mode, cenv.ModeRelease)
		}
		if st.Protected {
			t.Error("--release on its own must not protect: only --production does")
		}
		hidden := secretsExport(t, envStaging)
		if hidden.Revealed {
			t.Error("an export nobody asked to reveal says it was revealed")
		}
		if got := hidden.Values["DB_PASSWORD"]; got == "" {
			t.Fatalf("staging has no DB_PASSWORD: %v", hidden.Values)
		} else if got != "<secret>" {
			t.Errorf("a redacted export printed %q for DB_PASSWORD", got)
		}
		shown := secretsExport(t, envStaging, "--reveal")
		generated := shown.Values["DB_PASSWORD"]
		switch {
		case generated == "":
			t.Fatal("staging's generated DB_PASSWORD is empty")
		case generated == defaultPassword:
			t.Error("staging got the known-image default password, not a generated one")
		case generated == chosenPassword:
			t.Error("staging got production's password")
		case len(generated) < 16:
			t.Errorf("the generated password is %d characters, which is not a password", len(generated))
		}
		if ok := psql(t, m, envStaging, generated, "select 1"); ok.ExitCode != 0 {
			t.Errorf("psql with staging's generated password: exit %d: %s", ok.ExitCode, ok.Stderr)
		}
	})

	t.Run("env list shows the modes", func(t *testing.T) {
		list := listEnvs(t)
		for name, want := range map[string]cenv.Mode{
			envProd:    cenv.ModeRelease,
			envStaging: cenv.ModeRelease,
			envDev:     cenv.ModeDev,
			envPreview: cenv.ModeDev,
		} {
			if got := envNamed(t, list, name).Mode; got != want {
				t.Errorf("env list: %s is %q, want %q", name, got, want)
			}
		}
	})

	t.Run("env show says release mode and no release", func(t *testing.T) {
		detail := showEnv(t, envProd)
		if detail.Env.Mode != cenv.ModeRelease {
			t.Errorf("env show %s: mode %q", envProd, detail.Env.Mode)
		}
		if detail.Release != nil {
			t.Errorf("env show %s names release %v before any deploy", envProd, detail.Release.Short())
		}
		if detail.Deploy != nil {
			t.Errorf("env show %s names a deploy in progress before any deploy", envProd)
		}
		for _, svc := range detail.Services {
			if svc.Status == cenv.ServiceRunning {
				t.Errorf("env show %s: service %q is %q before any deploy", envProd, svc.Name, svc.Status)
			}
			for _, r := range svc.Replicas {
				if r.Status == cenv.ServiceRunning {
					t.Errorf("env show %s: %s/%d is running before any deploy", envProd, svc.Name, r.Index)
				}
			}
		}
	})

	t.Run("up is refused on a release environment", func(t *testing.T) {
		res := inRepo(t, "up", envProd)
		if res.ExitCode == 0 {
			t.Fatal("caramelo up succeeded on a release environment")
		}
		if !strings.Contains(res.Stderr, "deploy") {
			t.Errorf("the refusal does not name deploy: %q", res.Stderr)
		}
	})

	t.Run("deploy is refused on a development environment", func(t *testing.T) {
		res := inRepo(t, "deploy", envDev)
		if res.ExitCode == 0 {
			t.Fatal("caramelo deploy succeeded on a development environment")
		}
		if !strings.Contains(res.Stderr, "--release") {
			t.Errorf("the refusal does not name --release: %q", res.Stderr)
		}
	})
}

func TestGossProd(t *testing.T) {
	m := begin(t)
	needRepo(t)
	itest.RunGoss(t, m, itest.MustGossSpec(t, "prod.yaml"))
}

func TestFirstDeploy(t *testing.T) {
	m := begin(t)
	needRepo(t)

	secretsSet(t, envProd, "GREETING="+prodGreeting)

	t.Run("secrets list says which layer wins, and prints no value", func(t *testing.T) {
		list := secretsList(t, envProd)
		var scopes []string
		for _, e := range list.Entries {
			if e.Value != "" {
				t.Errorf("secrets list printed the value of %s", e.Name)
			}
			if e.Name == "GREETING" {
				scopes = append(scopes, string(e.Scope))
			}
		}
		if len(scopes) != 2 {
			t.Errorf("GREETING exists at %v, want two scopes (the app's and the environment's)", scopes)
		}
		if got := list.Resolved["GREETING"]; got != "env" {
			t.Errorf("GREETING resolves from %q, want the environment's scope", got)
		}
	})

	out := deploy(t, envProd)
	d := out.Deploy
	t.Logf("deploy %s: %s %s, %d step(s): %v", envProd, d.Kind, d.Status, len(d.Steps), stepNames(d))

	if d.Status != cenv.DeployPromoted {
		t.Fatalf("deploy %s ended %q, want %q; steps: %v", envProd, d.Status, cenv.DeployPromoted, stepNames(d))
	}
	if d.Kind != cenv.DeployKindDeploy {
		t.Errorf("kind = %q, want %q", d.Kind, cenv.DeployKindDeploy)
	}
	if d.Release == nil {
		t.Fatal("a promoted deploy with no release")
	}
	firstRelease = d.Release.Short()
	if d.Release.Commit != firstCommit {
		t.Errorf("the release is of commit %q, want %q", d.Release.Commit, firstCommit)
	}

	t.Run("every gate ran, in order", func(t *testing.T) {
		want := []cenv.DeployStepName{
			cenv.StepBuild, cenv.StepBefore, cenv.StepReplicas,
			cenv.StepCheck, cenv.StepSwitch, cenv.StepWatch, cenv.StepPromote,
		}
		at := 0
		for _, s := range d.Steps {
			if at < len(want) && s.Step == want[at] {
				at++
			}
		}
		if at != len(want) {
			t.Errorf("the walk reached %d of %d gates; steps: %v", at, len(want), stepNames(d))
		}
		for _, name := range want {
			step, ok := stepOf(d, name)
			if !ok {
				t.Errorf("no %s step", name)
				continue
			}
			if step.Status == cenv.StepFailed {
				t.Errorf("%s failed: %s", name, step.Detail)
			}
		}
	})

	t.Run("the migration left a row in postgres", func(t *testing.T) {
		res := psql(t, m, envProd, chosenPassword,
			"select version from migrations order by id desc limit 1")
		if res.ExitCode != 0 {
			t.Fatalf("psql: exit %d: %s", res.ExitCode, res.Stderr)
		}
		if got := strings.TrimSpace(res.Stdout); got != currentVersion {
			t.Errorf("the newest migration is %q, want %q: deploy.before did not run for this release",
				got, currentVersion)
		}
	})

	t.Run("the images exist by their tree tag", func(t *testing.T) {
		ref := crelease.ImageRef(appName, webService, d.Release.Tree)
		if !imageExists(t, m, ref) {
			t.Errorf("no image %s on the box; the release says %v", ref, d.Release.ImageList())
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(12*time.Minute))
	defer cancel()

	for _, url := range []string{urlProd, urlProdWWW} {
		t.Run("the world sees "+url, func(t *testing.T) {
			res, err := internet.GetWithin(ctx, url+"/", itest.Scale(90*time.Second))
			if err != nil {
				t.Fatalf("%v", err)
			}
			if res.Status != http.StatusOK {
				t.Fatalf("GET %s: HTTP %d", url, res.Status)
			}
			if res.Cert == nil {
				t.Fatal("no certificate on an https response")
			}
			if !strings.Contains(res.Cert.Issuer.CommonName, "Pebble") {
				t.Errorf("issuer = %q, want the lab's ACME server", res.Cert.Issuer.CommonName)
			}
			if !strings.HasPrefix(res.Body, prodGreeting+" "+envProd) {
				t.Errorf("GET %s = %q, want it to start with the environment's own GREETING secret",
					url, res.Body)
			}
			if got := versionOf(res.Body); got != currentVersion {
				t.Errorf("version = %q, want %q", got, currentVersion)
			}
		})
	}

	t.Run("the history has one row and env show names the release", func(t *testing.T) {
		hist := releases(t, envProd)
		if len(hist.Deploys) != 1 {
			t.Fatalf("%d deploy(s) in the history, want 1: %+v", len(hist.Deploys), hist.Deploys)
		}
		if hist.Current == nil || hist.Current.Short() != firstRelease {
			t.Errorf("the history's current release is %v, want %s", hist.Current, firstRelease)
		}
		detail := showEnv(t, envProd)
		if detail.Release == nil || detail.Release.Short() != firstRelease {
			t.Errorf("env show names release %v, want %s", detail.Release, firstRelease)
		}
		if detail.Deploy != nil {
			t.Errorf("env show names a deploy in progress after a promoted one: %+v", detail.Deploy)
		}
		web := serviceOf(t, "env show "+envProd, detail.Services, webService)
		if len(web.Replicas) != 2 {
			t.Errorf("web has %d replica(s), want 2", len(web.Replicas))
		}
	})

	t.Run("caramelo build of a tree that was built is a no-op", func(t *testing.T) {
		again := buildRelease(t, envProd, "--no-push")
		if again.Release == nil {
			t.Fatal("build answered no release")
		}
		if again.Release.Short() != firstRelease {
			t.Errorf("build made release %s, want the one the deploy built (%s)",
				again.Release.Short(), firstRelease)
		}
		if again.Built {
			t.Error("build of a tree that already has its images reported that it built")
		}
		forced := buildRelease(t, envProd, "--no-push", "--force")
		if !forced.Built {
			t.Error("build --force reported that it built nothing")
		}
		if forced.Release == nil || forced.Release.Short() != firstRelease {
			t.Errorf("build --force made release %v, want %s", forced.Release, firstRelease)
		}
	})

	t.Run("the other two environments are untouched", func(t *testing.T) {
		for _, env := range []string{envDev, envPreview} {
			mustInRepo(t, "up", env, "--json")
		}
		for _, url := range []string{urlDev, urlPreview} {
			res, err := internet.GetWithin(ctx, url+"/", itest.Scale(90*time.Second))
			if err != nil {
				t.Fatalf("GET %s: %v", url, err)
			}
			if res.Status != http.StatusOK {
				t.Errorf("GET %s: HTTP %d", url, res.Status)
			}
			if !strings.HasPrefix(res.Body, appGreeting+" ") {
				t.Errorf("GET %s = %q, want the app-scoped GREETING", url, res.Body)
			}
		}
	})
}

func TestSecondDeployUnderLoad(t *testing.T) {
	begin(t)
	needProduction(t)

	feed := startFollower(t, "--limit", "1")
	if err := feed.WaitFor(1, itest.Scale(2*time.Minute)); err != nil {
		t.Logf("the feed was quiet before the deploy: %v", err)
	}

	load := itest.StartLoad(itest.Load{
		Client: internet.Client(0),
		URL:    urlProd + "/",
		Marker: versionOf,
	})
	defer func() {
		if rep := load.Stop(); rep.Total > 0 {
			t.Logf("load generator: %s", rep)
		}
	}()
	if err := load.WaitForRequests(50, itest.Scale(time.Minute)); err != nil {
		t.Fatalf("the load never got going: %v", err)
	}

	before := servingVersion(t, urlProd)
	setVersion(t, "v2")

	out := deploy(t, envProd, "--no-watch")
	d := out.Deploy
	if _, ok := stepOf(d, cenv.StepSwitch); !ok {
		t.Fatalf("a --no-watch deploy did not reach the switch; steps: %v", stepNames(d))
	}
	secondRelease = ""
	if d.Release != nil {
		secondRelease = d.Release.Short()
	}
	if secondRelease == firstRelease {
		t.Errorf("the second deploy reused release %s; version.py moved, so the tree did", secondRelease)
	}

	t.Run("the old replicas are held, not gone", func(t *testing.T) {
		st := edgeStatus(t)
		if held := targetsInState(st.Routes, cedge.TargetHeld); held == 0 {
			t.Errorf("no held targets during the watch: %+v", st.Routes)
		} else {
			t.Logf("%d target(s) held", held)
		}
		detail := showEnv(t, envProd)
		if detail.Deploy == nil {
			t.Error("env show names no deploy in progress during a --no-watch watch")
		} else if detail.Deploy.Status != cenv.DeployWatching {
			t.Errorf("the deploy in progress is %q, want %q", detail.Deploy.Status, cenv.DeployWatching)
		}
	})

	deadline := time.Now().Add(itest.Scale(4 * time.Minute))
	for time.Now().Before(deadline) {
		hist := releases(t, envProd)
		if len(hist.Deploys) >= 2 && hist.Deploys[0].Status == cenv.DeployPromoted {
			break
		}
		time.Sleep(3 * time.Second)
	}

	rep := load.Stop()
	t.Logf("through the deploy: %s", rep)
	if rep.Failed != 0 {
		t.Errorf("%d of %d request(s) failed through the deploy: %v\n%s",
			rep.Failed, rep.Total, rep.Statuses, strings.Join(rep.Errors, "\n"))
	}
	if got := rep.Flips(); got != 1 {
		t.Errorf("the answer changed %d time(s) (%v), want exactly one: a flip is one pushed table",
			got, rep.Sequence())
	}
	if seq := rep.Sequence(); len(seq) > 0 && (seq[0] != before || seq[len(seq)-1] != "v2") {
		t.Errorf("the sequence is %v, want it to begin at %s and end at v2", seq, before)
	}

	t.Run("the held replicas are gone and the history has two rows", func(t *testing.T) {
		st := edgeStatus(t)
		if held := targetsInState(st.Routes, cedge.TargetHeld); held != 0 {
			t.Errorf("%d target(s) still held after the watch: %+v", held, st.Routes)
		}
		hist := releases(t, envProd)
		if len(hist.Deploys) != 2 {
			t.Fatalf("%d deploy(s) in the history, want 2", len(hist.Deploys))
		}
		if hist.Deploys[0].Status != cenv.DeployPromoted {
			t.Errorf("the newest deploy ended %q, want %q", hist.Deploys[0].Status, cenv.DeployPromoted)
		}
		if hist.Current == nil || hist.Current.Short() != secondRelease {
			t.Errorf("production is running %v, want %s", hist.Current, secondRelease)
		}
	})

	t.Run("a second client saw every step", func(t *testing.T) {
		seen := feed.Stop(t)
		if len(seen) == 0 {
			t.Fatal("events --follow produced nothing while a deploy walked")
		}
		steps := map[string]bool{}
		for _, e := range seen {
			if e.Env == envProd && e.Step != "" {
				steps[e.Step] = true
			}
		}
		for _, s := range d.Steps {
			if !steps[string(s.Step)] {
				t.Errorf("the feed never carried the %s step the deploy's own --json returned; "+
					"the feed had %v", s.Step, keys(steps))
			}
		}
	})
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestAutomaticRollback(t *testing.T) {
	begin(t)
	needProduction(t)

	good := servingVersion(t, urlProd)
	load := itest.StartLoad(itest.Load{
		Client:   internet.Client(0),
		URL:      urlProd + "/",
		Marker:   versionOf,
		Interval: itest.DefaultLoadInterval,
	})
	defer load.Stop()
	if err := load.WaitForRequests(50, itest.Scale(time.Minute)); err != nil {
		t.Fatalf("the load never got going: %v", err)
	}

	copyAs(t, repo, "behaviour.broken.py", "behaviour.py")
	setVersion(t, "v3")

	out, res := tryDeploy(t, envProd)
	d := out.Deploy
	if d == nil {
		t.Fatalf("a deploy that rolled back answered no row (exit %d):\n%s", res.ExitCode, res.Stderr)
	}
	t.Logf("deploy %s ended %q; steps: %v", envProd, d.Status, stepNames(d))
	if d.Status != cenv.DeployRolledBack {
		t.Fatalf("the deploy ended %q, want %q: a build that answers 500 must not be promoted",
			d.Status, cenv.DeployRolledBack)
	}
	if _, ok := stepOf(d, cenv.StepSwitch); !ok {
		t.Error("the flip never happened, so this is not the case under test")
	}
	if _, ok := stepOf(d, cenv.StepRollback); !ok {
		t.Errorf("no rollback step in a rolled-back deploy; steps: %v", stepNames(d))
	}
	if d.Watch == nil {
		t.Error("a rolled-back deploy carries no watch counts")
	} else {
		t.Logf("watch: %d request(s), %d error(s), %.1f%% against %.1f%%",
			d.Watch.Requests, d.Watch.Errors, d.Watch.Rate*100, d.Watch.MaxRate*100)
		if d.Watch.Errors == 0 {
			t.Error("the watch counted no errors, so something else ended this deploy")
		}
		if !d.Watch.Breached() {
			t.Error("the watch says its budget was not breached, but the deploy rolled back")
		}
	}

	rep := load.Stop()
	t.Logf("through the rollback: %s", rep)
	if rep.OK == 0 {
		t.Fatal("the load generator never got a single good response")
	}

	t.Run("the good release serves again", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
		defer cancel()
		res := internet.GetOK(t, ctx, urlProd+"/")
		if got := versionOf(res.Body); got != good {
			t.Errorf("version = %q, want %q: the release before the bad one", got, good)
		}
		hist := releases(t, envProd)
		if hist.Current == nil || hist.Current.Short() != secondRelease {
			t.Errorf("production is running %v, want %s", hist.Current, secondRelease)
		}
	})

	t.Run("the feed says why", func(t *testing.T) {
		list := events(t, envProd, "--limit", "300")
		e, ok := eventMatching(list, func(e cprogress.Event) bool {
			return e.Action == "deploy" && strings.Contains(e.Detail, string(cenv.DeployRolledBack))
		})
		if !ok {
			e, ok = eventMatching(list, func(e cprogress.Event) bool {
				return e.Step == string(cenv.StepRollback)
			})
		}
		if !ok {
			t.Fatalf("no rollback in %s's feed:\n%s", envProd, describeEvents(list))
		}
		t.Logf("the feed says: %s %s %s %q", e.Action, e.Step, e.Status, e.Detail)
	})

	copyAs(t, repo, "behaviour.py", "behaviour.py")
	commitAll(t, "sampleapp: a working / again")
}

func TestFailingCheckFlipsNothing(t *testing.T) {
	begin(t)
	needProduction(t)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(12*time.Minute))
	defer cancel()
	before := versionOf(internet.GetOK(t, ctx, urlProd+"/").Body)

	copyAs(t, repo, "caramelo.prodcheck.yaml", "caramelo.yaml")
	setVersion(t, "v4")

	out, res := tryDeploy(t, envProd)
	if res.ExitCode == 0 {
		t.Fatal("a deploy whose check refused exited 0")
	}
	d := out.Deploy
	if d == nil {
		t.Fatalf("a failed deploy answered no row:\n%s", res.Stderr)
	}
	if d.Status != cenv.DeployFailed {
		t.Errorf("the deploy ended %q, want %q; steps: %v", d.Status, cenv.DeployFailed, stepNames(d))
	}
	check, ok := stepOf(d, cenv.StepCheck)
	if !ok {
		t.Fatalf("no check step; steps: %v", stepNames(d))
	}
	if check.Status != cenv.StepFailed {
		t.Errorf("the check step is %q, want %q", check.Status, cenv.StepFailed)
	}
	if _, ok := stepOf(d, cenv.StepSwitch); ok {
		t.Error("a deploy whose check refused pushed a table: nothing may be flipped after a failed gate")
	}

	t.Run("the client saw nothing", func(t *testing.T) {
		res := internet.GetOK(t, ctx, urlProd+"/")
		if got := versionOf(res.Body); got != before {
			t.Errorf("version = %q, want %q: the release that was serving", got, before)
		}
	})

	copyAs(t, repo, "caramelo.prod.yaml", "caramelo.yaml")
	commitAll(t, "sampleapp: a check that passes again")
}

func TestManualPromoteAndInstantRollback(t *testing.T) {
	begin(t)
	needProduction(t)

	copyAs(t, repo, "caramelo.prodmanual.yaml", "caramelo.yaml")
	before := servingVersion(t, urlProd)
	setVersion(t, "v5")

	out := deploy(t, envProd)
	d := out.Deploy
	if d.Status != cenv.DeployWatching {
		t.Fatalf("a manual-promote deploy ended %q, want %q; steps: %v",
			d.Status, cenv.DeployWatching, stepNames(d))
	}
	if _, ok := stepOf(d, cenv.StepPromote); ok {
		t.Error("a manual-promote deploy promoted itself")
	}

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(15*time.Minute))
	defer cancel()

	t.Run("the new release is serving and the old pool is held", func(t *testing.T) {
		res := internet.GetOK(t, ctx, urlProd+"/")
		if got := versionOf(res.Body); got != "v5" {
			t.Errorf("version = %q, want v5: the flip happened before the watch", got)
		}
		hold := itest.Scale(15 * time.Second)
		time.Sleep(hold)
		st := edgeStatus(t)
		if held := targetsInState(st.Routes, cedge.TargetHeld); held == 0 {
			t.Errorf("nothing is held %s after the flip: %+v", hold, st.Routes)
		}
		detail := showEnv(t, envProd)
		if detail.Deploy == nil || detail.Deploy.Status != cenv.DeployWatching {
			t.Errorf("env show does not say the deploy is watching: %+v", detail.Deploy)
		}
	})

	t.Run("rollback inside the watch is a table push", func(t *testing.T) {
		load := itest.StartLoad(itest.Load{
			Client: internet.Client(0),
			URL:    urlProd + "/",
			Marker: versionOf,
		})
		if err := load.WaitForRequests(30, itest.Scale(time.Minute)); err != nil {
			load.Stop()
			t.Fatalf("the load never got going: %v", err)
		}
		started := time.Now()
		back, res := rollback(t, envProd)
		took := time.Since(started)
		rep := load.Stop()
		t.Logf("rollback took %s; through it: %s", took.Round(time.Millisecond), rep)
		if res.ExitCode != 0 {
			t.Fatalf("rollback: exit %d\n%s", res.ExitCode, res.Stderr)
		}
		if back.Deploy == nil {
			t.Fatal("rollback answered no row")
		}
		if back.Deploy.Status != cenv.DeployRolledBack {
			t.Errorf("status = %q, want %q", back.Deploy.Status, cenv.DeployRolledBack)
		}
		if back.Deploy.Kind != cenv.DeployKindDeploy && back.Deploy.Kind != cenv.DeployKindRollback {
			t.Errorf("kind = %q", back.Deploy.Kind)
		}
		if b, ok := stepOf(back.Deploy, cenv.StepBuild); ok && b.Status != cenv.StepSkipped &&
			b.Status != cenv.StepOK {
			t.Errorf("the build step is %q on a rollback inside the watch", b.Status)
		}
		if rep.Failed != 0 {
			t.Errorf("%d of %d request(s) failed through the rollback: %v",
				rep.Failed, rep.Total, rep.Statuses)
		}
		if got := rep.Sequence(); len(got) > 0 && got[len(got)-1] != before {
			t.Errorf("the load ended at %q, want %q: the release that was held", got[len(got)-1], before)
		}
		if took > itest.Scale(90*time.Second) {
			t.Errorf("a rollback inside the watch took %s, which is not a table push", took.Round(time.Second))
		}
	})

	t.Run("a third deploy, finished by promote", func(t *testing.T) {
		setVersion(t, "v6")
		out := deploy(t, envProd)
		if out.Deploy.Status != cenv.DeployWatching {
			t.Fatalf("deploy ended %q, want %q", out.Deploy.Status, cenv.DeployWatching)
		}
		done := promote(t, envProd)
		if done.Deploy == nil || done.Deploy.Status != cenv.DeployPromoted {
			t.Fatalf("promote left the deploy at %+v", done.Deploy)
		}
		st := edgeStatus(t)
		if held := targetsInState(st.Routes, cedge.TargetHeld); held != 0 {
			t.Errorf("%d target(s) still held after promote: %+v", held, st.Routes)
		}
		res := internet.GetOK(t, ctx, urlProd+"/")
		if got := versionOf(res.Body); got != "v6" {
			t.Errorf("version = %q, want v6", got)
		}
	})

	copyAs(t, repo, "caramelo.prod.yaml", "caramelo.yaml")
	commitAll(t, "sampleapp: automatic promotion again")
}

func TestRollbackToANamedRelease(t *testing.T) {
	begin(t)
	needProduction(t)

	hist := releases(t, envProd)
	if len(hist.Deploys) < 2 {
		t.Skipf("only %d deploy(s) in the history", len(hist.Deploys))
	}
	kept := keptReleases(hist, 5)
	if len(kept) == 0 {
		t.Skip("no release in the history")
	}
	target := kept[len(kept)-1]
	t.Logf("rolling back to %s, the oldest of the %d release(s) deploy.keep keeps", target, len(kept))
	out, res := rollback(t, envProd, "--to", target)
	if res.ExitCode != 0 {
		t.Fatalf("rollback --to %s: exit %d\n%s", target, res.ExitCode, res.Stderr)
	}
	d := out.Deploy
	if d == nil || d.Release == nil {
		t.Fatalf("rollback --to answered %+v", d)
	}
	if got := crelease.ShortTree(d.Release.Tree); got != target {
		t.Errorf("rollback --to %s deployed %s", target, got)
	}
	if b, ok := stepOf(d, cenv.StepBuild); ok && b.Status != cenv.StepSkipped {
		t.Errorf("the build step is %q, want %q: rollback --to must build nothing",
			b.Status, cenv.StepSkipped)
	}
	if d.Status != cenv.DeployPromoted {
		t.Errorf("the rollback ended %q, want %q; steps: %v", d.Status, cenv.DeployPromoted, stepNames(d))
	}

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
	defer cancel()
	answer := internet.GetOK(t, ctx, urlProd+"/")
	t.Logf("%s serves %q again", target, versionOf(answer.Body))
	if strings.TrimSpace(answer.Body) == "" {
		t.Error("the rolled-back release answers nothing")
	}

	t.Run("a release whose images were pruned is refused by name", func(t *testing.T) {
		all := keptReleases(hist, 0)
		if len(all) <= len(kept) {
			t.Skipf("nothing has been pruned yet: %d release(s), deploy.keep is 5", len(all))
		}
		gone := all[len(all)-1]
		_, res := rollback(t, envProd, "--to", gone)
		if res.ExitCode == 0 {
			t.Fatalf("rollback --to %s succeeded, and its images are pruned", gone)
		}
		for _, want := range []string{gone, "pruned", "deploy.keep"} {
			if !strings.Contains(res.Stderr, want) {
				t.Errorf("the refusal does not mention %q:\n%s", want, res.Stderr)
			}
		}
	})

	setVersion(t, "v7")
	if out := deploy(t, envProd); out.Deploy.Status != cenv.DeployPromoted {
		t.Fatalf("the deploy after the rollback ended %q", out.Deploy.Status)
	}
}

func TestASecretChangeRollsOut(t *testing.T) {
	m := begin(t)
	needProduction(t)

	const changed = "good morning"
	secretsSet(t, envProd, "GREETING="+changed)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(12*time.Minute))
	defer cancel()
	stale := internet.GetOK(t, ctx, urlProd+"/")
	if strings.HasPrefix(stale.Body, changed) {
		t.Error("the new secret reached the running containers without a deploy")
	}

	out := deploy(t, envProd)
	if out.Deploy.Status != cenv.DeployPromoted {
		t.Fatalf("the deploy ended %q; steps: %v", out.Deploy.Status, stepNames(out.Deploy))
	}
	if len(out.Deploy.Rollouts) == 0 {
		t.Error("a deploy that changed only a secret rolled nothing: the definition changed")
	}

	fresh := internet.GetOK(t, ctx, urlProd+"/")
	if !strings.HasPrefix(fresh.Body, changed) {
		t.Errorf("GET %s = %q, want it to start with the new secret", urlProd, fresh.Body)
	}

	t.Run("the value is in the container and in no file", func(t *testing.T) {
		container := liveReplica(t, m, envProd, webService)
		env := inspect(t, m, container, `{{range .Config.Env}}{{println .}}{{end}}`)
		if !strings.Contains(env, "GREETING="+changed) {
			t.Errorf("the container's environment has no GREETING=%q:\n%s", changed, env)
		}
		if left := secretsOnDisk(t, m); len(left) != 0 {
			t.Errorf("%s still holds %v: an env-file lives for one docker run", secretsRunDir, left)
		}
		if stray := straySecretsOnDisk(t, m); stray != "" {
			t.Errorf("%s exists: an env-file directory was made beside the data directory: %s", straySecretsDir, stray)
		}
		if left := buildsOnDisk(t, m, envProd); len(left) != 0 {
			t.Errorf("%s's builds directory still holds %v: an export lives for one build", envProd, left)
		}
	})

	t.Run("export --reveal is an event naming who asked", func(t *testing.T) {
		shown := secretsExport(t, envProd, "--reveal")
		if shown.Values["GREETING"] != changed {
			t.Errorf("export --reveal = %q, want %q", shown.Values["GREETING"], changed)
		}
		list := events(t, envProd, "--limit", "200")
		reveal, ok := eventMatching(list, func(e cprogress.Event) bool {
			return e.Action == "secrets" && e.Step == "export"
		})
		if !ok {
			t.Fatalf("no secrets/export event in %s's feed:\n%s", envProd, describeEvents(list))
		}
		if reveal.Identity == "" {
			t.Errorf("the reveal event names nobody: %+v", reveal)
		} else if who := whoAmI(t); reveal.Identity != who {
			t.Errorf("the reveal event names %q, want the peer this client authenticates as (%q)",
				reveal.Identity, who)
		}
	})
}

func TestStagingIsItsOwnHistory(t *testing.T) {
	begin(t)
	needProduction(t)

	copyAs(t, repo, "behaviour.broken.py", "behaviour.py")
	brokenCommit := commitAll(t, "sampleapp: a broken / for staging")

	secretsSet(t, envStaging, "GREETING=staging says")

	out := deploy(t, envStaging, "--ref", brokenCommit)
	if out.Deploy == nil {
		t.Fatal("deploy staging answered no row")
	}
	t.Logf("deploy staging ended %q; steps: %v", out.Deploy.Status, stepNames(out.Deploy))

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(12*time.Minute))
	defer cancel()

	t.Run("staging serves the broken build at its own name", func(t *testing.T) {
		res, err := internet.Get(ctx, urlStaging+"/")
		if err != nil {
			t.Fatalf("GET %s: %v", urlStaging, err)
		}
		if res.Status != http.StatusInternalServerError {
			t.Errorf("GET %s: HTTP %d, want 500: this is the broken commit", urlStaging, res.Status)
		}
	})

	t.Run("production is untouched", func(t *testing.T) {
		res := internet.GetOK(t, ctx, urlProd+"/")
		if res.Status != http.StatusOK {
			t.Errorf("GET %s: HTTP %d", urlProd, res.Status)
		}
	})

	t.Run("and the fixed commit repairs it", func(t *testing.T) {
		copyAs(t, repo, "behaviour.py", "behaviour.py")
		fixed := setVersion(t, "v9")
		out := deploy(t, envStaging, "--ref", fixed)
		if out.Deploy.Status != cenv.DeployPromoted {
			t.Fatalf("the fixing deploy ended %q; steps: %v", out.Deploy.Status, stepNames(out.Deploy))
		}
		res := internet.GetOK(t, ctx, urlStaging+"/")
		if !strings.HasPrefix(res.Body, "staging says") {
			t.Errorf("GET %s = %q, want staging's own greeting", urlStaging, res.Body)
		}
	})

	t.Run("two histories, separate", func(t *testing.T) {
		prodHist := releases(t, envProd)
		stagingHist := releases(t, envStaging)
		if len(stagingHist.Deploys) == len(prodHist.Deploys) {
			t.Logf("both histories have %d row(s); that is possible but suspicious",
				len(prodHist.Deploys))
		}
		if prodHist.Current == nil || stagingHist.Current == nil {
			t.Fatalf("a release environment with no current release: %v / %v",
				prodHist.Current, stagingHist.Current)
		}
		if prodHist.Current.Short() == stagingHist.Current.Short() {
			t.Errorf("both environments are running %s; they were deployed different commits",
				prodHist.Current.Short())
		}
	})
}

func TestCrashLoopLeavesThePool(t *testing.T) {
	m := begin(t)
	needProduction(t)

	pool := liveReplicas(t, m, envProd, webService)
	if len(pool) < 2 {
		t.Fatalf("%s/%s has %d running replica(s), want 2: %v", envProd, webService, len(pool), pool)
	}
	victim, survivor := pool[len(pool)-1], pool[0]
	t.Logf("killing %s repeatedly; %s must keep serving", victim, survivor)

	asCaramelo(t, m, fmt.Sprintf(
		"nohup sh -c 'for i in 1 2 3 4 5 6; do docker kill %s >/dev/null 2>&1; sleep 10; done' "+
			"</dev/null >/dev/null 2>&1 &", itest.ShellQuote(victim)))

	load := itest.StartLoad(itest.Load{
		Client: internet.Client(0),
		URL:    urlProd + "/",
		Marker: replicaOf,
	})
	event := waitForEvent(t, envProd, itest.Scale(3*time.Minute), func(e cprogress.Event) bool {
		return e.Action == cenv.HealthActionCrashloop ||
			(e.Action == cenv.HealthActionHealth && e.Status == cprogress.StatusFailed)
	})
	t.Logf("the feed says: %s/%d %s %s %q", event.Service, event.Replica,
		event.Action, event.Status, event.Detail)
	if event.Service != webService {
		t.Errorf("the event names service %q, want %q", event.Service, webService)
	}

	t.Run("the replica is out of the pool and the other one serves", func(t *testing.T) {
		index := replicaIndexOf(t, victim)
		state, states := "", []string(nil)
		deadline := time.Now().Add(itest.Scale(90 * time.Second))
		for time.Now().Before(deadline) && state == "" {
			route := routeOf(t, "edge status", edgeStatus(t).Routes, hostProd)
			states = nil
			for _, target := range route.Targets {
				states = append(states, fmt.Sprintf("%d:%s", target.Replica, target.State))
				if target.Replica == index && !target.State.Routable() {
					state = string(target.State)
				}
			}
			if state == "" {
				time.Sleep(2 * time.Second)
			}
		}
		t.Logf("targets on %s: %v", hostProd, states)
		if state == "" {
			t.Errorf("replica %d was killed six times and never left the pool: %v", index, states)
		} else {
			t.Logf("replica %d left the pool as %s", index, state)
		}
		if len(routeOf(t, "edge status", edgeStatus(t).Routes, hostProd).Active()) == 0 {
			t.Error("no active target on production: the surviving replica must keep serving")
		}
		rep := load.Report()
		if rep.Failed > rep.Total/10 {
			t.Errorf("%d of %d request(s) failed while one replica was crash-looping: %v",
				rep.Failed, rep.Total, rep.Statuses)
		}
	})

	t.Run("and it comes back when the crashing stops", func(t *testing.T) {
		deadline := time.Now().Add(itest.Scale(4 * time.Minute))
		for time.Now().Before(deadline) {
			st := edgeStatus(t)
			route := routeOf(t, "edge status", st.Routes, hostProd)
			if len(route.Active()) == len(route.Targets) && len(route.Targets) > 1 {
				rep := load.Stop()
				t.Logf("the pool is whole again; through it: %s", rep)
				return
			}
			time.Sleep(5 * time.Second)
		}
		rep := load.Stop()
		st := edgeStatus(t)
		t.Errorf("the pool never came back: %+v (load: %s)",
			routeOf(t, "edge status", st.Routes, hostProd).Targets, rep)
	})
}

func TestMemoryLimitIsRealAndAnOOMIsAnEvent(t *testing.T) {
	m := begin(t)
	needRepo(t)

	copyAs(t, repo, "caramelo.oom.yaml", "caramelo.yaml")
	commitAll(t, "sampleapp: a service that allocates past its limit")
	itest.Git(t, repo, clientEnv(), "branch", "-f", envOOM)

	createEnv(t, envOOM, "--from", envOOM, "--reset", "--no-deps")
	t.Cleanup(func() { destroyEnv(t, envOOM) })
	mustInRepo(t, "up", envOOM, "--json")

	container := liveReplica(t, m, envOOM, "hog")

	t.Run("the limit is on the container", func(t *testing.T) {
		got := inspect(t, m, container, "{{.HostConfig.Memory}}")
		if got != "33554432" {
			t.Errorf("Memory = %q, want 33554432 (32 MiB): resources.memory must become --memory", got)
		}
		opts := inspect(t, m, container, "{{json .HostConfig.LogConfig}}")
		for _, want := range []string{"max-size", "max-file"} {
			if !strings.Contains(opts, want) {
				t.Errorf("the container's log config has no %s: %s", want, opts)
			}
		}
	})

	t.Run("the kernel stops it and docker brings it back", func(t *testing.T) {
		deadline := time.Now().Add(itest.Scale(3 * time.Minute))
		for time.Now().Before(deadline) {
			if inspect(t, m, container, "{{.State.OOMKilled}}") == "true" {
				break
			}
			if n := inspect(t, m, container, "{{.RestartCount}}"); n != "0" {
				t.Logf("restart count %s", n)
				break
			}
			time.Sleep(5 * time.Second)
		}
		oom := inspect(t, m, container, "{{.State.OOMKilled}}")
		restarts := inspect(t, m, container, "{{.RestartCount}}")
		t.Logf("OOMKilled=%s RestartCount=%s", oom, restarts)
		if oom != "true" && restarts == "0" {
			t.Fatalf("the container was neither OOM-killed nor restarted; " +
				"hog.py allocates far past 32m")
		}
	})

	t.Run("and caramelod wrote it down", func(t *testing.T) {
		event := waitForEvent(t, envOOM, itest.Scale(3*time.Minute), func(e cprogress.Event) bool {
			return e.Action == cenv.HealthActionCrashloop ||
				e.Action == cenv.HealthActionRestart ||
				strings.Contains(strings.ToLower(e.Detail), "oom")
		})
		t.Logf("the feed says: %s %s %q", event.Action, event.Status, event.Detail)
	})

	copyAs(t, repo, "caramelo.prod.yaml", "caramelo.yaml")
	itest.Git(t, repo, clientEnv(), "checkout", branch)
	commitAll(t, "sampleapp: back to the prod branch's file")
}

func TestProductionSurvivesAPowerCycle(t *testing.T) {
	m := begin(t)
	needProduction(t)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(15*time.Minute))
	defer cancel()

	before := internet.GetOK(t, ctx, urlProd+"/")
	wantVersion := versionOf(before.Body)
	wantSerial := before.Serial()
	wantRelease := releases(t, envProd).Current
	if wantRelease == nil {
		t.Fatal("production has no current release to survive a power cycle with")
	}
	poolBefore := liveReplicas(t, m, envProd, webService)
	t.Logf("before the power cycle: %s on %v, certificate %s", wantVersion, poolBefore, wantSerial)

	itest.MustRestart(t, m)
	reconnect(t, m)

	if err := itest.WaitForCaramelod(ctx, m); err != nil {
		t.Fatalf("caramelod did not come back after a power cycle: %v", err)
	}
	if err := waitForEdgeSocket(ctx, m, itest.Scale(2*time.Minute)); err != nil {
		t.Fatalf("%v", err)
	}
	if err := refreshEdgeTrust(); err != nil {
		t.Fatalf("%v", err)
	}
	waitForClient(t)

	t.Run("nothing had to be redeployed", func(t *testing.T) {
		hist := releases(t, envProd)
		if hist.Current == nil || hist.Current.Short() != wantRelease.Short() {
			t.Fatalf("production is running %v after a power cycle, want %s", hist.Current, wantRelease.Short())
		}
		detail := showEnv(t, envProd)
		if detail.Env.Mode != cenv.ModeRelease || !detail.Env.Protected {
			t.Errorf("production came back as %q, protected=%v", detail.Env.Mode, detail.Env.Protected)
		}
		if detail.Deploy != nil {
			t.Errorf("a power cycle left a deploy in progress: %+v", detail.Deploy)
		}
	})

	t.Run("the replicas came back on their own", func(t *testing.T) {
		deadline := time.Now().Add(itest.Scale(3 * time.Minute))
		var pool []string
		for time.Now().Before(deadline) {
			pool = liveReplicas(t, m, envProd, webService)
			if len(pool) >= len(poolBefore) {
				break
			}
			time.Sleep(3 * time.Second)
		}
		if len(pool) < len(poolBefore) {
			t.Fatalf("%d replica(s) after a power cycle (%v), want %d (%v): every service is "+
				"restart unless-stopped", len(pool), pool, len(poolBefore), poolBefore)
		}
		t.Logf("the pool came back as %v", pool)
	})

	t.Run("the world sees the same release behind the same certificate", func(t *testing.T) {
		res, err := internet.GetWithin(ctx, urlProd+"/", itest.Scale(3*time.Minute))
		if err != nil {
			t.Fatalf("%v", err)
		}
		if res.Status != http.StatusOK {
			t.Fatalf("GET %s after a power cycle: HTTP %d", urlProd, res.Status)
		}
		if got := versionOf(res.Body); got != wantVersion {
			t.Errorf("version = %q after a power cycle, want %q", got, wantVersion)
		}
		if res.Cert == nil {
			t.Fatal("no certificate on an https response after a power cycle")
		}
		if got := res.Serial(); got != wantSerial {
			t.Errorf("the certificate is %s, want %s: a reboot must not cost a new ACME order",
				got, wantSerial)
		}
	})

	t.Run("the vault still opens", func(t *testing.T) {
		shown := secretsExport(t, envProd, "--reveal")
		if got := shown.Values["DB_PASSWORD"]; got != chosenPassword {
			t.Errorf("DB_PASSWORD reads %q after a power cycle, want the chosen one", got)
		}
	})
}

func TestProtectedDestroyCostsAFlag(t *testing.T) {
	begin(t)
	needProduction(t)

	res := inRepo(t, "env", "destroy", envProd, "--yes")
	if res.ExitCode == 0 {
		t.Fatal("env destroy of a protected environment succeeded without --force")
	}
	if !strings.Contains(res.Stderr, "--force") {
		t.Errorf("the refusal does not name --force: %q", res.Stderr)
	}
	for _, svc := range showEnv(t, envProd).Services {
		if svc.Status != cenv.ServiceRunning {
			t.Errorf("the refused destroy left %s %q, want it still running", svc.Name, svc.Status)
		}
	}
	refusedCtx, cancelRefused := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
	defer cancelRefused()
	if res := internet.GetOK(t, refusedCtx, urlProd+"/"); res.Status != http.StatusOK {
		t.Errorf("GET %s after a refused destroy: HTTP %d — the refusal took production off the air",
			urlProd, res.Status)
	}

	mustInRepo(t, "env", "destroy", envProd, "--force", "--yes")

	list := listEnvs(t)
	for _, e := range list {
		if e.Name == envProd {
			t.Errorf("%s is still in env list after a forced destroy", envProd)
		}
	}
	for _, name := range []string{envStaging, envDev, envPreview} {
		envNamed(t, list, name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
	defer cancel()
	for _, url := range []string{urlStaging, urlDev, urlPreview} {
		if res := internet.GetOK(t, ctx, url+"/"); res.Status != http.StatusOK {
			t.Errorf("GET %s: HTTP %d after production was destroyed", url, res.Status)
		}
	}
	st := edgeStatus(t)
	for _, r := range st.Routes {
		if r.Host == hostProd || r.Host == hostProdWWW {
			t.Errorf("the edge still routes %s after production was destroyed", r.Host)
		}
	}
}

func TestTheInterface(t *testing.T) {
	begin(t)
	needRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(20*time.Minute))
	defer cancel()
	bin := itest.BinaryPath(t)

	t.Run("env list is a table on a terminal and the same table in a pipe", func(t *testing.T) {
		pty, err := itest.PTYRun(ctx, repo, clientEnv(), bin, "env", "list", "--all")
		if err != nil {
			t.Fatalf("%v", err)
		}
		if pty.ExitCode != 0 {
			t.Fatalf("env list on a terminal: exit %d\n%s", pty.ExitCode, pty.Output)
		}
		frame := pty.LastFrame()
		for _, want := range []string{envStaging, "MODE"} {
			if !strings.Contains(frame, want) {
				t.Errorf("the table does not mention %q:\n%s", want, frame)
			}
		}
		plain := mustInRepo(t, "env", "list", "--all")
		if strings.Contains(plain.Stdout, "\x1b[") {
			t.Errorf("env list through a pipe wrote escape sequences:\n%q", plain.Stdout)
		}
		if !strings.Contains(plain.Stdout, "MODE") {
			t.Errorf("env list through a pipe is not the plain table:\n%s", plain.Stdout)
		}
	})

	t.Run("a deploy renders a live view on a terminal", func(t *testing.T) {
		setVersion(t, "v8")
		pty, err := itest.PTYRun(ctx, repo, clientEnv(), bin, "deploy", envStaging)
		if err != nil {
			t.Fatalf("%v", err)
		}
		if pty.ExitCode != 0 {
			t.Fatalf("deploy on a terminal: exit %d\n%s", pty.ExitCode, pty.Output)
		}
		t.Logf("the last frame of the deploy:\n%s", pty.LastFrame())
		for _, want := range []string{string(cenv.StepSwitch), string(cenv.StepPromote)} {
			if !strings.Contains(pty.Output, want) {
				t.Errorf("the view never showed the %s step:\n%s", want, pty.Output)
			}
		}
	})

	t.Run("events --follow on a terminal is the same plain feed", func(t *testing.T) {
		short, cancelShort := context.WithTimeout(ctx, itest.Scale(45*time.Second))
		defer cancelShort()
		done := make(chan struct{})
		probe := make(chan itest.Result, 1)
		go func() {
			defer close(done)
			time.Sleep(itest.Scale(8 * time.Second))
			res, err := clientExec(clientOpts{Dir: repo}, "secrets", "set", envStaging, "LIVE_VIEW_PROBE=1")
			if err != nil {
				res.Stderr += err.Error()
			}
			probe <- res
		}()
		pty, _ := itest.PTYRun(short, repo, clientEnv(), bin, "events", "--follow")
		<-done
		if res := <-probe; res.ExitCode != 0 {
			t.Fatalf("the secret the feed was meant to show could not be set: exit %d\n%s%s",
				res.ExitCode, res.Stdout, res.Stderr)
		}
		if strings.TrimSpace(pty.Output) == "" {
			t.Fatal("events --follow on a terminal drew nothing")
		}
		t.Logf("the last frame of the feed:\n%s", pty.LastFrame())
		if strings.Contains(pty.Output, "\x1b[") {
			t.Errorf("events --follow on a terminal wrote escape sequences:\n%q", pty.Output)
		}
		if !strings.Contains(pty.Output, "secrets set") {
			t.Errorf("the feed never showed the secret that was set while it watched:\n%s", pty.Output)
		}
		list := events(t, "--limit", "20")
		if len(list) == 0 {
			t.Error("events --json produced nothing after a whole suite of them")
		}
	})
}
