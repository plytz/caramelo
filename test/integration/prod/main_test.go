//go:build integration

package prod

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

const suiteName = "prod"

var errSuiteAborted = errors.New("the prod suite aborted while starting")

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
	dir, err := os.MkdirTemp("", "caramelo-prod-suite")
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
	suiteT *suiteTB
	lab    *itest.Lab
	box    *itest.Machine

	skipReason string
	startErr   error

	home        string
	tmpDir      string
	machineAddr string
	repo        string

	pebble    *itest.Pebble
	internet  *itest.EdgeClient
	edgeRoots []string

	firstCommit    string
	currentVersion = "v1"

	firstRelease, secondRelease string
)

func TestMain(m *testing.M) { os.Exit(runSuite(m)) }

func runSuite(m *testing.M) int {
	suiteT = &suiteTB{}
	defer suiteT.runCleanups()
	startErr = start()
	if startErr != nil {
		fmt.Fprintf(os.Stderr, "prod suite: %v\n", startErr)
	}
	return m.Run()
}

func start() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("starting the prod suite: %v", r)
		}
	}()

	if _, binErr := itest.BinaryPathOr(); binErr != nil {
		skipReason = binErr.Error()
		fmt.Fprintf(os.Stderr, "prod suite: %v; tests will skip\n", binErr)
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
	if err := itest.SeedImagesTo(box, itest.ProdImages...); err != nil {
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
		skipReason = "the provisioned image has no edge (" + csetup.EdgeSocketUnit + " is not installed)"
		fmt.Fprintf(os.Stderr, "prod suite: %s; tests will skip\n", skipReason)
		return nil
	}
	res, err = box.Run(ctx, "sudo test -s "+vaultKeyPath)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		skipReason = "the provisioned image has no vault key (" + vaultKeyPath + ")"
		fmt.Fprintf(os.Stderr, "prod suite: %s; tests will skip\n", skipReason)
		return nil
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

	edgeRoots = []string{pebble.RootPEM}
	if internet, err = itest.NewEdgeClient(box, edgeRoots...); err != nil {
		return err
	}

	machineAddr = box.CommanderMachine(suiteT)
	home = suiteT.TempDir()
	if err := itest.WriteCommanderHomeNoPeer(home, box); err != nil {
		return err
	}
	tmpDir = suiteT.TempDir()
	fmt.Fprintf(os.Stderr, "prod suite: machine %s, pebble %s\n", machineAddr, pebble.Directory)
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
	return waitForEdgeSocket(ctx, m, itest.Scale(30*time.Second))
}

func waitForEdgeSocket(ctx context.Context, m *itest.Machine, budget time.Duration) error {
	deadline := time.Now().Add(budget)
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

func refreshEdgeTrust() error {
	if pebble == nil || box == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
	defer cancel()
	ready := fmt.Sprintf("curl -sk --max-time 5 https://127.0.0.1:%d/dir >/dev/null", itest.PebbleACMEPort)
	if err := box.WaitFor(ctx, ready, time.Second); err != nil {
		return fmt.Errorf("pebble never came back after a power cycle: %w", err)
	}
	if err := pebble.Refresh(box); err != nil {
		return fmt.Errorf("re-read pebble's issuing root after a power cycle: %w", err)
	}
	for _, have := range edgeRoots {
		if have == pebble.RootPEM {
			return nil
		}
	}
	edgeRoots = append(edgeRoots, pebble.RootPEM)
	client, err := itest.NewEdgeClient(box, edgeRoots...)
	if err != nil {
		return fmt.Errorf("trust pebble's new issuing root: %w", err)
	}
	internet = client
	return nil
}
