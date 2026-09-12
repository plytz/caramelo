package docker

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/runtime"
)

const testUser = "caramelo"

func testCLI(f *fakeRunner) *CLI { return NewCLI(f, testUser) }

func wantArgv(t *testing.T, f *fakeRunner, want ...string) {
	t.Helper()
	got := f.log()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands\n got: %s\nwant: %s", strings.Join(got, "\n      "), strings.Join(want, "\n      "))
	}
	for _, c := range f.calls {
		if c.User != testUser {
			t.Errorf("command %q ran as %q, want %q", cmdKey(c), c.User, testUser)
		}
		if c.Name != "docker" {
			t.Errorf("command %q is not docker", cmdKey(c))
		}
	}
}

func TestPullSkipsAnImageThatIsAlreadyThere(t *testing.T) {
	f := (&fakeRunner{}).on("docker image inspect", ok("sha256:abc\n"))
	if err := testCLI(f).Pull(context.Background(), "postgres:16-alpine"); err != nil {
		t.Fatalf("Pull() = %v", err)
	}
	wantArgv(t, f, "docker image inspect --format {{.Id}} postgres:16-alpine")
}

func TestPullFetchesAMissingImage(t *testing.T) {
	f := (&fakeRunner{}).
		on("docker image inspect", fail(1, "Error response from daemon: No such image: postgres:16-alpine")).
		on("docker pull", ok("Status: Downloaded newer image\n"))
	if err := testCLI(f).Pull(context.Background(), "postgres:16-alpine"); err != nil {
		t.Fatalf("Pull() = %v", err)
	}
	wantArgv(t, f,
		"docker image inspect --format {{.Id}} postgres:16-alpine",
		"docker pull postgres:16-alpine")
}

func TestPullReportsWhatDockerSaid(t *testing.T) {
	f := (&fakeRunner{}).
		on("docker image inspect", fail(1, "No such image")).
		on("docker pull", fail(1, "Error response from daemon: pull access denied\nsecond line"))
	err := testCLI(f).Pull(context.Background(), "nope:1")
	if err == nil {
		t.Fatal("Pull() of an unfetchable image succeeded")
	}
	for _, want := range []string{"docker pull nope:1", "exit 1", "pull access denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "second line") {
		t.Errorf("err %q carries more than the first line of stderr", err)
	}
}

func TestCreateVolumeSortsItsLabels(t *testing.T) {
	f := &fakeRunner{}
	labels := map[string]string{"caramelo.env": "feat-x", "caramelo.app": "demo", "caramelo.dep": "db"}
	if err := testCLI(f).CreateVolume(context.Background(), "caramelo-demo-feat-x-db", labels); err != nil {
		t.Fatalf("CreateVolume() = %v", err)
	}
	wantArgv(t, f, "docker volume create "+
		"--label caramelo.app=demo --label caramelo.dep=db --label caramelo.env=feat-x "+
		"caramelo-demo-feat-x-db")
}

func TestRemoveVolumeToleratesAMissingVolume(t *testing.T) {
	f := (&fakeRunner{}).on("docker volume rm", fail(1, "Error response from daemon: get gone: no such volume"))
	if err := testCLI(f).RemoveVolume(context.Background(), "gone"); err != nil {
		t.Fatalf("RemoveVolume() of a missing volume = %v, want nil", err)
	}
	wantArgv(t, f, "docker volume rm gone")
}

func TestRemoveVolumeReportsRealFailures(t *testing.T) {
	f := (&fakeRunner{}).on("docker volume rm", fail(1, "Error response from daemon: volume is in use"))
	err := testCLI(f).RemoveVolume(context.Background(), "busy")
	if err == nil || !strings.Contains(err.Error(), "volume is in use") {
		t.Fatalf("RemoveVolume() = %v, want the daemon's message", err)
	}
}

