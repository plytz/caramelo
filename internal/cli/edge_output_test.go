package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/edge/certs"
	"github.com/plytz/caramelo/internal/env"
)

func exposedUp() *api.UpResult {
	now := time.Now()
	return &api.UpResult{
		Env: env.Env{App: "shop", Name: "feat-x", Status: "ready"},
		Services: []env.Service{{
			Name: "web", Container: "caramelo-shop-feat-x-web", Status: env.ServiceRunning,
			Health: env.HealthOK, Change: env.ChangeRecreated, URL: "http://127.0.0.1:20002",
			Host: "feat-x.shop.test", PublicURL: "https://feat-x.shop.test", Expose: "https",
			Replicas: []env.Replica{
				{Service: "web", Index: 3, Port: 20004, State: env.ReplicaActive,
					Status: env.ServiceRunning, Health: env.HealthOK, Since: now.Add(-30 * time.Second)},
				{Service: "web", Index: 4, Port: 20005, State: env.ReplicaActive,
					Status: env.ServiceRunning, Health: env.HealthOK, Inflight: 2, Since: now.Add(-20 * time.Second)},
			},
		}},
		Rollouts: []env.Rollout{{
			Service: "web", Host: "feat-x.shop.test", URL: "https://feat-x.shop.test",
			Steps: []env.RolloutStep{
				{Service: "web", Replica: 3, Step: env.StepStart, Status: env.StepOK},
				{Service: "web", Replica: 3, Step: env.StepFlip, Status: env.StepOK},
				{Service: "web", Replica: 1, Step: env.StepDrain, Status: env.StepOK, Inflight: 0},
			},
			Replicas: []env.Replica{{Service: "web", Index: 3}, {Service: "web", Index: 4}},
		}},
		Routes: []edge.Route{{
			Host: "feat-x.shop.test", Service: "web", CreatedAt: now.Add(-time.Hour),
			Targets: []edge.Target{
				{Replica: 3, Port: 20004, State: edge.TargetActive},
				{Replica: 4, Port: 20005, State: edge.TargetActive, Inflight: 2},
			},
		}},
		URL: "https://feat-x.shop.test",
	}
}

func TestUpPrintsTheRolloutTheRoutesAndTheURL(t *testing.T) {
	var out bytes.Buffer
	if err := writeUpResult(&out, exposedUp()); err != nil {
		t.Fatalf("writeUpResult: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"web/3", "web/4",
		"active",
		"20004",
		"https://feat-x.shop.test",
		"ROLLOUT",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("`up` output does not carry %q:\n%s", want, got)
		}
	}

	lines := strings.Split(strings.TrimSpace(got), "\n")
	if lines[len(lines)-1] != "https://feat-x.shop.test" {
		t.Errorf("the last line is %q, want the public URL", lines[len(lines)-1])
	}
}

func TestUpPrintsWhichReplicaFailed(t *testing.T) {
	r := exposedUp()
	failed := env.RolloutStep{Service: "web", Replica: 3, Step: env.StepHealth,
		Status: env.StepFailed, Detail: "/healthz answered 500"}
	r.Rollouts[0].Steps = append(r.Rollouts[0].Steps, failed)
	r.Rollouts[0].Failed = &failed
	r.URL = ""

	var out bytes.Buffer
	if err := writeUpResult(&out, r); err != nil {
		t.Fatalf("writeUpResult: %v", err)
	}
	for _, want := range []string{"failed at health of replica 3", "/healthz answered 500"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not carry %q:\n%s", want, out.String())
		}
	}
}

func TestUpOfAPlainServiceIsUnchanged(t *testing.T) {
	r := &api.UpResult{
		Env: env.Env{App: "shop", Name: "feat-x", Status: "ready"},
		Services: []env.Service{{
			Name: "web", Status: env.ServiceRunning, Health: env.HealthOK,
			Replicas: []env.Replica{{Service: "web", Index: 1, Port: 20002, State: env.ReplicaActive}},
		}},
	}
	var out bytes.Buffer
	if err := writeUpResult(&out, r); err != nil {
		t.Fatalf("writeUpResult: %v", err)
	}
	for _, never := range []string{"REPLICA", "ROLLOUT", "URL\tSERVICE"} {
		if strings.Contains(out.String(), never) {
			t.Errorf("a plain service printed %q:\n%s", never, out.String())
		}
	}
}

