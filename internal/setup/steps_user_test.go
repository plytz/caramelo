package setup

import (
	"context"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup/testutil"
)

func bareBox() *testutil.FakeRunner {
	run := testutil.New()
	run.Strict = true
	run.Exit("getent group caramelo", 2)
	run.Exit("getent passwd caramelo", 2)
	run.Stdout("getent group systemd-journal", "systemd-journal:x:999:\n")
	run.ExitPrefix("stat -c %U:%G:%a:%F -- ", 1)
	run.Stdout("cat -- /etc/subuid", "vagrant:100000:65536\n")
	run.Stdout("cat -- /etc/subgid", "vagrant:100000:65536\n")
	run.Exit("test -e /var/lib/systemd/linger/caramelo", 1)
	return run
}

func setUpBox() *testutil.FakeRunner {
	run := testutil.New()
	run.Strict = true
	run.Stdout("getent group caramelo", "caramelo:x:989:\n")
	run.Stdout("getent passwd caramelo", "caramelo:x:999:989::/var/lib/caramelo:/bin/sh\n")
	run.Stdout("getent group systemd-journal", "systemd-journal:x:999:\n")
	run.Stdout("stat -c %U:%G:%a:%F -- /var/lib/caramelo", "caramelo:caramelo:750:directory\n")
	run.Stdout("cat -- /etc/subuid", "vagrant:100000:65536\ncaramelo:200000:65536\n")
	run.Stdout("cat -- /etc/subgid", "vagrant:100000:65536\ncaramelo:200000:65536\n")
	run.Stdout("id -nG caramelo", "caramelo systemd-journal\n")
	run.Respond("test -e /var/lib/systemd/linger/caramelo", runner.Result{})
	run.Respond("test -d /run/user/999", runner.Result{})

	run.Respond("test -e /run/user/999", runner.Result{})
	return run
}

func TestUserCheckOnABareBox(t *testing.T) {
	env, _ := testEnv(t, bareBox())
	done, detail, err := (&UserStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if done {
		t.Fatalf("Check() said done on a box without the user")
	}
	for _, want := range []string{"group caramelo", "user caramelo", "subuid/subgid", "linger"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not mention %q", detail, want)
		}
	}
}

func TestUserCheckOnAFinishedBox(t *testing.T) {
	env, _ := testEnv(t, setUpBox())
	done, detail, err := (&UserStep{}).Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %q, %v; want done", done, detail, err)
	}
	if !strings.Contains(detail, "uid 999") || !strings.Contains(detail, "200000") {
		t.Errorf("detail %q does not describe the user", detail)
	}
}

func TestUserApplyCreatesEverythingOnce(t *testing.T) {
	run := bareBox()
	run.Stdout("id -u caramelo", "999\n")
	run.Respond("test -e /run/user/999", runner.Result{})
	run.Respond("groupadd --system caramelo", runner.Result{})
	run.RespondPrefix("useradd ", runner.Result{})
	run.RespondPrefix("tee -a -- /etc/sub", runner.Result{})
	run.Respond("usermod -aG systemd-journal caramelo", runner.Result{})
	run.Respond("loginctl enable-linger caramelo", runner.Result{})

	env, _ := testEnv(t, run)
	if err := (&UserStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v\n%s", err, run.Transcript())
	}

	want := []string{
		"groupadd --system caramelo",
		"useradd --system --gid caramelo --home-dir /var/lib/caramelo --shell /bin/sh --create-home caramelo",
		"tee -a -- /etc/subuid",
		"tee -a -- /etc/subgid",
		"usermod -aG systemd-journal caramelo",
		"loginctl enable-linger caramelo",
		"test -e /run/user/999",
	}
	for _, w := range want {
		if !run.Ran(w) {
			t.Errorf("Apply did not run %q; it ran:\n%s", w, run.Transcript())
		}
	}
	call, _ := run.Find("tee -a -- /etc/subuid")
	if call.Stdin != "caramelo:200000:65536\n" {
		t.Errorf("subuid line = %q, want the first free block", call.Stdin)
	}
}

