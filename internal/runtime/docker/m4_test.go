package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/runtime"
)

func TestRunRendersTheM4HalfOfTheSpec(t *testing.T) {
	f := (&fakeRunner{}).on("docker run", ok("id\n"))
	_, err := testCLI(f).Run(context.Background(), runtime.ContainerSpec{
		Name:    "caramelo-shop-feat-x-web",
		Image:   "python:3.12-alpine",
		Restart: runtime.RestartUnlessStopped,
		Labels:  map[string]string{"caramelo.app": "shop", "caramelo.service": "web"},
		Env:     map[string]string{"PORT": "20000"},
		Publish: []runtime.PortMap{{HostIP: "127.0.0.1", HostPort: 20000, ContainerPort: 20000}},
		Volumes: []runtime.VolumeMount{{Volume: "caramelo-shop-feat-x-cache", Path: "/root/.cache/pip"}},
		Binds:   []runtime.BindMount{{Host: "/mnt/caramelo/apps/shop/envs/feat-x/src", Path: "/app"}},
		Network: "caramelo-shop-feat-x",
		Aliases: []string{"web"},
		WorkDir: "/app",
		User:    "0",
		Command: []string{"sh", "-c", "set -e; python app.py"},
	})
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	wantArgv(t, f, "docker run --detach --name caramelo-shop-feat-x-web "+
		"--restart unless-stopped "+
		"--log-opt max-size=10m --log-opt max-file=5 "+
		"--label caramelo.app=shop --label caramelo.service=web "+
		"--publish 127.0.0.1:20000:20000 "+
		"--env PORT=20000 "+
		"--volume caramelo-shop-feat-x-cache:/root/.cache/pip "+
		"--volume /mnt/caramelo/apps/shop/envs/feat-x/src:/app "+
		"--network caramelo-shop-feat-x --network-alias web "+
		"--workdir /app --user 0 "+
		"python:3.12-alpine sh -c set -e; python app.py")
}

func TestRunPublishesUDPWithItsSuffix(t *testing.T) {
	f := (&fakeRunner{}).on("docker run", ok("id\n"))
	_, err := testCLI(f).Run(context.Background(), runtime.ContainerSpec{
		Name:    "caramelo-shop-feat-x-echo",
		Image:   "python:3.12-alpine",
		Publish: []runtime.PortMap{{HostIP: "127.0.0.1", HostPort: 20005, ContainerPort: 9999, Protocol: "udp"}},
	})
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	wantArgv(t, f, "docker run --detach --name caramelo-shop-feat-x-echo "+
		"--log-opt max-size=10m --log-opt max-file=5 "+
		"--publish 127.0.0.1:20005:9999/udp python:3.12-alpine")
}

func TestRunRendersTheResourceLimits(t *testing.T) {
	f := (&fakeRunner{}).on("docker run", ok("id\n"))
	_, err := testCLI(f).Run(context.Background(), runtime.ContainerSpec{
		Name:   "caramelo-shop-production-web-1",
		Image:  "caramelo/shop/web:a1b2c3d4e5f6",
		Memory: 512 << 20,
		CPU:    1.5,
	})
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	wantArgv(t, f, "docker run --detach --name caramelo-shop-production-web-1 "+
		"--log-opt max-size=10m --log-opt max-file=5 "+
		"--memory 536870912 --memory-swap 536870912 --cpus 1.5 "+
		"caramelo/shop/web:a1b2c3d4e5f6")
}

func TestRunOmitsUnwrittenResourceLimits(t *testing.T) {
	f := (&fakeRunner{}).on("docker run", ok("id\n"))
	if _, err := testCLI(f).Run(context.Background(), runtime.ContainerSpec{
		Name: "caramelo-shop-feat-x-web-1", Image: "alpine", Memory: 0, CPU: 0,
	}); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	wantArgv(t, f, "docker run --detach --name caramelo-shop-feat-x-web-1 "+
		"--log-opt max-size=10m --log-opt max-file=5 alpine")
}

