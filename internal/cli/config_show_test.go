package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/config"
)

type configService struct {
	api.Service

	asked    [2]string
	revealed bool
	cfg      *api.EffectiveConfig
	err      error
}

func (s *configService) EffectiveConfig(_ context.Context, app, name string, reveal bool) (*api.EffectiveConfig, error) {
	s.asked, s.revealed = [2]string{app, name}, reveal
	if s.err != nil {
		return nil, s.err
	}
	return s.cfg, nil
}

func sampleConfig() *api.EffectiveConfig {
	return &api.EffectiveConfig{
		App:   "shop",
		Env:   "feat-x",
		Stack: "go",
		Config: &config.App{
			Name:     "shop",
			Services: []config.Service{{Name: "web", Image: "golang:1.23", Run: "go run ."}},
			Test:     "go test ./...",
		},
		Fields: []api.ConfigField{
			{Key: "services.web.image", Value: "golang:1.23", Source: api.SourceDetected, Evidence: "go.mod says go 1.23"},
			{Key: "services.web.run", Value: "go run .", Source: api.SourceFile},
			{Key: "services.web.port", Value: "${port}", Source: api.SourceDefault, Evidence: "the environment's own port"},
			{Key: "test", Value: "go test ./...", Source: api.SourceFile},
		},
	}
}

func TestConfigShowHuman(t *testing.T) {
	svc := &configService{cfg: sampleConfig()}
	code, stdout, stderr := runWithService(t, svc, "config", "show", "feat-x", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitOK, stderr)
	}
	if svc.asked != [2]string{"shop", "feat-x"} {
		t.Errorf("daemon asked %v, want {shop feat-x}", svc.asked)
	}
	for _, w := range []string{
		"shop", "feat-x", "go",
		"services.web.image", "golang:1.23", "detected", "go.mod says go 1.23",
		"services.web.run", "file",
		"services.web.port", "${port}", "default",
		"test", "go test ./...",
	} {
		if !strings.Contains(stdout, w) {
			t.Errorf("stdout does not mention %q:\n%s", w, stdout)
		}
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

func TestConfigShowJSON(t *testing.T) {
	svc := &configService{cfg: sampleConfig()}
	code, stdout, _ := runWithService(t, svc, "config", "show", "feat-x", "--app", "shop", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	var got api.EffectiveConfig
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, stdout)
	}
	if got.Stack != "go" || got.App != "shop" || got.Env != "feat-x" {
		t.Errorf("got %+v, want the app, env and stack", got)
	}
	if len(got.Fields) != 4 || got.Fields[0].Source != api.SourceDetected {
		t.Errorf("fields = %+v", got.Fields)
	}
	if got.Config == nil || got.Config.Test != "go test ./..." {
		t.Errorf("config = %+v, want the merged configuration", got.Config)
	}
}

func TestConfigShowWithoutAnEnvironment(t *testing.T) {
	svc := &configService{cfg: &api.EffectiveConfig{App: "shop", Config: &config.App{Name: "shop"}}}
	code, stdout, _ := runWithService(t, svc, "config", "show", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	if svc.asked != [2]string{"shop", ""} {
		t.Errorf("daemon asked %v, want no environment", svc.asked)
	}
	for _, w := range []string{"default branch", "nothing detected", "nothing to run"} {
		if !strings.Contains(stdout, w) {
			t.Errorf("stdout does not say %q:\n%s", w, stdout)
		}
	}
}

func TestConfigShowReveal(t *testing.T) {
	svc := &configService{cfg: sampleConfig()}
	if code, _, stderr := runWithService(t, svc, "config", "show", "feat-x", "--app", "shop"); code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if svc.revealed {
		t.Error("config show asked the daemon to reveal without --reveal")
	}
	if code, _, stderr := runWithService(t, svc, "config", "show", "feat-x", "--app", "shop", "--reveal"); code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if !svc.revealed {
		t.Error("config show --reveal did not ask the daemon to reveal")
	}
}

func TestConfigShowWithTooManyArgumentsIsAUsageError(t *testing.T) {
	svc := &configService{cfg: sampleConfig()}
	code, _, stderr := runWithService(t, svc, "config", "show", "feat-x", "feat-y", "--app", "shop")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitUsage, stderr)
	}
	if svc.asked != [2]string{} {
		t.Errorf("the daemon was called with %v; the client should have refused", svc.asked)
	}
}

