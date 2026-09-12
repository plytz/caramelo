package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/edge/certs"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/sshapi"
)

type edgeService struct {
	api.Service

	status *edge.Status
	ca     *certs.CA
	expose *api.ExposeResult
	err    error

	exposeReq   env.ExposeRequest
	unexposeReq env.UnexposeRequest
}

func (s *edgeService) EdgeStatus(context.Context) (*edge.Status, error) {
	return s.status, s.err
}

func (s *edgeService) EdgeCA(context.Context) (*certs.CA, error) { return s.ca, s.err }

func (s *edgeService) Expose(_ context.Context, req env.ExposeRequest) (*api.ExposeResult, error) {
	s.exposeReq = req
	return s.expose, s.err
}

func (s *edgeService) Unexpose(_ context.Context, req env.UnexposeRequest) (*api.ExposeResult, error) {
	s.unexposeReq = req
	return s.expose, s.err
}

func edgeStatusFixture() *edge.Status {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	return &edge.Status{
		Running: true, Version: "v0.6.0", StartedAt: now.Add(-time.Hour),
		Listeners: []string{"tcp 0.0.0.0:80", "tcp 0.0.0.0:443", "udp 0.0.0.0:443"},
		HTTP3:     true, TLS: certs.ModeACME, ACMEDirectory: "https://127.0.0.1:14000/dir",
		Routes: []edge.Route{{
			Host: "feat-x.shop.test", Kind: edge.KindHTTPS, App: "shop", Env: "feat-x", Service: "web",
			Targets: []edge.Target{
				{Replica: 1, Port: 20002, State: edge.TargetActive, Inflight: 2},
				{Replica: 2, Port: 20003, State: edge.TargetDraining, Inflight: 1},
			},
		}},
		Certificates: []certs.Certificate{{
			Host: "feat-x.shop.test", Issuer: "Pebble Intermediate CA", Serial: "42C5",
			NotAfter: now.Add(48 * time.Hour), Managed: true,
		}},
	}
}

func TestEdgeStatusHumanOutput(t *testing.T) {
	svc := &edgeService{status: edgeStatusFixture()}
	code, stdout, stderr := runWithService(t, svc, "edge", "status")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	for _, want := range []string{
		"edge running", "v0.6.0", "https://127.0.0.1:14000/dir", "udp 0.0.0.0:443",
		"feat-x.shop.test", "shop/feat-x", "web", "active", "draining", "20002",
		"Pebble Intermediate CA",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not carry %q:\n%s", want, stdout)
		}
	}
}

func TestEdgeStatusReportsAnEdgeThatIsOff(t *testing.T) {
	svc := &edgeService{status: &edge.Status{Running: false, Error: "this machine has no edge"}}
	code, stdout, _ := runWithService(t, svc, "edge", "status")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0: a machine with no edge is an answer, not a failure", code)
	}
	if !strings.Contains(stdout, "edge not running") || !strings.Contains(stdout, "no edge") {
		t.Errorf("stdout = %q, want the reason", stdout)
	}
	if !strings.Contains(stdout, "no routes") {
		t.Errorf("stdout = %q, want it to say nothing is exposed", stdout)
	}
}

func TestEdgeStatusJSON(t *testing.T) {
	svc := &edgeService{status: edgeStatusFixture()}
	code, stdout, _ := runWithService(t, svc, "edge", "status", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var got edge.Status
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("--json emitted %q: %v", stdout, err)
	}
	if len(got.Routes) != 1 || len(got.Routes[0].Targets) != 2 || len(got.Certificates) != 1 {
		t.Errorf("parsed %+v, want the routes, their targets and the certificate", got)
	}
}

func TestEdgeCAPrintsThePEMAlone(t *testing.T) {
	svc := &edgeService{ca: &certs.CA{
		Subject: "Caramelo Internal CA", Fingerprint: "abc123",
		PEM: "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----",
	}}
	code, stdout, stderr := runWithService(t, svc, "edge", "ca")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if !strings.HasPrefix(stdout, "-----BEGIN CERTIFICATE-----") || !strings.HasSuffix(stdout, "\n") {
		t.Errorf("stdout = %q, want the PEM and a trailing newline", stdout)
	}
	if strings.Contains(stdout, "Caramelo Internal CA") {
		t.Errorf("stdout carries the description, which would break the trust store: %q", stdout)
	}
	for _, want := range []string{"Caramelo Internal CA", "sha256:abc123"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want %q", stderr, want)
		}
	}
}

func TestEdgeCAPassesTheRefusalThrough(t *testing.T) {
	svc := &edgeService{err: errors.New("this machine issues certificates from https://acme-v02…")}
	code, stdout, stderr := runWithService(t, svc, "edge", "ca")
	if code != ExitError {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	if !strings.Contains(stderr, "issues certificates from") {
		t.Errorf("stderr = %q, want the machine's reason", stderr)
	}
}

func TestEnvExposeOutput(t *testing.T) {
	svc := &edgeService{expose: &api.ExposeResult{
		Env: env.Env{App: "shop", Name: "feat-x"},
		Routes: []edge.Route{{
			Host: "feat-x.shop.test", Service: "web", CreatedAt: time.Now(),
			Targets: []edge.Target{
				{Replica: 1, Port: 20002, State: edge.TargetActive, Inflight: 1},
				{Replica: 2, Port: 20003, State: edge.TargetStarting},
			},
		}},
		Changed: []string{"feat-x.shop.test"},
		URL:     "https://feat-x.shop.test",
	}}
	code, stdout, stderr := runWithService(t, svc, "env", "expose", "feat-x", "--app", "shop", "--service", "web")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if svc.exposeReq != (env.ExposeRequest{App: "shop", Name: "feat-x", Service: "web"}) {
		t.Errorf("the daemon was asked %+v", svc.exposeReq)
	}
	for _, want := range []string{"feat-x.shop.test exposed", "https://feat-x.shop.test", "1/2"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not carry %q:\n%s", want, stdout)
		}
	}
}

