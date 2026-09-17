package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/fleet"
)

type envService struct {
	api.Service

	createReq    env.CreateRequest
	listedApp    string
	shownApp     string
	shownName    string
	destroyed    [3]string
	destroyReq   env.DestroyRequest
	execReq      env.ExecRequest
	exportArgs   [3]string
	exportReveal bool
	progress     string

	directory []fleet.DirectoryEntry

	envs     []env.Env
	env      *env.Env
	detail   *api.EnvDetail
	apps     []api.AppInfo
	export   string
	execCode int
	err      error
}

func (s *envService) CreateEnv(_ context.Context, req env.CreateRequest, progress io.Writer) (*env.Env, error) {
	s.createReq = req
	if s.progress != "" && progress != nil {
		fmt.Fprintln(progress, s.progress)
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.env != nil {
		return s.env, nil
	}
	return &env.Env{App: req.App, Name: req.Name, Branch: req.Name, Status: env.StatusReady}, nil
}

func (s *envService) Envs(_ context.Context, app string) ([]env.Env, error) {
	s.listedApp = app
	return s.envs, s.err
}

func (s *envService) Env(_ context.Context, app, name string) (*api.EnvDetail, error) {
	s.shownApp, s.shownName = app, name
	if s.err != nil {
		return nil, s.err
	}
	return s.detail, nil
}

func (s *envService) DestroyEnv(_ context.Context, req env.DestroyRequest, progress io.Writer) error {
	s.destroyReq = req
	s.destroyed = [3]string{req.App, req.Name, ""}
	if req.DeleteBranch {
		s.destroyed[2] = "branch"
	}
	if s.progress != "" && progress != nil {
		fmt.Fprintln(progress, s.progress)
	}
	return s.err
}

func (s *envService) ExecEnv(_ context.Context, req env.ExecRequest) (int, error) {
	s.execReq = req
	if s.err != nil {
		return 0, s.err
	}
	if req.Stdout != nil {
		fmt.Fprintf(req.Stdout, "ran %s\n", strings.Join(req.Argv, " "))
	}
	if req.Stdin != nil {
		if b, _ := io.ReadAll(req.Stdin); len(b) > 0 {
			fmt.Fprintf(req.Stdout, "stdin %s\n", strings.TrimSpace(string(b)))
		}
	}
	return s.execCode, nil
}

func (s *envService) ExportEnv(_ context.Context, app, name string, format env.ExportFormat, view config.View,
	reveal bool) (string, error) {
	s.exportArgs = [3]string{app, name, string(format)}
	s.exportReveal = reveal
	return s.export, s.err
}

func (s *envService) Apps(context.Context) ([]api.AppInfo, error) { return s.apps, s.err }

func sampleEnv() env.Env {
	return env.Env{
		ID: 1, App: "shop", Name: "feat-x", Branch: "feat-x",
		Commit:       "0123456789abcdef0123456789abcdef01234567",
		SourceBranch: "blob-store",
		PushedBy:     "alex@laptop",
		PushedAt:     time.Date(2026, 9, 8, 12, 2, 0, 0, time.UTC),
		Worktree:     "/mnt/caramelo/apps/shop/envs/feat-x/src",
		PortBase:     20000, PortCount: 16,
		Status:    env.StatusReady,
		Config:    json.RawMessage(`{"name":"shop","deps":[{"name":"db","image":"postgres:16"},{"name":"cache","image":"redis:7"}]}`),
		Vars:      map[string]string{"PORT": "20000", "DATABASE_URL": "postgres://127.0.0.1:20001/postgres"},
		CreatedBy: "alex@laptop",
		CreatedAt: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC),
	}
}

