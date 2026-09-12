package env

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidSlug(t *testing.T) {
	ok := []string{"a", "9", "shop", "feat-x", "feat-x-2", strings.Repeat("a", MaxSlugLen)}
	for _, s := range ok {
		if !ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = false, want true", s)
		}
	}
	bad := []string{"", "-x", "Feat", "feat_x", "feat x", "feat.x", "feat/x", strings.Repeat("a", MaxSlugLen+1)}
	for _, s := range bad {
		if ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = true, want false", s)
		}
	}
}

func TestValidateName(t *testing.T) {
	if err := ValidateName("env", "feat-x"); err != nil {
		t.Errorf("ValidateName: %v", err)
	}
	for _, name := range []string{"", "Feat", strings.Repeat("a", MaxSlugLen+1)} {
		err := ValidateName("env", name)
		if err == nil {
			t.Fatalf("ValidateName(%q) = nil, want an error", name)
		}
		if !strings.Contains(err.Error(), "env") {
			t.Errorf("ValidateName(%q) error %q does not say what was wrong", name, err)
		}
	}
}

func TestDerivedNames(t *testing.T) {
	if got, want := ContainerName("shop", "feat-x", "db"), "caramelo-shop-feat-x-db"; got != want {
		t.Errorf("ContainerName = %q, want %q", got, want)
	}
	if got, want := VolumeName("shop", "feat-x", "db"), ContainerName("shop", "feat-x", "db"); got != want {
		t.Errorf("VolumeName = %q, want %q", got, want)
	}
}

func TestCacheVolumeNameCollidesWithNoSlug(t *testing.T) {
	const app, environment = "sampleapp", "feat-x"
	cache := CacheVolumeName(app, environment)
	if want := "caramelo-sampleapp-feat-x--cache"; cache != want {
		t.Errorf("CacheVolumeName = %q, want %q", cache, want)
	}

	slugs := []string{
		"cache", "db", "web", "echo", "worker", "redis", "queue", "a", "9",
		"feat-x", "cache-2", "my-cache", strings.Repeat("c", MaxSlugLen),
	}
	for _, slug := range slugs {
		if !ValidSlug(slug) {
			t.Fatalf("%q is not a slug: the table is meant to hold only names a dep or a service can have", slug)
		}
		if got := VolumeName(app, environment, slug); got == cache {
			t.Errorf("the data volume of a dependency called %q is the cache volume (%q)", slug, got)
		}
		if got := ContainerName(app, environment, slug); got == cache {
			t.Errorf("the container of a dependency called %q is named after the cache volume (%q)", slug, got)
		}
		if got := ServiceContainerName(app, environment, slug); got == cache {
			t.Errorf("the container of a service called %q is named after the cache volume (%q)", slug, got)
		}
	}

	suffix := strings.TrimPrefix(cache, NetworkName(app, environment)+"-")
	if ValidSlug(suffix) {
		t.Errorf("%q is a valid slug: a dependency or a service could be given that name", suffix)
	}

	network := NetworkName(app, environment)
	if strings.ContainsAny(network, "/:") || !strings.HasPrefix(cache, network+"-") {
		t.Errorf("NetworkName = %q, want the prefix every other name of the env extends", network)
	}
	if ref := ImageRef(app, "0123abc"); !strings.Contains(ref, "/") || !strings.Contains(ref, ":") {
		t.Errorf("ImageRef = %q, want a reference no container or volume name can be", ref)
	}
}

func TestLabels(t *testing.T) {
	got := Labels("shop", "feat-x", "db", "v0.3.0")
	want := map[string]string{
		LabelApp:     "shop",
		LabelEnv:     "feat-x",
		LabelDep:     "db",
		LabelVersion: "v0.3.0",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Labels = %v, want %v", got, want)
	}

	got = Labels("shop", "feat-x", "", "")
	want = map[string]string{LabelApp: "shop", LabelEnv: "feat-x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Labels without dep = %v, want %v", got, want)
	}
}

func TestPaths(t *testing.T) {
	const data = "/mnt/caramelo"
	cases := map[string]string{
		AppDir(data, "shop"):                 "/mnt/caramelo/apps/shop",
		RepoPath(data, "shop"):               "/mnt/caramelo/apps/shop/repo.git",
		EnvDir(data, "shop", "feat-x"):       "/mnt/caramelo/apps/shop/envs/feat-x",
		WorktreePath(data, "shop", "feat-x"): "/mnt/caramelo/apps/shop/envs/feat-x/src",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	}
}

func TestParseExportFormat(t *testing.T) {
	for in, want := range map[string]ExportFormat{"": FormatShell, "shell": FormatShell, "dotenv": FormatDotenv, "json": FormatJSON} {
		got, err := ParseExportFormat(in)
		if err != nil {
			t.Fatalf("ParseExportFormat(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseExportFormat(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := ParseExportFormat("yaml"); err == nil {
		t.Error("ParseExportFormat(\"yaml\") = nil error, want one")
	}
}

func TestAppOfRepoIsRepoPathBackwards(t *testing.T) {
	for _, app := range []string{"shop", "sampleapp", "a"} {
		if got := AppOfRepo(RepoPath("/mnt/caramelo", app)); got != app {
			t.Errorf("AppOfRepo(RepoPath(%q)) = %q", app, got)
		}
	}

	if got := AppOfRepo("/mnt/caramelo/apps/shop/repo.git/"); got != "shop" {
		t.Errorf("with a trailing slash = %q, want shop", got)
	}
	if got := AppOfRepo("/srv/git/shop.git"); got != "shop" {
		t.Errorf("AppOfRepo of a plain bare repo = %q, want shop", got)
	}
}