func TestBuildTagsLabelsAndReturnsTheImageID(t *testing.T) {
	f := (&fakeRunner{}).
		on("docker build", ok("")).
		on("docker image inspect", ok("sha256:cafe\n"))
	id, err := testCLI(f).Build(context.Background(), runtime.BuildSpec{
		Context:    "/mnt/caramelo/apps/shop/envs/feat-x/src",
		Dockerfile: "Dockerfile.dev",
		Tag:        "caramelo/shop:abc123",
		Labels:     map[string]string{"caramelo.app": "shop", "caramelo.env": "feat-x"},
	})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if id != "sha256:cafe" {
		t.Errorf("Build() = %q, want the image id", id)
	}
	wantArgv(t, f,
		"docker build --tag caramelo/shop:abc123 --file /mnt/caramelo/apps/shop/envs/feat-x/src/Dockerfile.dev "+
			"--label caramelo.app=shop --label caramelo.env=feat-x "+
			"/mnt/caramelo/apps/shop/envs/feat-x/src",
		"docker image inspect --format {{.Id}} caramelo/shop:abc123")
}

func TestBuildStreamsItsOutputAndStillExplainsAFailure(t *testing.T) {
	f := (&fakeRunner{}).on("docker build", fail(1, "ERROR: failed to solve: process /bin/sh -c bad did not complete\n"))

	f.stream = true
	var progress bytes.Buffer
	_, err := testCLI(f).Build(context.Background(), runtime.BuildSpec{
		Context:  "/src",
		Tag:      "caramelo/shop:abc",
		NoCache:  true,
		Progress: &progress,
	})
	if err == nil {
		t.Fatal("Build() of a broken Dockerfile succeeded")
	}
	if !strings.Contains(err.Error(), "failed to solve") {
		t.Errorf("err %q does not carry what docker said", err)
	}
	if !strings.Contains(progress.String(), "failed to solve") {
		t.Errorf("progress %q did not receive the build output", progress.String())
	}
	wantArgv(t, f, "docker build --tag caramelo/shop:abc --no-cache /src")
}

func TestImageExistsAsksDockerAndDoesNotPull(t *testing.T) {
	f := (&fakeRunner{}).on("docker image inspect", fail(1, "Error response from daemon: No such image: caramelo/shop:abc"))
	ok, err := testCLI(f).ImageExists(context.Background(), "caramelo/shop:abc")
	if err != nil {
		t.Fatalf("ImageExists() = %v", err)
	}
	if ok {
		t.Error("ImageExists() = true for an image docker does not have")
	}
	wantArgv(t, f, "docker image inspect --format {{.Id}} caramelo/shop:abc")
}

func TestCreateNetworkToleratesOneThatIsAlreadyThere(t *testing.T) {
	f := (&fakeRunner{}).on("docker network create",
		fail(1, "Error response from daemon: network with name caramelo-shop-feat-x already exists"))
	err := testCLI(f).CreateNetwork(context.Background(), "caramelo-shop-feat-x",
		map[string]string{"caramelo.env": "feat-x", "caramelo.app": "shop"})
	if err != nil {
		t.Fatalf("CreateNetwork() of an existing network = %v", err)
	}
	wantArgv(t, f, "docker network create --label caramelo.app=shop --label caramelo.env=feat-x caramelo-shop-feat-x")
}

func TestCreateNetworkReportsARealFailure(t *testing.T) {
	f := (&fakeRunner{}).on("docker network create", fail(1, "Error response from daemon: could not find an available, non-overlapping IPv4 address pool"))
	if err := testCLI(f).CreateNetwork(context.Background(), "n", nil); err == nil {
		t.Fatal("CreateNetwork() of an unbuildable network succeeded")
	} else if !strings.Contains(err.Error(), "address pool") {
		t.Errorf("err %q does not carry what docker said", err)
	}
}

func TestRemoveNetworkToleratesAMissingNetwork(t *testing.T) {
	f := (&fakeRunner{}).on("docker network rm", fail(1, "Error response from daemon: network caramelo-shop-gone not found"))
	if err := testCLI(f).RemoveNetwork(context.Background(), "caramelo-shop-gone"); err != nil {
		t.Fatalf("RemoveNetwork() of a missing network = %v", err)
	}
	wantArgv(t, f, "docker network rm caramelo-shop-gone")
}

func TestRemoveNetworkReportsActiveEndpoints(t *testing.T) {
	f := (&fakeRunner{}).on("docker network rm", fail(1, `Error response from daemon: error while removing network: network caramelo-shop-feat-x has active endpoints (name:"caramelo-shop-feat-x-db")`))
	if err := testCLI(f).RemoveNetwork(context.Background(), "caramelo-shop-feat-x"); err == nil {
		t.Fatal("RemoveNetwork() of a network still in use succeeded")
	} else if !strings.Contains(err.Error(), "active endpoints") {
		t.Errorf("err %q does not say why the network stayed", err)
	}
}