func TestUserApplyIsANoOpWhenDone(t *testing.T) {
	run := setUpBox()
	env, _ := testEnv(t, run)
	if err := (&UserStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	for _, forbidden := range []string{"groupadd", "useradd", "usermod", "loginctl", "tee"} {
		if run.Ran(forbidden) {
			t.Errorf("Apply ran %q on a finished box:\n%s", forbidden, run.Transcript())
		}
	}
}

func TestUserApplyTakesOverAnExistingHome(t *testing.T) {
	run := bareBox()
	run.Stdout("stat -c %U:%G:%a:%F -- /var/lib/caramelo", "root:root:755:directory\n")
	run.Stdout("id -u caramelo", "999\n")
	run.Strict = false

	env, _ := testEnv(t, run)
	if err := (&UserStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	if run.Ran("useradd --system --gid caramelo --home-dir /var/lib/caramelo --shell /bin/sh --create-home") {
		t.Errorf("useradd asked to create a home that already exists:\n%s", run.Transcript())
	}
	if !run.Ran("useradd --system --gid caramelo --home-dir /var/lib/caramelo --shell /bin/sh caramelo") {
		t.Errorf("user was not created:\n%s", run.Transcript())
	}
	if !run.Ran("chown caramelo:caramelo -- /var/lib/caramelo") {
		t.Errorf("the existing home was not handed over:\n%s", run.Transcript())
	}
}

func TestUserApplyRepointsAnExistingUsersHome(t *testing.T) {
	run := setUpBox()
	run.Stdout("getent passwd caramelo", "caramelo:x:999:989::/home/caramelo:/bin/sh\n")
	run.Strict = false
	env, _ := testEnv(t, run)

	if err := (&UserStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	if !run.Ran("usermod --home /var/lib/caramelo caramelo") {
		t.Errorf("home was not repointed:\n%s", run.Transcript())
	}
}

func TestPickSubIDStart(t *testing.T) {
	tests := []struct {
		name   string
		ranges []idRange
		want   int64
	}{
		{name: "empty", want: 200000},
		{name: "someone else low", ranges: []idRange{{"vagrant", 100000, 65536}}, want: 200000},
		{name: "preferred block taken", ranges: []idRange{{"other", 200000, 65536}}, want: 265536},
		{name: "two taken", ranges: []idRange{{"a", 200000, 65536}, {"b", 265536, 65536}}, want: 331072},
		{name: "overlapping tail", ranges: []idRange{{"a", 190000, 65536}}, want: 265536},
		{name: "already ours", ranges: []idRange{{"caramelo", 500000, 65536}}, want: 500000},
	}
	for _, tc := range tests {
		if got := pickSubIDStart(tc.ranges, "caramelo"); got != tc.want {
			t.Errorf("%s: pickSubIDStart() = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestParseIDRanges(t *testing.T) {
	got := parseIDRanges("# comment\nvagrant:100000:65536\nbroken\nx:notanumber:1\ncaramelo:200000:65536\n")
	if len(got) != 2 {
		t.Fatalf("parseIDRanges() = %+v, want the two valid lines", got)
	}
	if got[1] != (idRange{Name: "caramelo", Start: 200000, Count: 65536}) {
		t.Errorf("second range = %+v", got[1])
	}
}

func TestEnsureSubIDAppendsANewlineWhenTheFileLacksOne(t *testing.T) {
	run := testutil.New()
	run.Stdout("cat -- /etc/subuid", "vagrant:100000:65536")
	env, _ := testEnv(t, run)

	if err := ensureSubID(context.Background(), env, "/etc/subuid", "caramelo", 200000); err != nil {
		t.Fatalf("ensureSubID() error: %v", err)
	}
	call, found := run.Find("tee -a -- /etc/subuid")
	if !found {
		t.Fatalf("nothing was appended")
	}
	if call.Stdin != "\ncaramelo:200000:65536\n" {
		t.Errorf("appended %q, want a leading newline so the last line is not joined", call.Stdin)
	}
}

func TestUserStepAddsTheSudoUserToTheGroup(t *testing.T) {
	prev := sudoUserEnv
	sudoUserEnv = func() string { return "vagrant" }
	t.Cleanup(func() { sudoUserEnv = prev })

	run := setUpBox()
	run.Stdout("id -nG vagrant", "vagrant sudo\n")
	env, _ := testEnv(t, run)

	done, detail, err := (&UserStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if done || !strings.Contains(detail, "vagrant in group caramelo") {
		t.Fatalf("Check() = %v, %q; want not done because vagrant is not in the group", done, detail)
	}

	run.Respond("usermod -aG caramelo vagrant", runner.Result{})
	if err := (&UserStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	if !run.Ran("usermod -aG caramelo vagrant") {
		t.Errorf("Apply() did not add vagrant to the caramelo group; ran:\n%s", run.Transcript())
	}

	run.Stdout("id -nG vagrant", "vagrant sudo caramelo\n")
	if done, _, _ := (&UserStep{}).Check(context.Background(), env); !done {
		t.Errorf("Check() still not done after the user joined the group")
	}
}

func TestInvokingUserIgnoresRootAndTheDaemonUser(t *testing.T) {
	prev := sudoUserEnv
	t.Cleanup(func() { sudoUserEnv = prev })
	cfg := serverconfig.Default()
	for in, want := range map[string]string{"": "", "root": "", "caramelo": "", "alex": "alex", " alex ": "alex"} {
		sudoUserEnv = func() string { return in }
		if got := invokingUser(cfg); got != want {
			t.Errorf("invokingUser(%q) = %q, want %q", in, got, want)
		}
	}
}
