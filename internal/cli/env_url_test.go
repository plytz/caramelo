package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/env"
)

type urlService struct {
	api.Service

	asked [3]string
	urls  []env.URL
	err   error

	routes []edge.Route
}

func (s *urlService) URLs(_ context.Context, app, name, target string) ([]env.URL, error) {
	s.asked = [3]string{app, name, target}
	if s.err != nil {
		return nil, s.err
	}
	if target == "" {
		return s.urls, nil
	}
	var out []env.URL
	for _, u := range s.urls {
		if u.Name == target {
			out = append(out, u)
		}
	}
	return out, nil
}

func (s *urlService) Env(_ context.Context, app, name string) (*api.EnvDetail, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &api.EnvDetail{Env: env.Env{App: app, Name: name}, Routes: s.routes}, nil
}

func sampleURLs() []env.URL {
	return []env.URL{
		{Name: "web", Kind: env.KindService, URL: "http://127.0.0.1:20000", Host: "127.0.0.1", Port: 20000, Protocol: "tcp",
			InternalHost: "web.feat-x.shop.internal", InternalPort: 20000,
			InternalAddress: "10.86.1.4:20000", InternalURL: "http://web.feat-x.shop.internal:20000"},
		{Name: "echo", Kind: env.KindService, URL: "udp://127.0.0.1:20002", Host: "127.0.0.1", Port: 20002, Protocol: "udp",
			InternalHost: "echo.feat-x.shop.internal", InternalPort: 9999,
			InternalAddress: "10.86.1.4:9999", InternalURL: "udp://echo.feat-x.shop.internal:9999"},
		{Name: "db", Kind: env.KindDep, URL: "tcp://127.0.0.1:20001", Host: "127.0.0.1", Port: 20001, Protocol: "tcp",
			InternalHost: "db.feat-x.shop.internal", InternalPort: 5432,
			InternalAddress: "10.86.1.4:5432", InternalURL: "tcp://db.feat-x.shop.internal:5432"},
	}
}

func onBoxURLs() []env.URL {
	out := sampleURLs()
	for i := range out {
		out[i].InternalHost, out[i].InternalPort = "", 0
		out[i].InternalAddress, out[i].InternalURL = "", ""
	}
	return out
}

func TestEnvURLPrintsTheFirstService(t *testing.T) {
	svc := &urlService{urls: sampleURLs()}
	code, stdout, stderr := runWithService(t, svc, "env", "url", "feat-x", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitOK, stderr)
	}
	if svc.asked != [3]string{"shop", "feat-x", ""} {
		t.Errorf("daemon asked %v, want {shop feat-x }", svc.asked)
	}

	if stdout != "http://127.0.0.1:20000\n" {
		t.Errorf("stdout = %q, want the first service's URL alone", stdout)
	}

	if !strings.Contains(stderr, "http://web.feat-x.shop.internal:20000") ||
		!strings.Contains(stderr, "10.86.1.4:20000") {
		t.Errorf("stderr = %q, want the .internal URL and address", stderr)
	}
}

func TestEnvURLWithoutANetworkSaysNothingAboutOne(t *testing.T) {
	svc := &urlService{urls: onBoxURLs()}
	code, stdout, stderr := runWithService(t, svc, "env", "url", "feat-x", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitOK, stderr)
	}
	if stdout != "http://127.0.0.1:20000\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if strings.Contains(stderr, "internal") {
		t.Errorf("stderr = %q, want no mention of a network this machine has not got", stderr)
	}
}

