package env

import (
	"context"
	"testing"

	"github.com/plytz/caramelo/internal/config"
)

func TestUpGivesAServiceItsResourceLimits(t *testing.T) {
	h := upHarness(t)
	h.cfg.Services[0].Resources = &config.Resources{Memory: 512 << 20, CPU: 1.5}
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	spec, ok := h.driver.spec(ReplicaContainerName("shop", "feat-x", "web", 1))
	if !ok {
		t.Fatalf("web has no container\n%s", h.out.String())
	}
	if spec.Memory != 512<<20 || spec.CPU != 1.5 {
		t.Errorf("web spec limits = %d bytes, %v cpus", spec.Memory, spec.CPU)
	}

	other, ok := h.driver.spec(ReplicaContainerName("shop", "feat-x", "echo", 1))
	if !ok {
		t.Fatal("echo has no container")
	}
	if other.Memory != 0 || other.CPU != 0 {
		t.Errorf("echo spec limits = %d bytes, %v cpus, want unlimited", other.Memory, other.CPU)
	}

	rec, err := h.store.Env(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := decodeConfig(rec)
	if err != nil {
		t.Fatal(err)
	}
	svc, ok := cfg.Service("web")
	if !ok || svc.Resources == nil || svc.Resources.Memory != 512<<20 {
		t.Errorf("the stored config's web resources = %+v", svc.Resources)
	}
}

func TestUpAppliesTheEnvOverride(t *testing.T) {
	h := upHarness(t)
	h.cfg.Envs = map[string]config.EnvOverride{
		"feat-x": {Services: map[string]config.ServiceOverride{
			"web": {Replicas: 2, Resources: &config.Resources{Memory: 64 << 20}},
		}},

		"production": {Services: map[string]config.ServiceOverride{"web": {Replicas: 9}}},
	}
	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	res := h.mustUp(UpRequest{App: "shop", Name: "feat-x"})

	if len(res.Services[0].Replicas) != 2 {
		t.Fatalf("web has %d replicas, want the override's 2\n%s", len(res.Services[0].Replicas), h.out.String())
	}
	for i := 1; i <= 2; i++ {
		spec, ok := h.driver.spec(ReplicaContainerName("shop", "feat-x", "web", i))
		if !ok {
			t.Fatalf("replica %d has no container", i)
		}
		if spec.Memory != 64<<20 {
			t.Errorf("replica %d memory = %d, want the override's", i, spec.Memory)
		}
	}
}

func TestEnvHostsReplaceTheDerivedName(t *testing.T) {
	h, e := edgeHarness(t, 1)
	h.cfg.Envs = map[string]config.EnvOverride{
		"production": {Hosts: []string{"shop.test", "www.shop.test"}},
	}
	h.mustCreate(CreateRequest{App: "shop", Name: "production"})
	res := h.mustUp(UpRequest{App: "shop", Name: "production"})

	hosts := h.store.hosts()
	if len(hosts) != 2 || hosts[0] != "shop.test" || hosts[1] != "www.shop.test" {
		t.Fatalf("routes = %v, want both names of the block\n%s", hosts, h.out.String())
	}

	for _, host := range hosts {
		r, ok := e.table().Route(host)
		if !ok {
			t.Fatalf("the edge does not serve %s", host)
		}
		if len(r.Targets) != 1 || r.Targets[0].State != "active" {
			t.Errorf("%s targets = %+v", host, r.Targets)
		}
	}

	if res.URL != "https://shop.test" {
		t.Errorf("up URL = %q, want the first name of the block", res.URL)
	}

	h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	h.mustUp(UpRequest{App: "shop", Name: "feat-x"})
	if _, err := h.store.RouteByHost(context.Background(), webHost); err != nil {
		t.Errorf("feat-x does not serve %s: %v", webHost, err)
	}
}
