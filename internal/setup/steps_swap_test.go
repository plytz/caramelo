package setup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup/testutil"
)

const (
	testSwapFile = "/var/lib/caramelo.swapfile"
	testSwapUnit = SystemUnitDir + "/var-lib-caramelo.swapfile.swap"
)

func swapStatLine(size, allocated int64) string {
	return fmt.Sprintf("root:root:600:%d:%d:512\n", size, allocated/512)
}

func bareSwapHost() *testutil.FakeRunner {
	run := testutil.New()
	run.Exit("systemd-detect-virt --container", 1)
	run.Stdout("systemd-escape -p --suffix=swap "+testSwapFile, "var-lib-caramelo.swapfile.swap\n")
	run.Stdout("swapon --show=NAME,SIZE --bytes --noheadings", "")
	run.ExitPrefix("stat -c %U:%G:%a:%s:%b:%B -- ", 1)
	run.ExitPrefix("stat -c %U:%G:%a:%F -- ", 1)
	run.ExitPrefix("cat -- ", 1)
	run.Stdout("findmnt -n -o FSTYPE,SOURCE -T /var/lib", "ext4 /dev/vda1\n")
	run.StdoutPrefix("df -B1 --output=avail ", "Avail\n64424509440\n")
	return run
}

func swappedHost(env *Env) *testutil.FakeRunner {
	return swappedHostSized(env, swapSizeBytes(env.Config))
}

func swappedHostSized(env *Env, size int64) *testutil.FakeRunner {
	run := bareSwapHost()
	run.Stdout("stat -c %U:%G:%a:%s:%b:%B -- "+testSwapFile, swapStatLine(size, size))
	run.Stdout("swapon --show=NAME,SIZE --bytes --noheadings", fmt.Sprintf("%s %d\n", testSwapFile, size))
	for _, f := range (&SwapStep{}).files(env.Config, testSwapUnit) {
		run.Stdout("stat -c %U:%G:%a:%F -- "+f.Path, "root:root:644:regular file\n")
		run.Stdout("cat -- "+f.Path, f.Content)
	}
	return run
}

func TestSwapCheckOnABareBox(t *testing.T) {
	env, _ := testEnv(t, bareSwapHost())
	done, detail, err := (&SwapStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if done {
		t.Fatal("Check() said done on a box with no swap")
	}
	for _, want := range []string{testSwapFile, SwapSysctlFile, testSwapUnit} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not mention %q", detail, want)
		}
	}
}

func TestSwapCheckIsDoneOnASwappedBox(t *testing.T) {
	env, _ := testEnv(t, nil)
	env.Run = swappedHost(env)
	done, detail, err := (&SwapStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if !done {
		t.Fatalf("Check() = false on a box that already swaps: %s", detail)
	}
	for _, want := range []string{"4.0 GiB", testSwapFile, "vm.swappiness 10"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not mention %q", detail, want)
		}
	}
}

func TestSwapApplyChangesNothingOnASecondRun(t *testing.T) {
	env, _ := testEnv(t, nil)
	run := swappedHost(env)
	env.Run = run
	step := &SwapStep{}
	if done, _, err := step.Check(context.Background(), env); err != nil || !done {
		t.Fatalf("Check() = %v, %v, want done", done, err)
	}
	run.Reset()
	if done, _, err := step.Check(context.Background(), env); err != nil || !done {
		t.Fatalf("a second Check() = %v, %v, want done", done, err)
	}
	for _, write := range []string{"fallocate", "dd ", "rm ", "mkswap", "swapoff", "tee", "install", "chmod", "chown", "systemctl", "sysctl"} {
		if run.Ran(write) {
			t.Errorf("a second run touched the machine with %s:\n%s", write, run.Transcript())
		}
	}
}

func TestSwapCheckLeavesTheSwapABoxAlreadyHas(t *testing.T) {
	env, _ := testEnv(t, nil)
	run := bareSwapHost()
	run.Stdout("swapon --show=NAME,SIZE --bytes --noheadings", "/dev/sda2 2147483648\n")
	env.Run = run
	done, detail, err := (&SwapStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if !done {
		t.Fatalf("Check() = false on a box that already has swap: %s", detail)
	}
	for _, want := range []string{"/dev/sda2", "2.0 GiB", "left alone"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not mention %q", detail, want)
		}
	}
	if run.Ran("fallocate") || run.Ran("mkswap") || run.Ran("tee") {
		t.Errorf("the step wrote to a box whose swap it should have left alone:\n%s", run.Transcript())
	}
}

