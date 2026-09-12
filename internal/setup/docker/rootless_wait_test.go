package dockersetup

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func testUser() dockerUser {
	return dockerUser{name: "caramelo", uid: "999", home: "/var/lib/caramelo"}
}

func TestWaitForDaemonGivesUpWhenTheSocketNeverAppears(t *testing.T) {
	f := &fakeRunner{}
	f.on("test -S", fail(1, ""))
	s := testStep()

	_, err := s.waitForDaemon(context.Background(), testEnv(f), testUser())
	if err == nil {
		t.Fatal("waitForDaemon must give up when the socket never appears")
	}
	for _, want := range []string{"did not appear", "systemctl --user"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q so the operator can look", err, want)
		}
	}
	if f.indexOf("docker info", 0) >= 0 {
		t.Errorf("docker info was run before the socket existed:\n%s", strings.Join(f.log(), "\n"))
	}
}

func TestWaitForDaemonReportsTheLastDockerErrorOnTimeout(t *testing.T) {
	f := &fakeRunner{}
	f.on("test -S", ok(""))
	f.on("docker info", fail(1, "Cannot connect to the Docker daemon at unix:///run/user/999/docker.sock\n"))
	s := testStep()

	_, err := s.waitForDaemon(context.Background(), testEnv(f), testUser())
	if err == nil {
		t.Fatal("waitForDaemon must give up on a daemon that never answers")
	}
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("err = %v, want it to say the daemon never became ready", err)
	}
	if !strings.Contains(err.Error(), "Cannot connect to the Docker daemon") {
		t.Errorf("err = %v, want the last error docker gave", err)
	}
}

func TestWaitForDaemonStopsWhenTheContextEnds(t *testing.T) {
	f := &fakeRunner{}
	f.on("test -S", ok(""))
	f.on("docker info", fail(1, "not up yet\n"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := testStep().waitForDaemon(ctx, testEnv(f), testUser())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to carry the context's own error", err)
	}
	if !strings.Contains(err.Error(), "not up yet") {
		t.Errorf("err = %v, want the last docker error alongside it", err)
	}
}

func TestWaitForBusRestartsTheSessionOnlyOnce(t *testing.T) {
	f := &fakeRunner{}
	f.on("test -S", fail(1, ""))
	s := testStep()

	err := s.waitForBus(context.Background(), testEnv(f), testUser())
	if err == nil {
		t.Fatal("waitForBus must give up when the bus never appears")
	}
	if !strings.Contains(err.Error(), "linger") {
		t.Errorf("err = %v, want the hint about linger", err)
	}
	restarts := 0
	for _, line := range f.log() {
		if strings.HasPrefix(line, "systemctl restart user@999.service") {
			restarts++
		}
	}
	if restarts != 1 {
		t.Errorf("restarted the user session %d times, want exactly 1", restarts)
	}
}

func TestWaitForBusReportsAFailedRestart(t *testing.T) {
	f := &fakeRunner{}
	f.on("test -S", fail(1, ""))
	f.on("systemctl restart user@999.service", fail(1, "Failed to restart user@999.service\n"))

	err := testStep().waitForBus(context.Background(), testEnv(f), testUser())
	if err == nil || !strings.Contains(err.Error(), "Failed to restart") {
		t.Fatalf("err = %v, want the reason systemctl gave", err)
	}
}

func TestUserNeedsBothTheUIDAndTheHome(t *testing.T) {
	boom := errors.New("no such user")
	env := testEnv(&fakeRunner{})

	s := testStep()
	s.lookupUID = func(string) (string, error) { return "", boom }
	if _, err := s.user(env); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the lookup error", err)
	}

	s = testStep()
	s.lookupHome = func(string) (string, error) { return "", boom }
	if _, err := s.user(env); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the lookup error", err)
	}

	u, err := testStep().user(env)
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if u.name != env.Config.User || u.uid != "999" || u.home != "/var/lib/caramelo" {
		t.Errorf("user = %+v, want the configured account", u)
	}
}

func TestWriteAsUserCreatesTheDirectoryFirst(t *testing.T) {
	f := &fakeRunner{}
	s := testStep()
	env := testEnv(f)

	err := s.writeAsUser(context.Background(), env, testUser(),
		"/var/lib/caramelo/.config/docker", daemonJSONPath, []byte("{}\n"))
	if err != nil {
		t.Fatalf("writeAsUser: %v", err)
	}
	requireOrder(t, f, "mkdir -p /var/lib/caramelo/.config/docker", "sh -c umask")
	for _, c := range f.calls {
		if c.User != "caramelo" {
			t.Errorf("%s ran as %q, want the caramelo user: nothing root-owned may land in their home",
				cmdKey(c), c.User)
		}
	}
}

func TestWriteAsUserReportsAFailedDirectory(t *testing.T) {
	f := &fakeRunner{}
	f.on("mkdir -p", fail(1, "mkdir: permission denied\n"))

	err := testStep().writeAsUser(context.Background(), testEnv(f), testUser(),
		"/var/lib/caramelo/.config/docker", daemonJSONPath, []byte("{}\n"))
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want the reason mkdir gave", err)
	}
	if f.indexOf("sh -c umask", 0) >= 0 {
		t.Error("the file was written even though its directory could not be created")
	}
}

func TestVersionFailureIsNotFatalToTheDetailLine(t *testing.T) {

	f := &fakeRunner{}
	f.on("docker version", fail(1, "docker: command not found\n"))
	f.on("docker info", ok(infoJSON))
	s := testStep()
	env := testEnv(f)

	if _, err := s.version(context.Background(), env, testUser()); err == nil {
		t.Fatal("a failing docker version must be an error to its caller")
	}

	info, err := s.info(context.Background(), env, testUser())
	if err != nil {
		t.Fatal(err)
	}
	detail := s.detail(context.Background(), env, testUser(), info)
	if !strings.Contains(detail, "rootless 29.8.0") {
		t.Errorf("detail = %q, want the daemon version", detail)
	}
	if strings.Contains(detail, "rootlesskit") {
		t.Errorf("detail = %q, want no rootlesskit part when docker version failed", detail)
	}
}
