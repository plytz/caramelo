package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/bootstrap"
	"github.com/plytz/caramelo/internal/remote"
)

func TestReleaseAndBinaryAreRefusedTogether(t *testing.T) {
	useScriptedTarget(t, greenReport(), okVerify)
	binary := filepath.Join(t.TempDir(), "caramelo-linux-arm64")
	if err := os.WriteFile(binary, []byte("elf"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"server", "setup", "--target", "root@box", "--yes", "--binary", binary, "--release", "v0.0.1"},
		{"machine", "add", "root@box", "--binary", binary, "--release", "v0.0.1"},
	} {
		code, _, stderr := run(t, args...)
		if code != ExitUsage {
			t.Errorf("caramelo %s: exit %d, want %d", strings.Join(args, " "), code, ExitUsage)
		}
		if !strings.Contains(stderr, "--binary and --release name two different binaries") {
			t.Errorf("caramelo %s: stderr = %q", strings.Join(args, " "), stderr)
		}
	}
}

func TestReleaseWantsATag(t *testing.T) {
	useScriptedTarget(t, greenReport(), okVerify)
	for _, args := range [][]string{
		{"server", "setup", "--target", "root@box", "--yes", "--release", "0.0.1"},
		{"machine", "add", "root@box", "--release", "latest"},
	} {
		code, _, stderr := run(t, args...)
		if code != ExitUsage {
			t.Errorf("caramelo %s: exit %d, want %d", strings.Join(args, " "), code, ExitUsage)
		}
		if !strings.Contains(stderr, "--release wants a tag of the form v<major>.<minor>.<patch>") {
			t.Errorf("caramelo %s: stderr = %q", strings.Join(args, " "), stderr)
		}
	}
}

func TestReleaseWithoutATargetIsAUsageError(t *testing.T) {
	useScriptedTarget(t, greenReport(), okVerify)
	code, _, stderr := run(t, "server", "setup", "--yes", "--release", "v0.0.1")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d (stderr %q)", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "--release only makes sense with --target") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestReleaseAndVersionReachTheBootstrapOptions(t *testing.T) {
	for _, args := range [][]string{
		{"server", "setup", "--target", "root@box", "--yes", "--release", "v0.0.1"},
		{"machine", "add", "root@box", "--release", "v0.0.1"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			useScriptedTarget(t, greenReport(), okVerify)
			stubHubForMachineAdd(t, "box")
			useVersion(t, "v9.9.9")

			var got bootstrap.Options
			prev := bootstrapRun
			bootstrapRun = func(_ context.Context, _ bootstrap.Shell, o bootstrap.Options) (bootstrap.Outcome, error) {
				got = o
				return bootstrap.Outcome{
					Probe:  bootstrap.Probe{OS: "linux", Arch: "arm64", Privilege: bootstrap.PrivilegeRoot},
					Report: greenReport(),
				}, nil
			}
			t.Cleanup(func() { bootstrapRun = prev })

			code, _, stderr := run(t, args...)
			if code != ExitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if got.Release != "v0.0.1" {
				t.Errorf("Options.Release = %q, want %q", got.Release, "v0.0.1")
			}
			if got.Version != "v9.9.9" {
				t.Errorf("Options.Version = %q, want %q", got.Version, "v9.9.9")
			}
		})
	}
}

func useVersion(t *testing.T, v string) {
	t.Helper()
	prev := version
	version = v
	t.Cleanup(func() { version = prev })
}

func stubHubForMachineAdd(t *testing.T, name string) {
	t.Helper()
	prev := forward
	forward = func(_ context.Context, a *app) (int, error) {
		switch strings.Join(a.args, " ") {
		case "machine token --json":
			fmt.Fprintf(a.stdout, `{"token":"t","hub":"hub","expires_at":"2026-01-01T00:00:00Z"}`)
		case "machine list --json":
			fmt.Fprintf(a.stdout, `[{"name":%q,"role":"member","public_key":"k"}]`, name)
		default:
			return ExitError, fmt.Errorf("the hub was asked for %q, which this test does not answer", strings.Join(a.args, " "))
		}
		return ExitOK, nil
	}
	t.Cleanup(func() { forward = prev })
}

func TestBootstrapPlanNamesTheRelease(t *testing.T) {
	target, err := remote.ParseTargetWith("admin@box", "me", bootstrapSSHPort)
	if err != nil {
		t.Fatal(err)
	}
	plan := bootstrapPlan(target, bootstrapFlags{release: "v0.0.1"})
	if !strings.Contains(plan, "release v0.0.1, downloaded for the target's platform") {
		t.Errorf("plan does not name the release:\n%s", plan)
	}
	plan = bootstrapPlan(target, bootstrapFlags{})
	if !strings.Contains(plan, "the same release downloaded for the target") {
		t.Errorf("plan does not explain the binary default:\n%s", plan)
	}
}
