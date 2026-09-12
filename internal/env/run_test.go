package env

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/stack"
)

func TestDownRemovesTheServicesAndKeepsEverythingElse(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	removed, err := h.m.Down(context.Background(), DownRequest{App: "shop", Name: "feat-x"}, &h.out)
	if err != nil {
		t.Fatalf("Down: %v\n%s", err, h.out.String())
	}
	if len(removed) != 2 || removed[0].Change != ChangeRemoved {
		t.Fatalf("Down removed %+v, want both services", removed)
	}
	for _, name := range []string{"web", "echo"} {
		if h.driver.hasContainer(ContainerName("shop", "feat-x", name)) {
			t.Errorf("the container of %q is still there", name)
		}
	}

	for _, dep := range []string{"db", "cache"} {
		if !h.driver.hasContainer(ContainerName("shop", "feat-x", dep)) {
			t.Errorf("dependency %q was removed by down", dep)
		}
	}
	if !h.driver.hasNetwork(NetworkName("shop", "feat-x")) {
		t.Error("down removed the env's network")
	}
	row, _ := h.store.service(1, "web")
	if row.Status != "stopped" {
		t.Errorf("the stored row says %q, want stopped", row.Status)
	}

	again, err := h.m.Down(context.Background(), DownRequest{App: "shop", Name: "feat-x"}, &h.out)
	if err != nil {
		t.Fatalf("the second Down = %v", err)
	}
	for _, s := range again {
		if s.Change == ChangeRemoved {
			t.Errorf("the second down removed %q again", s.Name)
		}
	}
}

func TestDownThenUpStartsTheServicesAgain(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if _, err := h.m.Down(context.Background(), DownRequest{App: "shop", Name: "feat-x"}, &h.out); err != nil {
		t.Fatalf("Down: %v", err)
	}
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	for _, s := range res.Services {
		if s.Change != ChangeCreated {
			t.Errorf("service %q = %q after down, want created", s.Name, s.Change)
		}
	}
}

func TestDestroyRemovesTheNetworkAndTheCacheVolume(t *testing.T) {
	h := upHarness(t)
	h.cfg.Services = []config.Service{{Name: "web", Image: "python:3.12-alpine", Run: "python app.py"}}
	h.m.Detect = func(string) (*stack.Guess, error) {
		return &stack.Guess{Stack: stack.Python, Cache: "/root/.cache/pip"}, nil
	}
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if !contains(h.driver.volumeNames(), CacheVolumeName("shop", "feat-x")) {
		t.Fatalf("up did not create the cache volume: %v", h.driver.volumeNames())
	}

	if data := VolumeName("shop", "feat-x", "cache"); data == CacheVolumeName("shop", "feat-x") {
		t.Fatalf("the cache volume is the data volume of the dependency `cache`: %q", data)
	} else if !contains(h.driver.volumeNames(), data) {
		t.Fatalf("the dependency `cache` has no data volume: %v", h.driver.volumeNames())
	}

	if err := h.m.Destroy(context.Background(), DestroyRequest{App: "shop", Name: "feat-x", DeleteBranch: false}, &h.out); err != nil {
		t.Fatalf("Destroy: %v\n%s", err, h.out.String())
	}
	if h.driver.hasNetwork(NetworkName("shop", "feat-x")) {
		t.Error("the env's network survived destroy")
	}
	if contains(h.driver.volumeNames(), CacheVolumeName("shop", "feat-x")) {
		t.Error("the cache volume survived destroy")
	}
	if left := h.driver.volumeNames(); len(left) != 0 {
		t.Errorf("volumes left after destroy: %v", left)
	}
	if len(h.driver.containerNames()) != 0 {
		t.Errorf("containers left after destroy: %v", h.driver.containerNames())
	}
}

