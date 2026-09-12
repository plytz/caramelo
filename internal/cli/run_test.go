package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/env"
)

type m4Service struct {
	api.Service

	upReq   env.UpRequest
	downReq env.DownRequest
	testReq env.RunRequest
	runReq  env.RunRequest
	logsReq env.LogsRequest

	up       *api.UpResult
	down     *api.DownResult
	progress string
	code     int
	err      error
}

func (s *m4Service) Up(_ context.Context, req env.UpRequest, progress io.Writer) (*api.UpResult, error) {
	s.upReq = req
	if s.progress != "" && progress != nil {
		fmt.Fprintln(progress, s.progress)
	}
	if s.err != nil {

		return s.up, s.err
	}
	if s.up != nil {
		return s.up, nil
	}
	return &api.UpResult{Env: env.Env{App: req.App, Name: req.Name, Status: env.StatusReady}}, nil
}

func (s *m4Service) Down(_ context.Context, req env.DownRequest, progress io.Writer) (*api.DownResult, error) {
	s.downReq = req
	if s.progress != "" && progress != nil {
		fmt.Fprintln(progress, s.progress)
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.down != nil {
		return s.down, nil
	}
	return &api.DownResult{}, nil
}

func (s *m4Service) Test(_ context.Context, req env.RunRequest) (int, error) {
	s.testReq = req
	return s.oneOff(req)
}

func (s *m4Service) Run(_ context.Context, req env.RunRequest) (int, error) {
	s.runReq = req
	return s.oneOff(req)
}

func (s *m4Service) oneOff(req env.RunRequest) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if req.Stdout != nil {
		fmt.Fprintf(req.Stdout, "ran %s\n", strings.Join(req.Argv, " "))
		if req.Stdin != nil {
			if b, _ := io.ReadAll(req.Stdin); len(b) > 0 {
				fmt.Fprintf(req.Stdout, "stdin %s\n", strings.TrimSpace(string(b)))
			}
		}
	}
	return s.code, nil
}

func (s *m4Service) Logs(_ context.Context, req env.LogsRequest) error {
	s.logsReq = req
	if s.err != nil {
		return s.err
	}
	if req.Stdout != nil {
		fmt.Fprintln(req.Stdout, "web | listening")
	}
	return nil
}

func sampleServices() []env.Service {
	return []env.Service{
		{
			Name: "web", Container: "caramelo-shop-feat-x-web", ID: "abc123",
			Image: "python:3.12-alpine", Port: 20000, ContainerPort: 20000, Protocol: "tcp",
			URL: "http://127.0.0.1:20000", Status: env.ServiceRunning,
			Health: env.HealthOK, Change: env.ChangeCreated,
		},
		{
			Name: "echo", Container: "caramelo-shop-feat-x-echo",
			Image: "python:3.12-alpine", Port: 20002, Protocol: "udp",
			URL: "udp://127.0.0.1:20002", Status: env.ServiceRunning,
			Health: env.HealthNone, Change: env.ChangeUnchanged,
		},
	}
}

func TestUpHuman(t *testing.T) {
	svc := &m4Service{
		progress: "[changed] service web",
		up: &api.UpResult{
			Env:      env.Env{App: "shop", Name: "feat-x", Status: env.StatusReady},
			Services: sampleServices(),
			Network:  "caramelo-shop-feat-x",
			Stack:    "python",
		},
	}
	code, stdout, stderr := runService(t, context.Background(), svc, "up", "feat-x", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if svc.upReq.App != "shop" || svc.upReq.Name != "feat-x" {
		t.Errorf("request = %+v", svc.upReq)
	}
	for _, want := range []string{"shop/feat-x", "python", "caramelo-shop-feat-x", "web", "http://127.0.0.1:20000", "echo", "udp://"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to mention %q", stdout, want)
		}
	}

	if !strings.Contains(stderr, "[changed] service web") {
		t.Errorf("stderr = %q, want the daemon's progress", stderr)
	}
	if strings.Contains(stdout, "[changed]") {
		t.Errorf("stdout = %q, want progress on stderr only", stdout)
	}
}

