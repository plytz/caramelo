package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

type fakeBuilder struct {
	rel   *Release
	calls []BuildRequest
	built bool
	err   error
}

func (b *fakeBuilder) Build(_ context.Context, req BuildRequest, out io.Writer) (*BuildResult, error) {
	b.calls = append(b.calls, req)
	if b.err != nil {
		return nil, b.err
	}
	if out != nil {
		fmt.Fprintln(out, "[changed] build: two images")
	}
	return &BuildResult{Release: b.rel, Built: b.built, Images: b.rel.ImageList()}, nil
}

func (b *fakeBuilder) Prune(context.Context, string, int) ([]string, error) { return nil, nil }

type fakePuller struct {
	from *Transfer

	asked []string
	err   error
}

func (p *fakePuller) PullImages(ctx context.Context, from string, req TransferRequest) (io.ReadCloser, error) {
	p.asked = append(p.asked, from)
	if p.err != nil {
		return nil, p.err
	}
	var buf bytes.Buffer
	if _, err := p.from.Send(ctx, req, &buf); err != nil {
		return nil, err
	}
	return io.NopCloser(&buf), nil
}

func twoMachines(t *testing.T) (*Supply, *fakeBuilder, *fakePuller, *fakeImages) {
	t.Helper()
	r := oneRelease()
	sender := newMover("arm64", refsOf(r)...)
	from := &Transfer{Mover: sender, Machine: "m2", Arch: "arm64", Now: func() time.Time { return frozen }}

	receiver := newMover("arm64")
	rows := newImages()
	for _, im := range imagesOn("m2", "arm64", "web", "worker") {
		if err := rows.PutImage(context.Background(), im); err != nil {
			t.Fatal(err)
		}
	}
	to := &Transfer{Mover: receiver, Store: rows, Machine: "m1", Arch: "arm64",
		Now: func() time.Time { return frozen }}
	builder := &fakeBuilder{rel: r, built: true}
	puller := &fakePuller{from: from}
	return &Supply{
		Images: to, Builder: builder, Puller: puller,
		Sources: func(context.Context) ([]Source, error) {
			return []Source{{Machine: "m2", Reachable: true}}, nil
		},
	}, builder, puller, rows
}

