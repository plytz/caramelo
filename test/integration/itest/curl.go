//go:build integration

package itest

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

const curlProbe = "command -v curl >/dev/null 2>&1"

const curlInstallTimeout = 5 * time.Minute

const curlInstall = "sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends curl || " +
	"{ sudo apt-get update && sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends curl; }"

func EnsureCurl(t testing.TB, m *Machine) {
	t.Helper()
	if err := EnsureCurlOn(m); err != nil {
		t.Fatalf("itest: %v", err)
	}
}

func EnsureCurlOn(m *Machine) error {
	ctx, cancel := context.WithTimeout(context.Background(), curlInstallTimeout)
	defer cancel()

	res, err := m.Run(ctx, curlProbe)
	if err != nil {
		return fmt.Errorf("look for curl on %s: %w", m.Alias, err)
	}
	if res.ExitCode == 0 {
		return nil
	}
	fmt.Fprintf(os.Stderr, "itest: no curl on %s; installing it (the image normally carries it)\n", m.Name)
	if err := mustSucceedOn(ctx, m, curlInstall); err != nil {
		return fmt.Errorf("install curl on %s: %w", m.Alias, err)
	}
	return nil
}
