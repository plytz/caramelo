package bootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestIsReleaseTag(t *testing.T) {
	cases := []struct {
		tag  string
		want bool
	}{
		{"v0.0.1", true},
		{"v1.2.3", true},
		{"v1.2.3-rc.1", true},
		{"v10.20.30-beta-2", true},
		{"0.0.1", false},
		{"v1.2", false},
		{"v0.0.1/..", false},
		{"v0.0.1 ", false},
		{"dev", false},
		{"", false},
		{"latest", false},
		{"v0.0.1-", false},
	}
	for _, c := range cases {
		if got := IsReleaseTag(c.tag); got != c.want {
			t.Errorf("IsReleaseTag(%q) = %v, want %v", c.tag, got, c.want)
		}
	}
}

func TestSupported(t *testing.T) {
	cases := []struct {
		os, arch string
		want     bool
	}{
		{"linux", "amd64", true},
		{"linux", "arm64", true},
		{"darwin", "amd64", true},
		{"darwin", "arm64", true},
		{"linux", "arm", false},
		{"windows", "amd64", false},
		{"freebsd", "amd64", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := Supported(c.os, c.arch); got != c.want {
			t.Errorf("Supported(%q, %q) = %v, want %v", c.os, c.arch, got, c.want)
		}
	}
}

type releaseServer struct {
	url  string
	sums string

	mu   sync.Mutex
	seen []string
}

func (s *releaseServer) record(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, path)
}

func (s *releaseServer) paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

func (s *releaseServer) count() int64 { return int64(len(s.paths())) }

func (s *releaseServer) forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = nil
}

func newReleaseServer(t *testing.T, tag string, files map[string][]byte) *releaseServer {
	t.Helper()
	var sums strings.Builder
	for name, body := range files {
		sum := sha256.Sum256(body)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	served := map[string][]byte{}
	for name, body := range files {
		served[name] = body
	}
	served["SHA256SUMS"] = []byte(sums.String())

	s := &releaseServer{sums: sums.String()}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.record(r.URL.Path)
		prefix := "/" + tag + "/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		body, ok := served[strings.TrimPrefix(r.URL.Path, prefix)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	s.url = ts.URL
	return s
}

func testRelease(t *testing.T, s *releaseServer, tag, goos, goarch string, log *bytes.Buffer) Release {
	t.Helper()
	base := ""
	if s != nil {
		base = s.url
	}
	r := Release{
		Tag:      tag,
		OS:       goos,
		Arch:     goarch,
		BaseURL:  base,
		CacheDir: t.TempDir(),
		Client:   &http.Client{},
	}
	if log != nil {
		r.Log = log
	}
	return r
}

func TestDownloadVerifiesAndCaches(t *testing.T) {
	const tag = "v0.0.1"
	want := []byte("a caramelo binary for linux/amd64")
	s := newReleaseServer(t, tag, map[string][]byte{"caramelo-linux-amd64": want})
	var log bytes.Buffer
	r := testRelease(t, s, tag, "linux", "amd64", &log)

	path, err := Download(context.Background(), r)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got := filepath.Join(r.CacheDir, tag, "caramelo-linux-amd64"); path != got {
		t.Errorf("path = %q, want %q", path, got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, want) {
		t.Errorf("downloaded %q, want %q", body, want)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want -rwxr-xr-x", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Join(r.CacheDir, tag))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "caramelo-linux-amd64" {
		t.Errorf("the cache holds %v, want the verified binary and nothing else", entryNames(entries))
	}
	if fi, err := os.Stat(filepath.Join(r.CacheDir, tag)); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o700 {
		t.Errorf("the cache directory is %v, want drwx------", fi.Mode().Perm())
	}
	if !strings.Contains(log.String(), "[bootstrap] downloading caramelo-linux-amd64 v0.0.1 from "+s.url+"/v0.0.1/caramelo-linux-amd64") {
		t.Errorf("log = %q, want the downloading line", log.String())
	}
}

func TestDownloadChecksumMismatchLeavesNothingBehind(t *testing.T) {
	const tag = "v0.0.1"
	lying := newLyingServer(t, tag, "caramelo-linux-arm64", []byte("the real binary"), []byte("a truncated body"))
	r := testRelease(t, lying, tag, "linux", "arm64", nil)

	if _, err := Download(context.Background(), r); err == nil {
		t.Fatal("Download succeeded, want a checksum mismatch error")
	} else if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("err = %v, want a checksum mismatch", err)
	}

	entries, err := os.ReadDir(filepath.Join(r.CacheDir, tag))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the cache directory holds %v after a failed download, want nothing", entryNames(entries))
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func newLyingServer(t *testing.T, tag, name string, honest, served []byte) *releaseServer {
	t.Helper()
	sum := sha256.Sum256(honest)
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name)
	s := &releaseServer{sums: sums}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r.URL.Path)
		switch r.URL.Path {
		case "/" + tag + "/SHA256SUMS":
			w.Write([]byte(sums))
		case "/" + tag + "/" + name:
			w.Write(served)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	s.url = ts.URL
	return s
}

func TestDownloadMissingReleaseSaysWhere(t *testing.T) {
	const tag = "v9.9.9"
	s := newReleaseServer(t, "v0.0.1", map[string][]byte{"caramelo-linux-amd64": []byte("x")})
	r := testRelease(t, s, tag, "linux", "amd64", nil)

	_, err := Download(context.Background(), r)
	if err == nil {
		t.Fatal("Download succeeded, want a 404 error")
	}
	for _, want := range []string{s.url + "/v9.9.9/SHA256SUMS", "404", "https://github.com/plytz/caramelo/releases"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %q", err, want)
		}
	}
}