func TestEnsureCopiesInsteadOfBuilding(t *testing.T) {
	s, builder, puller, rows := twoMachines(t)
	r := oneRelease()

	var out bytes.Buffer
	res, err := s.Ensure(context.Background(),
		EnsureRequest{App: "shop", Env: "production", Release: r}, &out)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Plan.Action != PlanCopy || res.Plan.From != "m2" {
		t.Fatalf("plan = %+v, want a copy from m2", res.Plan)
	}
	if len(builder.calls) != 0 {
		t.Errorf("built %d times, want none: the images were a stream away", len(builder.calls))
	}
	if strings.Join(puller.asked, ",") != "m2" {
		t.Errorf("pulled from %v, want m2", puller.asked)
	}
	if res.Copied == nil || res.Copied.Bytes == 0 {
		t.Fatalf("copied = %+v, want the bytes that crossed", res.Copied)
	}

	stored, err := rows.Images(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Machines("arm64", r.Services()); strings.Join(got, ",") != "m1,m2" {
		t.Errorf("Machines = %v, want both", got)
	}

	if !strings.Contains(out.String(), "copied from m2") {
		t.Errorf("the feed does not say a copy happened:\n%s", out.String())
	}
}

func TestEnsureDoesNothingWhenTheImagesAreAlreadyHere(t *testing.T) {
	s, builder, puller, _ := twoMachines(t)
	r := oneRelease()
	for _, ref := range refsOf(r) {
		s.Images.Mover.(*fakeMover).hold(ref, "arm64")
	}
	res, err := s.Ensure(context.Background(),
		EnsureRequest{App: "shop", Env: "production", Release: r}, nil)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Plan.Action != PlanHave || res.Built || res.Copied != nil {
		t.Fatalf("result = %+v, want nothing to have happened", res)
	}
	if len(builder.calls) != 0 || len(puller.asked) != 0 {
		t.Error("a machine that holds the images built or copied anyway")
	}
}

func TestEnsureBuildsWhenNobodyReachableHasThem(t *testing.T) {
	s, builder, puller, _ := twoMachines(t)
	s.Sources = func(context.Context) ([]Source, error) {
		return []Source{{Machine: "m2", Reachable: false}}, nil
	}
	res, err := s.Ensure(context.Background(),
		EnsureRequest{App: "shop", Env: "production", Release: oneRelease()}, nil)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Plan.Action != PlanBuild || !res.Built {
		t.Fatalf("result = %+v, want a build", res)
	}
	if len(puller.asked) != 0 {
		t.Errorf("pulled from %v with nobody reachable", puller.asked)
	}
	if len(builder.calls) != 1 || builder.calls[0].Env != "production" {
		t.Errorf("builds = %+v, want one for production", builder.calls)
	}
}

func TestEnsureBuildsWhenACopyFailsAndSaysWhy(t *testing.T) {

	s, builder, _, _ := twoMachines(t)
	s.Puller = &fakePuller{err: errors.New("dial 10.88.0.1:4022: connection refused")}

	var out bytes.Buffer
	res, err := s.Ensure(context.Background(),
		EnsureRequest{App: "shop", Env: "production", Release: oneRelease()}, &out)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Plan.Action != PlanBuild || !res.Built {
		t.Fatalf("result = %+v, want a build after the copy failed", res)
	}
	if len(builder.calls) != 1 {
		t.Fatalf("built %d times, want once", len(builder.calls))
	}
	line := out.String()
	if !strings.Contains(line, "connection refused") || !strings.Contains(line, "m2") {
		t.Errorf("the feed does not say why the copy failed:\n%s", line)
	}
	if !strings.Contains(line, "[warning]") {
		t.Errorf("a failed copy was not a warning:\n%s", line)
	}
}

func TestEnsureBuildsWhenThereIsNoReleaseYet(t *testing.T) {

	s, builder, puller, _ := twoMachines(t)
	res, err := s.Ensure(context.Background(),
		EnsureRequest{App: "shop", Env: "production", Ref: "HEAD"}, nil)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Plan.Action != PlanBuild || len(builder.calls) != 1 || builder.calls[0].Ref != "HEAD" {
		t.Fatalf("result = %+v, builds = %+v", res, builder.calls)
	}
	if len(puller.asked) != 0 {
		t.Error("a release that does not exist was copied from somewhere")
	}
}

func TestEnsureForcedNeverCopies(t *testing.T) {

	s, builder, puller, _ := twoMachines(t)
	if _, err := s.Ensure(context.Background(),
		EnsureRequest{App: "shop", Env: "production", Release: oneRelease(), Force: true}, nil); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(puller.asked) != 0 {
		t.Errorf("a forced build copied from %v", puller.asked)
	}
	if len(builder.calls) != 1 || !builder.calls[0].Force {
		t.Errorf("builds = %+v, want one forced", builder.calls)
	}
}

func TestEnsureOnAMachineOfOneOnlyEverBuilds(t *testing.T) {
	builder := &fakeBuilder{rel: oneRelease(), built: true}
	s := &Supply{Builder: builder}
	res, err := s.Ensure(context.Background(),
		EnsureRequest{App: "shop", Env: "production", Release: oneRelease()}, nil)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Plan.Action != PlanBuild || len(builder.calls) != 1 {
		t.Fatalf("result = %+v, want M7's one build", res)
	}
}

func TestEnsureCallsAnUnchangedTreeWhatItIs(t *testing.T) {

	builder := &fakeBuilder{rel: oneRelease(), built: false}
	s := &Supply{Builder: builder}
	res, err := s.Ensure(context.Background(),
		EnsureRequest{App: "shop", Env: "production"}, nil)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Plan.Action != PlanHave || res.Built {
		t.Fatalf("result = %+v, want nothing to have happened", res)
	}
}

func TestEnsureWithoutABuilderRefuses(t *testing.T) {
	s := &Supply{}
	if _, err := s.Ensure(context.Background(), EnsureRequest{App: "shop", Env: "production"}, nil); err == nil {
		t.Error("a machine that can neither build nor copy said yes")
	}
}

func TestEnsurePassesTheBuildersFailureOn(t *testing.T) {
	builder := &fakeBuilder{rel: oneRelease(), err: errors.New("no space left on device")}
	s := &Supply{Builder: builder}
	_, err := s.Ensure(context.Background(), EnsureRequest{App: "shop", Env: "production"}, nil)
	if err == nil || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("Ensure: %v, want the builder's own words", err)
	}
}

