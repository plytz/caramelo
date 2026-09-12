package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
	releaseTimeout      = 5 * time.Minute
	releaseMaxBytes     = 512 << 20
	releaseMaxSumsBytes = 1 << 20
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("%s: create the cache directory %s: %w", what, dir, err)
	}
	final := filepath.Join(dir, name)
	sumsPath := filepath.Join(dir, releaseSumsName)
	base := strings.TrimRight(r.BaseURL, "/") + "/" + r.Tag

	want, cachedSums, err := releaseWantedSum(ctx, r, what, name, sumsPath, base, releaseFileExists(final))
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
		if cachedSums {
			want, _, err = releaseWantedSum(ctx, r, what, name, sumsPath, base, false)
			if err != nil {
				return "", err
			}
		}
	}

	url := base + "/" + name
	fmt.Fprintf(r.Log, "[bootstrap] downloading %s %s from %s\n", name, r.Tag, url)
	if err := releaseDownload(ctx, r, what, url, final, want); err != nil {
		return "", err
	}
	return final, nil
}

func releaseWantedSum(ctx context.Context, r Release, what, name, sumsPath, base string, preferCache bool) (string, bool, error) {
	if preferCache {
		if body, err := os.ReadFile(sumsPath); err == nil {
			if sum, ok := releaseSumFor(string(body), name); ok {
				return sum, true, nil
			}
		}
	}
	url := base + "/" + releaseSumsName
	body, err := releaseFetch(ctx, r.Client, url, releaseMaxSumsBytes)
	if err != nil {
		return "", false, fmt.Errorf("%s: %w", what, err)
	}
	sum, ok := releaseSumFor(string(body), name)
	if !ok {
		return "", false, fmt.Errorf("%s: %s lists no %s; that release publishes no binary for %s/%s, check https://github.com/%s/releases", what, url, name, r.OS, r.Arch, ReleaseRepository)
	}
	if err := releaseWriteFileAtomic(sumsPath, body, 0o644); err != nil {
		return "", false, fmt.Errorf("%s: cache %s: %w", what, sumsPath, err)
	}
	return sum, false, nil
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
	out := &http.Client{Timeout: releaseTimeout}
	if c != nil {
		given := *c
		out = &given
	}
	if out.CheckRedirect == nil {
		out.CheckRedirect = releaseCheckRedirect
	}
	return out
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build the request for %s: %w", url, err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("GET %s: 404 not found; that release or that file does not exist, check the tags at https://github.com/%s/releases", url, ReleaseRepository)
		}
		return nil, fmt.Errorf("GET %s: %s; retry, or check the release at https://github.com/%s/releases", url, resp.Status, ReleaseRepository)
	}
	return resp.Body, nil
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

func releaseWriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".part-")
	if err != nil {
		return err
	}
	done := false
	defer func() {
		tmp.Close()
		if !done {
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	done = true
	return nil
}