func TestDownloadMissingPlatformInSumsIsAnError(t *testing.T) {
	const tag = "v0.0.1"
	s := newReleaseServer(t, tag, map[string][]byte{"caramelo-linux-amd64": []byte("x")})
	r := testRelease(t, s, tag, "darwin", "arm64", nil)

	_, err := Download(context.Background(), r)
	if err == nil {
		t.Fatal("Download succeeded, want an error about the missing file")
	}
	if !strings.Contains(err.Error(), "caramelo-darwin-arm64") {
		t.Errorf("err = %v, want it to name caramelo-darwin-arm64", err)
	}
}

func TestDownloadCacheHitVerifiesAgainstAFreshChecksum(t *testing.T) {
	const tag = "v1.2.3"
	body := []byte("a caramelo binary for darwin/arm64")
	s := newReleaseServer(t, tag, map[string][]byte{"caramelo-darwin-arm64": body})
	var log bytes.Buffer
	r := testRelease(t, s, tag, "darwin", "arm64", &log)

	first, err := Download(context.Background(), r)
	if err != nil {
		t.Fatalf("first Download: %v", err)
	}
	if s.count() == 0 {
		t.Fatal("the first download made no request")
	}
	s.forget()
	log.Reset()

	second, err := Download(context.Background(), r)
	if err != nil {
		t.Fatalf("second Download: %v", err)
	}
	if second != first {
		t.Errorf("second path = %q, want %q", second, first)
	}
	paths := s.paths()
	if len(paths) != 1 {
		t.Fatalf("the cache hit asked for %v, want one request", paths)
	}
	if paths[0] != "/"+tag+"/"+releaseSumsName {
		t.Errorf("the cache hit asked for %q, want the checksums", paths[0])
	}
	if !strings.Contains(log.String(), "[bootstrap] using the cached caramelo-darwin-arm64 v1.2.3") {
		t.Errorf("log = %q, want the cached line", log.String())
	}
}

func TestDownloadRefusesAPlantedCacheEntry(t *testing.T) {
	const tag = "v0.0.1"
	body := []byte("a caramelo binary for linux/amd64")
	s := newReleaseServer(t, tag, map[string][]byte{"caramelo-linux-amd64": body})
	r := testRelease(t, s, tag, "linux", "amd64", nil)

	path, err := Download(context.Background(), r)
	if err != nil {
		t.Fatalf("first Download: %v", err)
	}
	planted := []byte("a binary somebody else left in the cache")
	if err := os.WriteFile(path, planted, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(planted)
	lying := fmt.Sprintf("%s  caramelo-linux-amd64\n", hex.EncodeToString(sum[:]))
	if err := os.WriteFile(filepath.Join(r.CacheDir, tag, releaseSumsName), []byte(lying), 0o644); err != nil {
		t.Fatal(err)
	}

	again, err := Download(context.Background(), r)
	if err != nil {
		t.Fatalf("second Download: %v", err)
	}
	got, err := os.ReadFile(again)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("Download returned %q, want the bytes the release serves, %q", got, body)
	}
}

