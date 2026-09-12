package env

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLogsMergesTheServicesWithAPrefix(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	h.driver.logs[ReplicaContainerName("shop", "feat-x", "web", 1)] = "GET / 200\nGET /deps 200\n"
	h.driver.logs[ReplicaContainerName("shop", "feat-x", "echo", 1)] = "echo: ping\n"

	var out bytes.Buffer
	if err := h.m.Logs(context.Background(), LogsRequest{
		App: "shop", Name: "feat-x", Stdout: &out, Stderr: &h.out,
	}); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	got := out.String()
	for _, want := range []string{"web  | GET / 200", "web  | GET /deps 200", "echo | echo: ping"} {
		if !strings.Contains(got, want) {
			t.Errorf("logs do not contain %q:\n%s", want, got)
		}
	}
}

func TestLogsIncludesTheDependenciesWhenAsked(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	h.driver.logs[ReplicaContainerName("shop", "feat-x", "web", 1)] = "web up\n"
	h.driver.logs[ReplicaContainerName("shop", "feat-x", "echo", 1)] = "echo up\n"
	h.driver.logs[ContainerName("shop", "feat-x", "db")] = "database system is ready\n"

	var out bytes.Buffer
	if err := h.m.Logs(context.Background(), LogsRequest{
		App: "shop", Name: "feat-x", Deps: true, Stdout: &out, Stderr: &h.out,
	}); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !strings.Contains(out.String(), "database system is ready") {
		t.Errorf("--deps did not include the dependency's output:\n%s", out.String())
	}
}

func TestLogsOfOneServiceLeavesTheOthersOut(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	h.driver.logs[ReplicaContainerName("shop", "feat-x", "web", 1)] = "from web\n"
	h.driver.logs[ReplicaContainerName("shop", "feat-x", "echo", 1)] = "from echo\n"

	var out bytes.Buffer
	if err := h.m.Logs(context.Background(), LogsRequest{
		App: "shop", Name: "feat-x", Services: []string{"web"}, Stdout: &out, Stderr: &h.out,
	}); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if strings.Contains(out.String(), "from echo") {
		t.Errorf("logs web included another service:\n%s", out.String())
	}
}

func TestLogsJSONCarriesTheServiceTheStreamAndTheTimestamp(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	h.driver.logs[ReplicaContainerName("shop", "feat-x", "web", 1)] = "2026-09-08T12:00:00.123456789Z GET / 200\n"
	h.driver.logsErr[ReplicaContainerName("shop", "feat-x", "web", 1)] = "2026-09-08T12:00:01.000000000Z warning: slow\n"
	h.driver.logs[ReplicaContainerName("shop", "feat-x", "echo", 1)] = ""

	var out bytes.Buffer
	if err := h.m.Logs(context.Background(), LogsRequest{
		App: "shop", Name: "feat-x", Services: []string{"web"}, JSON: true, Stdout: &out, Stderr: &h.out,
	}); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("--json produced %d lines:\n%s", len(lines), out.String())
	}
	seen := map[string]LogLine{}
	for _, l := range lines {
		var one LogLine
		if err := json.Unmarshal([]byte(l), &one); err != nil {
			t.Fatalf("line %q is not JSON: %v", l, err)
		}
		if one.Service != "web" {
			t.Errorf("line %q does not carry the service", l)
		}
		seen[one.Stream] = one
	}
	if got := seen[streamStdout]; got.Line != "GET / 200" || got.TS != "2026-09-08T12:00:00.123456789Z" {
		t.Errorf("stdout line = %+v, want the timestamp in a field of its own", got)
	}
	if got := seen[streamStderr]; got.Line != "warning: slow" {
		t.Errorf("stderr line = %+v", got)
	}
}

func TestLogsWritesALineThatArrivedInPieces(t *testing.T) {
	sink := &logSink{out: &bytes.Buffer{}, width: 3}
	w := sink.writer("web", streamStdout)
	w.Write([]byte("half a "))
	w.Write([]byte("line\nand a whole one\n"))
	w.Write([]byte("no newline here"))
	w.flush()
	got := sink.out.(*bytes.Buffer).String()
	want := "web | half a line\nweb | and a whole one\nweb | no newline here\n"
	if got != want {
		t.Errorf("merged output =\n%q\nwant\n%q", got, want)
	}
}

func TestLogsFollowEndsWhenTheClientGoesAway(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	h.driver.logs[ReplicaContainerName("shop", "feat-x", "web", 1)] = "still here\n"
	h.driver.logs[ReplicaContainerName("shop", "feat-x", "echo", 1)] = ""

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var out bytes.Buffer
	go func() {
		done <- h.m.Logs(ctx, LogsRequest{App: "shop", Name: "feat-x", Follow: true, Stdout: &out, Stderr: &h.out})
	}()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("logs -f ended with %v, want nil: being cancelled is how it ends", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("logs -f did not end when the client went away")
	}
}

func TestLogsSkipsAServiceThatHasNoContainerYet(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	h.driver.logs[ReplicaContainerName("shop", "feat-x", "web", 1)] = "only me\n"

	var out bytes.Buffer
	if err := h.m.Logs(context.Background(), LogsRequest{
		App: "shop", Name: "feat-x", Stdout: &out, Stderr: &h.out,
	}); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !strings.Contains(out.String(), "only me") {
		t.Errorf("the service that does have logs was not shown:\n%s", out.String())
	}
	if !strings.Contains(h.out.String(), "echo") {
		t.Errorf("nothing was said about the missing container:\n%s", h.out.String())
	}
}
