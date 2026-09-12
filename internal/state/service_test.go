package state

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func sampleService(envID int64, name string) EnvService {
	return EnvService{
		EnvID:         envID,
		Name:          name,
		Image:         "python:3.12-alpine",
		Container:     "caramelo-shop-feat-x-" + name,
		Port:          20000,
		ContainerPort: 20000,
		Protocol:      "tcp",
		CommandJSON:   `["sh","-c","python app.py"]`,
		VarsJSON:      `{"PORT":"20000"}`,
		HealthJSON:    `{"path":"/healthz"}`,
		Status:        ServiceStarting,
		UpdatedAt:     time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC),
	}
}

func envForServices(t *testing.T, s Store) *EnvRecord {
	t.Helper()
	ctx := context.Background()
	if err := s.AddApp(ctx, sampleApp("shop")); err != nil {
		t.Fatal(err)
	}
	rec, err := s.CreateEnv(ctx, sampleEnv("shop", "feat-x", 20000))
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestServiceRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	rec := envForServices(t, s)

	if got, err := s.Services(ctx, rec.ID); err != nil || len(got) != 0 {
		t.Fatalf("services of a fresh env = %v, %v; want none", got, err)
	}

	want := sampleService(rec.ID, "web")
	if err := s.PutService(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Services(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("services = %+v, want one", got)
	}
	g := got[0]
	if g.ID == 0 {
		t.Error("the row came back without an id")
	}
	if g.Name != want.Name || g.Image != want.Image || g.Container != want.Container ||
		g.Port != want.Port || g.ContainerPort != want.ContainerPort || g.Protocol != want.Protocol ||
		g.CommandJSON != want.CommandJSON || g.VarsJSON != want.VarsJSON ||
		g.HealthJSON != want.HealthJSON || g.Status != want.Status {
		t.Errorf("round trip lost something:\n got %+v\nwant %+v", g, want)
	}
	if !g.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("UpdatedAt = %v, want %v", g.UpdatedAt, want.UpdatedAt)
	}
}

func TestPutServiceIsAnUpsert(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	rec := envForServices(t, s)

	for _, name := range []string{"web", "worker"} {
		if err := s.PutService(ctx, sampleService(rec.ID, name)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.Services(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}

	again := sampleService(rec.ID, "web")
	again.Status = ServiceRunning
	again.Image = "python:3.13-alpine"
	again.UpdatedAt = again.UpdatedAt.Add(time.Minute)
	if err := s.PutService(ctx, again); err != nil {
		t.Fatal(err)
	}
	got, err := s.Services(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("services = %+v, want two", got)
	}
	if got[0].Name != "web" || got[1].Name != "worker" {
		t.Errorf("services are not in creation order: %s, %s", got[0].Name, got[1].Name)
	}
	if got[0].ID != first[0].ID {
		t.Errorf("the upsert replaced the row (id %d -> %d) instead of updating it", first[0].ID, got[0].ID)
	}
	if got[0].Status != ServiceRunning || got[0].Image != "python:3.13-alpine" {
		t.Errorf("the upsert did not write the new values: %+v", got[0])
	}
}

func TestPutServiceFillsWhatItCan(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	rec := envForServices(t, s)

	before := time.Now()
	if err := s.PutService(ctx, EnvService{EnvID: rec.ID, Name: "web"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Services(ctx, rec.ID)
	if err != nil || len(got) != 1 {
		t.Fatalf("services = %v, %v", got, err)
	}
	if got[0].Status != ServiceStarting {
		t.Errorf("status = %q, want %q", got[0].Status, ServiceStarting)
	}
	if got[0].Protocol != "tcp" {
		t.Errorf("protocol = %q, want tcp", got[0].Protocol)
	}
	if got[0].UpdatedAt.Before(before.Add(-time.Second)) {
		t.Errorf("UpdatedAt = %v, want a time around now", got[0].UpdatedAt)
	}
}

func TestPutServiceRejectsIncompleteRows(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	rec := envForServices(t, s)

	for _, tc := range []struct {
		name string
		svc  EnvService
		want string
	}{
		{"no env", EnvService{Name: "web"}, "env id"},
		{"no name", EnvService{EnvID: rec.ID}, "name"},
		{"unknown env", EnvService{EnvID: rec.ID + 999, Name: "web"}, "no such env"},
	} {
		err := s.PutService(ctx, tc.svc)
		if err == nil {
			t.Errorf("PutService(%s) succeeded, want an error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("PutService(%s) = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestDeleteServiceIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	rec := envForServices(t, s)

	if err := s.PutService(ctx, sampleService(rec.ID, "web")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.DeleteService(ctx, rec.ID, "web"); err != nil {
			t.Fatalf("DeleteService (call %d): %v", i+1, err)
		}
	}
	if got, err := s.Services(ctx, rec.ID); err != nil || len(got) != 0 {
		t.Fatalf("services after delete = %v, %v; want none", got, err)
	}
	if err := s.DeleteService(ctx, rec.ID, "never-existed"); err != nil {
		t.Errorf("deleting a service that never existed = %v, want nil", err)
	}
}

func TestServicesCascadeWithTheEnv(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	rec := envForServices(t, s)

	if err := s.PutService(ctx, sampleService(rec.ID, "web")); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteEnv(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Services(ctx, rec.ID); err != nil || len(got) != 0 {
		t.Fatalf("services of a deleted env = %v, %v; want none", got, err)
	}
}

func TestSetAppStack(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	if err := s.AddApp(ctx, sampleApp("shop")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAppStack(ctx, "shop", "python"); err != nil {
		t.Fatal(err)
	}
	app, err := s.App(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if app.Stack != "python" {
		t.Errorf("stack = %q, want python", app.Stack)
	}

	if err := s.SetAppStack(ctx, "shop", ""); err != nil {
		t.Fatal(err)
	}
	if app, err := s.App(ctx, "shop"); err != nil || app.Stack != "" {
		t.Fatalf("stack = %v, %v; want empty", app, err)
	}
	if err := s.SetAppStack(ctx, "nosuchapp", "go"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetAppStack on a missing app = %v, want ErrNotFound", err)
	}
}

func TestServiceResourcesCarryTheirService(t *testing.T) {
	ctx := context.Background()
	s, _ := tempDB(t)
	rec := envForServices(t, s)

	for _, r := range []EnvResource{
		{EnvID: rec.ID, Kind: ResourceNetwork, Name: "caramelo-shop-feat-x"},
		{EnvID: rec.ID, Kind: ResourceCache, Name: "caramelo-shop-feat-x--cache"},
		{EnvID: rec.ID, Kind: ResourceImage, Name: "caramelo/shop:abc123", Service: "web"},
		{EnvID: rec.ID, Kind: ResourceContainer, Name: "caramelo-shop-feat-x-web", Service: "web", Port: 20000},
	} {
		if err := s.AddResource(ctx, r); err != nil {
			t.Fatalf("AddResource(%s): %v", r.Kind, err)
		}
	}
	got, err := s.Resources(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("resources = %+v, want four", got)
	}
	if got[0].Service != "" || got[2].Service != "web" || got[3].Service != "web" {
		t.Errorf("the service column did not round trip: %+v", got)
	}
	if got[3].Dep != "" {
		t.Errorf("a service resource carries a dep: %+v", got[3])
	}
}