func TestConnectToleratesAContainerAlreadyOnTheNetwork(t *testing.T) {
	f := (&fakeRunner{}).on("docker network connect",
		fail(1, "Error response from daemon: endpoint with name caramelo-shop-feat-x-db already exists in network caramelo-shop-feat-x"))
	err := testCLI(f).Connect(context.Background(), "caramelo-shop-feat-x", "caramelo-shop-feat-x-db", []string{"db"})
	if err != nil {
		t.Fatalf("Connect() of an attached container = %v", err)
	}
	wantArgv(t, f, "docker network connect --alias db caramelo-shop-feat-x caramelo-shop-feat-x-db")
}

func TestConnectReportsAMissingContainer(t *testing.T) {
	f := (&fakeRunner{}).on("docker network connect", fail(1, "Error response from daemon: No such container: gone"))
	if err := testCLI(f).Connect(context.Background(), "n", "gone", nil); err == nil {
		t.Fatal("Connect() of a missing container succeeded")
	}
}

func TestRunAttachedWiresTheStreamsAndPassesTheExitCodeBack(t *testing.T) {
	f := (&fakeRunner{}).on("docker run", runner.Result{ExitCode: 3})
	f.stream, f.stdout, f.stderr = true, "ran the suite\n", "1 failed\n"
	var out, errb bytes.Buffer
	in := strings.NewReader("piped\n")
	code, err := testCLI(f).RunAttached(context.Background(), runtime.ContainerSpec{
		Image:   "python:3.12-alpine",
		Network: "caramelo-shop-feat-x",
		Binds:   []runtime.BindMount{{Host: "/src", Path: "/app"}},
		WorkDir: "/app",
		User:    "0",
		Command: []string{"sh", "-c", "pytest"},
	}, runtime.Streams{Stdin: in, Stdout: &out, Stderr: &errb})
	if err != nil {
		t.Fatalf("RunAttached() = %v", err)
	}
	if code != 3 {
		t.Errorf("RunAttached() = %d, want the container's own exit code", code)
	}
	if out.String() != "ran the suite\n" || errb.String() != "1 failed\n" {
		t.Errorf("streams = %q / %q, want the container's output", out.String(), errb.String())
	}
	wantArgv(t, f, "docker run --interactive --rm "+
		"--log-opt max-size=10m --log-opt max-file=5 "+
		"--volume /src:/app --network caramelo-shop-feat-x --workdir /app --user 0 "+
		"python:3.12-alpine sh -c pytest")
	if f.calls[0].Stdin == nil {
		t.Error("the container was given no standard input")
	}
}

func TestRunAttachedNeverReturnsTheReservedExitCode(t *testing.T) {
	f := (&fakeRunner{}).on("docker run", runner.Result{ExitCode: 255})
	f.stream = true
	code, err := testCLI(f).RunAttached(context.Background(), runtime.ContainerSpec{Image: "alpine"},
		runtime.Streams{Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("RunAttached() = %v", err)
	}
	if code != 1 {
		t.Errorf("RunAttached() = %d, want 255 mapped to 1 (ADR 0003 reserves 255)", code)
	}
}

func TestRunAttachedWithoutATTYOmitsIt(t *testing.T) {
	f := (&fakeRunner{}).on("docker run", runner.Result{})
	f.stream = true
	if _, err := testCLI(f).RunAttached(context.Background(), runtime.ContainerSpec{Image: "alpine", TTY: true},
		runtime.Streams{Stdout: io.Discard}); err != nil {
		t.Fatalf("RunAttached() = %v", err)
	}
	wantArgv(t, f, "docker run --rm --tty --log-opt max-size=10m --log-opt max-file=5 alpine")
}

func TestRunAttachedRemovesTheContainerWhenItsClientIsKilled(t *testing.T) {
	f := (&fakeRunner{}).onErr("docker run", context.Canceled)
	f.stream = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	name := "caramelo-shop-feat-x-web-run-deadbeef"
	_, err := testCLI(f).RunAttached(ctx, runtime.ContainerSpec{Name: name, Image: "alpine"},
		runtime.Streams{Stdout: io.Discard, Stderr: io.Discard})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunAttached() = %v, want the cancellation", err)
	}
	if got := f.log(); len(got) != 2 || got[1] != "docker rm --force "+name {
		t.Errorf("commands = %v, want the run and then a forced removal of %s", got, name)
	}
}

