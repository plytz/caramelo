package release

import (
	"reflect"
	"testing"
)

func TestImagesMachines(t *testing.T) {
	l := Images{
		{Service: "web", Arch: "arm64", Machine: "m1"},
		{Service: "worker", Arch: "arm64", Machine: "m1"},
		{Service: "web", Arch: "arm64", Machine: "m2"},
		{Service: "web", Arch: "amd64", Machine: "hub"},
		{Service: "worker", Arch: "amd64", Machine: "hub"},
	}
	services := []string{"web", "worker"}

	if got := l.Machines("arm64", services); !reflect.DeepEqual(got, []string{"m1"}) {
		t.Errorf("arm64 machines = %v, want only the one with the whole set", got)
	}
	if got := l.Machines("amd64", services); !reflect.DeepEqual(got, []string{"hub"}) {
		t.Errorf("amd64 machines = %v", got)
	}
	if got := l.Machines("riscv64", services); got != nil {
		t.Errorf("an architecture nothing was built for = %v, want none", got)
	}
	if got := l.Machines("arm64", nil); got != nil {
		t.Errorf("no services asked for = %v, want none", got)
	}
	if got := l.Arches(); !reflect.DeepEqual(got, []string{"amd64", "arm64"}) {
		t.Errorf("Arches() = %v", got)
	}
	if got := len(l.ForArch("arm64")); got != 3 {
		t.Errorf("ForArch(arm64) has %d images, want 3", got)
	}
}

func TestImagesSortIsStable(t *testing.T) {
	l := Images{
		{Service: "web", Arch: "arm64", Machine: "m2"},
		{Service: "web", Arch: "amd64", Machine: "hub"},
		{Service: "web", Arch: "arm64", Machine: "m1"},
	}
	l.Sort()
	want := []string{"amd64/hub", "arm64/m1", "arm64/m2"}
	for i, im := range l {
		if got := im.Arch + "/" + im.Machine; got != want[i] {
			t.Errorf("image %d = %s, want %s", i, got, want[i])
		}
	}
}
