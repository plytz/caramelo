//go:build integration

package itest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	ImageMachine = "machine"
	ImageLaptop  = "laptop"
)

const imageBuildTimeout = 20 * time.Minute

type imageBuild struct {
	tag string
	err error
}

var (
	imageMu    sync.Mutex
	imageCache = map[string]imageBuild{}
)

func DockerfileHash(kind string) (string, error) {
	dir, err := imageContext(kind)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		return "", fmt.Errorf("read the %s Dockerfile: %w", kind, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12], nil
}

func ImageTag(kind string) (string, error) {
	hash, err := DockerfileHash(kind)
	if err != nil {
		return "", err
	}
	return "caramelo-itest-" + kind + ":" + hash, nil
}

func EnsureImage(ctx context.Context, kind string) (string, error) {
	imageMu.Lock()
	if b, ok := imageCache[kind]; ok {
		imageMu.Unlock()
		return b.tag, b.err
	}
	imageMu.Unlock()

	tag, err := buildImage(ctx, kind)

	imageMu.Lock()
	imageCache[kind] = imageBuild{tag: tag, err: err}
	imageMu.Unlock()
	return tag, err
}

func buildImage(ctx context.Context, kind string) (string, error) {
	tag, err := ImageTag(kind)
	if err != nil {
		return "", err
	}
	if imageExists(ctx, tag) {
		return tag, nil
	}
	lock, err := lockCache("image-" + kind)
	if err != nil {
		return "", err
	}
	defer lock.release()
	if imageExists(ctx, tag) {
		return tag, nil
	}
	dir, err := imageContext(kind)
	if err != nil {
		return "", err
	}
	buildCtx, cancel := context.WithTimeout(ctx, imageBuildTimeout)
	defer cancel()
	fmt.Fprintf(os.Stderr, "itest: building the %s image as %s\n", kind, tag)
	start := time.Now()
	res, err := runDocker(buildCtx, nil, os.Stderr,
		"build", "--label", label(), "--tag", tag, "--file", filepath.Join(dir, "Dockerfile"), dir)
	if err != nil {
		return "", fmt.Errorf("build the %s image: %w", kind, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("build the %s image: exit %d: %s", kind, res.ExitCode, res.Stderr)
	}
	fmt.Fprintf(os.Stderr, "itest: built %s in %s\n", tag, time.Since(start).Round(time.Second))
	return tag, nil
}
