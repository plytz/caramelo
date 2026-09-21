//go:build integration

package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	setuppkg "github.com/plytz/caramelo/internal/setup"
	"github.com/plytz/caramelo/test/integration/itest"
)

var errSuiteAborted = errors.New("the setup suite aborted while starting")

type suiteTB struct {
	testing.TB

	mu       sync.Mutex
	cleanups []func()
	failed   bool
}

func (b *suiteTB) Helper() {}

func (b *suiteTB) Name() string { return "setup" }

func (b *suiteTB) Log(args ...any) { fmt.Fprintln(os.Stderr, args...) }

func (b *suiteTB) Logf(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }

func (b *suiteTB) Error(args ...any) {
	b.Log(args...)
	b.Fail()
}

func (b *suiteTB) Errorf(format string, args ...any) {
	b.Logf(format, args...)
	b.Fail()
}

func (b *suiteTB) Fatal(args ...any) {
	b.Log(args...)
	b.FailNow()
}

func (b *suiteTB) Fatalf(format string, args ...any) {
	b.Logf(format, args...)
	b.FailNow()
}

func (b *suiteTB) Fail() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failed = true
}

func (b *suiteTB) FailNow() { panic(errSuiteAborted) }

func (b *suiteTB) Failed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failed
}

func (b *suiteTB) Skip(args ...any) {
	b.Log(args...)
	b.SkipNow()
}

func (b *suiteTB) Skipf(format string, args ...any) {
	b.Logf(format, args...)
	b.SkipNow()
}

func (b *suiteTB) SkipNow() { panic(errSuiteAborted) }

func (b *suiteTB) Skipped() bool { return false }

func (b *suiteTB) TempDir() string {
	dir, err := os.MkdirTemp("", "caramelo-setup-suite")
	if err != nil {
		b.Fatalf("temporary directory: %v", err)
	}
	b.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func (b *suiteTB) Cleanup(fn func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cleanups = append(b.cleanups, fn)
}

func (b *suiteTB) runCleanups() {
	b.mu.Lock()
	fns := b.cleanups
	b.cleanups = nil
	b.mu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}

var (
	suiteT    *suiteTB
	lab       *itest.Lab
	machine   *itest.Machine
	commander *itest.Machine

	skipReason string
	startErr   error

	firstRun    setuppkg.Report
	firstRunRaw string
	firstRunErr error

	failures atomic.Int64
)

func TestMain(m *testing.M) { os.Exit(runSuite(m)) }

func runSuite(m *testing.M) int {
	suiteT = &suiteTB{}
	defer suiteT.runCleanups()
	startErr = start()
	return m.Run()
}

func start() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("starting the setup suite: %v", r)
		}
	}()

	opts := itest.Options{Suite: "setup", Roles: []string{itest.RoleHub}}
	if !itest.OverSSH() {
		opts.Commanders = []string{itest.RoleCommander}
	}
	lab = itest.New(suiteT, opts)
	machine = lab.Machine(itest.RoleHub)
	if !itest.OverSSH() {
		commander = lab.Commander(itest.RoleCommander)
	}

	ctx, cancel := context.WithTimeout(context.Background(), lab.Budget().Setup)
	defer cancel()

	bin, binErr := itest.BinaryFor(ctx, machine)
	if binErr != nil {
		skipReason = binErr.Error()
		fmt.Fprintf(os.Stderr, "setup suite: %v; tests will skip\n", binErr)
		return nil
	}
	if _, err := itest.EnsureGossFor(ctx, machine); err != nil {
		return err
	}
	if err := machine.Copy(ctx, bin, itest.RemoteBin); err != nil {
		return err
	}
	if res, err := machine.Run(ctx, "chmod +x "+itest.RemoteBin); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("chmod %s: exit %d: %s", itest.RemoteBin, res.ExitCode, res.Stderr)
	}

	firstRun, firstRunRaw, firstRunErr = runSetup(ctx, machine, itest.RemoteBin)
	if firstRunErr == nil {
		if err := itest.WaitForCaramelod(ctx, machine); err != nil {
			firstRunErr = fmt.Errorf("fleet setup finished but caramelod never came up: %w", err)
		} else if err := itest.Relogin(ctx, machine); err != nil {
			firstRunErr = err
		}
	}
	return nil
}

func authorizedKeys(m *itest.Machine) string { return itest.HomeOf(m) + "/.ssh/authorized_keys" }

func runSetup(ctx context.Context, m *itest.Machine, bin string) (setuppkg.Report, string, error) {
	return runSetupWith(ctx, m, bin)
}

func runSetupWith(ctx context.Context, m *itest.Machine, bin string, extra ...string) (setuppkg.Report, string, error) {
	cmd := strings.TrimSpace(fmt.Sprintf("sudo %s fleet setup --yes --json --authorized-keys %s %s",
		bin, authorizedKeys(m), strings.Join(extra, " ")))
	res, err := m.Run(ctx, cmd)
	raw := res.Stdout + "\n--- stderr ---\n" + res.Stderr
	if err != nil {
		return setuppkg.Report{}, raw, err
	}
	var report setuppkg.Report
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &report); jsonErr != nil {
		return report, raw, fmt.Errorf("fleet setup: exit %d, stdout is not a setup.Report: %w", res.ExitCode, jsonErr)
	}
	if res.ExitCode != 0 {
		return report, raw, fmt.Errorf("fleet setup: exit %d", res.ExitCode)
	}
	return report, raw, nil
}

func begin(t *testing.T) (*itest.Lab, *itest.Machine) {
	t.Helper()
	if startErr != nil {
		t.Fatalf("setup suite: %v", startErr)
	}
	if skipReason != "" {
		t.Skip("setup suite: " + skipReason)
	}
	t.Cleanup(func() {
		if t.Failed() {
			failures.Add(1)
		}
	})
	return lab, machine
}
