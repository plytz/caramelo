package env

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/ports"
	"github.com/plytz/caramelo/internal/runtime"
)

func TestServicesReportsWhatIsRunningNow(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	got, err := h.m.Services(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Services = %d, want 2", len(got))
	}
	if got[0].Status != ServiceRunning || got[0].ID == "" {
		t.Errorf("web = %+v, want it running with a container id", got[0])
	}

	if err := h.driver.Remove(context.Background(), ReplicaContainerName("shop", "feat-x", "web", 1), true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got, err = h.m.Services(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if got[0].Status != ServiceMissing {
		t.Errorf("web = %q after its container was removed, want missing", got[0].Status)
	}
}

func TestServicesReportsAnExitedContainer(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	h.driver.setStatus(ReplicaContainerName("shop", "feat-x", "web", 1), runtime.StatusExited)

	got, err := h.m.Services(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if got[0].Status != ServiceExited {
		t.Errorf("web = %q, want exited", got[0].Status)
	}
}

func TestURLsListsEveryServiceAndDependency(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	urls, err := h.m.URLs(context.Background(), "shop", "feat-x", "")
	if err != nil {
		t.Fatalf("URLs: %v", err)
	}
	want := map[string]string{
		"web":   fmt.Sprintf("http://127.0.0.1:%d", ports.Base),
		"echo":  fmt.Sprintf("udp://127.0.0.1:%d", ports.Base+3),
		"db":    fmt.Sprintf("tcp://127.0.0.1:%d", ports.Base+1),
		"cache": fmt.Sprintf("tcp://127.0.0.1:%d", ports.Base+2),
	}
	if len(urls) != len(want) {
		t.Fatalf("URLs = %+v, want %d of them", urls, len(want))
	}
	for _, u := range urls {
		if u.URL != want[u.Name] {
			t.Errorf("%s = %q, want %q", u.Name, u.URL, want[u.Name])
		}
	}

	one, err := h.m.URLs(context.Background(), "shop", "feat-x", "db")
	if err != nil {
		t.Fatalf("URLs(db): %v", err)
	}
	if len(one) != 1 || one[0].Kind != KindDep || one[0].Port != ports.Base+1 {
		t.Errorf("URLs(db) = %+v", one)
	}
	if _, err := h.m.URLs(context.Background(), "shop", "feat-x", "nope"); err == nil {
		t.Fatal("URLs of an unknown target succeeded")
	}
}

func TestExportRendersEitherView(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	host, err := h.m.ExportView(context.Background(), "shop", "feat-x", FormatDotenv, config.ViewHost, false)
	if err != nil {
		t.Fatalf("export host: %v", err)
	}
	if !strings.Contains(host, fmt.Sprintf("127.0.0.1:%d", ports.Base+1)) {
		t.Errorf("the host view does not carry the published port:\n%s", host)
	}
	network, err := h.m.ExportView(context.Background(), "shop", "feat-x", FormatDotenv, config.ViewNetwork, false)
	if err != nil {
		t.Fatalf("export network: %v", err)
	}
	if !strings.Contains(network, "@db:5432/") {
		t.Errorf("the network view does not carry the dependency's alias:\n%s", network)
	}
	if strings.Contains(network, "127.0.0.1") {
		t.Errorf("the network view still carries loopback addresses:\n%s", network)
	}

	for _, view := range []string{host, network} {
		if !strings.Contains(view, "CARAMELO_ENV=feat-x") {
			t.Errorf("a view is missing the built-in variables:\n%s", view)
		}
	}
}

func TestNetworkViewOfAnEnvCreatedBeforeM4(t *testing.T) {
	h := upHarness(t)
	h.cfg = sampleConfig()
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})

	ctx := context.Background()
	rec, err := h.store.Env(ctx, "shop", "feat-x")
	if err != nil {
		t.Fatalf("Env: %v", err)
	}
	views, err := h.m.Views(ctx, "shop", "feat-x")
	if err != nil {
		t.Fatalf("Views: %v", err)
	}
	stored, err := decodeConfig(rec)
	if err != nil {
		t.Fatalf("decodeConfig: %v", err)
	}
	resolved, err := stored.Resolve(config.ViewHost, views)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	blob, err := json.Marshal(resolved)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec.ConfigJSON = string(blob)
	if err := h.store.UpdateEnv(ctx, *rec); err != nil {
		t.Fatalf("UpdateEnv: %v", err)
	}

	vars, err := h.m.VarsIn(ctx, "shop", "feat-x", config.ViewNetwork, false)
	if err != nil {
		t.Fatalf("VarsIn(network): %v", err)
	}
	if got := vars["DATABASE_URL"]; !strings.Contains(got, "@db:5432/") {
		t.Errorf("DATABASE_URL = %q, want the dependency's alias on the env network", got)
	}
	for k, v := range vars {
		if strings.Contains(v, DepHost) {
			t.Errorf("%s = %q: the network view still carries a loopback address", k, v)
		}
	}

	host, err := h.m.VarsIn(ctx, "shop", "feat-x", config.ViewHost, false)
	if err != nil {
		t.Fatalf("VarsIn(host): %v", err)
	}
	if got := host["DATABASE_URL"]; !strings.Contains(got, DepHost) {
		t.Errorf("host DATABASE_URL = %q, want the published loopback address", got)
	}
}

func TestViewsCarryBothAddressTables(t *testing.T) {
	h := upHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	v, err := h.m.Views(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("Views: %v", err)
	}
	if got := v.Host.Deps["db"]; got.Host != DepHost || got.Port != ports.Base+1 {
		t.Errorf("host view of db = %+v", got)
	}
	if got := v.Network.Deps["db"]; got.Host != "db" || got.Port != 5432 {
		t.Errorf("network view of db = %+v", got)
	}
	if got := v.Network.Services["echo"]; got.Host != "echo" || got.Port != 9999 {
		t.Errorf("network view of echo = %+v", got)
	}
	if v.Host.Port != v.Network.Port {
		t.Errorf("${port} differs between views: %d and %d", v.Host.Port, v.Network.Port)
	}
}