func TestConfigShowRejectsAnInvalidEnvironmentName(t *testing.T) {
	svc := &configService{cfg: sampleConfig()}
	code, _, stderr := runWithService(t, svc, "config", "show", "Feat X", "--app", "shop")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitUsage, stderr)
	}
	if svc.asked != [2]string{} {
		t.Errorf("the daemon was called with %v; the client should have refused", svc.asked)
	}
}

func TestResolveConfigAppNamesTheCheckout(t *testing.T) {
	fakeGit(t, map[string]string{
		"rev-parse --git-common-dir": "/mnt/caramelo/apps/shop/repo.git",
		"rev-parse --show-toplevel":  "/mnt/caramelo/apps/shop/envs/feat-x/src",
	})
	a := &app{args: []string{"config", "show"}}
	t2 := &envTargets{a: a}
	cmd := newRootCmd(a)
	if err := resolveConfigApp(t2, cmd); err != nil {
		t.Fatalf("resolveConfigApp: %v", err)
	}
	if t2.app != "shop" {
		t.Errorf("app = %q, want shop", t2.app)
	}
	if strings.Join(a.args, " ") != "config show --app shop" {
		t.Errorf("args = %v, want --app written into the invocation", a.args)
	}
}

func TestResolveConfigAppOutsideACheckoutIsAUsageError(t *testing.T) {
	fakeGit(t, nil)
	a := &app{args: []string{"config", "show"}}
	err := resolveConfigApp(&envTargets{a: a}, newRootCmd(a))
	var ue *usageError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want a usage error", err)
	}
	if !strings.Contains(err.Error(), "--app") {
		t.Errorf("err = %v, want it to name the fix", err)
	}
}

func TestResolveConfigEnvUsesCarameloEnv(t *testing.T) {

	a := &app{args: []string{"config", "show", "--app", "shop"}}
	tg := &envTargets{a: a, env: "feat-x"}
	resolveConfigEnv(tg, &cobra.Command{}, nil)
	if strings.Join(a.args, " ") != "config show --app shop feat-x" {
		t.Errorf("args = %v, want feat-x appended", a.args)
	}

	a = &app{args: []string{"config", "show", "feat-y", "--app", "shop"}}
	tg = &envTargets{a: a, env: "feat-x"}
	resolveConfigEnv(tg, &cobra.Command{}, []string{"feat-y"})
	if strings.Join(a.args, " ") != "config show feat-y --app shop" {
		t.Errorf("args = %v, want them untouched", a.args)
	}
	name, err := configShowEnv(tg, []string{"feat-y"})
	if err != nil || name != "feat-y" {
		t.Errorf("configShowEnv = %q, %v, want feat-y", name, err)
	}

	a = &app{args: []string{"config", "show"}}
	tg = &envTargets{a: a}
	resolveConfigEnv(tg, &cobra.Command{}, nil)
	if strings.Join(a.args, " ") != "config show" {
		t.Errorf("args = %v, want them untouched", a.args)
	}
	if name, err := configShowEnv(tg, nil); err != nil || name != "" {
		t.Errorf("configShowEnv = %q, %v, want no environment", name, err)
	}
}

func TestResolveConfigEnvUsesTheWorktree(t *testing.T) {
	fakeGit(t, map[string]string{
		"rev-parse --show-toplevel": "/mnt/caramelo/apps/shop/envs/feat-x/src",
	})
	a := &app{args: []string{"config", "show", "--app", "shop"}}
	tg := &envTargets{a: a}
	resolveConfigEnv(tg, &cobra.Command{}, nil)
	if strings.Join(a.args, " ") != "config show --app shop feat-x" {
		t.Errorf("args = %v, want feat-x appended from the worktree", a.args)
	}
}