func TestAFailedRunDoesNotPrintTheValuesOfTheVariables(t *testing.T) {
	f := (&fakeRunner{}).on("docker run", fail(125, "docker: port is already allocated"))
	_, err := testCLI(f).Run(context.Background(), runtime.ContainerSpec{
		Name:  "caramelo-shop-feat-x-web",
		Image: "python:3.12-alpine",
		Env:   map[string]string{"DATABASE_URL": "postgres://postgres:s3cret@db:5432/app"},
	})
	if err == nil {
		t.Fatal("a failed docker run reported no error")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("the error carries the value of a variable: %v", err)
	}
	for _, want := range []string{"DATABASE_URL=***", "port is already allocated"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestRunRefusesAnImageThatIsAFlag(t *testing.T) {
	for _, run := range []func(*CLI) error{
		func(c *CLI) error {
			_, err := c.Run(context.Background(), runtime.ContainerSpec{Name: "x", Image: "--privileged"})
			return err
		},
		func(c *CLI) error {
			_, err := c.RunAttached(context.Background(), runtime.ContainerSpec{Name: "x", Image: "--privileged"},
				runtime.Streams{})
			return err
		},
	} {
		f := &fakeRunner{}
		if err := run(testCLI(f)); err == nil {
			t.Error("an image that is a flag was accepted")
		}
		if len(f.calls) != 0 {
			t.Errorf("docker was called anyway: %v", f.log())
		}
	}
}

func TestLogsRendersEveryOption(t *testing.T) {
	f := &fakeRunner{}
	f.stream, f.stdout = true, "GET / 200\n"
	var out bytes.Buffer
	err := testCLI(f).Logs(context.Background(), "caramelo-shop-feat-x-web",
		runtime.LogOptions{Follow: true, Since: "10m", Tail: 50, Timestamps: true}, &out, io.Discard)
	if err != nil {
		t.Fatalf("Logs() = %v", err)
	}
	wantArgv(t, f, "docker logs --follow --since 10m --tail 50 --timestamps caramelo-shop-feat-x-web")
	if out.String() != "GET / 200\n" {
		t.Errorf("stdout = %q, want the container's output", out.String())
	}
}

func TestLogsWithoutATailAsksDockerForItsDefault(t *testing.T) {
	f := &fakeRunner{}
	f.stream = true
	if err := testCLI(f).Logs(context.Background(), "c", runtime.LogOptions{}, io.Discard, io.Discard); err != nil {
		t.Fatalf("Logs() = %v", err)
	}
	wantArgv(t, f, "docker logs c")
}

func TestLogsOfAMissingContainerIsErrNotFoundEvenWhenStreaming(t *testing.T) {
	f := (&fakeRunner{}).on("docker logs", runner.Result{ExitCode: 1})
	f.stream, f.stderr = true, "Error response from daemon: No such container: gone\n"
	err := testCLI(f).Logs(context.Background(), "gone", runtime.LogOptions{}, io.Discard, io.Discard)
	if !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("Logs() = %v, want ErrNotFound", err)
	}
}

func TestLogsEndsQuietlyWhenTheClientGoesAway(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := (&fakeRunner{}).onErr("docker logs", context.Canceled)
	f.stream = true
	if err := testCLI(f).Logs(ctx, "c", runtime.LogOptions{Follow: true}, io.Discard, io.Discard); err != nil {
		t.Fatalf("Logs() after a cancelled context = %v, want nil: -f ends by being cancelled", err)
	}
}

func TestInspectReadsTheRestartCount(t *testing.T) {
	for _, c := range []struct {
		name string
		doc  string
		want int
	}{
		{"top level", `[{"Id":"abc","Name":"/web","RestartCount":4,"State":{"Status":"running"}}]`, 4},
		{"inside State", `[{"Id":"abc","Name":"/web","State":{"Status":"running","RestartCount":9}}]`, 9},
		{"absent", `[{"Id":"abc","Name":"/web","State":{"Status":"running"}}]`, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := (&fakeRunner{}).on("docker inspect", ok(c.doc))
			cur, err := testCLI(f).Inspect(context.Background(), "web")
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if cur.Restarts != c.want {
				t.Errorf("restarts = %d, want %d", cur.Restarts, c.want)
			}
			if !cur.Running() {
				t.Errorf("status = %q, want running", cur.Status)
			}
		})
	}
}
