//go:build integration

package itest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	GossVersion    = "v0.4.9"
	GossRemotePath = "/var/tmp/goss"

	gossDownloadTimeout = 5 * time.Minute
)

var gossReleases = map[string]struct{ Asset, SHA256 string }{
	"amd64": {"goss-linux-amd64", "87dd36cfa1b8b50554e6e2ca29168272e26755b19ba5438341f7c66b36decc19"},
	"arm64": {"goss-linux-arm64", "14fd24ac08236559f4809e6a627792d1b947ed98654bba1662ef1d6122d77e18"},
}

func GossAsset(arch string) (string, error) {
	r, ok := gossReleases[arch]
	if !ok {
		return "", fmt.Errorf("goss %s: no pinned release for %s", GossVersion, arch)
	}
	return r.Asset, nil
}

func GossSHA256For(arch string) (string, error) {
	r, ok := gossReleases[arch]
	if !ok {
		return "", fmt.Errorf("goss %s: no pinned release for %s", GossVersion, arch)
	}
	return r.SHA256, nil
}

func GossURLFor(arch string) (string, error) {
	asset, err := GossAsset(arch)
	if err != nil {
		return "", err
	}
	return "https://github.com/goss-org/goss/releases/download/" + GossVersion + "/" + asset, nil
}

func EnsureGoss(dir, arch string) (string, error) {
	asset, err := GossAsset(arch)
	if err != nil {
		return "", err
	}
	sha, err := GossSHA256For(arch)
	if err != nil {
		return "", err
	}
	url, err := GossURLFor(arch)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, asset)
	if sum, err := FileSHA256(path); err == nil && sum == sha {
		return path, nil
	}
	lock, err := lockCache("goss-" + arch)
	if err != nil {
		return "", err
	}
	defer lock.release()
	if sum, err := FileSHA256(path); err == nil && sum == sha {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("goss cache %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, asset+".*.part")
	if err != nil {
		return "", fmt.Errorf("goss cache %s: %w", dir, err)
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	client := &http.Client{Timeout: gossDownloadTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %s", url, resp.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		return "", fmt.Errorf("download %s: sha256 %s, want %s", url, got, sha)
	}
	if err := tmp.Chmod(0o755); err != nil {
		return "", fmt.Errorf("chmod goss: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("write goss: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("install goss into %s: %w", dir, err)
	}
	return path, nil
}

const (
	GossSuccess = 0
	GossFail    = 1
	GossSkip    = 2
)

type GossAssertion struct {
	ResourceType string          `json:"resource-type"`
	ResourceID   string          `json:"resource-id"`
	Property     string          `json:"property"`
	Result       int             `json:"result"`
	Successful   bool            `json:"successful"`
	Skipped      bool            `json:"skipped"`
	Summary      string          `json:"summary-line-compact"`
	SummaryLong  string          `json:"summary-line"`
	Err          json.RawMessage `json:"err"`
}

func (a GossAssertion) Failed() bool { return a.Result == GossFail || (!a.Successful && !a.Skipped) }

func (a GossAssertion) Describe() string {
	s := a.Summary
	if s == "" {
		s = a.SummaryLong
	}
	if s == "" {
		s = fmt.Sprintf("%s: %s: %s: failed", a.ResourceType, a.ResourceID, a.Property)
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(a.Err) > 0 && string(a.Err) != "null" {
		s += " (err: " + string(a.Err) + ")"
	}
	return s
}

type GossSummary struct {
	FailedCount   int    `json:"failed-count"`
	SkippedCount  int    `json:"skipped-count"`
	TestCount     int    `json:"test-count"`
	SummaryLine   string `json:"summary-line"`
	TotalDuration int64  `json:"total-duration"`
}

type GossReport struct {
	Results []GossAssertion `json:"results"`
	Summary GossSummary     `json:"summary"`
}

func (r *GossReport) Failures() []GossAssertion {
	var out []GossAssertion
	for _, a := range r.Results {
		if a.Failed() {
			out = append(out, a)
		}
	}
	return out
}

func ParseGossReport(stdout []byte) (*GossReport, error) {
	var r GossReport
	if err := json.Unmarshal(stdout, &r); err != nil {
		snippet := string(stdout)
		if len(snippet) > 400 {
			snippet = snippet[:400] + "..."
		}
		return nil, fmt.Errorf("parse goss JSON: %w (output: %q)", err, snippet)
	}
	return &r, nil
}
