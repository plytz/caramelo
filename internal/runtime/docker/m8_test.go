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

func TestImageInfoReadsTheThreeFields(t *testing.T) {
	r := (&fakeRunner{}).on("docker image inspect", ok("sha256:abc\tarm64\t152043520\n"))
	c := NewCLI(r, "caramelo")

	info, err := c.ImageInfo(context.Background(), "caramelo/shop/web:t1")
	if err != nil {
		t.Fatalf("ImageInfo: %v", err)
	}
	want := runtime.ImageInfo{Ref: "caramelo/shop/web:t1", ID: "sha256:abc", Arch: "arm64", Size: 152043520}
	if info != want {
		t.Errorf("ImageInfo = %+v, want %+v", info, want)
	}
	got := r.log()[0]
	if want := "docker image inspect --format " + imageInfoFormat + " caramelo/shop/web:t1"; got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func TestImageInfoOfAnImageThatIsNotHereIsNotFound(t *testing.T) {
	r := (&fakeRunner{}).on("docker image inspect",
		fail(1, "Error: No such image: caramelo/shop/web:t1"))
	c := NewCLI(r, "caramelo")

	_, err := c.ImageInfo(context.Background(), "caramelo/shop/web:t1")
	if !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("ImageInfo of a missing image: %v, want ErrNotFound", err)
	}

	if !strings.Contains(err.Error(), "caramelo/shop/web:t1") {
		t.Errorf("error %q does not name the image", err)
	}
}

func TestImageInfoDaemonFailureIsNotNotFound(t *testing.T) {
	r := (&fakeRunner{}).on("docker image inspect",
		fail(1, "Cannot connect to the Docker daemon at unix:///run/user/998/docker.sock"))
	c := NewCLI(r, "caramelo")

	_, err := c.ImageInfo(context.Background(), "caramelo/shop/web:t1")
	if err == nil || errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("ImageInfo with no daemon: %v, want a failure that is not ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "Cannot connect") {
		t.Errorf("error %q does not carry docker's own words", err)
	}
}

func TestNormaliseArchIsAlwaysAGOARCH(t *testing.T) {
	cases := map[string]string{
		"x86_64": "amd64", "amd64": "amd64", "AMD64": "amd64",
		"aarch64": "arm64", "arm64": "arm64", " arm64 ": "arm64",
		"armv7l": "arm", "i686": "386", "riscv64": "riscv64", "": "",
	}
	for in, want := range cases {
		if got := NormaliseArch(in); got != want {
			t.Errorf("NormaliseArch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSaveImagesWritesTheTarToTheCallersWriter(t *testing.T) {
	r := &fakeRunner{stream: true, stdout: "tar-bytes-here"}
	r.on("docker save", ok(""))
	c := NewCLI(r, "caramelo")

	var buf bytes.Buffer
	n, err := c.SaveImages(context.Background(), []string{"caramelo/shop/web:t1", "caramelo/shop/worker:t1"}, &buf)
	if err != nil {
		t.Fatalf("SaveImages: %v", err)
	}
	if buf.String() != "tar-bytes-here" {
		t.Errorf("wrote %q, want the tar", buf.String())
	}
	if n != int64(len("tar-bytes-here")) {
		t.Errorf("counted %d bytes, want %d", n, len("tar-bytes-here"))
	}

	if len(r.calls) != 1 {
		t.Fatalf("ran %d commands, want 1: %v", len(r.calls), r.log())
	}
	if want := "docker save caramelo/shop/web:t1 caramelo/shop/worker:t1"; r.log()[0] != want {
		t.Errorf("argv = %q, want %q", r.log()[0], want)
	}
}

func TestSaveImagesReportsDockersWordsAndWhatItManagedToWrite(t *testing.T) {
	r := &fakeRunner{stream: true, stderr: "Error response from daemon: No such image: nope:latest"}
	r.on("docker save", fail(1, ""))
	c := NewCLI(r, "caramelo")

	n, err := c.SaveImages(context.Background(), []string{"nope:latest"}, io.Discard)
	if err == nil {
		t.Fatal("SaveImages of a missing image: no error")
	}
	if !strings.Contains(err.Error(), "No such image") {
		t.Errorf("error %q does not carry docker's own words", err)
	}
	if n != 0 {
		t.Errorf("counted %d bytes of a save that failed, want 0", n)
	}
}

func TestSaveImagesRefusesWhatIsNotAReference(t *testing.T) {
	c := NewCLI(&fakeRunner{}, "caramelo")
	for _, refs := range [][]string{nil, {}, {""}, {"--force"}} {
		if _, err := c.SaveImages(context.Background(), refs, io.Discard); err == nil {
			t.Errorf("SaveImages(%q): no error", refs)
		}
	}
	if _, err := c.SaveImages(context.Background(), []string{"a:1"}, nil); err == nil {
		t.Error("SaveImages with no writer: no error")
	}
}

func TestLoadImagesReadsTheStreamAndReportsWhatArrived(t *testing.T) {
	r := (&fakeRunner{}).on("docker load", ok(
		"Loaded image: caramelo/shop/worker:t1\nLoaded image: caramelo/shop/web:t1\nLoaded image ID: sha256:deadbeef\n"))
	c := NewCLI(r, "caramelo")

	refs, err := c.LoadImages(context.Background(), strings.NewReader("tar-bytes-here"))
	if err != nil {
		t.Fatalf("LoadImages: %v", err)
	}

	want := []string{"caramelo/shop/web:t1", "caramelo/shop/worker:t1"}
	if strings.Join(refs, ",") != strings.Join(want, ",") {
		t.Errorf("loaded %v, want %v", refs, want)
	}
	if r.log()[0] != "docker load" {
		t.Errorf("argv = %q, want %q", r.log()[0], "docker load")
	}

	body, err := io.ReadAll(r.calls[0].Stdin)
	if err != nil {
		t.Fatalf("read the command's stdin: %v", err)
	}
	if string(body) != "tar-bytes-here" {
		t.Errorf("stdin = %q, want the tar", body)
	}
}

func TestLoadImagesRefusesNothingToRead(t *testing.T) {
	c := NewCLI(&fakeRunner{}, "caramelo")
	if _, err := c.LoadImages(context.Background(), nil); err == nil {
		t.Error("LoadImages with no reader: no error")
	}
}

func TestLoadImagesReportsAFailedLoad(t *testing.T) {
	r := (&fakeRunner{}).on("docker load", fail(1, "unexpected EOF"))
	c := NewCLI(r, "caramelo")

	if _, err := c.LoadImages(context.Background(), strings.NewReader("half")); err == nil ||
		!strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("LoadImages of a truncated tar: %v, want docker's own words", err)
	}
}

func TestTheDriverIsAnImageStreamer(t *testing.T) {
	var d runtime.Driver = NewCLI(&fakeRunner{}, "caramelo")
	s, err := runtime.Streamer(d)
	if err != nil {
		t.Fatalf("Streamer: %v", err)
	}
	if s == nil {
		t.Fatal("Streamer returned nothing")
	}
}

func TestStreamerNamesADriverThatCannotMoveImages(t *testing.T) {
	if _, err := runtime.Streamer(nil); err == nil {
		t.Error("Streamer(nil): no error")
	}
	_, err := runtime.Streamer(halfADriver{})
	if err == nil || !strings.Contains(err.Error(), "halfADriver") {
		t.Fatalf("Streamer of a driver that cannot stream: %v, want it named", err)
	}
}

type halfADriver struct{ runtime.Driver }

var _ runner.Runner = (*fakeRunner)(nil)
