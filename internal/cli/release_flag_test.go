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
		{"fleet", "setup", "--target", "root@box", "--yes", "--binary", binary, "--release", "v0.0.1"},
		{"member", "add", "root@box", "--binary", binary, "--release", "v0.0.1"},
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
		{"fleet", "setup", "--target", "root@box", "--yes", "--release", "0.0.1"},
		{"member", "add", "root@box", "--release", "latest"},
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
	code, _, stderr := run(t, "fleet", "setup", "--yes", "--release", "v0.0.1")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d (stderr %q)", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "--release only makes sense with --target") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestBinaryWithoutATargetIsAUsageError(t *testing.T) {
	useScriptedTarget(t, greenReport(), okVerify)
	binary := filepath.Join(t.TempDir(), "caramelo-linux-arm64")
	if err := os.WriteFile(binary, []byte("elf"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run(t, "fleet", "setup", "--yes", "--dry-run",
		"--config-dir", t.TempDir(), "--binary", binary)
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d (stderr %q)", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "--binary only makes sense with --target") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestAnEmptyBinaryOrReleaseIsAUsageError(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"fleet", "setup", "--target", "root@box", "--yes", "--release="}, "--release was given with no value"},
		{[]string{"fleet", "setup", "--target", "root@box", "--yes", "--binary="}, "--binary was given with no value"},
		{[]string{"member", "add", "root@box", "--release="}, "--release was given with no value"},
		{[]string{"member", "add", "root@box", "--binary="}, "--binary was given with no value"},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			useScriptedTarget(t, greenReport(), okVerify)
			stubHubForMachineAdd(t, "box")
			code, _, stderr := run(t, c.args...)
			if code != ExitUsage {
				t.Fatalf("exit %d, want %d (stderr %q)", code, ExitUsage, stderr)
			}
			if !strings.Contains(stderr, c.want) {
				t.Errorf("stderr = %q, want it to say %q", stderr, c.want)
			}
		})
	}
}

func TestAnEmptyMachineAddTargetIsNotBlamedOnATargetFlag(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "caramelo-linux-arm64")
	if err := os.WriteFile(binary, []byte("elf"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"member", "add", "", "--release", "v0.0.1"},
		{"member", "add", "", "--binary", binary},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			useScriptedTarget(t, greenReport(), okVerify)
			stubHubForMachineAdd(t, "box")
			code, _, stderr := run(t, args...)
			if code != ExitUsage {
				t.Fatalf("exit %d, want %d (stderr %q)", code, ExitUsage, stderr)
			}
			if strings.Contains(stderr, "only makes sense with --target") {
				t.Errorf("member add has no --target flag, but stderr = %q", stderr)
			}
			if !strings.Contains(stderr, "empty machine address") {
				t.Errorf("stderr = %q, want the empty address refusal", stderr)
			}
		})
	}
}

func TestReleaseAndVersionReachTheBootstrapOptions(t *testing.T) {
	for _, args := range [][]string{
		{"fleet", "setup", "--target", "root@box", "--yes", "--release", "v0.0.1"},
		{"member", "add", "root@box", "--release", "v0.0.1"},
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
		case "member token --json":
			fmt.Fprintf(a.stdout, `{"token":"t","hub":"hub","expires_at":"2026-01-01T00:00:00Z"}`)
		case "member list --json":
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
}

func TestTheBinaryDefaultNamesTheRuleForThisBuild(t *testing.T) {
	release := defaultBinaryPlan("v9.9.9")
	for _, want := range []string{"caramelo-<os>-<arch>", "release v9.9.9", "downloaded for the target"} {
		if !strings.Contains(release, want) {
			t.Errorf("the plan for a release build does not say %q: %s", want, release)
		}
	}

	dev := defaultBinaryPlan("dev")
	for _, want := range []string{
		"caramelo-<os>-<arch>", "this build is not a release", "--binary <path>", "--release <tag>",
	} {
		if !strings.Contains(dev, want) {
			t.Errorf("the plan for a dev build does not say %q: %s", want, dev)
		}
	}
	if strings.Contains(dev, "downloaded") {
		t.Errorf("the plan for a dev build still promises a download: %s", dev)
	}
}

func TestTheBootstrapPlanUsesTheBinaryDefaultOfThisBuild(t *testing.T) {
	target, err := remote.ParseTargetWith("admin@box", "me", bootstrapSSHPort)
	if err != nil {
		t.Fatal(err)
	}
	useVersion(t, "v9.9.9")
	plan := bootstrapPlan(target, bootstrapFlags{})
	if !strings.Contains(plan, defaultBinaryPlan("v9.9.9")) {
		t.Errorf("the plan does not carry the binary default of this build:\n%s", plan)
	}
}
