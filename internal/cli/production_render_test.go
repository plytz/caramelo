package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/cli/ui"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/vault"
)

func TestWriteBuildResult(t *testing.T) {
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	rel := &release.Release{
		App: "shop", Commit: "1234567890abcdef1234567890abcdef12345678",
		Tree: "abcdef0123456789", Ref: "v1.2.3",
		Images:  map[string]string{"web": "caramelo/shop/web:abcdef012345"},
		BuiltBy: "commander", BuiltAt: at,
	}
	var buf bytes.Buffer
	if err := writeBuildResult(&buf, &api.BuildResult{App: "shop", Env: "production", Release: rel, Built: true}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"shop/production", "built", "v1.2.3", "caramelo/shop/web:abcdef012345", "commander"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("build result has no %q:\n%s", want, buf.String())
		}
	}

	buf.Reset()
	if err := writeBuildResult(&buf, &api.BuildResult{App: "shop", Env: "production", Release: rel}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "unchanged") {
		t.Errorf("a build that built nothing did not say so:\n%s", buf.String())
	}

	buf.Reset()
	if err := writeBuildResult(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "no release\n" {
		t.Errorf("nil build result = %q", buf.String())
	}
}

func TestWriteDeployResult(t *testing.T) {
	start := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	d := &env.Deploy{
		ID: 7, App: "shop", Env: "production",
		Kind: env.DeployKindDeploy, Status: env.DeployRolledBack,
		Release:     &release.Release{App: "shop", Tree: "abcdef0123456789", Commit: "deadbeefdeadbeef"},
		FromRelease: &release.Release{App: "shop", Tree: "0123456789abcdef", Commit: "cafebabecafebabe"},
		Steps: []env.DeployStep{
			{Step: env.StepBuild, Status: env.StepOK, Detail: "1 image", Duration: 3 * time.Second},
			{Step: env.StepSwitch, Status: env.StepOK, Detail: "1 host", Duration: 20 * time.Millisecond},
			{Step: env.StepWatch, Status: env.StepFailed, Detail: "11% of 120 requests failed"},
		},
		Watch: &env.DeployWatch{
			Window: 10 * time.Second, Elapsed: 4 * time.Second,
			Requests: 120, Errors: 13, Rate: 0.108, MaxRate: 0.05, MinRequests: 20,
		},
		Routes: []string{"shop.test"}, Identity: "commander",
		StartedAt: start, FinishedAt: start.Add(30 * time.Second),
		Error: "rolled back: the edge counted more errors than deploy.max_errors allows",
	}
	var buf bytes.Buffer
	if err := writeDeployResult(&buf, &api.DeployResult{Deploy: d}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"shop/production", "deploy rolled_back",
		"switch", "watch", "failed",
		"120 request(s), 13 error(s)",
		"over the 5.0% deploy.max_errors allows",
		"serving shop.test",
		"rolled back:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("deploy result has no %q:\n%s", want, out)
		}
	}
}

func TestDescribeWatchUnderTheFloor(t *testing.T) {
	got := describeWatch(env.DeployWatch{
		Window: 10 * time.Second, Elapsed: 10 * time.Second,
		Requests: 3, Errors: 1, Rate: 0.33, MaxRate: 0.05, MinRequests: 20,
	})
	if !strings.Contains(got, "under the 20-request floor") {
		t.Errorf("watch = %q", got)
	}
}

func TestWriteReleasesResult(t *testing.T) {
	current := &release.Release{App: "shop", Tree: "aaaaaaaaaaaabbbb", Commit: "1111111111111111"}
	res := &api.ReleasesResult{
		App: "shop", Env: "production", Current: current,
		Deploys: []env.Deploy{
			{Kind: env.DeployKindDeploy, Status: env.DeployPromoted, Release: current, Identity: "agent-1",
				StartedAt:  time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
				FinishedAt: time.Date(2026, 9, 10, 12, 1, 0, 0, time.UTC)},
			{Kind: env.DeployKindRollback, Status: env.DeployRolledBack, Reason: "max_errors"},
		},
	}
	var buf bytes.Buffer
	if err := writeReleasesResult(&buf, res); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"RELEASE", "promoted", "rolled_back", "agent-1", "max_errors",
		current.Short() + " *", "is what shop/production is running"} {
		if !strings.Contains(out, want) {
			t.Errorf("releases has no %q:\n%s", want, out)
		}
	}

	buf.Reset()
	if err := writeReleasesResult(&buf, &api.ReleasesResult{App: "shop", Env: "staging"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "has never been deployed") {
		t.Errorf("empty history = %q", buf.String())
	}
}