func TestDownloadRefusesACacheHitItCannotVerify(t *testing.T) {
	const tag = "v1.2.3"
	body := []byte("a caramelo binary for linux/amd64")
	s := newReleaseServer(t, tag, map[string][]byte{"caramelo-linux-amd64": body})
	r := testRelease(t, s, tag, "linux", "amd64", nil)

	cached, err := Download(context.Background(), r)
	if err != nil {
		t.Fatalf("first Download: %v", err)
	}

	offline := r
	offline.Client = &http.Client{Transport: refusingTransport{}}
	path, err := Download(context.Background(), offline)
	if err == nil {
		t.Fatalf("Download served %s out of the cache with no checksum fetched", path)
	}
	if path != "" {
		t.Errorf("Download returned %q beside its error, want no path", path)
	}
	if !strings.Contains(err.Error(), s.url+"/"+tag+"/"+releaseSumsName) {
		t.Errorf("err = %v, want it to name the checksums it could not fetch", err)
	}
	if _, err := os.Stat(cached); err != nil {
		t.Errorf("the cached binary was thrown away: %v", err)
	}
}

func TestDownloadRefusesAWorldWritableCacheDir(t *testing.T) {
	const tag = "v0.0.1"
	s := newReleaseServer(t, tag, map[string][]byte{"caramelo-linux-amd64": []byte("a caramelo binary")})
	r := testRelease(t, s, tag, "linux", "amd64", nil)
	if err := os.Chmod(r.CacheDir, 0o777); err != nil {
		t.Fatal(err)
	}

	_, err := Download(context.Background(), r)
	if err == nil {
		t.Fatal("Download succeeded out of a world-writable cache directory")
	}
	for _, want := range []string{r.CacheDir, "chmod 700"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to say %q", err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(r.CacheDir, tag, "caramelo-linux-amd64")); err == nil {
		t.Error("the refused download wrote a binary anyway")
	}
}

func TestDownloadRefusesAForeignOwnedCacheDir(t *testing.T) {
	const tag = "v0.0.1"
	body := []byte("a caramelo binary for linux/amd64")

	t.Run("another user owns it", func(t *testing.T) {
		s := newReleaseServer(t, tag, map[string][]byte{"caramelo-linux-amd64": body})
		r := testRelease(t, s, tag, "linux", "amd64", nil)
		useEUID(t, ownerOf(t, r.CacheDir)+1)

		_, err := Download(context.Background(), r)
		if err == nil {
			t.Fatal("Download succeeded out of a cache directory owned by somebody else")
		}
		if !strings.Contains(err.Error(), r.CacheDir) {
			t.Errorf("err = %v, want it to name %s", err, r.CacheDir)
		}
	})

	t.Run("root may use any owner", func(t *testing.T) {
		s := newReleaseServer(t, tag, map[string][]byte{"caramelo-linux-amd64": body})
		r := testRelease(t, s, tag, "linux", "amd64", nil)
		useEUID(t, 0)

		if _, err := Download(context.Background(), r); err != nil {
			t.Fatalf("Download as root: %v", err)
		}
	})
}

func TestDownloadCreatesAPrivateCacheDir(t *testing.T) {
	const tag = "v0.0.1"
	s := newReleaseServer(t, tag, map[string][]byte{"caramelo-linux-amd64": []byte("a caramelo binary")})
	r := testRelease(t, s, tag, "linux", "amd64", nil)
	r.CacheDir = filepath.Join(r.CacheDir, "caramelo", "releases")

	if _, err := Download(context.Background(), r); err != nil {
		t.Fatalf("Download: %v", err)
	}
	for _, dir := range []string{r.CacheDir, filepath.Join(r.CacheDir, tag)} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("%s is %v, want drwx------", dir, fi.Mode().Perm())
		}
	}
}

func useEUID(t *testing.T, uid int) {
	t.Helper()
	prev := releaseEUID
	releaseEUID = func() int { return uid }
	t.Cleanup(func() { releaseEUID = prev })
}

func ownerOf(t *testing.T, path string) int {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skipf("%s reports no Unix owner", path)
	}
	return int(st.Uid)
}