func TestRunRendersTheWholeSpec(t *testing.T) {
	f := (&fakeRunner{}).on("docker run", ok("331f3eff0e50a604d15864c9c9f6c6f786333b40ce507526af503580c50e4215\n"))
	id, err := testCLI(f).Run(context.Background(), runtime.ContainerSpec{
		Name:    "caramelo-demo-feat-x-db",
		Image:   "postgres:16-alpine",
		Restart: runtime.RestartUnlessStopped,
		Labels:  map[string]string{"caramelo.env": "feat-x", "caramelo.app": "demo"},
		Env:     map[string]string{"POSTGRES_PASSWORD": "caramelo", "PGDATA": "/var/lib/postgresql/data"},
		Publish: []runtime.PortMap{{HostIP: "127.0.0.1", HostPort: 20001, ContainerPort: 5432}},
		Volumes: []runtime.VolumeMount{{Volume: "caramelo-demo-feat-x-db", Path: "/var/lib/postgresql/data"}},
	})
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if id != "331f3eff0e50a604d15864c9c9f6c6f786333b40ce507526af503580c50e4215" {
		t.Errorf("Run() returned id %q", id)
	}
	wantArgv(t, f, "docker run --detach --name caramelo-demo-feat-x-db "+
		"--restart unless-stopped "+

		"--log-opt max-size=10m --log-opt max-file=5 "+
		"--label caramelo.app=demo --label caramelo.env=feat-x "+
		"--publish 127.0.0.1:20001:5432 "+
		"--env PGDATA=/var/lib/postgresql/data --env POSTGRES_PASSWORD=caramelo "+
		"--volume caramelo-demo-feat-x-db:/var/lib/postgresql/data "+
		"postgres:16-alpine")
}

func TestRunWithoutOptionalPieces(t *testing.T) {
	f := (&fakeRunner{}).on("docker run", ok("abc\n"))
	_, err := testCLI(f).Run(context.Background(), runtime.ContainerSpec{
		Name:    "caramelo-demo-feat-x-queue",
		Image:   "nats:2",
		Publish: []runtime.PortMap{{HostPort: 20003, ContainerPort: 4222}},
		Volumes: []runtime.VolumeMount{{Volume: "v", Path: "/data", ReadOnly: true}},
	})
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	wantArgv(t, f, "docker run --detach --name caramelo-demo-feat-x-queue "+
		"--log-opt max-size=10m --log-opt max-file=5 "+
		"--publish 20003:4222 --volume v:/data:ro nats:2")
}

func TestRunSurfacesANameConflict(t *testing.T) {
	f := (&fakeRunner{}).on("docker run", fail(125, `docker: Error response from daemon: Conflict. The container name "/caramelo-demo-feat-x-db" is already in use`))
	_, err := testCLI(f).Run(context.Background(), runtime.ContainerSpec{Name: "caramelo-demo-feat-x-db", Image: "alpine"})
	if err == nil {
		t.Fatal("Run() over an existing name succeeded")
	}
	for _, want := range []string{"exit 125", "already in use"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q does not mention %q", err, want)
		}
	}
}

func TestInspectParsesARunningContainer(t *testing.T) {
	f := (&fakeRunner{}).on("docker inspect", ok(string(readTestdata(t, "inspect-running.json"))))
	st, err := testCLI(f).Inspect(context.Background(), "caramelo-demo-feat-x-db")
	if err != nil {
		t.Fatalf("Inspect() = %v", err)
	}
	wantArgv(t, f, "docker inspect --type container --format json caramelo-demo-feat-x-db")
	if st.Name != "caramelo-demo-feat-x-db" {
		t.Errorf("Name = %q, want the container name without the leading slash", st.Name)
	}
	if !st.Running() || st.Status != runtime.StatusRunning {
		t.Errorf("Status = %q, want running", st.Status)
	}
	if st.ID != "331f3eff0e50a604d15864c9c9f6c6f786333b40ce507526af503580c50e4215" {
		t.Errorf("ID = %q", st.ID)
	}
	if st.Labels["caramelo.env"] != "feat-x" || st.Labels["caramelo.dep"] != "db" {
		t.Errorf("Labels = %v", st.Labels)
	}

	want := []runtime.PortMap{
		{HostIP: "127.0.0.1", HostPort: 20001, ContainerPort: 5432, Protocol: "tcp"},
		{HostIP: "127.0.0.1", HostPort: 20005, ContainerPort: 9001, Protocol: "udp"},
	}
	if !reflect.DeepEqual(st.Ports, want) {
		t.Errorf("Ports = %+v, want %+v", st.Ports, want)
	}
}

func TestInspectOfAStoppedContainerStillKnowsItsPort(t *testing.T) {

	f := (&fakeRunner{}).on("docker inspect", ok(string(readTestdata(t, "inspect-exited.json"))))
	st, err := testCLI(f).Inspect(context.Background(), "caramelo-demo-feat-x-db")
	if err != nil {
		t.Fatalf("Inspect() = %v", err)
	}
	if st.Status != runtime.StatusExited || st.Running() {
		t.Errorf("Status = %q, want exited", st.Status)
	}

	want := []runtime.PortMap{
		{HostIP: "127.0.0.1", HostPort: 20001, ContainerPort: 5432, Protocol: "tcp"},
		{HostIP: "127.0.0.1", HostPort: 20005, ContainerPort: 9001, Protocol: "udp"},
	}
	if !reflect.DeepEqual(st.Ports, want) {
		t.Errorf("Ports = %+v, want %+v", st.Ports, want)
	}
}