func TestEnvListHuman(t *testing.T) {
	svc := &envService{envs: []env.Env{sampleEnv()}}
	code, stdout, stderr := runService(t, context.Background(), svc, "env", "list", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if svc.listedApp != "shop" {
		t.Errorf("service asked for app %q, want shop", svc.listedApp)
	}
	for _, want := range []string{"NAME", "COMMIT", "SOURCE", "PORTS", "DEPS", "feat-x", "20000-20015",
		"0123456", "blob-store", "db,cache", "alex@laptop"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
	if strings.Contains(stdout, "BRANCH") {
		t.Errorf("env list still prints a BRANCH column, which only ever repeats NAME:\n%s", stdout)
	}
}

func TestEnvListEmpty(t *testing.T) {
	code, stdout, _ := runService(t, context.Background(), &envService{}, "env", "list", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "no environments") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestEnvListJSONIsAlwaysAnArray(t *testing.T) {
	code, stdout, _ := runService(t, context.Background(), &envService{}, "env", "list", "--app", "shop", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var got []env.Env
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, stdout)
	}
	if len(got) != 0 {
		t.Errorf("got %d envs", len(got))
	}
}

func TestEnvListAllIgnoresTheApp(t *testing.T) {
	svc := &envService{envs: []env.Env{sampleEnv()}}
	if code, _, _ := runService(t, context.Background(), svc, "env", "list", "--app", "shop", "--all"); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if svc.listedApp != "" {
		t.Errorf("--all asked for app %q, want every app", svc.listedApp)
	}
}

func TestEnvCreateJSON(t *testing.T) {
	e := sampleEnv()
	svc := &envService{env: &e, progress: "ok  ports  20000-20015"}
	code, stdout, stderr := runService(t, context.Background(), svc,
		"env", "create", "feat-x", "--app", "shop", "--from", "main", "--timeout", "30s", "--no-deps", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	want := env.CreateRequest{App: "shop", Name: "feat-x", From: "main", NoDeps: true, Timeout: 30 * time.Second}
	if !reflect.DeepEqual(svc.createReq, want) {
		t.Errorf("request = %+v, want %+v", svc.createReq, want)
	}
	var got env.Env
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if got.Name != "feat-x" || got.PortBase != 20000 {
		t.Errorf("env = %+v", got)
	}

	if !strings.Contains(stderr, "20000-20015") {
		t.Errorf("stderr = %q, want the daemon's progress", stderr)
	}
}

func TestEnvCreateHuman(t *testing.T) {
	e := sampleEnv()
	svc := &envService{env: &e}
	code, stdout, _ := runService(t, context.Background(), svc, "env", "create", "feat-x", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"shop/feat-x", "ready", "20000-20015", "db, cache"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

func TestEnvCreateRejectsABadName(t *testing.T) {
	svc := &envService{}
	code, stdout, stderr := runService(t, context.Background(), svc, "env", "create", "Feat_X", "--app", "shop")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "Feat_X") {
		t.Errorf("stderr = %q, want it to name the offending env", stderr)
	}
	if svc.createReq.Name != "" {
		t.Error("the daemon was asked to create it anyway")
	}
}

func TestEnvCommandsNeedAnApp(t *testing.T) {

	for _, args := range [][]string{
		{"env", "show", "feat-x"},
		{"env", "create", "feat-x"},
		{"env", "destroy", "feat-x", "--yes"},
		{"env", "export", "feat-x"},
		{"env", "exec", "feat-x", "--", "true"},
	} {
		t.Setenv("CARAMELO_APP", "")
		code, _, stderr := runService(t, context.Background(), &envService{}, args...)
		if code != ExitUsage {
			t.Errorf("%v: exit = %d, want %d", args, code, ExitUsage)
		}
		if !strings.Contains(stderr, "--app") {
			t.Errorf("%v: stderr = %q, want it to name --app", args, stderr)
		}
	}
}

func TestEnvShow(t *testing.T) {
	e := sampleEnv()
	svc := &envService{detail: &api.EnvDetail{
		Env: e,
		Deps: []env.DepState{
			{Name: "db", Container: "caramelo-shop-feat-x-db", Status: env.DepRunning, Port: 20001},
			{Name: "cache", Container: "caramelo-shop-feat-x-cache", Status: env.DepMissing, Port: 20002},
		},
		Events: []env.Event{{Action: "create", Status: "ok", Identity: "alex@laptop", At: e.CreatedAt}},
	}}
	code, stdout, _ := runService(t, context.Background(), svc, "env", "show", "feat-x", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if svc.shownApp != "shop" || svc.shownName != "feat-x" {
		t.Errorf("service got %q/%q", svc.shownApp, svc.shownName)
	}
	for _, want := range []string{"shop/feat-x", "running", "missing", "caramelo-shop-feat-x-db", "create", "alex@laptop"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

func TestEnvShowJSON(t *testing.T) {
	e := sampleEnv()
	svc := &envService{detail: &api.EnvDetail{Env: e, Deps: []env.DepState{{Name: "db", Status: env.DepRunning}}}}
	code, stdout, _ := runService(t, context.Background(), svc, "env", "show", "feat-x", "--app", "shop", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var got api.EnvDetail
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if got.Env.Name != "feat-x" || len(got.Deps) != 1 || got.Deps[0].Status != env.DepRunning {
		t.Errorf("detail = %+v", got)
	}
}

func TestEnvShowReportsTheDaemonsError(t *testing.T) {
	svc := &envService{err: errors.New("no such env \"nope\" in app \"shop\"")}
	code, _, stderr := runService(t, context.Background(), svc, "env", "show", "nope", "--app", "shop", "--json")
	if code != ExitError {
		t.Fatalf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "no such env") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestEnvDestroy(t *testing.T) {
	svc := &envService{progress: "removed container caramelo-shop-feat-x-db"}
	code, stdout, stderr := runService(t, context.Background(), svc, "env", "destroy", "feat-x", "--app", "shop", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if svc.destroyed != [3]string{"shop", "feat-x", ""} {
		t.Errorf("service got %v", svc.destroyed)
	}
	if !strings.Contains(stdout, "destroyed shop/feat-x") || !strings.Contains(stdout, "branch kept") {
		t.Errorf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "removed container") {
		t.Errorf("stderr = %q, want the daemon's progress", stderr)
	}
}

func TestEnvDestroyDeleteBranchJSON(t *testing.T) {
	svc := &envService{}
	code, stdout, _ := runService(t, context.Background(), svc,
		"env", "destroy", "feat-x", "--app", "shop", "--yes", "--delete-branch", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if svc.destroyed[2] != "branch" {
		t.Error("--delete-branch did not reach the daemon")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if got["destroyed"] != true || got["branch_deleted"] != true || got["name"] != "feat-x" {
		t.Errorf("json = %v", got)
	}
}

func TestEnvDestroyWithoutYesIsAUsageError(t *testing.T) {

	svc := &envService{}
	code, _, stderr := runService(t, context.Background(), svc, "env", "destroy", "feat-x", "--app", "shop")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Errorf("stderr = %q, want it to say how to confirm", stderr)
	}
	if svc.destroyed[1] != "" {
		t.Error("the env was destroyed without confirmation")
	}
}

func TestEnvExecNeedsTheDoubleDash(t *testing.T) {
	svc := &envService{}
	code, _, stderr := runService(t, context.Background(), svc, "env", "exec", "feat-x", "--app", "shop", "true")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "--") {
		t.Errorf("stderr = %q", stderr)
	}
	if len(svc.execReq.Argv) != 0 {
		t.Errorf("the daemon was asked to run %v", svc.execReq.Argv)
	}
}

func TestEnvExecPassesArgvAndStreams(t *testing.T) {
	svc := &envService{}
	code, stdout, _ := runService(t, context.Background(), svc,
		"env", "exec", "feat-x", "--app", "shop", "--", "sh", "-c", "echo $DATABASE_URL")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	want := []string{"sh", "-c", "echo $DATABASE_URL"}
	if strings.Join(svc.execReq.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv = %v, want %v", svc.execReq.Argv, want)
	}
	if svc.execReq.App != "shop" || svc.execReq.Name != "feat-x" {
		t.Errorf("request = %+v", svc.execReq)
	}
	if !strings.Contains(stdout, "ran sh -c echo $DATABASE_URL") {
		t.Errorf("stdout = %q, want the child's output", stdout)
	}
}

func TestEnvExecPassesTheExitCodeThrough(t *testing.T) {
	svc := &envService{execCode: 3}
	code, _, _ := runService(t, context.Background(), svc, "env", "exec", "feat-x", "--app", "shop", "--", "false")
	if code != 3 {
		t.Fatalf("exit = %d, want the child's 3", code)
	}
}

func TestEnvExecReadsTheSessionsStdin(t *testing.T) {
	svc := &envService{}
	ctx := withStdin(context.Background(), strings.NewReader("piped\n"))
	code, stdout, _ := runService(t, ctx, svc, "env", "exec", "feat-x", "--app", "shop", "--", "cat")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "stdin piped") {
		t.Errorf("stdout = %q, want the piped input to have reached the child", stdout)
	}
}

func TestEnvExport(t *testing.T) {
	svc := &envService{export: "export PORT='20000'\n"}
	code, stdout, _ := runService(t, context.Background(), svc, "env", "export", "feat-x", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if svc.exportArgs != [3]string{"shop", "feat-x", "shell"} {
		t.Errorf("service got %v, want the shell format by default", svc.exportArgs)
	}
	if stdout != "export PORT='20000'\n" {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestEnvExportFormats(t *testing.T) {
	for _, tc := range []struct{ args, want string }{
		{"dotenv", "dotenv"},
		{"json", "json"},
	} {
		svc := &envService{export: "{}"}
		code, _, _ := runService(t, context.Background(), svc, "env", "export", "feat-x", "--app", "shop", "--format", tc.args)
		if code != ExitOK {
			t.Fatalf("%s: exit = %d", tc.args, code)
		}
		if svc.exportArgs[2] != tc.want {
			t.Errorf("format = %q, want %q", svc.exportArgs[2], tc.want)
		}
	}

	svc := &envService{export: `{"PORT":"20000"}`}
	code, stdout, _ := runService(t, context.Background(), svc, "env", "export", "feat-x", "--app", "shop", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if svc.exportArgs[2] != "json" {
		t.Errorf("--json asked for format %q", svc.exportArgs[2])
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if got["PORT"] != "20000" {
		t.Errorf("vars = %v", got)
	}

	bad := &envService{}
	if code, _, _ := runService(t, context.Background(), bad, "env", "export", "feat-x", "--app", "shop", "--format", "toml"); code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	if bad.exportArgs[1] != "" {
		t.Error("the daemon was asked for an unknown format")
	}
}

func TestAppList(t *testing.T) {
	svc := &envService{apps: []api.AppInfo{
		{Name: "shop", DefaultBranch: "main", EnvCount: 2, RepoBytes: 3 << 20},
		{Name: "blog", EnvCount: 0},
	}}
	code, stdout, _ := runService(t, context.Background(), svc, "app", "list")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"NAME", "DEFAULT BRANCH", "ENVS", "shop", "main", "3.0 MiB", "blog"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}

	code, stdout, _ = runService(t, context.Background(), svc, "app", "list", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var got []api.AppInfo
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if len(got) != 2 || got[0].Name != "shop" || got[0].EnvCount != 2 {
		t.Errorf("apps = %+v", got)
	}
}

func TestAppListEmpty(t *testing.T) {
	code, stdout, _ := runService(t, context.Background(), &envService{}, "app", "list")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "no apps") {
		t.Errorf("stdout = %q", stdout)
	}
	code, stdout, _ = runService(t, context.Background(), &envService{}, "app", "list", "--json")
	if code != ExitOK || strings.TrimSpace(stdout) != "[]" {
		t.Errorf("empty JSON: exit %d, stdout %q", code, stdout)
	}
}

func (s *envService) EnvsAll(context.Context, string) ([]fleet.DirectoryEntry, error) {
	return s.directory, nil
}