func TestEnvShowPrintsRoutesAndCertificates(t *testing.T) {
	now := time.Now()
	d := &api.EnvDetail{
		Env: env.Env{App: "shop", Name: "feat-x", Status: "ready", Branch: "feat-x"},
		Services: []env.Service{{
			Name: "web", Status: env.ServiceRunning, Host: "feat-x.shop.test",
			Replicas: []env.Replica{
				{Service: "web", Index: 1, Port: 20002, State: env.ReplicaActive, Inflight: 3},
				{Service: "web", Index: 2, Port: 20003, State: env.ReplicaDraining, Inflight: 1,
					Detail: "waiting for a websocket"},
			},
		}},
		Routes: []edge.Route{{Host: "feat-x.shop.test", Service: "web", CreatedAt: now.Add(-time.Hour),
			Targets: []edge.Target{{Replica: 1, Port: 20002, State: edge.TargetActive, Inflight: 3}}}},
		Certificates: []certs.Certificate{{Host: "feat-x.shop.test", Issuer: "Pebble Intermediate CA",
			NotAfter: now.Add(6 * 24 * time.Hour), Managed: true}},
	}
	var out bytes.Buffer
	if err := writeEnvDetail(&out, d); err != nil {
		t.Fatalf("writeEnvDetail: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"web/1", "web/2", "draining", "waiting for a websocket",
		"https://feat-x.shop.test", "Pebble Intermediate CA",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("`env show` does not carry %q:\n%s", want, got)
		}
	}
}

func TestCertificateTableMarksAnExpiredOne(t *testing.T) {
	var out bytes.Buffer
	if err := writeCertificates(&out, []certs.Certificate{
		{Host: "old.shop.test", NotAfter: time.Now().Add(-time.Hour)},
	}); err != nil {
		t.Fatalf("writeCertificates: %v", err)
	}
	if !strings.Contains(out.String(), "(expired)") {
		t.Errorf("output = %q, want it to say the certificate expired", out.String())
	}
}

func TestEnvURLNamesThePublicAddress(t *testing.T) {
	svc := &urlService{urls: sampleURLs(), routes: []edge.Route{
		{Host: "feat-x.shop.test", Service: "web"},
		{Host: "api.feat-x.shop.test", Service: "api"},
	}}
	code, stdout, stderr := runWithService(t, svc, "env", "url", "feat-x", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Errorf("stdout = %q, want exactly the one loopback URL", stdout)
	}
	if strings.Contains(stdout, "shop.test") {
		t.Errorf("stdout carries a public name: %q", stdout)
	}
	for _, want := range []string{"public: https://feat-x.shop.test (web)", "public: https://api.feat-x.shop.test (api)"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want %q", stderr, want)
		}
	}
}

func TestLogsEdgeNeedsNoEnvironment(t *testing.T) {
	svc := &m4Service{}
	code, _, stderr := runWithService(t, svc, "logs", "--edge")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if !svc.logsReq.Edge {
		t.Error("the daemon was not asked for the edge log")
	}
	if svc.logsReq.App != "" || svc.logsReq.Name != "" {
		t.Errorf("the request named %q/%q, want the machine", svc.logsReq.App, svc.logsReq.Name)
	}
}

func TestLogsEdgeKeepsTheEnvironmentWhenOneIsNamed(t *testing.T) {
	svc := &m4Service{}
	code, _, stderr := runWithService(t, svc, "logs", "feat-x", "--app", "shop", "--edge")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if svc.logsReq.Name != "feat-x" || svc.logsReq.App != "shop" {
		t.Errorf("the request named %q/%q, want shop/feat-x", svc.logsReq.App, svc.logsReq.Name)
	}
	if code, _, _ := runWithService(t, &m4Service{}, "logs"); code != ExitUsage {
		t.Errorf("`logs` with no environment exited %d, want a usage error", code)
	}
}

func TestEnvURLSaysNothingWhenNothingIsExposed(t *testing.T) {
	svc := &urlService{urls: sampleURLs()}
	code, _, stderr := runWithService(t, svc, "env", "url", "feat-x", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(stderr, "public:") {
		t.Errorf("stderr = %q, want no public note", stderr)
	}
}

func TestEdgeCountsPrintsPerReplicaAndATotal(t *testing.T) {
	since := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	counts := &edge.Counts{
		Since: since, At: since.Add(30 * time.Second),
		Hosts: []edge.HostCounts{{
			Host: "shop.test", Requests: 100, Status5xx: 3, ConnectFailures: 1,
			Targets: []edge.TargetCounts{
				{Replica: 1, Requests: 50},
				{Replica: 2, Requests: 50, Status5xx: 3, ConnectFailures: 1},
			},
		}},
	}
	var buf bytes.Buffer
	if err := writeEdgeCounts(&buf, counts); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"REPLICA", "shop.test", "total", "4.0%"} {
		if !strings.Contains(out, want) {
			t.Errorf("counts has no %q:\n%s", want, out)
		}
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 5 {
		t.Fatalf("%d line(s):\n%s", len(lines), out)
	}

	buf.Reset()
	if err := writeEdgeCounts(&buf, &edge.Counts{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "nothing has been served") {
		t.Errorf("empty counts = %q", buf.String())
	}

	buf.Reset()
	if err := writeEdgeCounts(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "no counts\n" {
		t.Errorf("nil counts = %q", buf.String())
	}
}