func TestInspectOfAMissingContainerIsErrNotFound(t *testing.T) {
	f := (&fakeRunner{}).on("docker inspect", runner.Result{
		ExitCode: 1,
		Stdout:   "[]\n",
		Stderr:   "error: no such object: caramelo-demo-feat-x-db\n",
	})
	_, err := testCLI(f).Inspect(context.Background(), "caramelo-demo-feat-x-db")
	if !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("Inspect() of a missing container = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "caramelo-demo-feat-x-db") {
		t.Errorf("err %q does not name the container", err)
	}
}

func TestInspectDistinguishesARealFailure(t *testing.T) {
	f := (&fakeRunner{}).on("docker inspect", fail(1, "Cannot connect to the Docker daemon"))
	_, err := testCLI(f).Inspect(context.Background(), "x")
	if err == nil || errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("Inspect() = %v, want a plain failure", err)
	}
}

func TestExecReportsTheExitCodeInsteadOfFailing(t *testing.T) {
	f := (&fakeRunner{}).on("docker exec", runner.Result{Stdout: "out\n", Stderr: "err\n", ExitCode: 7})
	res, err := testCLI(f).Exec(context.Background(), "caramelo-demo-feat-x-db", []string{"pg_isready", "-U", "postgres"})
	if err != nil {
		t.Fatalf("Exec() = %v, want the result and no error", err)
	}
	if res.ExitCode != 7 || res.Stdout != "out\n" || res.Stderr != "err\n" {
		t.Errorf("Exec() = %+v", res)
	}
	wantArgv(t, f, "docker exec caramelo-demo-feat-x-db pg_isready -U postgres")
}

func TestExecRefusesAnEmptyCommand(t *testing.T) {
	f := &fakeRunner{}
	if _, err := testCLI(f).Exec(context.Background(), "c", nil); err == nil {
		t.Fatal("Exec() with no argv succeeded")
	}
	if len(f.calls) != 0 {
		t.Errorf("it ran %v", f.log())
	}
}

func TestLogsCombinesBothStreams(t *testing.T) {
	f := (&fakeRunner{}).on("docker logs", runner.Result{Stdout: "on stdout\n", Stderr: "on stderr\n"})
	out, err := testCLI(f).LogTail(context.Background(), "caramelo-demo-feat-x-db", 20)
	if err != nil {
		t.Fatalf("Logs() = %v", err)
	}
	if out != "on stdout\non stderr\n" {
		t.Errorf("Logs() = %q", out)
	}
	wantArgv(t, f, "docker logs --tail 20 caramelo-demo-feat-x-db")
}

func TestLogsWithoutATailAsksForEverything(t *testing.T) {
	f := &fakeRunner{}
	if _, err := testCLI(f).LogTail(context.Background(), "c", 0); err != nil {
		t.Fatalf("Logs() = %v", err)
	}
	wantArgv(t, f, "docker logs --tail all c")
}

func TestLogsOfAMissingContainerIsErrNotFound(t *testing.T) {
	f := (&fakeRunner{}).on("docker logs", fail(1, "Error response from daemon: No such container: gone"))
	if _, err := testCLI(f).LogTail(context.Background(), "gone", 20); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("Logs() = %v, want ErrNotFound", err)
	}
}

func TestRemoveForcesAndToleratesAMissingContainer(t *testing.T) {
	f := (&fakeRunner{}).on("docker rm", fail(1, "Error response from daemon: No such container: gone"))
	if err := testCLI(f).Remove(context.Background(), "gone", true); err != nil {
		t.Fatalf("Remove() of a missing container = %v, want nil", err)
	}
	wantArgv(t, f, "docker rm --force gone")
}

func TestRemoveWithoutForce(t *testing.T) {
	f := &fakeRunner{}
	if err := testCLI(f).Remove(context.Background(), "c", false); err != nil {
		t.Fatalf("Remove() = %v", err)
	}
	wantArgv(t, f, "docker rm c")
}

func TestRemoveReportsRealFailures(t *testing.T) {
	f := (&fakeRunner{}).on("docker rm", fail(1, "Error response from daemon: container is paused"))
	if err := testCLI(f).Remove(context.Background(), "c", false); err == nil {
		t.Fatal("Remove() swallowed a real failure")
	}
}

