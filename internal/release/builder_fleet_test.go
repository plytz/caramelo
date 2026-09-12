package release

import (
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
)

func TestBuildRecordsWhereTheImagesAreAndOnWhatArchitecture(t *testing.T) {
	h := newHarness(t)
	rows := newImages()
	h.b.Machine, h.b.Images = "m1", rows
	h.driver.arch = "arm64"
	commit := h.commit("first", goApp)
	tree := h.tree(commit)

	res, err := h.b.Build(context.Background(), BuildRequest{App: "shop", Env: h.branch}, io.Discard)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Release.Machine != "m1" {
		t.Errorf("release machine = %q, want m1", res.Release.Machine)
	}
	if strings.Join(res.Release.Arches(), ",") != "arm64" {
		t.Errorf("arches = %v, want arm64 alone", res.Release.Arches())
	}
	stored, err := rows.Images(context.Background(), res.Release.ID)
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("recorded %d rows, want one per service", len(stored))
	}
	for _, im := range stored {
		if im.Machine != "m1" || im.Arch != "arm64" || im.ID == "" {
			t.Errorf("row = %+v, want m1/arm64 with a content address", im)
		}
	}
	if stored[0].Ref != ImageRef("shop", "web", tree) {
		t.Errorf("first row is %s, want web's image", stored[0].Ref)
	}

	if len(res.Release.Built) != 2 {
		t.Errorf("the release carries %d images, want 2", len(res.Release.Built))
	}
}

func TestBuildOfAnAlreadyBuiltTreeStillRecordsWhereTheImagesAre(t *testing.T) {

	h := newHarness(t)
	h.driver.arch = "amd64"
	h.commit("first", goApp)
	if _, err := h.b.Build(context.Background(), BuildRequest{App: "shop", Env: h.branch}, io.Discard); err != nil {
		t.Fatalf("first Build: %v", err)
	}

	rows := newImages()
	h.b.Machine, h.b.Images = "m1", rows
	res, err := h.b.Build(context.Background(), BuildRequest{App: "shop", Env: h.branch}, io.Discard)
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if res.Built {
		t.Error("the second build rebuilt an unchanged tree")
	}
	stored, err := rows.Images(context.Background(), res.Release.ID)
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("recorded %d rows on the cached path, want 2", len(stored))
	}
	if got := stored.Machines("amd64", res.Release.Services()); strings.Join(got, ",") != "m1" {
		t.Errorf("Machines = %v, want m1: this is what a copy is decided on", got)
	}
}

func TestBuildOnAMachineOfOneRecordsNothingAndSaysNothing(t *testing.T) {

	h := newHarness(t)
	h.commit("first", goApp)
	res, err := h.b.Build(context.Background(), BuildRequest{App: "shop", Env: h.branch}, io.Discard)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Release.Machine != "" || len(res.Release.Built) != 0 {
		t.Errorf("release = %+v, want no machine and no index on a machine of one", res.Release)
	}
}

func TestBuildRefusesWhenTheIndexCannotBeWritten(t *testing.T) {

	h := newHarness(t)
	rows := newImages()
	rows.err = errors.New("database is locked")
	h.b.Machine, h.b.Images = "m1", rows
	h.commit("first", goApp)

	_, err := h.b.Build(context.Background(), BuildRequest{App: "shop", Env: h.branch}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("Build with a failing index: %v, want the store's own words", err)
	}
	if !strings.Contains(err.Error(), "m1") {
		t.Errorf("error %q does not name the machine", err)
	}
}

func TestABuilderTakesItsArchitectureFromTheBinaryWhenNobodySaid(t *testing.T) {
	b := &Docker{}
	if got := b.arch(); got != runtime.GOARCH {
		t.Errorf("arch = %q, want the running binary's %q", got, runtime.GOARCH)
	}
	b.Arch = " arm64 "
	if got := b.arch(); got != "arm64" {
		t.Errorf("arch = %q, want arm64", got)
	}
}
