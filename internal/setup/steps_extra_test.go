package setup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/setup/testutil"
)

var errBoom = errors.New("fork/exec: no such file or directory")

func stubSleep(t *testing.T) *int {
	t.Helper()
	n := 0
	prev := sleep
	sleep = func(ctx context.Context, d time.Duration) error {
		n++
		return ctx.Err()
	}
	t.Cleanup(func() { sleep = prev })
	return &n
}

func TestHostStepsAreInDependencyOrder(t *testing.T) {
	before, after := HostSteps()

	var names []string
	for _, s := range append(append([]Step{}, before...), after...) {
		names = append(names, s.Name())
	}
	want := []string{"preflight", "firewall", "gauge", "user", "dirs", "host-config", "vpn", "vault", "caramelod",
		"edge", "peer", "join", "summary"}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("host steps = %q, want %q", names, want)
	}

	if got := before[len(before)-1].Name(); got != "host-config" {
		t.Errorf("last step before Docker = %q, want host-config", got)
	}

	if got := after[0].Name(); got != "vpn" {
		t.Errorf("first step after Docker = %q, want vpn", got)
	}

	if got := after[3].Name(); got != "edge" {
		t.Errorf("the edge step runs %q, want it right after caramelod", got)
	}
	if got := after[4].Name(); got != "peer" {
		t.Errorf("the peer step runs %q, want it after the edge", got)
	}

	if got := after[5].Name(); got != "join" {
		t.Errorf("the join step runs %q, want it after the peer", got)
	}
}

func TestSkipIsAnErrorCarryingItsReason(t *testing.T) {
	err := error(Skip{Reason: "packages are assumed installed"})
	if !strings.Contains(err.Error(), "packages are assumed installed") {
		t.Errorf("Skip.Error() = %q, want it to carry the reason", err.Error())
	}
	var skip Skip
	if !errors.As(err, &skip) {
		t.Error("a Skip must be recognisable with errors.As, which is how Execute reads it")
	}
}

func TestReadOnlyStepsDoNotApply(t *testing.T) {
	env, _ := testEnv(t, testutil.New())
	ctx := context.Background()

	if err := NewGaugeStep().Apply(ctx, env); err != nil {
		t.Errorf("GaugeStep.Apply = %v, want nil: gauging only reports", err)
	}
	if err := NewSummaryStep().Apply(ctx, env); err != nil {
		t.Errorf("SummaryStep.Apply = %v, want nil: the summary only reports", err)
	}
	if err := NewPreflightStep().Apply(ctx, env); err == nil {
		t.Error("PreflightStep.Apply = nil, want a refusal: preflight problems are fixed by hand")
	}
	if err := NewFirewallStep().Apply(ctx, env); err == nil {
		t.Error("FirewallStep.Apply = nil, want a refusal: a firewall is read, never changed without --open-ports")
	}
	if run := env.Run.(*testutil.FakeRunner); len(run.Calls()) != 0 {
		t.Errorf("a read-only Apply ran commands:\n%s", run.Transcript())
	}
}

func TestAStepWithoutARunnerSaysSo(t *testing.T) {
	env, _ := testEnv(t, nil)
	env.Run = nil

	_, err := statPath(context.Background(), env, "/var/lib/caramelo")
	if err == nil || !strings.Contains(err.Error(), "no command runner") {
		t.Fatalf("err = %v, want it to say there is no runner", err)
	}
}

func TestRunCmdNamesTheCommandItCouldNotRun(t *testing.T) {
	run := testutil.New()
	run.Fail("stat -c %U:%G:%a:%F -- /var/lib/caramelo", errBoom)
	env, _ := testEnv(t, run)

	_, err := statPath(context.Background(), env, "/var/lib/caramelo")
	if err == nil {
		t.Fatal("a runner error must reach the caller")
	}
	if !strings.Contains(err.Error(), "stat -c") || !errors.Is(err, errBoom) {
		t.Errorf("err = %v, want the command line and the wrapped cause", err)
	}
}

func TestMustRunReportsTheExitCodeAndTheFirstLineOfTheError(t *testing.T) {
	run := testutil.New()
	run.Respond("id -u caramelo", runner.Result{ExitCode: 1, Stderr: "id: 'caramelo': no such user\nand more\n"})
	env, _ := testEnv(t, run)

	_, err := uidOf(context.Background(), env, "caramelo")
	if err == nil {
		t.Fatal("a non-zero exit must be an error")
	}
	for _, want := range []string{"exit 1", "no such user"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "and more") {
		t.Errorf("err = %v, want only the first line of the failure", err)
	}
}

func TestUidOfRejectsOutputThatIsNotANumber(t *testing.T) {
	run := testutil.New()
	run.Stdout("id -u caramelo", "not-a-uid\n")
	env, _ := testEnv(t, run)

	if _, err := uidOf(context.Background(), env, "caramelo"); err == nil {
		t.Fatal("a non-numeric uid must be an error")
	}
}

