package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/plytz/caramelo/internal/userdir"
)

const ReleaseRepository = "plytz/caramelo"

var ReleaseBaseURL = "https://github.com/" + ReleaseRepository + "/releases/download"

var SupportedPlatforms = []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"}

var releaseTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

func IsReleaseTag(s string) bool {
	return releaseTag.MatchString(s)
}

func Supported(os, arch string) bool {
	for _, p := range SupportedPlatforms {
		if p == os+"/"+arch {
			return true
		}
	}
	return false
}

type Release struct {
	Tag      string
	OS       string
	Arch     string
	BaseURL  string
	CacheDir string
	Client   *http.Client
	Log      io.Writer
}

const (
	releaseSumsName     = "SHA256SUMS"
	releaseMaxBytes     = 512 << 20
	releaseMaxSumsBytes = 1 << 20
)

var (
	releaseDialTimeout   = 10 * time.Second
	releaseHeaderTimeout = 30 * time.Second
	releaseIdleTimeout   = 60 * time.Second
)

func Download(ctx context.Context, r Release) (string, error) {
	name := SiblingName(r.OS, r.Arch)
	if !IsReleaseTag(r.Tag) {
		return "", fmt.Errorf("download %s: %q is not a release tag; use the shape v<major>.<minor>.<patch>, for example v0.0.1, and see https://github.com/%s/releases for the tags that exist", name, r.Tag, ReleaseRepository)
	}
	if !Supported(r.OS, r.Arch) {
		return "", fmt.Errorf("download %s %s: no release binary is published for %s/%s; releases cover %s. Build one for that platform yourself and ship it with --binary <path>", name, r.Tag, r.OS, r.Arch, strings.Join(SupportedPlatforms, ", "))
	}

	what := fmt.Sprintf("download %s %s", name, r.Tag)
	if r.Log == nil {
		r.Log = io.Discard
	}
	r.Client = releaseClient(r.Client)
	if r.BaseURL == "" {
		r.BaseURL = ReleaseBaseURL
	}
	if r.CacheDir == "" {
		cache, err := userdir.Cache()
		if err != nil {
			return "", fmt.Errorf("%s: locate the cache directory for the download: %w", what, err)
		}
		r.CacheDir = filepath.Join(cache, "caramelo", "releases")
	}

	dir := filepath.Join(r.CacheDir, r.Tag)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("%s: create the cache directory %s: %w", what, dir, err)
	}
	for _, d := range []string{r.CacheDir, dir} {
		if err := checkCacheDir(d); err != nil {
			return "", fmt.Errorf("%s: %w", what, err)
		}
	}
	final := filepath.Join(dir, name)
	base := strings.TrimRight(r.BaseURL, "/") + "/" + r.Tag

	want, err := releaseWantedSum(ctx, r, what, name, base)
	if err != nil {
		return "", err
	}

	if releaseFileExists(final) {
		got, err := releaseSHA256File(final)
		if err != nil {
			return "", fmt.Errorf("%s: hash the cached %s: %w", what, final, err)
		}
		if got == want {
			fmt.Fprintf(r.Log, "[bootstrap] using the cached %s %s\n", name, r.Tag)
			return final, nil
		}
		if err := os.Remove(final); err != nil {
			return "", fmt.Errorf("%s: the cached %s does not match its checksum and could not be removed: %w", what, final, err)
		}
	}

	url := base + "/" + name
	fmt.Fprintf(r.Log, "[bootstrap] downloading %s %s from %s\n", name, r.Tag, url)
	if err := releaseDownload(ctx, r, what, url, final, want); err != nil {
		return "", err
	}
	return final, nil
}

func releaseWantedSum(ctx context.Context, r Release, what, name, base string) (string, error) {
	url := base + "/" + releaseSumsName
	body, err := releaseFetch(ctx, r.Client, url, releaseMaxSumsBytes)
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	sum, ok := releaseSumFor(string(body), name)
	if !ok {
		return "", fmt.Errorf("%s: %s lists no %s; that release publishes no binary for %s/%s, check https://github.com/%s/releases", what, url, name, r.OS, r.Arch, ReleaseRepository)
	}
	return sum, nil
}

var releaseEUID = os.Geteuid

func checkCacheDir(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("check the cache directory %s: %w", path, err)
	}
	if mode := info.Mode().Perm(); mode&0o022 != 0 {
		return fmt.Errorf("the cache directory %s is %v: anyone who can write there can plant a binary this run would ship to the target. Run chmod 700 %s, or point XDG_CACHE_HOME at a directory only you can write", path, mode, path)
	}
	me := releaseEUID()
	if me == 0 {
		return nil
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(st.Uid) != me {
		return fmt.Errorf("the cache directory %s is owned by uid %d, not by you (uid %d): its owner can replace the binary after it is verified and before it is shipped. Point XDG_CACHE_HOME at a directory of your own", path, st.Uid, me)
	}
	return nil
}

