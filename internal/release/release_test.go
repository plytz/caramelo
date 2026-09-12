package release

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestImageRef(t *testing.T) {
	for _, tc := range []struct {
		app, service, tree, want string
	}{
		{"shop", "web", "a1b2c3d4e5f6", "caramelo/shop/web:a1b2c3d4e5f6"},

		{"shop", "worker", "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678",
			"caramelo/shop/worker:a1b2c3d4e5f6"},

		{"shop", "web", "abc", "caramelo/shop/web:abc"},
	} {
		if got := ImageRef(tc.app, tc.service, tc.tree); got != tc.want {
			t.Errorf("ImageRef(%q, %q, %q) = %q, want %q", tc.app, tc.service, tc.tree, got, tc.want)
		}
	}
}

func TestImageRefIsAddressedByTheTreeAlone(t *testing.T) {
	a := ImageRef("shop", "web", "a1b2c3d4e5f6")
	b := ImageRef("shop", "web", "a1b2c3d4e5f6")
	if a != b {
		t.Errorf("%q != %q", a, b)
	}
	if ImageRef("shop", "web", "a1b2c3d4e5f6") == ImageRef("shop", "web", "ffffffffffff") {
		t.Error("two trees share an image reference")
	}
}

func TestShortTree(t *testing.T) {
	if got := ShortTree("  a1b2c3d4e5f60718  "); got != "a1b2c3d4e5f6" {
		t.Errorf("= %q", got)
	}
	if got := ShortTree(""); got != "" {
		t.Errorf("= %q", got)
	}
}

func TestReleaseServicesAndImages(t *testing.T) {
	r := Release{App: "shop", Commit: "0123456789abcdef0123", Tree: "a1b2c3d4e5f6",
		Images: map[string]string{
			"worker": ImageRef("shop", "worker", "a1b2c3d4e5f6"),
			"web":    ImageRef("shop", "web", "a1b2c3d4e5f6"),
		}}
	if got := r.Services(); !reflect.DeepEqual(got, []string{"web", "worker"}) {
		t.Errorf("Services = %v, want them sorted", got)
	}
	want := []string{"caramelo/shop/web:a1b2c3d4e5f6", "caramelo/shop/worker:a1b2c3d4e5f6"}
	if got := r.ImageList(); !reflect.DeepEqual(got, want) {
		t.Errorf("ImageList = %v, want %v", got, want)
	}
	if ref, ok := r.Image("web"); !ok || ref != want[0] {
		t.Errorf("Image(web) = %q, %v", ref, ok)
	}
	if _, ok := r.Image("db"); ok {
		t.Error("Image(db) found an image for a dependency")
	}
	if got := r.Short(); got != "a1b2c3d4e5f6 (0123456789ab)" {
		t.Errorf("Short = %q", got)
	}
	if got := (Release{Tree: "a1b2c3d4e5f6"}).Short(); got != "a1b2c3d4e5f6" {
		t.Errorf("Short with no commit = %q", got)
	}

	var empty Release
	if len(empty.Services()) != 0 || len(empty.ImageList()) != 0 {
		t.Error("an empty release has services")
	}
}

func TestNotImplementedBuilder(t *testing.T) {
	var b Builder = NotImplemented{}
	_, err := b.Build(context.Background(), BuildRequest{App: "shop", Env: "production"}, nil)
	if !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Build = %v, want ErrNotImplemented", err)
	}
	if err != nil && !strings.Contains(err.Error(), "production") {
		t.Errorf("Build error %q does not name the environment", err)
	}
	if _, err := b.Prune(context.Background(), "shop", 5); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Prune = %v, want ErrNotImplemented", err)
	}
}