func TestCmdLineShowsTheUserACommandRunsAs(t *testing.T) {
	got := cmdLine(runner.Cmd{Name: "mkdir", Args: []string{"-p", "/x"}, User: "caramelo"})
	if want := "(as caramelo) mkdir -p /x"; got != want {
		t.Errorf("cmdLine = %q, want %q", got, want)
	}
	if got := cmdLine(runner.Cmd{Name: "true"}); got != "true" {
		t.Errorf("cmdLine = %q, want %q", got, "true")
	}
}

func TestFirstLineSkipsEmptyCandidates(t *testing.T) {
	if got := firstLine("", "  \n", "second\nthird"); got != "second" {
		t.Errorf("firstLine = %q, want %q", got, "second")
	}
	if got := firstLine("", ""); got != "" {
		t.Errorf("firstLine = %q, want empty", got)
	}
}

func TestStatPathRejectsUnexpectedStatOutput(t *testing.T) {
	run := testutil.New()
	run.Stdout("stat -c %U:%G:%a:%F -- /x", "root:root\n")
	env, _ := testEnv(t, run)

	if _, err := statPath(context.Background(), env, "/x"); err == nil {
		t.Fatal("stat output with too few fields must be an error")
	}
}

func TestDirSpecApplyStopsAtTheFirstFailure(t *testing.T) {
	run := testutil.New()
	run.Exit("chown caramelo:caramelo -- /var/lib/caramelo", 1)
	env, _ := testEnv(t, run)

	d := dirSpec{Path: "/var/lib/caramelo", Mode: "0750", Owner: "caramelo", Group: "caramelo"}
	if err := d.apply(context.Background(), env); err == nil {
		t.Fatal("a failing chown must fail the step")
	}
	if run.Ran("chmod") {
		t.Errorf("the mode was set after ownership failed:\n%s", run.Transcript())
	}
}

func TestDirSpecCheckDescribesEveryKindOfDifference(t *testing.T) {
	cases := []struct {
		name string
		stat string
		want string
	}{
		{"not a directory", "root:root:750:regular file", "is not a directory"},
		{"wrong owner", "root:root:750:directory", "owned by root:root, want caramelo:caramelo"},
		{"wrong mode", "caramelo:caramelo:700:directory", "mode 0700, want 0750"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := testutil.New()
			run.Stdout("stat -c %U:%G:%a:%F -- /d", c.stat+"\n")
			env, _ := testEnv(t, run)

			ok, detail, err := (dirSpec{Path: "/d", Mode: "0750", Owner: "caramelo", Group: "caramelo"}).
				check(context.Background(), env)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				t.Fatal("check() said the directory was fine")
			}
			if !strings.Contains(detail, c.want) {
				t.Errorf("detail = %q, want it to mention %q", detail, c.want)
			}
		})
	}
}

func TestFileSpecApplyReportsAFailedWrite(t *testing.T) {
	run := testutil.New()
	run.Respond("tee -- /etc/caramelo/config.yaml", runner.Result{ExitCode: 1, Stderr: "tee: permission denied\n"})
	env, _ := testEnv(t, run)

	f := fileSpec{Path: "/etc/caramelo/config.yaml", Content: "x\n", Mode: "0640", Owner: "root", Group: "caramelo"}
	err := f.apply(context.Background(), env)
	if err == nil {
		t.Fatal("a failing write must fail the step")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v, want the reason tee gave", err)
	}
	if run.Ran("chown") {
		t.Errorf("ownership was set after the write failed:\n%s", run.Transcript())
	}
}

func TestFileSpecApplyCreatesTheFileAtItsMode(t *testing.T) {
	run := testutil.New()
	env, _ := testEnv(t, run)

	f := fileSpec{Path: "/var/lib/caramelo/vault.key", Content: "key\n", Mode: "0600",
		Owner: "caramelo", Group: "caramelo", AsUser: "caramelo"}
	if err := f.apply(context.Background(), env); err != nil {
		t.Fatalf("apply: %v\n%s", err, run.Transcript())
	}
	create, ok := run.Find("install -m 0600 /dev/null /var/lib/caramelo/vault.key")
	if !ok {
		t.Fatalf("the file was not created at its mode:\n%s", run.Transcript())
	}
	if create.Cmd.User != "caramelo" {
		t.Errorf("created as %q, want the user it belongs to", create.Cmd.User)
	}

	transcript := run.Transcript()
	if i, j := strings.Index(transcript, "install -m 0600"), strings.Index(transcript, "tee --"); i < 0 || j < 0 || i > j {
		t.Errorf("the content was written before the file was created:\n%s", transcript)
	}
}

