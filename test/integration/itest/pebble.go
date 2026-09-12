//go:build integration

package itest

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	PebbleImage      = "ghcr.io/letsencrypt/pebble:latest"
	PebbleContainer  = "pebble"
	PebbleACMEPort   = 14000
	PebbleManagePort = 15000
)

const PebbleTrustName = "caramelo-lab-pebble"

const (
	pebbleMinicaPath = "/test/certs/pebble.minica.pem"
	localMinicaPath  = "/tmp/pebble.minica.pem"
)

const (
	pebbleTimeout      = 3 * time.Minute
	pebblePullTimeout  = 10 * time.Minute
	pebbleTrustTimeout = 2 * time.Minute
)

type Pebble struct {
	Directory       string
	MinicaPEM       string
	RootPEM         string
	IntermediatePEM string
}

func StartPebble(t testing.TB, m *Machine) *Pebble {
	t.Helper()
	p, err := StartPebbleOn(m)
	if err != nil {
		t.Fatalf("itest: pebble: %v", err)
	}
	t.Logf("pebble is serving %s on %s", p.Directory, m.Alias)
	return p
}

func StartPebbleOn(m *Machine) (*Pebble, error) {
	pullCtx, cancelPull := context.WithTimeout(context.Background(), pebblePullTimeout)
	defer cancelPull()
	if err := EnsurePebbleImageOn(pullCtx, m); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), pebbleTimeout)
	defer cancel()

	run := fmt.Sprintf("docker rm -f %s >/dev/null 2>&1; "+
		"docker run -d --name %s --restart unless-stopped "+
		"-e PEBBLE_VA_ALWAYS_VALID=1 -e PEBBLE_VA_NOSLEEP=1 "+
		"-e PEBBLE_WFE_NONCEREJECT=0 "+
		"-p 127.0.0.1:%d:%d -p 127.0.0.1:%d:%d %s",
		PebbleContainer, PebbleContainer,
		PebbleACMEPort, PebbleACMEPort, PebbleManagePort, PebbleManagePort, PebbleImage)
	if _, err := output(ctx, m, AsUser(m, CarameloUser, run)); err != nil {
		return nil, fmt.Errorf("start pebble on %s: %w", m.Alias, err)
	}

	ready := fmt.Sprintf("curl -sk --max-time 5 https://127.0.0.1:%d/dir >/dev/null", PebbleACMEPort)
	if err := m.WaitFor(ctx, ready, time.Second); err != nil {
		return nil, fmt.Errorf("pebble never served %s on %s: %w", PebbleDirectory, m.Alias, err)
	}

	p := &Pebble{Directory: PebbleDirectory}
	copyOut := fmt.Sprintf("docker cp %s:%s %s && cat %s",
		PebbleContainer, pebbleMinicaPath, localMinicaPath, localMinicaPath)
	minica, err := output(ctx, m, AsUser(m, CarameloUser, copyOut))
	if err != nil {
		return nil, fmt.Errorf("read pebble's minica root on %s: %w", m.Alias, err)
	}
	p.MinicaPEM = minica
	if err := p.readIssuingChain(ctx, m); err != nil {
		return nil, err
	}
	return p, nil
}

func EnsurePebbleImageOn(ctx context.Context, m *Machine) error {
	have := AsUser(m, CarameloUser, "docker image inspect "+PebbleImage+" >/dev/null 2>&1")
	res, err := m.Run(ctx, have)
	if err != nil {
		return fmt.Errorf("look for %s on %s: %w", PebbleImage, m.Alias, err)
	}
	if res.ExitCode == 0 {
		return nil
	}
	if _, err := output(ctx, m, AsUser(m, CarameloUser, "docker pull "+PebbleImage)); err != nil {
		return fmt.Errorf("pull %s on %s: %w", PebbleImage, m.Alias, err)
	}
	return nil
}

func (p *Pebble) Refresh(m *Machine) error {
	ctx, cancel := context.WithTimeout(context.Background(), pebbleTimeout)
	defer cancel()
	return p.readIssuingChain(ctx, m)
}

func (p *Pebble) readIssuingChain(ctx context.Context, m *Machine) error {
	if err := ensureMinica(ctx, m); err != nil {
		return err
	}
	get := func(path string) (string, error) {
		cmd := fmt.Sprintf("curl -sS --max-time 10 --cacert %s https://127.0.0.1:%d%s",
			localMinicaPath, PebbleManagePort, path)
		return output(ctx, m, AsUser(m, CarameloUser, cmd))
	}
	root, err := get("/roots/0")
	if err != nil {
		return fmt.Errorf("read pebble's issuing root on %s: %w", m.Alias, err)
	}
	intermediate, err := get("/intermediates/0")
	if err != nil {
		return fmt.Errorf("read pebble's intermediate on %s: %w", m.Alias, err)
	}
	if !strings.Contains(root, "BEGIN CERTIFICATE") {
		return fmt.Errorf("pebble's /roots/0 on %s is not a certificate: %q", m.Alias, truncate(root, 120))
	}
	p.RootPEM = root
	p.IntermediatePEM = intermediate
	return nil
}

func ensureMinica(ctx context.Context, m *Machine) error {
	cmd := fmt.Sprintf("[ -s %s ] || docker cp %s:%s %s",
		localMinicaPath, PebbleContainer, pebbleMinicaPath, localMinicaPath)
	if _, err := output(ctx, m, AsUser(m, CarameloUser, cmd)); err != nil {
		return fmt.Errorf("put pebble's own root back on %s: %w", m.Alias, err)
	}
	return nil
}

func StopPebble(m *Machine) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := "docker rm -f " + PebbleContainer + " >/dev/null 2>&1 || true"
	if _, err := output(ctx, m, AsUser(m, CarameloUser, cmd)); err != nil {
		return fmt.Errorf("stop pebble on %s: %w", m.Alias, err)
	}
	return nil
}

func TrustOnBox(m *Machine, name, pem string) error {
	ctx, cancel := context.WithTimeout(context.Background(), pebbleTrustTimeout)
	defer cancel()
	enc := base64.StdEncoding.EncodeToString([]byte(pem))
	cmd := fmt.Sprintf("printf %%s %s | base64 -d > /usr/local/share/ca-certificates/%s.crt "+
		"&& update-ca-certificates >/dev/null 2>&1", enc, name)
	res, err := m.RunAsRoot(ctx, cmd)
	if err != nil {
		return fmt.Errorf("trust %s on %s: %w", name, m.Alias, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("trust %s on %s: exit %d: %s", name, m.Alias, res.ExitCode,
			strings.TrimSpace(res.Stderr+res.Stdout))
	}
	return nil
}

func output(ctx context.Context, m *Machine, cmd string) (string, error) {
	res, err := m.Run(ctx, cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr+res.Stdout))
	}
	return res.Stdout, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