func TestWriteVaultResultNeverPrintsAValue(t *testing.T) {
	res := &api.VaultResult{
		App: "shop", Env: "production",
		Entries: []vault.Entry{
			{Ref: vault.AppRef("shop", "GREETING"), Version: 1, UpdatedBy: "commander"},
			{Ref: vault.EnvRef("shop", "production", "GREETING"), Version: 2, UpdatedBy: "commander"},
		},
		Resolved: map[string]vault.Scope{"GREETING": vault.ScopeEnv},
		Changed:  []string{"GREETING"},
		Warnings: []string{"db was initialised with another password"},
	}

	for i := range res.Entries {
		res.Entries[i].Value = "s3cret"
	}
	var buf bytes.Buffer
	if err := writeVaultResult(&buf, res); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "s3cret") {
		t.Fatalf("a value was printed:\n%s", out)
	}
	for _, want := range []string{"GREETING", "app", "env", "the narrowest scope wins",
		"db was initialised with another password"} {
		if !strings.Contains(out, want) {
			t.Errorf("vault result has no %q:\n%s", want, out)
		}
	}

	if n := strings.Count(out, "*"); n != 2 {
		t.Errorf("%d markers in:\n%s", n, out)
	}
}

func TestWriteVaultExportRedactedSaysSo(t *testing.T) {
	var buf bytes.Buffer
	res := &api.VaultExportResult{
		App: "shop", Env: "production",
		Values: map[string]string{"GREETING": vault.Redacted, "DB_PASSWORD": vault.Redacted},
		Text:   "DB_PASSWORD=<secret>\nGREETING=<secret>\n",
	}
	if err := writeVaultExport(&buf, res); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "DB_PASSWORD=<secret>\nGREETING=<secret>\n") {
		t.Errorf("export = %q", buf.String())
	}
	if !strings.Contains(buf.String(), "2 value(s) redacted") {
		t.Errorf("a redacted export did not say so:\n%s", buf.String())
	}

	buf.Reset()
	res.Revealed = true
	res.Text = "GREETING=hi\n"
	if err := writeVaultExport(&buf, res); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "GREETING=hi\n" {
		t.Errorf("revealed export = %q", buf.String())
	}
}

func TestRegisteredProductionRenderersRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		path  string
		value any
		want  string
	}{
		{"build", &api.BuildResult{App: "shop", Env: "production",
			Release: &release.Release{App: "shop", Tree: "abcdef0123456789"}, Built: true}, "built"},
		{"deploy", &api.DeployResult{Deploy: &env.Deploy{App: "shop", Env: "production",
			Kind: env.DeployKindDeploy, Status: env.DeployPromoted}}, "deploy promoted"},
		{"releases", &api.ReleasesResult{App: "shop", Env: "production"}, "never been deployed"},
		{"secrets list", &api.VaultResult{App: "shop", Env: "production"}, "no secrets"},
		{"secrets export", &api.VaultExportResult{App: "shop", Env: "production",
			Values: map[string]string{"A": "<secret>"}, Text: "A=<secret>\n"}, "A=<secret>"},
	} {
		raw, err := json.Marshal(tc.value)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		had, err := render(tc.path, &buf, raw)
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		if !had {
			t.Fatalf("%s has no renderer", tc.path)
		}
		if !strings.Contains(buf.String(), tc.want) {
			t.Errorf("%s rendered %q, want %q in it", tc.path, buf.String(), tc.want)
		}
	}

	var buf bytes.Buffer
	if _, err := render("deploy", &buf, json.RawMessage("null")); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "no deploy\n" {
		t.Errorf("null deploy = %q", buf.String())
	}
}

