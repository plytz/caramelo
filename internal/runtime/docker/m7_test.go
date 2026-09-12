package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/runtime"
)

func TestRunPassesTheEnvFileBeforeEnv(t *testing.T) {
	f := (&fakeRunner{}).on("docker run", ok("abc\n"))
	_, err := testCLI(f).Run(context.Background(), runtime.ContainerSpec{
		Name:    "caramelo-shop-production-web-1",
		Image:   "caramelo/shop/web:abc123def456",
		EnvFile: "/run/caramelo/secrets/caramelo-shop-production-web-1-1234",
		Env:     map[string]string{"PORT": "20000"},
	})
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	wantArgv(t, f, "docker run --detach --name caramelo-shop-production-web-1 "+
		"--log-opt max-size=10m --log-opt max-file=5 "+
		"--env-file /run/caramelo/secrets/caramelo-shop-production-web-1-1234 "+
		"--env PORT=20000 "+
		"caramelo/shop/web:abc123def456")
}

func TestRunWithNoEnvFile(t *testing.T) {
	f := (&fakeRunner{}).on("docker run", ok("abc\n"))
	if _, err := testCLI(f).Run(context.Background(), runtime.ContainerSpec{
		Name: "caramelo-shop-feat-x-web-1", Image: "python:3.12-alpine",
	}); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	wantArgv(t, f, "docker run --detach --name caramelo-shop-feat-x-web-1 "+
		"--log-opt max-size=10m --log-opt max-file=5 python:3.12-alpine")
}

func TestRunAttachedPassesTheEnvFile(t *testing.T) {
	f := (&fakeRunner{}).on("docker run", ok(""))
	if _, err := testCLI(f).RunAttached(context.Background(), runtime.ContainerSpec{
		Name:       "caramelo-shop-production-web-run-aabb",
		Image:      "caramelo/shop/web:abc123def456",
		EnvFile:    "/run/caramelo/secrets/migrate-99",
		AutoRemove: true,
		Command:    []string{"sh", "-c", "npm run migrate"},
	}, runtime.Streams{}); err != nil {
		t.Fatalf("RunAttached() = %v", err)
	}
	argv := strings.Join(f.calls[0].Args, " ")
	if !strings.Contains(argv, "--env-file /run/caramelo/secrets/migrate-99") {
		t.Errorf("the attached argv carries no env-file: %s", argv)
	}
}

func TestShownArgvKeepsTheEnvFilePath(t *testing.T) {
	got := shownArgs([]string{"run", "--env-file", "/run/caramelo/secrets/web-1", "--env", "TOKEN=sk_live"})
	if !strings.Contains(got, "/run/caramelo/secrets/web-1") {
		t.Errorf("the shown argv lost the env-file path: %s", got)
	}
	if strings.Contains(got, "sk_live") {
		t.Errorf("the shown argv carries a value: %s", got)
	}
}

func TestRemoveImage(t *testing.T) {
	f := (&fakeRunner{}).on("docker image rm", ok("Untagged: caramelo/shop/web:a1b2c3d4e5f6\n"))
	if err := testCLI(f).RemoveImage(context.Background(), "caramelo/shop/web:a1b2c3d4e5f6"); err != nil {
		t.Fatalf("RemoveImage() = %v", err)
	}
	wantArgv(t, f, "docker image rm caramelo/shop/web:a1b2c3d4e5f6")
}

func TestRemoveImageAlreadyGone(t *testing.T) {
	f := (&fakeRunner{}).on("docker image rm",
		fail(1, "Error response from daemon: No such image: caramelo/shop/web:a1b2c3d4e5f6\n"))
	if err := testCLI(f).RemoveImage(context.Background(), "caramelo/shop/web:a1b2c3d4e5f6"); err != nil {
		t.Errorf("RemoveImage() = %v, want nil for an image that is not there", err)
	}
}

func TestRemoveImageStillInUse(t *testing.T) {
	f := (&fakeRunner{}).on("docker image rm",
		fail(1, "Error response from daemon: conflict: unable to remove repository reference "+
			`"caramelo/shop/web:a1b2c3d4e5f6" (must force) - container 7f3a is using its referenced image 9c1d`+"\n"))
	if err := testCLI(f).RemoveImage(context.Background(), "caramelo/shop/web:a1b2c3d4e5f6"); err != nil {
		t.Errorf("RemoveImage() = %v, want nil for an image in use", err)
	}
}

func TestRemoveImageOtherFailure(t *testing.T) {
	f := (&fakeRunner{}).on("docker image rm",
		fail(1, "Cannot connect to the Docker daemon at unix:///run/user/1001/docker.sock\n"))
	err := testCLI(f).RemoveImage(context.Background(), "caramelo/shop/web:a1b2c3d4e5f6")
	if err == nil {
		t.Fatal("RemoveImage() = nil for an unreachable daemon")
	}
	if !strings.Contains(err.Error(), "Cannot connect") {
		t.Errorf("RemoveImage() = %v, want docker's own words", err)
	}
}

func TestRemoveImageRefusesAFlag(t *testing.T) {
	f := &fakeRunner{}
	for _, ref := range []string{"", "--force"} {
		if err := testCLI(f).RemoveImage(context.Background(), ref); err == nil {
			t.Errorf("RemoveImage(%q) = nil", ref)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("docker was run anyway: %v", f.log())
	}
}