func TestSwapCheckSeesASwapfileThatIsNotSwappedOn(t *testing.T) {
	env, _ := testEnv(t, nil)
	run := swappedHost(env)
	run.Stdout("swapon --show=NAME,SIZE --bytes --noheadings", "")
	env.Run = run
	done, detail, err := (&SwapStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if done {
		t.Fatalf("Check() = done on a machine whose swapfile is not swapped on: %s", detail)
	}
	if !strings.Contains(detail, testSwapFile+" is not swapped on") {
		t.Errorf("detail %q does not say the swapfile is not swapped on", detail)
	}
}

func TestSwapApplySwapsOnAFileThatIsAlreadyThere(t *testing.T) {
	env, _ := testEnv(t, nil)
	run := swappedHost(env)
	run.Stdout("swapon --show=NAME,SIZE --bytes --noheadings", "")
	env.Run = run
	if err := (&SwapStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	for _, rewrite := range []string{"fallocate", "dd ", "rm "} {
		if run.Ran(rewrite) {
			t.Errorf("Apply ran %s on a swapfile that was already the right size:\n%s", rewrite, run.Transcript())
		}
	}
	if !run.Ran("systemctl enable --now var-lib-caramelo.swapfile.swap") {
		t.Errorf("Apply left the swapfile inactive:\n%s", run.Transcript())
	}
}

func TestSwapRemakesAWrongSizedFileOnATightDisk(t *testing.T) {
	env, _ := testEnv(t, nil)
	run := bareSwapHost()
	run.Stdout("stat -c %U:%G:%a:%s:%b:%B -- "+testSwapFile, swapStatLine(8<<30, 8<<30))
	run.StdoutPrefix("df -B1 --output=avail ", "Avail\n2147483648\n")
	env.Run = run
	done, detail, err := (&SwapStep{}).Check(context.Background(), env)
	var skip Skip
	if errors.As(err, &skip) {
		t.Fatalf("Check() refused with %q although the swapfile's own blocks are the room it needs", skip.Reason)
	}
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if done {
		t.Fatalf("Check() = done on a swapfile of the wrong size: %s", detail)
	}
	if !strings.Contains(detail, "want 4.0 GiB") {
		t.Errorf("detail %q does not say what size the swapfile should be", detail)
	}
}

func TestSwapRoundsToWholeMegabytesSoApplyAndCheckAgree(t *testing.T) {
	env, _ := testEnv(t, nil)
	env.Config.Swap.SizeBytes = 3543348019
	want := swapSizeBytes(env.Config)
	if want > env.Config.Swap.SizeBytes {
		t.Fatalf("swapSizeBytes = %d, more than the %d asked for", want, env.Config.Swap.SizeBytes)
	}
	run := bareSwapHost()
	run.Handler = func(c runner.Cmd) (runner.Result, error, bool) {
		if c.Name != "stat" || len(c.Args) < 2 || c.Args[1] != "%U:%G:%a:%s:%b:%B" {
			return runner.Result{}, nil, false
		}
		switch {
		case run.Ran("dd if=/dev/zero"):
			return runner.Result{Stdout: swapStatLine(want, want)}, nil, true
		case run.Ran("fallocate"):
			return runner.Result{Stdout: swapStatLine(want, 0)}, nil, true
		}
		return runner.Result{ExitCode: 1}, nil, true
	}
	env.Run = run
	if err := (&SwapStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	count := want / swapAllocationUnit
	dd := fmt.Sprintf("dd if=/dev/zero of=%s bs=1M count=%d status=none", testSwapFile, count)
	if !strings.Contains(run.Transcript(), dd) {
		t.Fatalf("transcript has no %q:\n%s", dd, run.Transcript())
	}

	wrote := count * swapAllocationUnit
	env.Run = swappedHostSized(env, wrote)
	done, detail, err := (&SwapStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if !done {
		t.Fatalf("Check() wants a size dd cannot write: dd wrote %d bytes and Check says %q", wrote, detail)
	}
}

func TestSwapOffOnAMachineCarameloGaveSwap(t *testing.T) {
	env, _ := testEnv(t, nil)
	run := swappedHost(env)
	env.Config.Swap = serverconfig.Swap{Backend: serverconfig.SwapOff}
	env.Run = run
	step := &SwapStep{}
	done, detail, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check() = %v, want the swap caramelo made to be taken away: %v", detail, err)
	}
	if done {
		t.Fatal("Check() = done although the machine is still swapping")
	}
	for _, want := range []string{testSwapFile, testSwapUnit, SwapSysctlFile} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not name %q", detail, want)
		}
	}
	if err := step.Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	for _, want := range []string{
		"systemctl disable --now var-lib-caramelo.swapfile.swap",
		"swapoff -- " + testSwapFile,
		"rm -f -- " + testSwapUnit,
		"rm -f -- " + SwapSysctlFile,
		"rm -f -- " + testSwapFile,
		"systemctl daemon-reload",
	} {
		if !run.Ran(want) {
			t.Errorf("Apply did not run %q:\n%s", want, run.Transcript())
		}
	}
	if run.Ran("fallocate") || run.Ran("mkswap") {
		t.Errorf("--swap off made swap:\n%s", run.Transcript())
	}
}

func TestSwapOffSkipsOnAMachineWithoutCarameloSwap(t *testing.T) {
	env, _ := testEnv(t, nil)
	env.Config.Swap = serverconfig.Swap{Backend: serverconfig.SwapOff}
	run := bareSwapHost()
	env.Run = run
	done, _, err := (&SwapStep{}).Check(context.Background(), env)
	var skip Skip
	if !errors.As(err, &skip) {
		t.Fatalf("Check() = %v, %v, want a Skip", done, err)
	}
	if !strings.Contains(skip.Reason, "swap off") {
		t.Errorf("skip %q does not say the machine is set up without swap", skip.Reason)
	}
	for _, write := range []string{"rm ", "swapoff", "systemctl"} {
		if run.Ran(write) {
			t.Errorf("a skipped step touched the machine with %s:\n%s", write, run.Transcript())
		}
	}
}

func TestSwapCheckSkips(t *testing.T) {
	tests := []struct {
		name   string
		cfg    func(*serverconfig.Config)
		script func(*testutil.FakeRunner)
		want   []string
	}{
		{
			name: "swap off",
			cfg:  func(c *serverconfig.Config) { c.Swap = serverconfig.Swap{Backend: serverconfig.SwapOff} },
			want: []string{"swap off", "--swap 4G"},
		},
		{
			name: "zram",
			cfg:  func(c *serverconfig.Config) { c.Swap = serverconfig.Swap{Backend: serverconfig.SwapZram} },
			want: []string{"zram is not implemented yet"},
		},
		{
			name:   "a container",
			script: func(r *testutil.FakeRunner) { r.Exit("systemd-detect-virt --container", 0) },
			want:   []string{"container", "host kernel's swap"},
		},
		{
			name: "btrfs",
			script: func(r *testutil.FakeRunner) {
				r.Stdout("findmnt -n -o FSTYPE,SOURCE -T /var/lib", "btrfs /dev/vda2\n")
			},
			want: []string{"btrfs", "NOCOW", "--swap off"},
		},
		{
			name: "zfs",
			script: func(r *testutil.FakeRunner) {
				r.Stdout("findmnt -n -o FSTYPE,SOURCE -T /var/lib", "zfs tank/root\n")
			},
			want: []string{"ZFS", "deadlock", "--swap off"},
		},
		{
			name: "flash",
			script: func(r *testutil.FakeRunner) {
				r.Stdout("findmnt -n -o FSTYPE,SOURCE -T /var/lib", "ext4 /dev/mmcblk0p2\n")
			},
			want: []string{"/dev/mmcblk0p2", "flash", "zram", "--swap off"},
		},
		{
			name: "a disk too small",
			script: func(r *testutil.FakeRunner) {
				r.StdoutPrefix("df -B1 --output=avail ", "Avail\n6442450944\n")
			},
			want: []string{"6.0 GiB", "4.0 GiB", "5.0 GiB", "--swap 1024M"},
		},
		{
			name: "a disk with no room for any swap at all",
			script: func(r *testutil.FakeRunner) {
				r.StdoutPrefix("df -B1 --output=avail ", "Avail\n5368709120\n")
			},
			want: []string{"--swap off"},
		},
		{
			name: "systemd-escape says nothing",
			script: func(r *testutil.FakeRunner) {
				r.Exit("systemd-escape -p --suffix=swap "+testSwapFile, 1)
			},
			want: []string{"name the swap unit"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, _ := testEnv(t, nil)
			if tc.cfg != nil {
				tc.cfg(&env.Config)
			}
			run := bareSwapHost()
			if tc.script != nil {
				tc.script(run)
			}
			env.Run = run
			done, _, err := (&SwapStep{}).Check(context.Background(), env)
			var skip Skip
			if !errors.As(err, &skip) {
				t.Fatalf("Check() = %v, %v, want a Skip", done, err)
			}
			for _, want := range tc.want {
				if !strings.Contains(skip.Reason, want) {
					t.Errorf("skip %q does not mention %q", skip.Reason, want)
				}
			}
			if run.Ran("fallocate") || run.Ran("mkswap") || run.Ran("dd") {
				t.Errorf("a skipped step still touched the machine:\n%s", run.Transcript())
			}
		})
	}
}

func TestSwapCheckProceedsWhenAProbeDoesNotAnswer(t *testing.T) {
	for _, silence := range []func(*testutil.FakeRunner){
		func(r *testutil.FakeRunner) { r.Exit("findmnt -n -o FSTYPE,SOURCE -T /var/lib", 1) },
		func(r *testutil.FakeRunner) { r.ExitPrefix("df -B1 --output=avail ", 1) },
		func(r *testutil.FakeRunner) { r.Exit("swapon --show=NAME,SIZE --bytes --noheadings", 127) },
	} {
		env, _ := testEnv(t, nil)
		run := bareSwapHost()
		silence(run)
		env.Run = run
		done, detail, err := (&SwapStep{}).Check(context.Background(), env)
		if err != nil {
			t.Fatalf("a probe that did not answer became a refusal: %v", err)
		}
		if done {
			t.Errorf("Check() = done with detail %q, want the file backend to go ahead", detail)
		}
	}
}

func TestSwapApplyMakesTheSwapfile(t *testing.T) {
	env, log := testEnv(t, nil)
	run := bareSwapHost()
	size := swapSizeBytes(env.Config)
	run.Handler = func(c runner.Cmd) (runner.Result, error, bool) {
		if c.Name == "stat" && run.Ran("fallocate") {
			return runner.Result{Stdout: swapStatLine(size, size)}, nil, true
		}
		return runner.Result{}, nil, false
	}
	env.Run = run
	if err := (&SwapStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v\n%s", err, log)
	}
	wantInOrder := []string{
		"systemd-escape -p --suffix=swap " + testSwapFile,
		"fallocate -l 4294967296 -- " + testSwapFile,
		"chmod 0600 -- " + testSwapFile,
		"chown root:root -- " + testSwapFile,
		"mkswap -- " + testSwapFile,
		"install -m 0644 /dev/null " + testSwapUnit,
		"tee -- " + testSwapUnit,
		"install -m 0644 /dev/null " + SwapSysctlFile,
		"tee -- " + SwapSysctlFile,
		"systemctl daemon-reload",
		"systemctl enable --now var-lib-caramelo.swapfile.swap",
		"sysctl --quiet --load=" + SwapSysctlFile,
	}
	transcript := run.Transcript()
	rest := transcript
	for _, want := range wantInOrder {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("transcript has no %q after what came before it:\n%s", want, transcript)
		}
		rest = rest[i+len(want):]
	}
	if run.Ran("dd ") {
		t.Errorf("dd ran although fallocate wrote a whole file:\n%s", transcript)
	}
	if c, ok := run.Find("tee -- " + testSwapUnit); ok {
		for _, want := range []string{"What=" + testSwapFile, "WantedBy=swap.target"} {
			if !strings.Contains(c.Stdin, want) {
				t.Errorf("the unit does not say %q:\n%s", want, c.Stdin)
			}
		}
	}
	if c, ok := run.Find("tee -- " + SwapSysctlFile); ok {
		if !strings.Contains(c.Stdin, "vm.swappiness=10") {
			t.Errorf("the sysctl drop-in does not set vm.swappiness:\n%s", c.Stdin)
		}
	}
}

