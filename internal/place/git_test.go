package place

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeGit(answers map[string]string) GitFunc {
	return func(_ context.Context, _ string, args ...string) (string, error) {
		out, ok := answers[strings.Join(args, " ")]
		if !ok {
			return "", errors.New("not a git repository")
		}
		return out, nil
	}
}

func countingGit(answers map[string]string, calls *int) GitFunc {
	inner := fakeGit(answers)
	return func(ctx context.Context, dir string, args ...string) (string, error) {
		*calls++
		return inner(ctx, dir, args...)
	}
}

func TestCheckoutReadsTheAppAndTheEnvironmentOfACarameloWorktree(t *testing.T) {
	git := fakeGit(map[string]string{
		"rev-parse --git-common-dir": "/mnt/caramelo/apps/shop/repo.git",
		"rev-parse --show-toplevel":  "/mnt/caramelo/apps/shop/envs/feat-x/src",
	})
	app, environment := Checkout(context.Background(), git, ".")
	if app != "shop" || environment != "feat-x" {
		t.Errorf("Checkout() = %q, %q, want shop, feat-x", app, environment)
	}
}

func TestCheckoutOfAPlainRepositoryNamesNoEnvironment(t *testing.T) {
	dir := t.TempDir()
	top := filepath.Join(dir, "my-checkout")
	if err := os.MkdirAll(top, 0o755); err != nil {
		t.Fatal(err)
	}
	git := fakeGit(map[string]string{
		"rev-parse --git-common-dir": filepath.Join(top, ".git"),
		"rev-parse --show-toplevel":  top,
	})
	app, environment := Checkout(context.Background(), git, ".")
	if app != "my-checkout" || environment != "" {
		t.Errorf("Checkout() = %q, %q, want my-checkout and no environment", app, environment)
	}

	if err := os.WriteFile(filepath.Join(top, "caramelo.yaml"), []byte("name: shop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if app, _ := Checkout(context.Background(), git, "."); app != "shop" {
		t.Errorf("app = %q, want the name caramelo.yaml gives", app)
	}
}

func TestCheckoutOutsideARepository(t *testing.T) {
	app, environment := Checkout(context.Background(), fakeGit(nil), ".")
	if app != "" || environment != "" {
		t.Errorf("Checkout() = %q, %q, want nothing outside a repository", app, environment)
	}
}

func TestCheckoutAsksGitTwiceAtMost(t *testing.T) {
	var calls int
	git := countingGit(map[string]string{
		"rev-parse --git-common-dir": "/mnt/caramelo/apps/shop/repo.git",
		"rev-parse --show-toplevel":  "/mnt/caramelo/apps/shop/envs/feat-x/src",
	}, &calls)
	Checkout(context.Background(), git, ".")
	if calls > 2 {
		t.Errorf("Checkout asked git %d times; the app and the environment come from one pair of answers", calls)
	}
}

func TestAppFromRepoPath(t *testing.T) {
	for in, want := range map[string]string{
		"/mnt/caramelo/apps/shop/repo.git": "shop",
		"/var/data/apps/my-app/repo.git":   "my-app",
	} {
		if got := appFromRepoPath(in); got != want {
			t.Errorf("appFromRepoPath(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"", "/home/alex/code/shop/.git", "/mnt/caramelo/shop/repo.git", "/apps/Shop/repo.git"} {
		if got := appFromRepoPath(in); got != "" {
			t.Errorf("appFromRepoPath(%q) = %q, want none", in, got)
		}
	}
}

func TestEnvFromCheckoutNeedsTheEnvironmentLayout(t *testing.T) {
	for _, tc := range []struct{ top, want string }{
		{"/mnt/caramelo/apps/shop/envs/feat-x/src", "feat-x"},
		{"/mnt/caramelo/apps/shop/envs/feat-x", ""},
		{"/home/alex/code/shop", ""},
		{"/mnt/caramelo/apps/shop/other/feat-x/src", ""},
	} {
		git := fakeGit(map[string]string{"rev-parse --show-toplevel": tc.top})
		if got := EnvFromCheckout(context.Background(), git, "."); got != tc.want {
			t.Errorf("EnvFromCheckout() in %q = %q, want %q", tc.top, got, tc.want)
		}
	}
}