func TestEnvURLWithATargetNotesItsInternalURL(t *testing.T) {
	svc := &urlService{urls: sampleURLs()}
	code, stdout, stderr := runWithService(t, svc, "env", "url", "feat-x", "echo", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	if stdout != "udp://127.0.0.1:20002\n" {
		t.Errorf("stdout = %q, want the on-box address", stdout)
	}

	if !strings.Contains(stderr, "udp://echo.feat-x.shop.internal:9999") {
		t.Errorf("stderr = %q, want the udp service on its own port", stderr)
	}
}

func TestEnvURLJSONCarriesBothAddresses(t *testing.T) {
	svc := &urlService{urls: sampleURLs()}
	code, stdout, stderr := runWithService(t, svc, "env", "url", "feat-x", "--app", "shop", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	var got []env.URL
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, stdout)
	}
	if got[0].URL != "http://127.0.0.1:20000" || got[0].InternalURL != "http://web.feat-x.shop.internal:20000" {
		t.Errorf("web = %+v, want both addresses", got[0])
	}
	if got[0].InternalPort != 20000 || got[1].InternalPort != 9999 || got[2].InternalPort != 5432 {
		t.Errorf("internal ports = %d, %d, %d", got[0].InternalPort, got[1].InternalPort, got[2].InternalPort)
	}
	if strings.Contains(stderr, "internal") {
		t.Errorf("stderr = %q, want --json to say everything on stdout", stderr)
	}
}

func TestEnvURLWithATarget(t *testing.T) {
	svc := &urlService{urls: sampleURLs()}
	code, stdout, _ := runWithService(t, svc, "env", "url", "feat-x", "db", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	if svc.asked[2] != "db" {
		t.Errorf("target = %q, want db", svc.asked[2])
	}
	if stdout != "tcp://127.0.0.1:20001\n" {
		t.Errorf("stdout = %q, want the dependency's URL", stdout)
	}
}

func TestEnvURLJSONListsEverything(t *testing.T) {
	svc := &urlService{urls: sampleURLs()}
	code, stdout, _ := runWithService(t, svc, "env", "url", "feat-x", "--app", "shop", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	var got []env.URL
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, stdout)
	}
	if len(got) != 3 {
		t.Fatalf("got %d URLs, want every service and dependency", len(got))
	}
	if got[0].Name != "web" || got[0].Kind != env.KindService || got[2].Kind != env.KindDep {
		t.Errorf("got %+v, want the services first and the deps after", got)
	}
}

func TestEnvURLWithoutAServiceSaysWhatToDo(t *testing.T) {
	svc := &urlService{urls: []env.URL{
		{Name: "db", Kind: env.KindDep, URL: "tcp://127.0.0.1:20001"},
	}}
	code, stdout, stderr := runWithService(t, svc, "env", "url", "feat-x", "--app", "shop")
	if code != ExitError {
		t.Fatalf("exit = %d, want %d", code, ExitError)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	for _, w := range []string{"caramelo up feat-x", "db"} {
		if !strings.Contains(stderr, w) {
			t.Errorf("stderr = %q, want it to mention %q", stderr, w)
		}
	}
}

func TestEnvURLJSONWithoutAServiceStillSucceeds(t *testing.T) {

	svc := &urlService{urls: []env.URL{}}
	code, stdout, _ := runWithService(t, svc, "env", "url", "feat-x", "--app", "shop", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	if strings.TrimSpace(stdout) != "[]" {
		t.Errorf("stdout = %q, want an empty JSON array", stdout)
	}
}

func TestEnvURLTooManyArgumentsIsAUsageError(t *testing.T) {
	for _, args := range [][]string{
		{"env", "url", "feat-x", "db", "web", "--app", "shop"},
		{"env", "url", "feat-x", "db", "web", "echo", "--app", "shop"},
	} {
		svc := &urlService{urls: sampleURLs()}
		code, _, stderr := runWithService(t, svc, args...)
		if code != ExitUsage {
			t.Fatalf("%v: exit = %d, want %d (stderr %q)", args, code, ExitUsage, stderr)
		}
		if !strings.Contains(stderr, "env url") {
			t.Errorf("%v: stderr = %q, want the command named", args, stderr)
		}
		if svc.asked != [3]string{} {
			t.Errorf("%v: the daemon was called with %v; the commander should have refused", args, svc.asked)
		}
	}
}

func TestEnvURLNeedsAnEnvironment(t *testing.T) {
	code, _, _ := runWithService(t, &urlService{}, "env", "url", "--app", "shop")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
}