func TestListByLabelParsesRecordedPsOutput(t *testing.T) {
	f := (&fakeRunner{}).on("docker ps", ok(string(readTestdata(t, "ps.json"))))
	got, err := testCLI(f).ListByLabel(context.Background(), map[string]string{
		"caramelo.env": "feat-x",
		"caramelo.app": "demo",
	})
	if err != nil {
		t.Fatalf("ListByLabel() = %v", err)
	}
	wantArgv(t, f, "docker ps --all --no-trunc "+
		"--filter label=caramelo.app=demo --filter label=caramelo.env=feat-x --format json")
	if len(got) != 2 {
		t.Fatalf("ListByLabel() returned %d containers, want 2: %+v", len(got), got)
	}
	db := got[0]
	if db.Name != "caramelo-demo-feat-x-db" || !db.Running() {
		t.Errorf("first container = %+v", db)
	}
	if len(db.ID) != 64 {
		t.Errorf("ID %q is not the full id; --no-trunc is what asks for it", db.ID)
	}
	if db.Labels["caramelo.dep"] != "db" || db.Labels["caramelo.version"] != "0.1.0" {
		t.Errorf("labels = %v", db.Labels)
	}
	wantPorts := []runtime.PortMap{
		{HostIP: "127.0.0.1", HostPort: 20001, ContainerPort: 5432, Protocol: "tcp"},
		{HostIP: "127.0.0.1", HostPort: 20005, ContainerPort: 9001, Protocol: "udp"},
	}
	if !reflect.DeepEqual(db.Ports, wantPorts) {
		t.Errorf("ports = %+v, want %+v", db.Ports, wantPorts)
	}
	cache := got[1]
	if cache.Name != "caramelo-demo-feat-x-cache" || cache.Status != runtime.StatusExited {
		t.Errorf("second container = %+v", cache)
	}
	if len(cache.Ports) != 0 {
		t.Errorf("a stopped container publishes nothing, got %+v", cache.Ports)
	}
}

func TestListByLabelOfNothing(t *testing.T) {
	f := (&fakeRunner{}).on("docker ps", ok("\n"))
	got, err := testCLI(f).ListByLabel(context.Background(), map[string]string{"caramelo.env": "nope"})
	if err != nil || len(got) != 0 {
		t.Fatalf("ListByLabel() = %+v, %v; want none and no error", got, err)
	}
}

func TestListVolumesByLabelParsesRecordedOutput(t *testing.T) {
	f := (&fakeRunner{}).on("docker volume ls", ok(string(readTestdata(t, "volumes.json"))))
	got, err := testCLI(f).ListVolumesByLabel(context.Background(), map[string]string{"caramelo.env": "feat-x"})
	if err != nil {
		t.Fatalf("ListVolumesByLabel() = %v", err)
	}
	wantArgv(t, f, "docker volume ls --filter label=caramelo.env=feat-x --format json")
	want := []string{"caramelo-demo-feat-x-db", "caramelo-demo-feat-x-cache"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListVolumesByLabel() = %v, want %v", got, want)
	}
}

func TestListVolumesByLabelOfNothing(t *testing.T) {
	f := (&fakeRunner{}).on("docker volume ls", ok(""))
	got, err := testCLI(f).ListVolumesByLabel(context.Background(), map[string]string{"caramelo.env": "nope"})
	if err != nil || len(got) != 0 {
		t.Fatalf("ListVolumesByLabel() = %+v, %v", got, err)
	}
}

func TestAFailedListIsAnError(t *testing.T) {
	f := (&fakeRunner{}).on("docker ps", fail(1, "Cannot connect to the Docker daemon at unix:///run/user/1001/docker.sock"))
	if _, err := testCLI(f).ListByLabel(context.Background(), nil); err == nil {
		t.Fatal("ListByLabel() hid a dead daemon")
	}
}

func TestARunnerThatCannotRunIsWrapped(t *testing.T) {
	f := (&fakeRunner{}).onErr("docker", errors.New("exec: \"docker\": not found"))
	err := testCLI(f).CreateVolume(context.Background(), "v", nil)
	if err == nil || !strings.Contains(err.Error(), "docker volume create v") {
		t.Fatalf("CreateVolume() = %v, want the argv in the message", err)
	}
}

func TestADriverWithoutARunnerSaysSo(t *testing.T) {
	c := &CLI{User: testUser}
	if err := c.RemoveVolume(context.Background(), "v"); err == nil || !strings.Contains(err.Error(), "no command runner") {
		t.Fatalf("RemoveVolume() = %v", err)
	}
}