func TestBytesOfReadsLikeASize(t *testing.T) {
	cases := map[int64]string{
		0: "0 B", 512: "512 B", 1024: "1.0 KiB", 1536: "1.5 KiB",
		150 << 20: "150.0 MiB", 3 << 30: "3.0 GiB",
	}
	for n, want := range cases {
		if got := bytesOf(n); got != want {
			t.Errorf("bytesOf(%d) = %q, want %q", n, got, want)
		}
	}
}

type resolvingBuilder struct {
	*fakeBuilder
	tree string
}

func (b *resolvingBuilder) ResolveTree(context.Context, string, string, string) (string, string, error) {
	return "c0ffee1234567890", b.tree, nil
}

func TestEnsureDoesNotReadTheLocalIndexWithTheHubsReleaseID(t *testing.T) {
	s, builder, puller, rows := twoMachines(t)
	hubs := oneRelease()
	hubs.Built = imagesOn("m2", "arm64", "web", "worker")
	s.Builder = &resolvingBuilder{fakeBuilder: builder, tree: hubs.Tree}
	s.Lookup = func(context.Context, string, string) (*Release, error) { return hubs, nil }

	ctx := context.Background()
	for _, im := range imagesOn("m1", "arm64", "other") {
		if err := rows.PutImage(ctx, im); err != nil {
			t.Fatal(err)
		}
	}

	res, err := s.Ensure(ctx, EnsureRequest{App: "shop", Env: "production"}, nil)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Plan.Action != PlanCopy || res.Plan.From != "m2" {
		t.Fatalf("plan = %+v, want a copy from m2 rather than a build", res.Plan)
	}
	if len(builder.calls) != 0 {
		t.Errorf("built %d times, want none: m2 was holding the whole set", len(builder.calls))
	}
	if strings.Join(puller.asked, ",") != "m2" {
		t.Errorf("pulled from %v, want m2", puller.asked)
	}
}

func TestEnsureMergesTheLocalIndexForItsOwnRelease(t *testing.T) {
	s, _, _, rows := twoMachines(t)
	r := oneRelease()

	r.Built = imagesOn("m3", "arm64", "web", "worker")
	s.Sources = func(context.Context) ([]Source, error) {
		return []Source{{Machine: "m2", Reachable: true}, {Machine: "m3", Reachable: true}}, nil
	}
	ctx := context.Background()

	plan, err := s.plan(ctx, EnsureRequest{App: "shop", Env: "production", Release: r}, false)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if strings.Join(plan.Candidates, ",") != "m2,m3" {
		t.Fatalf("candidates = %v, want both halves of the index", plan.Candidates)
	}
	_ = rows
}

func TestEnsureTellsTheFleetAboutABuildAfterAFailedCopy(t *testing.T) {
	s, _, _, _ := twoMachines(t)
	s.Puller = &fakePuller{err: errors.New("dial 10.88.0.1:4022: connection refused")}
	var told []Images
	s.Report = func(_ context.Context, _ *Release, images Images) error {
		told = append(told, images)
		return nil
	}

	if _, err := s.Ensure(context.Background(),
		EnsureRequest{App: "shop", Env: "production", Release: oneRelease()}, nil); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(told) != 1 {
		t.Fatalf("the fleet was told %d time(s) about a build that happened, want once", len(told))
	}
	for _, im := range told[0] {
		if im.Machine != "m1" {
			t.Errorf("the index was told about %s on %q, want this machine", im.Service, im.Machine)
		}
	}
}

func TestEnsureSaysTheMachineSentNothingRatherThanBlamingTheLoader(t *testing.T) {
	s, _, _, _ := twoMachines(t)
	s.Puller = &emptyPuller{}

	var out bytes.Buffer
	if _, err := s.Ensure(context.Background(),
		EnsureRequest{App: "shop", Env: "production", Release: oneRelease()}, &out); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !strings.Contains(out.String(), "it sent nothing") {
		t.Errorf("the feed blames something other than the machine that answered nothing:\n%s", out.String())
	}
}

type emptyPuller struct{}

func (p *emptyPuller) PullImages(context.Context, string, TransferRequest) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}