func TestUpJSON(t *testing.T) {
	svc := &m4Service{up: &api.UpResult{
		Env:      env.Env{App: "shop", Name: "feat-x", Status: env.StatusReady},
		Services: sampleServices(),
		Network:  "caramelo-shop-feat-x",
	}}
	code, stdout, stderr := runService(t, context.Background(), svc, "up", "feat-x", "--app", "shop", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	var got api.UpResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if len(got.Services) != 2 || got.Services[0].URL != "http://127.0.0.1:20000" {
		t.Errorf("services = %+v", got.Services)
	}
	if strings.Count(stdout, "\n") != 1 {
		t.Errorf("stdout = %q, want exactly one JSON line", stdout)
	}
}

func TestUpPassesItsFlagsThrough(t *testing.T) {
	svc := &m4Service{}
	code, _, stderr := runService(t, context.Background(), svc, "up", "feat-x", "--app", "shop",
		"--service", "web", "--service", "worker", "--build", "--no-wait", "--timeout", "30s")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	req := svc.upReq
	if len(req.Services) != 2 || req.Services[0] != "web" || req.Services[1] != "worker" {
		t.Errorf("services = %v", req.Services)
	}
	if !req.Build || !req.NoWait || req.Timeout.String() != "30s" {
		t.Errorf("request = %+v", req)
	}
}

func TestUpReportsAFailedHealthWait(t *testing.T) {
	svc := &m4Service{err: errors.New("service web never became healthy within 2m")}
	code, stdout, stderr := runService(t, context.Background(), svc, "up", "feat-x", "--app", "shop")
	if code != ExitError {
		t.Fatalf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "never became healthy") {
		t.Errorf("stderr = %q", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
}

func TestUpJSONStillReportsTheServicesWhenHealthFails(t *testing.T) {
	svc := &m4Service{
		up:  &api.UpResult{Env: env.Env{App: "shop", Name: "feat-x"}, Services: sampleServices()},
		err: errors.New("service web never became healthy within 2m"),
	}
	code, stdout, stderr := runService(t, context.Background(), svc, "up", "feat-x", "--app", "shop", "--json")
	if code != ExitError {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitError, stderr)
	}
	var got api.UpResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &got); err != nil {
		t.Fatalf("stdout is not the result document: %v in %q", err, stdout)
	}
	if len(got.Services) != len(sampleServices()) {
		t.Errorf("services = %+v, want what each one is doing", got.Services)
	}
	if !strings.Contains(stderr, "never became healthy") {
		t.Errorf("stderr = %q, want the reason", stderr)
	}
}

func TestDownReportsWhatItStopped(t *testing.T) {
	stopped := sampleServices()
	for i := range stopped {
		stopped[i].Status, stopped[i].Change = env.ServiceStopped, env.ChangeRemoved
	}
	svc := &m4Service{down: &api.DownResult{Services: stopped}}
	code, stdout, stderr := runService(t, context.Background(), svc, "down", "feat-x", "--app", "shop", "--service", "web")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if svc.downReq.Name != "feat-x" || len(svc.downReq.Services) != 1 {
		t.Errorf("request = %+v", svc.downReq)
	}
	if !strings.Contains(stdout, "removed") {
		t.Errorf("stdout = %q, want what happened to each service", stdout)
	}
}

func TestDownOfAStoppedEnvironmentSaysSo(t *testing.T) {
	svc := &m4Service{}
	code, stdout, _ := runService(t, context.Background(), svc, "down", "feat-x", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "no services were running") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestTestPassesArgumentsAfterTheDash(t *testing.T) {
	svc := &m4Service{}
	code, stdout, stderr := runService(t, context.Background(), svc,
		"test", "feat-x", "--app", "shop", "--service", "web", "--", "-k", "nonexistent")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	req := svc.testReq
	if !req.Test {
		t.Error("the request did not select the test command")
	}
	if strings.Join(req.Argv, " ") != "-k nonexistent" {
		t.Errorf("argv = %v", req.Argv)
	}
	if req.Service != "web" || req.Name != "feat-x" || req.App != "shop" {
		t.Errorf("request = %+v", req)
	}
	if !strings.Contains(stdout, "ran -k nonexistent") {
		t.Errorf("stdout = %q, want the child's output", stdout)
	}
}

func TestTestPassesTheExitCodeBack(t *testing.T) {
	svc := &m4Service{code: 5}
	if code, _, _ := runService(t, context.Background(), svc, "test", "feat-x", "--app", "shop"); code != 5 {
		t.Errorf("exit = %d, want the test command's 5", code)
	}
}

func TestRunNeedsACommand(t *testing.T) {
	for _, args := range [][]string{
		{"run", "feat-x", "--app", "shop"},
		{"run", "feat-x", "--app", "shop", "--"},
	} {
		svc := &m4Service{}
		code, _, stderr := runService(t, context.Background(), svc, args...)
		if code != ExitUsage {
			t.Errorf("%v: exit = %d, want %d", args, code, ExitUsage)
		}

		if !strings.Contains(stderr, "caramelo up feat-x") {
			t.Errorf("%v: stderr = %q, want it to point at up", args, stderr)
		}
	}
}

func TestRunWiresTheStreamsThrough(t *testing.T) {
	svc := &m4Service{}
	ctx := withStdin(context.Background(), strings.NewReader("piped\n"))
	code, stdout, stderr := runService(t, ctx, svc, "run", "feat-x", "--app", "shop", "--", "npm", "run", "lint")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if strings.Join(svc.runReq.Argv, " ") != "npm run lint" {
		t.Errorf("argv = %v", svc.runReq.Argv)
	}
	if svc.runReq.Test {
		t.Error("run asked for the test command")
	}
	for _, want := range []string{"ran npm run lint", "stdin piped"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want %q", stdout, want)
		}
	}
}

func TestLogsReadsServicesAfterTheEnvironment(t *testing.T) {
	svc := &m4Service{}
	code, stdout, stderr := runService(t, context.Background(), svc,
		"logs", "feat-x", "web", "worker", "--app", "shop", "-f", "--tail", "50", "--since", "10m", "--deps")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	req := svc.logsReq
	if req.Name != "feat-x" || strings.Join(req.Services, ",") != "web,worker" {
		t.Errorf("request = %+v", req)
	}
	if !req.Follow || !req.Deps || req.Tail != 50 || req.Since != "10m" {
		t.Errorf("request = %+v", req)
	}
	if req.JSON {
		t.Error("--json was not asked for")
	}
	if !strings.Contains(stdout, "web | listening") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestLogsWithAnEnvironmentFlagTakesOnlyServices(t *testing.T) {
	svc := &m4Service{}
	code, _, stderr := runService(t, context.Background(), svc,
		"logs", "web", "--env", "feat-x", "--app", "shop", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if svc.logsReq.Name != "feat-x" || strings.Join(svc.logsReq.Services, ",") != "web" {
		t.Errorf("request = %+v", svc.logsReq)
	}
	if !svc.logsReq.JSON {
		t.Error("--json did not reach the request")
	}
}

func TestCommandsResolveTheEnvironmentFromTheFlag(t *testing.T) {
	svc := &m4Service{}
	if code, _, stderr := runService(t, context.Background(), svc, "up", "--app", "shop", "--env", "feat-y"); code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if svc.upReq.Name != "feat-y" {
		t.Errorf("env = %q, want feat-y", svc.upReq.Name)
	}
}

func TestThePositionalEnvironmentWins(t *testing.T) {
	svc := &m4Service{}
	if code, _, _ := runService(t, context.Background(), svc, "up", "feat-x", "--app", "shop", "--env", "feat-y"); code != ExitOK {
		t.Fatal("exit")
	}
	if svc.upReq.Name != "feat-x" {
		t.Errorf("env = %q, want the positional feat-x", svc.upReq.Name)
	}
}

func TestWithoutAnEnvironmentIsAUsageError(t *testing.T) {
	for _, args := range [][]string{
		{"up", "--app", "shop"},
		{"down", "--app", "shop"},
		{"test", "--app", "shop"},
		{"run", "--app", "shop", "--", "true"},
		{"logs", "--app", "shop"},
	} {
		svc := &m4Service{}
		code, _, stderr := runService(t, context.Background(), svc, args...)
		if code != ExitUsage {
			t.Errorf("%v: exit = %d, want %d", args, code, ExitUsage)
		}
		for _, want := range []string{"--env", "CARAMELO_ENV"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("%v: stderr = %q, want it to name %q", args, stderr, want)
			}
		}
	}
}

func TestWithoutAnAppIsAUsageError(t *testing.T) {
	svc := &m4Service{}
	code, _, stderr := runService(t, context.Background(), svc, "up", "feat-x")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "--app") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestAnInvalidEnvironmentNameIsAUsageError(t *testing.T) {
	svc := &m4Service{}
	code, _, stderr := runService(t, context.Background(), svc, "up", "Feat_X", "--app", "shop")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "lowercase") {
		t.Errorf("stderr = %q", stderr)
	}
}