func TestEventsPlainFeedIsTheOneRendering(t *testing.T) {
	var out bytes.Buffer
	sink := ui.SinkFor(ui.NewPlainFeed(&out))
	w := progress.New(sink, progress.FormatJSON)
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	events := []progress.Event{
		{App: "shop", Env: "production", Service: "web", Replica: 2, Action: "rollout",
			Step: "flip", Status: progress.StatusChanged, Detail: "active", At: at},
		{App: "shop", Env: "production", Action: "deploy", Step: "watch",
			Status: progress.StatusOK, Detail: "10s, 0 errors", Identity: "commander", At: at},
	}
	for _, e := range events {
		if err := w.Emit(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines:\n%s", len(lines), out.String())
	}
	for i, e := range events {
		if lines[i] != ui.FeedLine(e) {
			t.Errorf("line %d = %q, want the feed line %q", i, lines[i], ui.FeedLine(e))
		}
	}
	if !strings.Contains(lines[0], "shop/production") || !strings.Contains(lines[0], "rollout flip web-2") {
		t.Errorf("row = %q, want the environment, the action and the replica", lines[0])
	}
	if !strings.Contains(lines[1], "(commander)") {
		t.Errorf("identity is not in the row: %q", lines[1])
	}
}

type prodService struct {
	api.Service

	build    *api.BuildResult
	deploy   *api.DeployResult
	releases *api.ReleasesResult
	vault    *api.VaultResult
}

func (s *prodService) Build(_ context.Context, _ release.BuildRequest, _ io.Writer) (*api.BuildResult, error) {
	return s.build, nil
}

func (s *prodService) Deploy(_ context.Context, _ env.DeployRequest, _ io.Writer) (*api.DeployResult, error) {
	return s.deploy, nil
}

func (s *prodService) Rollback(_ context.Context, _ env.RollbackRequest, _ io.Writer) (*api.DeployResult, error) {
	return s.deploy, nil
}

func (s *prodService) Promote(_ context.Context, _ env.PromoteRequest, _ io.Writer) (*api.DeployResult, error) {
	return s.deploy, nil
}

func (s *prodService) Releases(_ context.Context, _, _ string, _ int) (*api.ReleasesResult, error) {
	return s.releases, nil
}

func (s *prodService) VaultList(_ context.Context, req api.VaultListRequest) (*api.VaultResult, error) {
	res := *s.vault
	res.App, res.Env = req.App, req.Env
	return &res, nil
}

func TestCommanderRenderMatchesTheDaemonForProduction(t *testing.T) {
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	rel := &release.Release{
		App: "shop", Commit: "1234567890abcdef1234567890abcdef12345678",
		Tree: "abcdef0123456789", Ref: "v1.2.3",
		Images:  map[string]string{"web": "caramelo/shop/web:abcdef012345"},
		BuiltBy: "commander", BuiltAt: at,
	}
	deployed := &api.DeployResult{Deploy: &env.Deploy{
		ID: 7, App: "shop", Env: "production", Kind: env.DeployKindDeploy,
		Status: env.DeployPromoted, Release: rel, Identity: "commander",
		Steps: []env.DeployStep{
			{Step: env.StepBuild, Status: env.StepOK, Detail: "1 image", Duration: 3 * time.Second},
			{Step: env.StepSwitch, Status: env.StepOK, Detail: "1 host", Duration: 20 * time.Millisecond},
			{Step: env.StepPromote, Status: env.StepOK, Detail: "2 replicas stopped"},
		},
		Routes: []string{"shop.test"}, StartedAt: at, FinishedAt: at.Add(30 * time.Second),
	}}
	svc := &prodService{
		build:  &api.BuildResult{App: "shop", Env: "production", Release: rel, Built: true},
		deploy: deployed,
		releases: &api.ReleasesResult{App: "shop", Env: "production", Current: rel,
			Deploys: []env.Deploy{*deployed.Deploy}},
		vault: &api.VaultResult{Entries: []vault.Entry{
			{Ref: vault.Ref{Scope: vault.ScopeApp, App: "shop", Name: "STRIPE_KEY"}, Version: 2},
			{Ref: vault.Ref{Scope: vault.ScopeEnv, App: "shop", Env: "production", Name: "GREETING"}, Version: 1},
		}, Resolved: map[string]vault.Scope{"STRIPE_KEY": vault.ScopeApp, "GREETING": vault.ScopeEnv}},
	}
	info := renderInfo{app: "shop", env: "production", args: []string{"production"}}
	for _, tc := range []struct {
		name string
		args []string
		path string
	}{
		{"build", []string{"build", "production", "--app", "shop", "--no-push"}, "build"},
		{"deploy", []string{"deploy", "production", "--app", "shop", "--no-push"}, "deploy"},
		{"rollback", []string{"rollback", "production", "--app", "shop"}, "rollback"},
		{"promote", []string{"promote", "production", "--app", "shop"}, "promote"},
		{"releases", []string{"releases", "production", "--app", "shop"}, "releases"},
		{"secrets-list", []string{"secrets", "list", "production", "--app", "shop"}, "secrets list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, human, stderr := runWithService(t, svc, tc.args...)
			if code != ExitOK {
				t.Fatalf("exit = %d (stderr %q)", code, stderr)
			}
			jsonArgs := append(append([]string{}, tc.args...), "--json")
			code, body, stderr := runWithService(t, svc, jsonArgs...)
			if code != ExitOK {
				t.Fatalf("--json exit = %d (stderr %q)", code, stderr)
			}
			if !json.Valid([]byte(strings.TrimSpace(body))) {
				t.Fatalf("--json did not print one JSON value: %q", body)
			}
			a := &app{stdout: io.Discard, stderr: io.Discard, render: info}
			fn := rendererFor(a, tc.path)
			if fn == nil {
				t.Fatalf("no renderer registered for %q", tc.path)
			}
			var b bytes.Buffer
			if err := fn(&b, json.RawMessage(strings.TrimSpace(body))); err != nil {
				t.Fatalf("commander render: %v", err)
			}
			if b.String() != human {
				t.Errorf("the commander's rendering is not the daemon's.\n--- commander ---\n%s\n--- daemon ---\n%s",
					b.String(), human)
			}
		})
	}
}
