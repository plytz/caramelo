//go:build integration

package edge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	csetup "github.com/plytz/caramelo/internal/setup"
	"github.com/plytz/caramelo/test/integration/itest"
)

const suiteName = "edge"

var errSuiteAborted = errors.New("the edge suite aborted while starting")

type suiteTB struct {
	testing.TB

	mu       sync.Mutex
	cleanups []func()
	failed   bool
}

func (b *suiteTB) Helper() {}

func (b *suiteTB) Name() string { return suiteName }

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
	dir, err := os.MkdirTemp("", "caramelo-edge-suite")
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

var suiteImages = []string{itest.PythonImage, itest.PebbleImage}

var (
	suiteT *suiteTB
	lab    *itest.Lab
	box    *itest.Machine

	skipReason string
	startErr   error

	home        string
	tmpDir      string
	machineAddr string
	repo        string

	pebble   *itest.Pebble
	internet *itest.EdgeClient

	firstCommit    string
	currentVersion = "v1"
)

func TestMain(m *testing.M) { os.Exit(runSuite(m)) }

func runSuite(m *testing.M) int {
	suiteT = &suiteTB{}
	defer suiteT.runCleanups()
	startErr = start()
	if startErr != nil {
		fmt.Fprintf(os.Stderr, "edge suite: %v\n", startErr)
	}
	return m.Run()
}

func start() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("starting the edge suite: %v", r)
		}
	}()

	if _, binErr := itest.BinaryPathOr(); binErr != nil {
		skipReason = binErr.Error()
		fmt.Fprintf(os.Stderr, "edge suite: %v; tests will skip\n", binErr)
		return nil
	}

	lab = itest.New(suiteT, itest.Options{Suite: suiteName, State: itest.StateProvisioned})
	box = lab.Machine(itest.RoleHub)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(20*time.Minute))
	defer cancel()

	if err := itest.WaitForCaramelod(ctx, box); err != nil {
		return err
	}
	if _, err := itest.EnsureGossFor(ctx, box); err != nil {
		return err
	}
	if err := itest.DeployBinaryTo(box); err != nil {
		return err
	}
	if err := itest.SeedImagesTo(box, suiteImages...); err != nil {
		return err
	}
	if err := itest.EnsureCurlOn(box); err != nil {
		return err
	}

	res, err := box.Run(ctx, "systemctl cat "+csetup.EdgeSocketUnit+" >/dev/null 2>&1")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s is not installed on %s: every tier sets this machine up with --edge, "+
			"so a machine without the edge is a failure and not a reason to skip",
			csetup.EdgeSocketUnit, box.Alias)
	}

	if pebble, err = itest.StartPebbleOn(box); err != nil {
		return err
	}
	if err := itest.TrustOnBox(box, itest.PebbleTrustName, pebble.MinicaPEM); err != nil {
		return err
	}
	if err := restartEdgeOn(ctx, box); err != nil {
		return err
	}

	if internet, err = itest.NewEdgeClient(box, pebble.RootPEM); err != nil {
		return err
	}

	machineAddr = box.ClientMachine(suiteT)
	home = suiteT.TempDir()
	if err := itest.WriteClientHomeNoPeer(home, box); err != nil {
		return err
	}
	tmpDir = suiteT.TempDir()
	fmt.Fprintf(os.Stderr, "edge suite: machine %s, pebble %s\n", machineAddr, pebble.Directory)
	return nil
}

func restartEdgeOn(ctx context.Context, m *itest.Machine) error {
	res, err := m.Run(ctx, "sudo systemctl restart "+csetup.EdgeServiceUnit)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("restart %s: exit %d: %s", csetup.EdgeServiceUnit, res.ExitCode,
			strings.TrimSpace(res.Stderr))
	}
	deadline := time.Now().Add(itest.Scale(30 * time.Second))
	for {
		if r, err := m.Run(ctx, "test -S "+edgeControlSocket); err == nil && r.ExitCode == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the edge's control socket did not come back after a restart")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the edge's control socket did not come back after a restart: %w", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}