func TestTestRunsTheConfiguredCommandInTheServicesToolchain(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	var got runtime.ContainerSpec
	var stdin string
	h.driver.Attached = func(spec runtime.ContainerSpec, streams runtime.Streams) (int, error) {
		got = spec
		if streams.Stdin != nil {
			b, _ := io.ReadAll(streams.Stdin)
			stdin = string(b)
		}
		io.WriteString(streams.Stdout, "OK\n")
		return 0, nil
	}
	var out bytes.Buffer
	code, err := h.m.RunOneOff(context.Background(), RunRequest{
		App: "shop", Name: "feat-x", Test: true, Argv: []string{"-k", "not slow"},
		Stdin: strings.NewReader("piped"), Stdout: &out, Stderr: &h.out,
	})
	if err != nil {
		t.Fatalf("RunOneOff: %v\n%s", err, h.out.String())
	}
	if code != 0 || out.String() != "OK\n" {
		t.Errorf("test exited %d and said %q", code, out.String())
	}
	if stdin != "piped" {
		t.Errorf("the container's standard input was %q", stdin)
	}
	if !got.AutoRemove {
		t.Error("a one-off container is not removed when it exits")
	}
	if len(got.Publish) != 0 || got.Restart != "" {
		t.Errorf("a one-off publishes %v with restart %q, want neither", got.Publish, got.Restart)
	}
	if got.Network != NetworkName("shop", "feat-x") || got.WorkDir != MountPath || got.User != rootUser {
		t.Errorf("one-off spec = %+v, want the env network, the worktree and root", got)
	}
	if got.Env["DATABASE_URL"] != "postgres://postgres:caramelo@db:5432/postgres" {
		t.Errorf("a one-off got %q, want the network view", got.Env["DATABASE_URL"])
	}

	if !strings.Contains(got.Command[2], `python -m unittest "$@"`) {
		t.Errorf("command = %q, want the test command with the arguments appended", got.Command[2])
	}
	if !strings.Contains(got.Command[2], "pip install") {
		t.Errorf("command = %q, want the install step first", got.Command[2])
	}
	if want := []string{"caramelo", "-k", "not slow"}; !equalStrings(got.Command[3:], want) {
		t.Errorf("arguments = %v, want %v", got.Command[3:], want)
	}
}

func TestTestPassesTheChildsExitCodeBack(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.driver.Attached = func(runtime.ContainerSpec, runtime.Streams) (int, error) { return 5, nil }
	code, err := h.m.RunOneOff(context.Background(), RunRequest{App: "shop", Name: "feat-x", Test: true, Stderr: &h.out})
	if err != nil {
		t.Fatalf("RunOneOff: %v", err)
	}
	if code != 5 {
		t.Errorf("exit code = %d, want the child's", code)
	}
}

func TestTestWithoutATestCommandSaysHowToAddOne(t *testing.T) {
	h := upHarness(t)
	h.cfg.Test = ""
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	_, err := h.m.RunOneOff(context.Background(), RunRequest{App: "shop", Name: "feat-x", Test: true, Stderr: &h.out})
	if err == nil || !strings.Contains(err.Error(), "test:") {
		t.Fatalf("test without a test command = %v", err)
	}
}

func TestRunExecutesTheCommandAfterTheDashes(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	var got runtime.ContainerSpec
	h.driver.Attached = func(spec runtime.ContainerSpec, _ runtime.Streams) (int, error) {
		got = spec
		return 0, nil
	}
	argv := []string{"python", "-c", "import socket; socket.create_connection(('db', 5432))"}
	if _, err := h.m.RunOneOff(context.Background(), RunRequest{
		App: "shop", Name: "feat-x", Argv: argv, Stderr: &h.out,
	}); err != nil {
		t.Fatalf("RunOneOff: %v", err)
	}
	if !strings.Contains(got.Command[2], `exec "$@"`) {
		t.Errorf("command = %q, want the caller's argv exec'd", got.Command[2])
	}
	if !equalStrings(got.Command[4:], argv) {
		t.Errorf("arguments = %v, want %v passed through untouched", got.Command[4:], argv)
	}
}

func TestRunWithoutACommandSaysSo(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	_, err := h.m.RunOneOff(context.Background(), RunRequest{App: "shop", Name: "feat-x"})
	if err == nil || !strings.Contains(err.Error(), "--") {
		t.Fatalf("run without a command = %v, want it to name the missing --", err)
	}
}

func TestRunPicksTheServiceItWasAskedFor(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	var got runtime.ContainerSpec
	h.driver.Attached = func(spec runtime.ContainerSpec, _ runtime.Streams) (int, error) {
		got = spec
		return 0, nil
	}
	if _, err := h.m.RunOneOff(context.Background(), RunRequest{
		App: "shop", Name: "feat-x", Service: "echo", Argv: []string{"true"}, Stderr: &h.out,
	}); err != nil {
		t.Fatalf("RunOneOff: %v", err)
	}
	if got.Env["CARAMELO_SERVICE"] != "echo" {
		t.Errorf("the one-off ran as %q, want echo", got.Env["CARAMELO_SERVICE"])
	}
	_, err := h.m.RunOneOff(context.Background(), RunRequest{
		App: "shop", Name: "feat-x", Service: "nope", Argv: []string{"true"}, Stderr: &h.out,
	})
	if err == nil || !strings.Contains(err.Error(), "no such service") {
		t.Fatalf("run --service nope = %v", err)
	}
}

func TestRunReportsADriverFailure(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.driver.Attached = func(runtime.ContainerSpec, runtime.Streams) (int, error) {
		return 0, errors.New("cannot connect to the Docker daemon")
	}
	if _, err := h.m.RunOneOff(context.Background(), RunRequest{
		App: "shop", Name: "feat-x", Argv: []string{"true"}, Stderr: &h.out,
	}); err == nil || !strings.Contains(err.Error(), "Docker daemon") {
		t.Fatalf("RunOneOff = %v, want the driver's failure", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