func releaseDownload(ctx context.Context, r Release, what, url, final, want string) error {
	body, err := releaseOpen(ctx, r.Client, url)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer body.Close()

	tmp, err := os.CreateTemp(filepath.Dir(final), filepath.Base(final)+".part-")
	if err != nil {
		return fmt.Errorf("%s: create a temporary file beside %s: %w", what, final, err)
	}
	done := false
	defer func() {
		tmp.Close()
		if !done {
			os.Remove(tmp.Name())
		}
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(body, releaseMaxBytes+1))
	if err != nil {
		return fmt.Errorf("%s: read %s: %w", what, url, err)
	}
	if n > releaseMaxBytes {
		return fmt.Errorf("%s: %s is larger than the %d MiB a caramelo binary may be; the URL is wrong or the release is not what it claims", what, url, int64(releaseMaxBytes)>>20)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("verify %s from %s: checksum mismatch, want %s got %s; the download was corrupt or the release was replaced, try again", filepath.Base(final), url, want, got)
	}
	if err := tmp.Chmod(0o755); err != nil {
		return fmt.Errorf("%s: make %s executable: %w", what, tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%s: write %s: %w", what, tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return fmt.Errorf("%s: move the verified download to %s: %w", what, final, err)
	}
	done = true
	return nil
}

func releaseClient(c *http.Client) *http.Client {
	if c != nil {
		given := *c
		if given.CheckRedirect == nil {
			given.CheckRedirect = releaseCheckRedirect
		}
		return &given
	}
	return &http.Client{Transport: releaseTransport(), CheckRedirect: releaseCheckRedirect}
}

func releaseTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: releaseDialTimeout}).DialContext
	tr.TLSHandshakeTimeout = releaseHeaderTimeout
	tr.ResponseHeaderTimeout = releaseHeaderTimeout
	return tr
}

const releaseMaxRedirects = 10

func releaseCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= releaseMaxRedirects {
		return fmt.Errorf("stopped after %d redirects from %s; the release URL does not settle, check https://github.com/%s/releases", releaseMaxRedirects, via[0].URL, ReleaseRepository)
	}
	from := via[len(via)-1].URL
	if from.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("%s redirects to %s, which is not https; the checksum comes over the same connection as the binary, so a plaintext hop would prove nothing. Download the release yourself and ship it with --binary <path>", from, req.URL)
	}
	return nil
}

func releaseOpen(ctx context.Context, c *http.Client, url string) (io.ReadCloser, error) {
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("build the request for %s: %w", url, err)
	}
	resp, err := c.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("GET %s: 404 not found; that release or that file does not exist, check the tags at https://github.com/%s/releases", url, ReleaseRepository)
		}
		return nil, fmt.Errorf("GET %s: %s; retry, or check the release at https://github.com/%s/releases", url, resp.Status, ReleaseRepository)
	}
	return watchForStalls(resp.Body, cancel, url, releaseIdleTimeout), nil
}

type stallWatcher struct {
	body    io.ReadCloser
	url     string
	idle    time.Duration
	timer   *time.Timer
	cancel  context.CancelFunc
	stalled atomic.Bool
}

func watchForStalls(body io.ReadCloser, cancel context.CancelFunc, url string, idle time.Duration) *stallWatcher {
	w := &stallWatcher{body: body, url: url, idle: idle, cancel: cancel}
	w.timer = time.AfterFunc(idle, func() {
		w.stalled.Store(true)
		cancel()
	})
	return w
}

func (w *stallWatcher) Read(p []byte) (int, error) {
	n, err := w.body.Read(p)
	if n > 0 {
		w.timer.Reset(w.idle)
	}
	if err != nil && !errors.Is(err, io.EOF) && w.stalled.Load() {
		return n, fmt.Errorf("nothing arrived from %s for %s; the download stalled, retry or download the release yourself and ship it with --binary <path>", w.url, w.idle)
	}
	return n, err
}

func (w *stallWatcher) Close() error {
	w.timer.Stop()
	err := w.body.Close()
	w.cancel()
	return err
}

func releaseFetch(ctx context.Context, c *http.Client, url string, max int64) ([]byte, error) {
	body, err := releaseOpen(ctx, c, url)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, max))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	return b, nil
}

func releaseSumFor(sums, name string) (string, bool) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		sum := strings.ToLower(fields[0])
		if len(sum) != sha256.Size*2 {
			continue
		}
		if _, err := hex.DecodeString(sum); err != nil {
			continue
		}
		return sum, true
	}
	return "", false
}

func releaseSHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func releaseFileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
