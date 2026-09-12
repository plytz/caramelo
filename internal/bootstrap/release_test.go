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
	"strings"
	"sync/atomic"
	"testing"
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
	url      string
	requests *int64
	sums     string
}

func (s *releaseServer) count() int64 { return atomic.LoadInt64(s.requests) }

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

	var requests int64
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requests, 1)
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
	return &releaseServer{url: ts.URL, requests: &requests, sums: sums.String()}
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
	sums, err := os.ReadFile(filepath.Join(r.CacheDir, tag, "SHA256SUMS"))
	if err != nil {
		t.Fatalf("SHA256SUMS was not cached: %v", err)
	}
	if string(sums) != s.sums {
		t.Errorf("cached sums = %q, want %q", sums, s.sums)
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
	for _, e := range entries {
		if e.Name() != "SHA256SUMS" {
			t.Errorf("the cache directory holds %q after a failed download, want only SHA256SUMS", e.Name())
		}
	}
}

func newLyingServer(t *testing.T, tag, name string, honest, served []byte) *releaseServer {
	t.Helper()
	sum := sha256.Sum256(honest)
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name)
	var requests int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requests, 1)
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
	return &releaseServer{url: ts.URL, requests: &requests, sums: sums}
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

func TestDownloadCacheHitMakesNoRequest(t *testing.T) {
	const tag = "v1.2.3"
	body := []byte("a caramelo binary for darwin/arm64")
	s := newReleaseServer(t, tag, map[string][]byte{"caramelo-darwin-arm64": body})
	var log bytes.Buffer
	r := testRelease(t, s, tag, "darwin", "arm64", &log)

	first, err := Download(context.Background(), r)
	if err != nil {
		t.Fatalf("first Download: %v", err)
	}
	after := s.count()
	if after == 0 {
		t.Fatal("the first download made no request")
	}
	log.Reset()

	second, err := Download(context.Background(), r)
	if err != nil {
		t.Fatalf("second Download: %v", err)
	}
	if second != first {
		t.Errorf("second path = %q, want %q", second, first)
	}
	if s.count() != after {
		t.Errorf("the cache hit made %d request(s), want none", s.count()-after)
	}
	if !strings.Contains(log.String(), "[bootstrap] using the cached caramelo-darwin-arm64 v1.2.3") {
		t.Errorf("log = %q, want the cached line", log.String())
	}
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

type refusingTransport struct{}

func (refusingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("no network in this test")
}