func TestParsePortsHandlesTheShapesDockerPrints(t *testing.T) {
	got := parsePorts("127.0.0.1:20001->5432/tcp, 0.0.0.0:80->80/tcp, [::]:80->80/tcp, 9000/tcp, " +
		"127.0.0.1:20005->9001/udp, 127.0.0.1:20006->80/udp, " +
		"127.0.0.1:20007->53/udp, 127.0.0.1:20007->53/tcp")
	want := []runtime.PortMap{

		{HostIP: "127.0.0.1", HostPort: 20007, ContainerPort: 53, Protocol: "tcp"},
		{HostIP: "127.0.0.1", HostPort: 20007, ContainerPort: 53, Protocol: "udp"},
		{HostIP: "0.0.0.0", HostPort: 80, ContainerPort: 80, Protocol: "tcp"},
		{HostIP: "::", HostPort: 80, ContainerPort: 80, Protocol: "tcp"},
		{HostIP: "127.0.0.1", HostPort: 20006, ContainerPort: 80, Protocol: "udp"},
		{HostIP: "127.0.0.1", HostPort: 20001, ContainerPort: 5432, Protocol: "tcp"},
		{HostIP: "127.0.0.1", HostPort: 20005, ContainerPort: 9001, Protocol: "udp"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsePorts() = %+v, want %+v", got, want)
	}
	if got := parsePorts(""); got != nil {
		t.Errorf("parsePorts(\"\") = %+v", got)
	}
}

func TestAPublishedPortReadsBackAsItWasWritten(t *testing.T) {
	cases := []struct {
		written runtime.PortMap
		spec    string
		read    runtime.PortMap
	}{
		{
			written: runtime.PortMap{HostIP: "127.0.0.1", HostPort: 20000, ContainerPort: 20000},
			spec:    "127.0.0.1:20000:20000",
			read:    runtime.PortMap{HostIP: "127.0.0.1", HostPort: 20000, ContainerPort: 20000, Protocol: "tcp"},
		},
		{
			written: runtime.PortMap{HostIP: "127.0.0.1", HostPort: 20001, ContainerPort: 5432, Protocol: "tcp"},
			spec:    "127.0.0.1:20001:5432",
			read:    runtime.PortMap{HostIP: "127.0.0.1", HostPort: 20001, ContainerPort: 5432, Protocol: "tcp"},
		},
		{
			written: runtime.PortMap{HostIP: "127.0.0.1", HostPort: 20005, ContainerPort: 9001, Protocol: "udp"},
			spec:    "127.0.0.1:20005:9001/udp",
			read:    runtime.PortMap{HostIP: "127.0.0.1", HostPort: 20005, ContainerPort: 9001, Protocol: "udp"},
		},
	}
	for _, c := range cases {
		if got := publishSpec(c.written); got != c.spec {
			t.Errorf("publishSpec(%+v) = %q, want %q", c.written, got, c.spec)
		}

		line := fmt.Sprintf("%s:%d->%d/%s", c.read.HostIP, c.read.HostPort, c.read.ContainerPort, c.read.Protocol)
		got := parsePorts(line)
		if len(got) != 1 || got[0] != c.read {
			t.Errorf("parsePorts(%q) = %+v, want %+v", line, got, c.read)
		}
		bindings := map[string][]inspectBinding{
			fmt.Sprintf("%d/%s", c.read.ContainerPort, c.read.Protocol): {
				{HostIP: c.read.HostIP, HostPort: strconv.Itoa(c.read.HostPort)},
			},
		}
		if got := portsFromBindings(bindings); len(got) != 1 || got[0] != c.read {
			t.Errorf("portsFromBindings(%v) = %+v, want %+v", bindings, got, c.read)
		}
	}
}

func TestContainerPortReadsTheProtocol(t *testing.T) {
	cases := map[string]struct {
		port  int
		proto string
		ok    bool
	}{
		"5432/tcp":        {5432, "tcp", true},
		"9001/udp":        {9001, "udp", true},
		"9001/UDP":        {9001, "udp", true},
		"5432":            {5432, "tcp", true},
		"5432/":           {5432, "tcp", true},
		"20000-20002/udp": {20000, "udp", true},
		"":                {0, "", false},
		"http/tcp":        {0, "", false},
	}
	for spec, want := range cases {
		port, proto, ok := containerPort(spec)
		if port != want.port || proto != want.proto || ok != want.ok {
			t.Errorf("containerPort(%q) = %d, %q, %v; want %d, %q, %v",
				spec, port, proto, ok, want.port, want.proto, want.ok)
		}
	}
}