func TestDownloadReplacesACorruptCachedBinary(t *testing.T) {
	const tag = "v0.0.1"
	body := []byte("a caramelo binary for linux/amd64")
	s := newReleaseServer(t, tag, map[string][]byte{"caramelo-linux-amd64": body})
	r := testRelease(t, s, tag, "linux", "amd64", nil)

	path, err := Download(context.Background(), r)
	if err != nil {
		t.Fatalf("first Download: %v", err)
	}
	if err := os.WriteFile(path, []byte("rubbish"), 0o755); err != nil {
		t.Fatal(err)
	}

	again, err := Download(context.Background(), r)
	if err != nil {
		t.Fatalf("second Download: %v", err)
	}
	got, err := os.ReadFile(again)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("cached file = %q, want it replaced with %q", got, body)
	}
}

func TestDownloadRefusesBeforeAnyRequest(t *testing.T) {
	const tag = "v0.0.1"
	s := newReleaseServer(t, tag, map[string][]byte{"caramelo-linux-amd64": []byte("x")})

	cases := []struct {
		name           string
		tag, os, arch  string
		wantInTheError []string
	}{
		{"unsupported platform", tag, "windows", "amd64", []string{"windows/amd64", "linux/amd64, linux/arm64, darwin/amd64, darwin/arm64", "--binary"}},
		{"unsupported arch", tag, "linux", "riscv64", []string{"linux/riscv64", "--binary"}},
		{"not a tag", "dev", "linux", "amd64", []string{`"dev"`, "v<major>.<minor>.<patch>"}},
		{"path in the tag", "v0.0.1/..", "linux", "amd64", []string{"v0.0.1/..", "v<major>.<minor>.<patch>"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := s.count()
			r := testRelease(t, s, c.tag, c.os, c.arch, nil)
			_, err := Download(context.Background(), r)
			if err == nil {
				t.Fatal("Download succeeded, want a refusal")
			}
			for _, want := range c.wantInTheError {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to say %q", err, want)
				}
			}
			if s.count() != before {
				t.Errorf("the refusal made %d request(s), want none", s.count()-before)
			}
		})
	}
}

func TestDownloadFollowsARedirect(t *testing.T) {
	const tag = "v0.0.1"
	body := []byte("a caramelo binary served from a CDN")
	sum := sha256.Sum256(body)
	sums := fmt.Sprintf("%s  caramelo-linux-amd64\n", hex.EncodeToString(sum[:]))

	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	t.Cleanup(cdn.Close)

	var redirects int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + tag + "/SHA256SUMS":
			w.Write([]byte(sums))
		case "/" + tag + "/caramelo-linux-amd64":
			atomic.AddInt64(&redirects, 1)
			http.Redirect(w, r, cdn.URL+"/objects/caramelo-linux-amd64", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(origin.Close)

	r := Release{Tag: tag, OS: "linux", Arch: "amd64", BaseURL: origin.URL, CacheDir: t.TempDir(), Client: &http.Client{}}
	path, err := Download(context.Background(), r)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("downloaded %q, want %q", got, body)
	}
	if atomic.LoadInt64(&redirects) != 1 {
		t.Errorf("the origin served %d redirect(s), want 1", atomic.LoadInt64(&redirects))
	}
}

func TestDownloadRefusesAPlaintextRedirect(t *testing.T) {
	const tag = "v0.0.1"
	body := []byte("a binary a network attacker would like you to run")
	sum := sha256.Sum256(body)
	sums := fmt.Sprintf("%s  caramelo-linux-amd64\n", hex.EncodeToString(sum[:]))

	plaintext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	t.Cleanup(plaintext.Close)

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + tag + "/SHA256SUMS":
			w.Write([]byte(sums))
		case "/" + tag + "/caramelo-linux-amd64":
			http.Redirect(w, r, plaintext.URL+"/objects/caramelo-linux-amd64", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(origin.Close)

	r := Release{Tag: tag, OS: "linux", Arch: "amd64", BaseURL: origin.URL, CacheDir: t.TempDir(), Client: origin.Client()}
	path, err := Download(context.Background(), r)
	if err == nil {
		t.Fatalf("Download followed the downgrade and wrote %s", path)
	}
	for _, want := range []string{"not https", "--binary"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to say %q", err, want)
		}
	}
}

func TestDownloadDefaultsToTheReleaseBaseURL(t *testing.T) {
	r := Release{Tag: "v0.0.1", OS: "linux", Arch: "amd64", CacheDir: t.TempDir(), Client: &http.Client{Transport: refusingTransport{}}}
	_, err := Download(context.Background(), r)
	if err == nil {
		t.Fatal("Download succeeded, want the transport's refusal")
	}
	if !strings.Contains(err.Error(), ReleaseBaseURL+"/v0.0.1/SHA256SUMS") {
		t.Errorf("err = %v, want it to name the default release URL", err)
	}
}

