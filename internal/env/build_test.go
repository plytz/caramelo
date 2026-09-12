package env

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/release"
)

type fakeBuilder struct {
	res  *release.BuildResult
	err  error
	reqs []release.BuildRequest

	line string
}

func (b *fakeBuilder) Build(_ context.Context, req release.BuildRequest, w io.Writer) (*release.BuildResult, error) {
	b.reqs = append(b.reqs, req)
	if b.line != "" {

		_ = progress.Emit(w, progress.Event{Action: release.ActionBuild, Status: progress.StatusChanged, Detail: b.line})
	}
	return b.res, b.err
}

func (b *fakeBuilder) Prune(_ context.Context, _ string, _ int) ([]string, error) { return nil, nil }

func TestManagerBuild(t *testing.T) {
	h := newHarness(t)
	e := h.mustCreate(CreateRequest{App: "shop", Name: "production"})
	rel := &release.Release{ID: 7, App: "shop", Tree: "a1b2c3d4e5f6", Commit: strings.Repeat("c", 40),
		Images: map[string]string{"web": release.ImageRef("shop", "web", "a1b2c3d4e5f6")}}
	b := &fakeBuilder{res: &release.BuildResult{Release: rel, Built: true, Images: rel.ImageList()},
		line: "building web"}
	h.m.Builder = b

	var out bytes.Buffer
	res, err := h.m.Build(context.Background(), release.BuildRequest{App: "shop", Env: "production"}, &out)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Release.ID != 7 {
		t.Errorf("release = %+v", res.Release)
	}
	if len(b.reqs) != 1 || b.reqs[0].Env != "production" {
		t.Errorf("the builder was asked for %+v", b.reqs)
	}
	if !strings.Contains(out.String(), "building web") {
		t.Errorf("the builder's progress did not reach the caller: %q", out.String())
	}

	events, err := h.m.events(context.Background(), e.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Action == release.ActionBuild {
			found = true
			if ev.Status != progress.StatusChanged {
				t.Errorf("the build event says %q", ev.Status)
			}
			if !strings.Contains(ev.Detail, "a1b2c3d4e5f6") || !strings.Contains(ev.Detail, "1 image") {
				t.Errorf("the build event says %q", ev.Detail)
			}
		}
	}
	if !found {
		t.Errorf("no build event in %+v", events)
	}
}

func TestManagerBuildOfAnUnchangedTree(t *testing.T) {
	h := newHarness(t)
	e := h.mustCreate(CreateRequest{App: "shop", Name: "production"})
	rel := &release.Release{ID: 7, App: "shop", Tree: "a1b2c3d4e5f6"}
	h.m.Builder = &fakeBuilder{res: &release.BuildResult{Release: rel, Built: false}}
	if _, err := h.m.Build(context.Background(), release.BuildRequest{App: "shop", Env: "production"}, nil); err != nil {
		t.Fatalf("Build: %v", err)
	}
	events, err := h.m.events(context.Background(), e.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Action == release.ActionBuild && ev.Status != progress.StatusOK {
			t.Errorf("the build event says %q, want %q", ev.Status, progress.StatusOK)
		}
	}
}

func TestManagerBuildRecordsAFailure(t *testing.T) {
	h := newHarness(t)
	e := h.mustCreate(CreateRequest{App: "shop", Name: "production"})
	h.m.Builder = &fakeBuilder{err: errors.New("no space left on device")}
	if _, err := h.m.Build(context.Background(), release.BuildRequest{App: "shop", Env: "production"}, nil); err == nil {
		t.Fatal("Build reported success")
	}
	events, err := h.m.events(context.Background(), e.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Action == release.ActionBuild && ev.Status == progress.StatusFailed {
			found = true
			if !strings.Contains(ev.Detail, "no space left") {
				t.Errorf("the failure event says %q", ev.Detail)
			}
		}
	}
	if !found {
		t.Errorf("no failed build event in %+v", events)
	}
}

func TestManagerBuildNeedsAnEnvAndABuilder(t *testing.T) {
	h := newHarness(t)
	h.mustCreate(CreateRequest{App: "shop", Name: "production"})
	ctx := context.Background()
	b := &fakeBuilder{res: &release.BuildResult{Release: &release.Release{}}}
	h.m.Builder = b

	if _, err := h.m.Build(ctx, release.BuildRequest{App: "shop", Env: "nope"}, nil); err == nil ||
		!strings.Contains(err.Error(), `no such env "nope"`) {
		t.Errorf("Build of an unknown env = %v", err)
	}
	if _, err := h.m.Build(ctx, release.BuildRequest{App: "shop", Env: "NOPE"}, nil); err == nil ||
		!strings.Contains(err.Error(), "invalid env name") {
		t.Errorf("Build of an invalid name = %v", err)
	}
	if len(b.reqs) != 0 {
		t.Errorf("the builder was called anyway: %+v", b.reqs)
	}

	h.m.Builder = nil
	if _, err := h.m.Build(ctx, release.BuildRequest{App: "shop", Env: "production"}, nil); err == nil ||
		!strings.Contains(err.Error(), "cannot build releases") {
		t.Errorf("Build with no builder = %v", err)
	}
}

func TestManagerBuildWorksOnADevEnv(t *testing.T) {
	h := newHarness(t)
	e := h.mustCreate(CreateRequest{App: "shop", Name: "feat-x"})
	if e.Mode.IsRelease() {
		t.Fatalf("the fixture made a release env: %s", e.Mode)
	}
	h.m.Builder = &fakeBuilder{res: &release.BuildResult{Release: &release.Release{Tree: "abcdef123456"}}}
	if _, err := h.m.Build(context.Background(), release.BuildRequest{App: "shop", Env: "feat-x"}, nil); err != nil {
		t.Errorf("Build of a dev env: %v", err)
	}
}

func TestImageLabelKeysAgree(t *testing.T) {
	for _, pair := range []struct{ name, in, out string }{
		{"app", LabelApp, release.LabelApp},
		{"service", LabelService, release.LabelService},
		{"tree", LabelTree, release.LabelTree},
		{"version", LabelVersion, release.LabelVersion},
	} {
		if pair.in != pair.out {
			t.Errorf("the %s label is %q in env and %q in release", pair.name, pair.in, pair.out)
		}
	}

	if got := release.ShortTree(strings.Repeat("a", 40)); len(got) != release.TreeLen {
		t.Errorf("ShortTree of a full hash is %d characters", len(got))
	}
}
