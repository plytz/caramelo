//go:build integration

package itest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup"
)

const ProvisionedHostname = "machine"

const provisionedImagePrefix = "caramelo-itest-provisioned:"

var (
	provisionedMu   sync.Mutex
	provisionedTag  string
	provisionedErr  error
	provisionedDone bool
)

func provisionedImageSetupCommand(peerKey string) string { return SetupCommandFor(LoginHome, peerKey) }

func SetupCommandFor(home, peerKey string) string {
	return fmt.Sprintf(
		"sudo %s hub setup --yes --json --authorized-keys %s/.ssh/authorized_keys "+
			"--api-listen %s --peer %s %s --edge --acme-ca %s --acme-email %s",
		RemoteBin, home, serverconfig.APIListenBoth, LabPeerName, peerKey, ACMEDirectory, ACMEEmail)
}

func ProvisionedHash() (string, error) {
	bin, err := MachineBinary()
	if err != nil {
		return "", err
	}
	binSum, err := FileSHA256(bin)
	if err != nil {
		return "", err
	}
	imageHash, err := DockerfileHash(ImageMachine)
	if err != nil {
		return "", err
	}
	kp, err := LabPeerKey()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintln(h, binSum)
	fmt.Fprintln(h, imageHash)
	fmt.Fprintln(h, provisionedImageSetupCommand(kp.Public))
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

func provisionedVolumeTar(hash string) (string, error) {
	dir, err := SharedCacheDir("provisioned")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, hash+".tar"), nil
}

func EnsureProvisionedImage(ctx context.Context) (string, error) {
	provisionedMu.Lock()
	defer provisionedMu.Unlock()
	if provisionedDone {
		return provisionedTag, provisionedErr
	}
	provisionedTag, provisionedErr = buildProvisionedImage(ctx)
	provisionedDone = true
	return provisionedTag, provisionedErr
}

