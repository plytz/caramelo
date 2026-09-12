package release

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func oneRelease() *Release {
	return &Release{
		ID: 7, App: "shop", Commit: "c0ffee1234567890", Tree: "a1b2c3d4e5f6",
		Images: map[string]string{
			"web":    ImageRef("shop", "web", "a1b2c3d4e5f6"),
			"worker": ImageRef("shop", "worker", "a1b2c3d4e5f6"),
		},
	}
}

func imagesOn(machine, arch string, services ...string) Images {
	out := Images{}
	for _, s := range services {
		out = append(out, Image{
			ReleaseID: 7, Service: s, Ref: ImageRef("shop", s, "a1b2c3d4e5f6"),
			Arch: arch, Machine: machine, ID: "sha256:" + machine + s,
			BuiltAt: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
		})
	}
	return out
}

func TestDecideNothingToDoWhenTheMachineHoldsThemAll(t *testing.T) {
	r := oneRelease()
	plan, err := Decide(Need{
		Release: r, Machine: "m1", Arch: "arm64",
		Present: []string{r.Images["web"], r.Images["worker"]},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if plan.Action != PlanHave {
		t.Errorf("action = %q, want %q (%s)", plan.Action, PlanHave, plan.Why)
	}
	if len(plan.Refs) != 0 {
		t.Errorf("refs = %v, want none", plan.Refs)
	}
	if !strings.Contains(plan.Why, "m1") || !strings.Contains(plan.Why, r.Short()) {
		t.Errorf("why = %q, want the machine and the release named", plan.Why)
	}
}

func TestDecideCopiesFromAMachineOfTheSameArchitecture(t *testing.T) {
	r := oneRelease()
	plan, err := Decide(Need{
		Release: r, Machine: "m1", Arch: "arm64",
		Known:   imagesOn("m2", "arm64", "web", "worker"),
		Sources: []Source{{Machine: "m2", Reachable: true}},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if plan.Action != PlanCopy || plan.From != "m2" {
		t.Fatalf("plan = %+v, want a copy from m2", plan)
	}
	if strings.Join(plan.Services, ",") != "web,worker" {
		t.Errorf("services = %v, want both", plan.Services)
	}
	if len(plan.Refs) != 2 {
		t.Errorf("refs = %v, want two", plan.Refs)
	}
	if !strings.Contains(plan.Why, "arm64") {
		t.Errorf("why = %q, want the architecture named", plan.Why)
	}
}

func TestDecideBuildsWhenOnlyAnotherArchitectureHasThem(t *testing.T) {

	plan, err := Decide(Need{
		Release: oneRelease(), Machine: "m2", Arch: "amd64",
		Known:   imagesOn("m1", "arm64", "web", "worker"),
		Sources: []Source{{Machine: "m1", Reachable: true}},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if plan.Action != PlanBuild {
		t.Fatalf("plan = %+v, want a build", plan)
	}
	if len(plan.Candidates) != 0 {
		t.Errorf("candidates = %v, want none: they are arm64", plan.Candidates)
	}
	if !strings.Contains(plan.Why, "amd64") {
		t.Errorf("why = %q, want the architecture named", plan.Why)
	}
}

func TestDecideWillNotCopyHalfASet(t *testing.T) {

	plan, err := Decide(Need{
		Release: oneRelease(), Machine: "m1", Arch: "arm64",
		Known:   imagesOn("m2", "arm64", "web"),
		Sources: []Source{{Machine: "m2", Reachable: true}},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if plan.Action != PlanBuild {
		t.Fatalf("plan = %+v, want a build", plan)
	}
}

func TestDecideCompletesAPartialSetFromAMachineThatHasTheRest(t *testing.T) {
	r := oneRelease()
	plan, err := Decide(Need{
		Release: r, Machine: "m1", Arch: "arm64",
		Present: []string{r.Images["web"]},
		Known:   imagesOn("m2", "arm64", "worker"),
		Sources: []Source{{Machine: "m2", Reachable: true}},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if plan.Action != PlanCopy || plan.From != "m2" {
		t.Fatalf("plan = %+v, want a copy from m2", plan)
	}
	if strings.Join(plan.Services, ",") != "worker" {
		t.Errorf("services = %v, want worker alone: web is already here", plan.Services)
	}
}

func TestDecideIgnoresAnUnreachableHolderAndAnUnknownOne(t *testing.T) {
	known := append(imagesOn("m2", "arm64", "web", "worker"), imagesOn("gone", "arm64", "web", "worker")...)
	plan, err := Decide(Need{
		Release: oneRelease(), Machine: "m1", Arch: "arm64",
		Known: known,

		Sources: []Source{{Machine: "m2", Reachable: false}},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if plan.Action != PlanBuild {
		t.Fatalf("plan = %+v, want a build: nobody that can be reached has them", plan)
	}
}

func TestDecidePrefersTheFirstHolderByNameAndSaysWhoElseCould(t *testing.T) {
	known := append(imagesOn("m3", "arm64", "web", "worker"), imagesOn("m2", "arm64", "web", "worker")...)
	plan, err := Decide(Need{
		Release: oneRelease(), Machine: "m1", Arch: "arm64",
		Known:   known,
		Sources: []Source{{Machine: "m3", Reachable: true}, {Machine: "m2", Reachable: true}},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if plan.From != "m2" {
		t.Errorf("copied from %q, want m2: the same decision twice", plan.From)
	}
	if strings.Join(plan.Candidates, ",") != "m2,m3" {
		t.Errorf("candidates = %v, want both, sorted", plan.Candidates)
	}
}

func TestDecideNeverCopiesFromItself(t *testing.T) {

	plan, err := Decide(Need{
		Release: oneRelease(), Machine: "m1", Arch: "arm64",
		Known:   imagesOn("m1", "arm64", "web", "worker"),
		Sources: []Source{{Machine: "m1", Reachable: true}},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if plan.Action != PlanBuild {
		t.Fatalf("plan = %+v, want a build: a machine cannot copy from itself", plan)
	}
}

func TestDecideNarrowsToTheServicesAsked(t *testing.T) {
	plan, err := Decide(Need{
		Release: oneRelease(), Machine: "m1", Arch: "arm64", Services: []string{"web"},
		Known:   imagesOn("m2", "arm64", "web"),
		Sources: []Source{{Machine: "m2", Reachable: true}},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if plan.Action != PlanCopy || strings.Join(plan.Services, ",") != "web" {
		t.Fatalf("plan = %+v, want a copy of web alone", plan)
	}
}

func TestDecideRefusesWhatItCannotAnswer(t *testing.T) {
	r := oneRelease()
	cases := map[string]Need{
		"no release":  {Machine: "m1", Arch: "arm64"},
		"no images":   {Release: &Release{App: "shop", Tree: "a1b2c3"}, Machine: "m1", Arch: "arm64"},
		"no arch":     {Release: r, Machine: "m1"},
		"no such svc": {Release: r, Machine: "m1", Arch: "arm64", Services: []string{"nope"}},
	}
	for name, n := range cases {
		if _, err := Decide(n); err == nil {
			t.Errorf("Decide with %s: no error", name)
		}
	}
}

func TestDecideOnAMachineOfOneNamesItselfReadably(t *testing.T) {
	plan, err := Decide(Need{Release: oneRelease(), Arch: "amd64"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if plan.Action != PlanBuild {
		t.Fatalf("plan = %+v, want a build", plan)
	}
	if strings.Contains(plan.Why, `""`) {
		t.Errorf("why = %q, want no empty quotes on a machine with no fleet name", plan.Why)
	}
}

func TestServiceOfReadsCarameloOwnReferences(t *testing.T) {
	cases := map[string]string{
		ImageRef("shop", "web", "a1b2c3d4e5f6"): "web",
		"caramelo/shop/worker:t1":               "worker",
		"postgres:16-alpine":                    "",
		"caramelo/shop:t1":                      "",
		"caramelo/shop/web":                     "",
		"ghcr.io/caramelo/shop/web:t1":          "",
		"":                                      "",
	}
	for ref, want := range cases {
		if got := ServiceOf(ref); got != want {
			t.Errorf("ServiceOf(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestAttachFillsInWhereTheImagesAre(t *testing.T) {
	rows := newImages()
	ctx := context.Background()
	for _, im := range append(imagesOn("m1", "arm64", "web", "worker"),
		imagesOn("m2", "amd64", "web", "worker")...) {
		if err := rows.PutImage(ctx, im); err != nil {
			t.Fatal(err)
		}
	}
	r := oneRelease()
	if err := Attach(ctx, rows, r, nil); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if len(r.Built) != 4 {
		t.Fatalf("the release carries %d images, want 4", len(r.Built))
	}
	if strings.Join(r.Arches(), ",") != "amd64,arm64" {
		t.Errorf("arches = %v, want both, sorted", r.Arches())
	}

	plain := oneRelease()
	if err := Attach(ctx, nil, plain); err != nil {
		t.Fatalf("Attach with no index: %v", err)
	}
	if len(plain.Built) != 0 {
		t.Errorf("a machine with no index invented %d rows", len(plain.Built))
	}
}

func TestAttachSaysWhichReleaseCouldNotBeRead(t *testing.T) {
	rows := newImages()
	rows.err = errors.New("the database is locked")
	r := oneRelease()
	err := Attach(context.Background(), rows, r)
	if err == nil || !strings.Contains(err.Error(), r.Short()) {
		t.Fatalf("Attach: %v, want the release named", err)
	}
}