func TestFileSpecApplyFixesOwnershipAndMode(t *testing.T) {
	run := testutil.New()
	env, _ := testEnv(t, run)

	f := fileSpec{Path: "/x", Content: "hello\n", Mode: "0640", Owner: "root", Group: "caramelo", AsUser: "caramelo"}
	if err := f.apply(context.Background(), env); err != nil {
		t.Fatalf("apply: %v\n%s", err, run.Transcript())
	}
	call, ok := run.Find("tee -- /x")
	if !ok {
		t.Fatalf("nothing was written:\n%s", run.Transcript())
	}
	if call.Stdin != "hello\n" {
		t.Errorf("wrote %q, want the file content", call.Stdin)
	}
	if call.Cmd.User != "caramelo" {
		t.Errorf("wrote as %q, want caramelo", call.Cmd.User)
	}
	if !run.Ran("chown root:caramelo -- /x") || !run.Ran("chmod 0640 -- /x") {
		t.Errorf("owner and mode were not set:\n%s", run.Transcript())
	}
}

func TestFileSpecCheckSpotsDifferentContent(t *testing.T) {
	run := testutil.New()
	run.Stdout("stat -c %U:%G:%a:%F -- /x", "root:caramelo:640:regular file\n")
	run.Stdout("cat -- /x", "old\n")
	env, _ := testEnv(t, run)

	ok, detail, err := (fileSpec{Path: "/x", Content: "new\n", Mode: "0640", Owner: "root", Group: "caramelo"}).
		check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if ok || !strings.Contains(detail, "differs") {
		t.Errorf("check() = %v, %q; want it to report that the content differs", ok, detail)
	}
}

func TestWaitForGivesUpWithATimeout(t *testing.T) {
	pauses := stubSleep(t)
	err := waitFor(context.Background(), 10*time.Millisecond, "the socket", func() (bool, error) {
		return false, nil
	})
	if err == nil {
		t.Fatal("waitFor must give up")
	}
	if !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "the socket") {
		t.Errorf("err = %v, want it to name what it waited for", err)
	}
	if *pauses == 0 {
		t.Error("waitFor never paused between polls")
	}
}

func TestWaitForStopsOnAnError(t *testing.T) {
	stubSleep(t)
	want := errors.New("the box is gone")
	err := waitFor(context.Background(), time.Minute, "anything", func() (bool, error) { return false, want })
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want the condition's error", err)
	}
}

func TestWaitPathPollsUntilThePathAppears(t *testing.T) {
	stubSleep(t)
	run := testutil.New()
	n := 0
	run.Handler = func(c runner.Cmd) (runner.Result, error, bool) {
		if testutil.Key(c) != "test -e /run/caramelo/caramelod.sock" {
			return runner.Result{}, nil, false
		}
		n++
		if n < 3 {
			return runner.Result{ExitCode: 1}, nil, true
		}
		return runner.Result{}, nil, true
	}
	env, _ := testEnv(t, run)

	if err := waitPath(context.Background(), env, "/run/caramelo/caramelod.sock", time.Minute); err != nil {
		t.Fatalf("waitPath: %v", err)
	}
	if n != 3 {
		t.Errorf("polled %d times, want 3", n)
	}
}

func TestAvailBytesWalksUpToAFilesystemThatExists(t *testing.T) {
	run := testutil.New()
	run.ExitPrefix("df -B1 --output=avail /mnt/caramelo", 1)
	run.Stdout("df -B1 --output=avail /mnt", "Avail\n21474836480\n")
	env, _ := testEnv(t, run)

	got, err := availBytes(context.Background(), env, "/mnt/caramelo")
	if err != nil {
		t.Fatalf("availBytes: %v\n%s", err, run.Transcript())
	}
	if want := int64(20 << 30); got != want {
		t.Errorf("availBytes = %d, want %d", got, want)
	}
}

func TestAvailBytesGivesUpAtTheRoot(t *testing.T) {
	run := testutil.New()
	run.ExitPrefix("df -B1 --output=avail ", 1)
	env, _ := testEnv(t, run)

	if _, err := availBytes(context.Background(), env, "/mnt/caramelo"); err == nil {
		t.Fatal("availBytes must fail when no filesystem answers")
	}
	if !run.Ran("df -B1 --output=avail /") {
		t.Errorf("availBytes did not walk up to /:\n%s", run.Transcript())
	}
}

func TestParseDFAvailRejectsOutputThatIsNotANumber(t *testing.T) {
	for _, in := range []string{"", "Avail\nlots\n"} {
		if got, err := parseDFAvail(in); err == nil {
			t.Errorf("parseDFAvail(%q) = %d, want an error", in, got)
		}
	}
}

func TestBytesIECAtTheExtremes(t *testing.T) {
	cases := map[int64]string{
		-1:         "0 B",
		1536:       "1.5 KiB",
		3 << 40:    "3.0 TiB",
		2048 << 40: "2.0 PiB",
	}
	for n, want := range cases {
		if got := bytesIEC(n); got != want {
			t.Errorf("bytesIEC(%d) = %q, want %q", n, got, want)
		}
	}
}