func buildProvisionedImage(ctx context.Context) (string, error) {
	hash, err := ProvisionedHash()
	if err != nil {
		return "", err
	}
	tag := provisionedImagePrefix + hash
	tarPath, err := provisionedVolumeTar(hash)
	if err != nil {
		return "", err
	}
	if imageExists(ctx, tag) && fileExists(tarPath) {
		return tag, nil
	}
	lock, err := lockCache("provisioned")
	if err != nil {
		return "", err
	}
	defer lock.release()
	if imageExists(ctx, tag) && fileExists(tarPath) {
		return tag, nil
	}

	budget := Budget()
	buildCtx, cancel := context.WithTimeout(ctx, budget.Setup+budget.Boot+imageBuildTimeout)
	defer cancel()

	base, err := EnsureImage(buildCtx, ImageMachine)
	if err != nil {
		return "", err
	}
	bin, err := MachineBinary()
	if err != nil {
		return "", err
	}
	kp, err := LabPeerKey()
	if err != nil {
		return "", err
	}
	_, pub, err := LabSSHKey()
	if err != nil {
		return "", err
	}

	name := "caramelo-itest-provision-" + hash
	volume := name + "-vol"
	_ = removeContainer(buildCtx, name)
	_ = removeVolume(buildCtx, volume)
	if err := createVolume(buildCtx, volume); err != nil {
		return "", err
	}
	defer func() {
		_ = removeContainer(context.Background(), name)
		_ = removeVolume(context.Background(), volume)
	}()

	fmt.Fprintf(os.Stderr, "itest: provisioning %s (hub setup, this takes minutes)\n", tag)
	start := time.Now()
	if _, err := docker(buildCtx, "run", "-d",
		"--name", name,
		"--hostname", ProvisionedHostname,
		"--privileged", "--cgroupns=private",
		"--tmpfs", "/run", "--tmpfs", "/run/lock", "--tmpfs", "/tmp:exec,mode=1777,size=512m",
		"-v", volume+":"+DockerDataDir,
		"--label", label(),
		base); err != nil {
		return "", fmt.Errorf("provision: %w", err)
	}

	builder := &Machine{
		Name:     name,
		Role:     RoleHub,
		Alias:    RoleHub,
		Kind:     KindMachine,
		State:    StateClean,
		Hostname: ProvisionedHostname,
		Volume:   volume,
		lab:      &Lab{Network: "bridge", budget: budget},
	}
	newDockerDriver(builder)
	if err := waitForSystemd(buildCtx, builder, budget.Boot); err != nil {
		return "", fmt.Errorf("provision: %w", err)
	}
	if err := requireHostBrNetfilter(buildCtx, builder); err != nil {
		return "", fmt.Errorf("provision: %w", err)
	}
	if err := builder.Copy(buildCtx, pub, LoginHome+"/.ssh/authorized_keys"); err != nil {
		return "", fmt.Errorf("provision: %w", err)
	}
	if err := chmodOn(buildCtx, builder, "0600", LoginHome+"/.ssh/authorized_keys"); err != nil {
		return "", fmt.Errorf("provision: %w", err)
	}
	if err := builder.Copy(buildCtx, bin, RemoteBin); err != nil {
		return "", fmt.Errorf("provision: %w", err)
	}
	if res, err := builder.RunAsRoot(buildCtx, "chmod +x "+RemoteBin); err != nil {
		return "", fmt.Errorf("provision: %w", err)
	} else if res.ExitCode != 0 {
		return "", fmt.Errorf("provision: chmod %s: %s", RemoteBin, res.Stderr)
	}

	cmd := provisionedImageSetupCommand(kp.Public)
	res, err := builder.Run(buildCtx, cmd)
	if err != nil {
		return "", fmt.Errorf("provision: %w", err)
	}
	var report setup.Report
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &report); jsonErr != nil {
		return "", fmt.Errorf("provision: hub setup exit %d, stdout is not a report: %w\nstdout:\n%s\nstderr:\n%s",
			res.ExitCode, jsonErr, res.Stdout, res.Stderr)
	}
	if res.ExitCode != 0 || report.Failed != 0 {
		return "", fmt.Errorf("provision: hub setup exit %d, %d step(s) failed\n%s",
			res.ExitCode, report.Failed, res.Stdout)
	}
	if err := WaitForPort(buildCtx, builder, CarameloSSHPort); err != nil {
		return "", fmt.Errorf("provision: %w", err)
	}

	if err := saveProvisionedVolume(buildCtx, builder, tarPath); err != nil {
		return "", err
	}
	if _, err := docker(buildCtx, "stop", "-t", "30", name); err != nil {
		return "", fmt.Errorf("provision: %w", err)
	}
	if _, err := docker(buildCtx, "commit", "--change", "LABEL "+label(), name, tag); err != nil {
		return "", fmt.Errorf("provision: %w", err)
	}
	fmt.Fprintf(os.Stderr, "itest: provisioned %s in %s\n", tag, time.Since(start).Round(time.Second))
	return tag, nil
}

func saveProvisionedVolume(ctx context.Context, m *Machine, tarPath string) error {
	if err := os.MkdirAll(filepath.Dir(tarPath), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(tarPath), err)
	}
	tmp := tarPath + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	defer func() {
		f.Close()
		os.Remove(tmp)
	}()
	res, err := runDocker(ctx, nil, f, m.docker().execArgs("root", "tar -cf - -C "+DockerDataDir+" .")...)
	if err != nil {
		return fmt.Errorf("save the docker data directory of %s: %w", m.Alias, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("save the docker data directory of %s: exit %d: %s",
			m.Alias, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, tarPath); err != nil {
		return fmt.Errorf("install %s: %w", tarPath, err)
	}
	return nil
}

func restoreProvisionedVolume(ctx context.Context, volume string) error {
	hash, err := ProvisionedHash()
	if err != nil {
		return err
	}
	tarPath, err := provisionedVolumeTar(hash)
	if err != nil {
		return err
	}
	f, err := os.Open(tarPath)
	if err != nil {
		return fmt.Errorf("restore the docker data directory: %w", err)
	}
	defer f.Close()
	base, err := EnsureImage(ctx, ImageMachine)
	if err != nil {
		return err
	}
	res, err := runDocker(ctx, f, nil,
		"run", "--rm", "-i", "--label", label(),
		"-v", volume+":/restore", base,
		"tar", "-xpf", "-", "-C", "/restore")
	if err != nil {
		return fmt.Errorf("restore the docker data directory into %s: %w", volume, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("restore the docker data directory into %s: exit %d: %s",
			volume, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}