func TestEnvExposeIsIdempotentInItsOutput(t *testing.T) {
	svc := &edgeService{expose: &api.ExposeResult{
		Env:    env.Env{App: "shop", Name: "feat-x"},
		Routes: []edge.Route{{Host: "feat-x.shop.test", Service: "web"}},
		URL:    "https://feat-x.shop.test",
	}}
	code, stdout, _ := runWithService(t, svc, "env", "expose", "feat-x", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "nothing changed") {
		t.Errorf("stdout = %q, want it to say nothing changed", stdout)
	}
}

func TestEnvUnexposeOutput(t *testing.T) {
	svc := &edgeService{expose: &api.ExposeResult{
		Env:     env.Env{App: "shop", Name: "feat-x"},
		Changed: []string{"api.feat-x.shop.test"},
	}}
	code, stdout, stderr := runWithService(t, svc,
		"env", "unexpose", "feat-x", "--app", "shop", "--host", "api.feat-x.shop.test")
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if svc.unexposeReq.Host != "api.feat-x.shop.test" {
		t.Errorf("the daemon was asked %+v", svc.unexposeReq)
	}
	for _, want := range []string{"api.feat-x.shop.test no longer exposed", "still reachable on the machine's network"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not carry %q:\n%s", want, stdout)
		}
	}
}

func TestEnvExposeJSON(t *testing.T) {
	svc := &edgeService{expose: &api.ExposeResult{
		Env:     env.Env{App: "shop", Name: "feat-x"},
		Routes:  []edge.Route{{Host: "feat-x.shop.test", Service: "web"}},
		Changed: []string{"feat-x.shop.test"},
		URL:     "https://feat-x.shop.test",
	}}
	code, stdout, _ := runWithService(t, svc, "env", "expose", "feat-x", "--app", "shop", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var got api.ExposeResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("--json emitted %q: %v", stdout, err)
	}
	if got.URL != "https://feat-x.shop.test" || len(got.Routes) != 1 {
		t.Errorf("parsed %+v", got)
	}
}

func TestEdgeTestKnobsAreReadFromTheEnvironment(t *testing.T) {
	env := map[string]string{
		envACMEProfile:        " shortlived ",
		envRenewalWindowRatio: "1.0",
		envRenewCheckInterval: "5s",
	}
	var opts edge.Options
	var warn bytes.Buffer
	applyEdgeTestKnobs(&opts, func(k string) string { return env[k] }, &warn)

	if opts.ACMEProfile != "shortlived" {
		t.Errorf("profile = %q, want the trimmed value", opts.ACMEProfile)
	}
	if opts.RenewalWindowRatio != 1.0 {
		t.Errorf("ratio = %v, want 1", opts.RenewalWindowRatio)
	}
	if opts.RenewCheckInterval != 5*time.Second {
		t.Errorf("interval = %s, want 5s", opts.RenewCheckInterval)
	}
	if warn.Len() != 0 {
		t.Errorf("a valid set warned: %s", warn.String())
	}
}

func TestEdgeTestKnobsRefuseNonsenseWithoutFailing(t *testing.T) {
	var opts edge.Options
	var warn bytes.Buffer
	applyEdgeTestKnobs(&opts, func(string) string { return "" }, &warn)
	if opts.ACMEProfile != "" || opts.RenewalWindowRatio != 0 || opts.RenewCheckInterval != 0 {
		t.Errorf("an empty environment moved something: %+v", opts)
	}
	if warn.Len() != 0 {
		t.Errorf("an empty environment warned: %s", warn.String())
	}

	bad := map[string]string{
		envRenewalWindowRatio: "7",
		envRenewCheckInterval: "later",
	}
	applyEdgeTestKnobs(&opts, func(k string) string { return bad[k] }, &warn)
	if opts.RenewalWindowRatio != 0 || opts.RenewCheckInterval != 0 {
		t.Errorf("a refused value was used anyway: %+v", opts)
	}
	for _, want := range []string{envRenewalWindowRatio, envRenewCheckInterval} {
		if !strings.Contains(warn.String(), want) {
			t.Errorf("the warning does not name %s: %s", want, warn.String())
		}
	}
}

func TestEveryEdgeSubcommandReachesTheAPI(t *testing.T) {
	root := NewRootCmd(io.Discard, io.Discard)
	edge, _, err := root.Find([]string{"edge"})
	if err != nil {
		t.Fatalf("no edge command: %v", err)
	}
	for _, sub := range edge.Commands() {
		name := sub.Name()
		if !sshapi.EdgeSubcommands[name] {
			t.Errorf("`caramelo edge %s` is not in sshapi.EdgeSubcommands, "+
				"so the daemon refuses it over the API", name)
		}
		for _, alias := range sub.Aliases {
			if !sshapi.EdgeSubcommands[alias] {
				t.Errorf("the alias `caramelo edge %s` is not in sshapi.EdgeSubcommands", alias)
			}
		}
	}
}
