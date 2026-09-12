//go:build integration

package itest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	remoteImageDir = "/var/tmp/caramelo-images"
	cacheTimeout   = 10 * time.Minute
)

func ImageCacheDir(ctx context.Context, m *Machine) (string, error) {
	return CacheDirFor(ctx, m, "images")
}

func ImageFileName(image string) string {
	r := strings.NewReplacer("/", "_", ":", "_")
	return r.Replace(image) + ".tar"
}

func SeedImages(t testing.TB, m *Machine, images ...string) {
	t.Helper()
	if err := SeedImagesTo(m, images...); err != nil {
		t.Fatalf("itest: seed images: %v", err)
	}
}

func SeedImagesTo(m *Machine, images ...string) error {
	if len(images) == 0 {
		return errors.New("no images named: say which ones this suite needs")
	}
	if m.Kind != KindMachine {
		return fmt.Errorf("seed images: %s is a %s, and only a machine runs docker", m.Alias, m.Kind)
	}
	if m.Target() != TargetDocker {
		fmt.Fprintf(os.Stderr, "itest: %s is a %s target, so it pulls its images itself instead of taking them over the wire\n", m.Alias, m.Target())
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), cacheTimeout)
	defer cancel()
	dir, err := ImageCacheDir(ctx, m)
	if err != nil {
		return err
	}
	if err := mustSucceedOn(ctx, m, "sudo mkdir -p "+remoteImageDir); err != nil {
		return fmt.Errorf("seed images: %w", err)
	}
	loaded := 0
	for _, image := range images {
		local := filepath.Join(dir, ImageFileName(image))
		if !fileExists(local) {
			fmt.Fprintf(os.Stderr, "itest: %s is not in %s; docker will pull it\n", image, dir)
			continue
		}
		if has, err := hasImage(ctx, m, image); err != nil {
			return fmt.Errorf("seed image %s: %w", image, err)
		} else if has {
			continue
		}
		remote := remoteImageDir + "/" + filepath.Base(local)
		if err := m.Copy(ctx, local, remote); err != nil {
			return fmt.Errorf("seed image %s: %w", image, err)
		}
		if err := mustSucceedOn(ctx, m, "sudo chmod 0644 "+ShellQuote(remote)); err != nil {
			return fmt.Errorf("seed image %s: %w", image, err)
		}
		if err := mustSucceedOn(ctx, m, AsUserSession(m, CarameloUser, "docker load -i "+remote)); err != nil {
			return fmt.Errorf("seed image %s: docker load: %w", image, err)
		}
		if err := mustSucceedOn(ctx, m, "sudo rm -f "+ShellQuote(remote)); err != nil {
			return fmt.Errorf("seed image %s: %w", image, err)
		}
		loaded++
	}
	fmt.Fprintf(os.Stderr, "itest: loaded %d cached image archive(s) on %s\n", loaded, m.Name)
	return nil
}

func ExportImages(t testing.TB, m *Machine, images ...string) {
	t.Helper()
	if err := ExportImagesFrom(m, images...); err != nil {
		t.Errorf("itest: export images: %v", err)
	}
}

func ExportImagesFrom(m *Machine, images ...string) error {
	if len(images) == 0 {
		return errors.New("no images named: say which ones this suite caches")
	}
	if m.Kind != KindMachine {
		return fmt.Errorf("export images: %s is a %s, and only a machine runs docker", m.Alias, m.Kind)
	}
	if m.Target() != TargetDocker {
		fmt.Fprintf(os.Stderr, "itest: %s is a %s target, so its images are left where they are instead of being fetched over the wire\n", m.Alias, m.Target())
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), cacheTimeout)
	defer cancel()
	dir, err := ImageCacheDir(ctx, m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("export images: create %s: %w", dir, err)
	}
	for _, image := range images {
		local := filepath.Join(dir, ImageFileName(image))
		if fileExists(local) {
			continue
		}
		if has, err := hasImage(ctx, m, image); err != nil {
			return fmt.Errorf("export image %s: %w", image, err)
		} else if !has {
			fmt.Fprintf(os.Stderr, "itest: %s is not on %s; nothing to cache\n", image, m.Name)
			continue
		}
		remote := remoteImageDir + "/" + ImageFileName(image)
		save := fmt.Sprintf("sudo mkdir -p %s && sudo chown %s %s && %s && sudo chmod 0644 %s",
			remoteImageDir, CarameloUser, remoteImageDir,
			AsUserSession(m, CarameloUser, "docker save -o "+remote+" "+image), ShellQuote(remote))
		if err := mustSucceedOn(ctx, m, save); err != nil {
			return fmt.Errorf("export image %s: %w", image, err)
		}
		part, err := os.CreateTemp(dir, ImageFileName(image)+".*.part")
		if err != nil {
			return fmt.Errorf("export image %s: %w", image, err)
		}
		partPath := part.Name()
		if err := part.Close(); err != nil {
			os.Remove(partPath)
			return fmt.Errorf("export image %s: %w", image, err)
		}
		if err := m.Fetch(ctx, remote, partPath); err != nil {
			os.Remove(partPath)
			return fmt.Errorf("export image %s: %w", image, err)
		}
		if err := os.Chmod(partPath, 0o644); err != nil {
			os.Remove(partPath)
			return fmt.Errorf("export image %s: %w", image, err)
		}
		if err := os.Rename(partPath, local); err != nil {
			os.Remove(partPath)
			return fmt.Errorf("export image %s: %w", image, err)
		}
		if err := mustSucceedOn(ctx, m, "sudo rm -f "+ShellQuote(remote)); err != nil {
			return fmt.Errorf("export image %s: %w", image, err)
		}
		fmt.Fprintf(os.Stderr, "itest: cached image %s in %s\n", image, local)
	}
	return nil
}

func hasImage(ctx context.Context, m *Machine, image string) (bool, error) {
	cmd := AsUserSession(m, CarameloUser, "docker image inspect --format '{{.Id}}' "+ShellQuote(image)+" >/dev/null 2>&1")
	res, err := m.Run(ctx, cmd)
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}