func TestSwapApplyFallsBackToDDOnASparseFile(t *testing.T) {
	env, _ := testEnv(t, nil)
	run := bareSwapHost()
	size := swapSizeBytes(env.Config)
	run.Handler = func(c runner.Cmd) (runner.Result, error, bool) {
		if c.Name != "stat" || !run.Ran("fallocate") {
			return runner.Result{}, nil, false
		}
		if run.Ran("dd ") {
			return runner.Result{Stdout: swapStatLine(size, size)}, nil, true
		}
		return runner.Result{Stdout: swapStatLine(size, 0)}, nil, true
	}
	env.Run = run
	if err := (&SwapStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	want := "dd if=/dev/zero of=" + testSwapFile + " bs=1M count=4096 status=none"
	if !run.Ran("dd if=/dev/zero") {
		t.Errorf("no dd fallback for a sparse file:\n%s", run.Transcript())
	}
	if !strings.Contains(run.Transcript(), want) {
		t.Errorf("transcript has no %q:\n%s", want, run.Transcript())
	}
	if !run.Ran("rm -f -- " + testSwapFile) {
		t.Errorf("the sparse file was not removed before dd wrote it out:\n%s", run.Transcript())
	}
}

func TestSwapApplySurvivesASysctlThatWillNotLoad(t *testing.T) {
	env, log := testEnv(t, nil)
	run := bareSwapHost()
	size := swapSizeBytes(env.Config)
	run.Exit("sysctl --quiet --load="+SwapSysctlFile, 1)
	run.Handler = func(c runner.Cmd) (runner.Result, error, bool) {
		if c.Name == "stat" && run.Ran("fallocate") {
			return runner.Result{Stdout: swapStatLine(size, size)}, nil, true
		}
		return runner.Result{}, nil, false
	}
	env.Run = run
	if err := (&SwapStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() = %v, want a warning rather than a failure", err)
	}
	if !strings.Contains(log.String(), "warning") {
		t.Errorf("log says nothing about the sysctl that would not load:\n%s", log)
	}
}

func TestSwapApplyRemakesAFileOfTheWrongSize(t *testing.T) {
	env, _ := testEnv(t, nil)
	run := bareSwapHost()
	size := swapSizeBytes(env.Config)
	run.Stdout("swapon --show=NAME,SIZE --bytes --noheadings", testSwapFile+" 1073741824\n")
	run.Handler = func(c runner.Cmd) (runner.Result, error, bool) {
		if c.Name != "stat" || len(c.Args) < 2 || c.Args[1] != "%U:%G:%a:%s:%b:%B" {
			return runner.Result{}, nil, false
		}
		if run.Ran("fallocate") {
			return runner.Result{Stdout: swapStatLine(size, size)}, nil, true
		}
		return runner.Result{Stdout: swapStatLine(1<<30, 1<<30)}, nil, true
	}
	env.Run = run
	if err := (&SwapStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	if !run.Ran("swapoff -- " + testSwapFile) {
		t.Errorf("the old swapfile was rewritten while it was still in use:\n%s", run.Transcript())
	}
}

func TestSwapFollowsACustomStateDir(t *testing.T) {
	env, _ := testEnv(t, nil)
	env.Config.StateDir = "/srv/state"
	path := env.Config.SwapFilePath()
	if path != "/srv/state.swapfile" {
		t.Fatalf("SwapFilePath = %q, want /srv/state.swapfile", path)
	}
	run := testutil.New()
	run.Exit("systemd-detect-virt --container", 1)
	run.Stdout("systemd-escape -p --suffix=swap "+path, "srv-state.swapfile.swap\n")
	run.Stdout("swapon --show=NAME,SIZE --bytes --noheadings", "")
	run.ExitPrefix("stat -c ", 1)
	run.ExitPrefix("cat -- ", 1)
	run.Exit("findmnt -n -o FSTYPE,SOURCE -T /srv", 1)
	run.StdoutPrefix("df -B1 --output=avail ", "Avail\n64424509440\n")
	env.Run = run
	done, detail, err := (&SwapStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if done {
		t.Fatal("Check() said done on a box with no swap")
	}
	for _, want := range []string{path, SystemUnitDir + "/srv-state.swapfile.swap"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not mention %q", detail, want)
		}
	}
}

func TestSwapFreeDiskIsMeasuredOnAParentThatExists(t *testing.T) {
	env, _ := testEnv(t, nil)
	run := bareSwapHost()
	run.Exit("df -B1 --output=avail /var/lib", 1)
	run.Stdout("df -B1 --output=avail /var", "Avail\n6442450944\n")
	env.Run = run
	_, _, err := (&SwapStep{}).Check(context.Background(), env)
	var skip Skip
	if !errors.As(err, &skip) {
		t.Fatalf("Check() = %v, want the skip df found one directory up", err)
	}
	if !strings.Contains(skip.Reason, "6.0 GiB") {
		t.Errorf("skip %q does not carry the free space df reported", skip.Reason)
	}
}