func TestReleaseClientHasNoWholeRequestDeadline(t *testing.T) {
	mine := releaseClient(nil)
	if mine.Timeout != 0 {
		t.Errorf("Timeout = %v, want no deadline on the whole request", mine.Timeout)
	}
	tr, ok := mine.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want an *http.Transport with its own budgets", mine.Transport)
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Error("the transport waits forever for the response headers")
	}
	if tr.TLSHandshakeTimeout == 0 {
		t.Error("the transport waits forever for the TLS handshake")
	}
	if mine.CheckRedirect == nil {
		t.Error("the default client does not check its redirects")
	}
	if tr.Proxy == nil {
		t.Fatal("the transport dials directly, so HTTPS_PROXY, HTTP_PROXY and NO_PROXY are ignored")
	}
	if reflect.ValueOf(tr.Proxy).Pointer() != reflect.ValueOf(http.ProxyFromEnvironment).Pointer() {
		t.Error("the transport does not take its proxy from the environment")
	}
	std := http.DefaultTransport.(*http.Transport)
	if tr.IdleConnTimeout != std.IdleConnTimeout || tr.MaxIdleConns != std.MaxIdleConns {
		t.Errorf("the transport keeps idle connections as %v/%d, want the standard %v/%d",
			tr.IdleConnTimeout, tr.MaxIdleConns, std.IdleConnTimeout, std.MaxIdleConns)
	}

	given := &http.Client{Transport: refusingTransport{}, Timeout: 7 * time.Second}
	out := releaseClient(given)
	if out.Transport != given.Transport {
		t.Errorf("Transport = %#v, want the caller's left as it was", out.Transport)
	}
	if out.Timeout != given.Timeout {
		t.Errorf("Timeout = %v, want the caller's %v", out.Timeout, given.Timeout)
	}
	if out.CheckRedirect == nil {
		t.Error("a client given by the caller did not inherit the redirect check")
	}
}

func TestDownloadGivesUpOnAStalledBody(t *testing.T) {
	const tag = "v0.0.1"
	const name = "caramelo-linux-amd64"
	body := []byte("a caramelo binary for linux/amd64")
	sum := sha256.Sum256(body)
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name)

	block := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+tag+"/"+releaseSumsName {
			w.Write([]byte(sums))
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body[:4])
		w.(http.Flusher).Flush()
		<-block
	}))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(block) })

	useIdleTimeout(t, 200*time.Millisecond)
	r := Release{Tag: tag, OS: "linux", Arch: "amd64", BaseURL: ts.URL, CacheDir: t.TempDir(), Client: &http.Client{}}

	type outcome struct {
		path string
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		path, err := Download(context.Background(), r)
		done <- outcome{path, err}
	}()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatalf("Download returned %s, want it to give up on the stall", got.path)
		}
		for _, want := range []string{ts.URL + "/" + tag + "/" + name, "stalled"} {
			if !strings.Contains(got.err.Error(), want) {
				t.Errorf("err = %v, want it to say %q", got.err, want)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Download is still waiting on a body that stopped arriving")
	}
}

func TestDownloadSurvivesASlowLink(t *testing.T) {
	const tag = "v0.0.1"
	const name = "caramelo-linux-amd64"
	body := []byte(strings.Repeat("caramelo", 50))
	sum := sha256.Sum256(body)
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name)

	const chunk = 16
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+tag+"/"+releaseSumsName {
			w.Write([]byte(sums))
			return
		}
		for i := 0; i < len(body); i += chunk {
			w.Write(body[i:min(i+chunk, len(body))])
			w.(http.Flusher).Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	t.Cleanup(ts.Close)

	useIdleTimeout(t, 150*time.Millisecond)
	r := Release{Tag: tag, OS: "linux", Arch: "amd64", BaseURL: ts.URL, CacheDir: t.TempDir(), Client: &http.Client{}}

	path, err := Download(context.Background(), r)
	if err != nil {
		t.Fatalf("Download over a slow link: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("downloaded %q, want %q", got, body)
	}
}

func useIdleTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := releaseIdleTimeout
	releaseIdleTimeout = d
	t.Cleanup(func() { releaseIdleTimeout = prev })
}

type refusingTransport struct{}

func (refusingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("no network in this test")
}
