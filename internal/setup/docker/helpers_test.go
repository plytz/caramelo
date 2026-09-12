package dockersetup

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/runner"
)

func TestLookupUIDAndHome(t *testing.T) {

	u, err := user.Lookup("root")
	if err != nil {
		t.Skipf("no root account to look up: %v", err)
	}
	if got, err := lookupUID("root"); err != nil || got != u.Uid {
		t.Errorf("lookupUID(root) = %q, %v; want %q", got, err, u.Uid)
	}
	if got, err := lookupHome("root"); err != nil || got != u.HomeDir {
		t.Errorf("lookupHome(root) = %q, %v; want %q", got, err, u.HomeDir)
	}
}

func TestLookupUIDAndHomeNameTheMissingUser(t *testing.T) {
	const missing = "caramelo-no-such-user"
	_, err := lookupUID(missing)
	if err == nil {
		t.Fatal("looking up a user that does not exist must fail")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("err = %v, want it to name the user", err)
	}
	if _, err := lookupHome(missing); err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("lookupHome err = %v, want it to name the user", err)
	}
}

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.json")
	if fileExists(path) {
		t.Error("fileExists said a missing file is there")
	}
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !fileExists(path) {
		t.Error("fileExists did not see the file that was just written")
	}
	if !fileExists(dir) {
		t.Error("fileExists must accept a directory too")
	}
}

func TestLogfToleratesNoLog(t *testing.T) {
	logf(nil, "this goes nowhere: %d", 1)
	var buf bytes.Buffer
	logf(&buf, "installed %s", "docker-ce")
	if got := buf.String(); got != "installed docker-ce\n" {
		t.Errorf("logf wrote %q, want a single terminated line", got)
	}
}

func TestSleepCtxReturnsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	sleepCtx(ctx, time.Minute)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("sleepCtx waited %s on a cancelled context, want it to return at once", elapsed)
	}
}

func TestSleepCtxWaitsForTheTimer(t *testing.T) {
	start := time.Now()
	sleepCtx(context.Background(), 10*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 5*time.Millisecond {
		t.Errorf("sleepCtx returned after %s, want it to wait for the timer", elapsed)
	}
}

func TestRunWrapsAFailureWithTheCommandAndItsStderr(t *testing.T) {
	f := &fakeRunner{}
	f.on("apt-get install", fail(100, "E: Unable to locate package docker-ce\nmore noise\n"))

	_, err := run(context.Background(), f, runner.Cmd{Name: "apt-get", Args: []string{"install", "-y", "docker-ce"}})
	if err == nil {
		t.Fatal("a non-zero exit must be an error")
	}
	for _, want := range []string{"apt-get install -y docker-ce", "exit 100", "Unable to locate package"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "more noise") {
		t.Errorf("err = %v, want a single line", err)
	}
}

func TestRunWrapsARunnerThatCouldNotStart(t *testing.T) {
	boom := errors.New("no such file or directory")
	f := &fakeRunner{}
	f.onErr("docker", boom)

	_, err := run(context.Background(), f, runner.Cmd{Name: "docker", Args: []string{"info"}})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the runner's own error wrapped", err)
	}
	if !strings.Contains(err.Error(), "docker") {
		t.Errorf("err = %v, want it to name the command", err)
	}
}

func TestFirstLineTakesTheFirstNonEmptyLineOfTheFirstNonEmptyInput(t *testing.T) {
	if got := firstLine("", "\n\n  real error\nrest\n"); got != "real error" {
		t.Errorf("firstLine = %q, want %q", got, "real error")
	}
	if got := firstLine("", "\n \n"); got != "" {
		t.Errorf("firstLine = %q, want empty", got)
	}
}

func TestFetchURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gpg":
			w.Write([]byte("-----BEGIN PGP PUBLIC KEY BLOCK-----\n"))
		case "/huge":
			w.Write(bytes.Repeat([]byte("x"), 2<<20))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	got, err := fetchURL(context.Background(), srv.URL+"/gpg")
	if err != nil {
		t.Fatalf("fetchURL: %v", err)
	}
	if !strings.HasPrefix(string(got), "-----BEGIN PGP PUBLIC KEY BLOCK-----") {
		t.Errorf("body = %q, want the key", got)
	}

	if _, err := fetchURL(context.Background(), srv.URL+"/nope"); err == nil {
		t.Error("a 404 must be an error")
	} else if !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want it to carry the status", err)
	}

	big, err := fetchURL(context.Background(), srv.URL+"/huge")
	if err != nil {
		t.Fatalf("fetchURL: %v", err)
	}
	if len(big) != 1<<20 {
		t.Errorf("read %d bytes, want the 1 MiB limit", len(big))
	}
}

func TestFetchURLReportsAServerItCannotReach(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL + "/gpg"
	srv.Close()

	if _, err := fetchURL(context.Background(), url); err == nil {
		t.Fatal("an unreachable server must be an error")
	} else if !strings.Contains(err.Error(), url) {
		t.Errorf("err = %v, want it to name the URL", err)
	}
}

func TestFetchURLRejectsABadURL(t *testing.T) {
	if _, err := fetchURL(context.Background(), "://not a url"); err == nil {
		t.Fatal("a malformed URL must be an error")
	}
}
