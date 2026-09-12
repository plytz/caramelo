//go:build integration

package fleet

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

const suiteName = "fleet"

var errSuiteAborted = errors.New("the fleet suite aborted while starting")

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
	dir, err := os.MkdirTemp("", "caramelo-fleet-suite")
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

var fleetImages = []string{itest.PostgresImage, itest.PythonImage, itest.PebbleImage}

var (
	suiteT *suiteTB
	lab    *itest.Lab
	hub    *itest.Machine

	skipReason string
	startErr   error

	memberBoxes []*itest.Machine
	memberNames []string

	home        string
	otherHome   string
	peerHome    string
	tmpDir      string
	machineAddr string
	tunnelAddr  string
	repo        string

	pebble       *itest.Pebble
	internet     *itest.EdgeClient
	memberPebble = map[string]*itest.Pebble{}
	memberRoots  = map[string][]string{}

	otherKey string

	joinedM1, joinedM2 bool
	remoteEnv          string
	firstCommit        string
	currentVersion     = "v1"
)

func TestMain(m *testing.M) { os.Exit(runSuite(m)) }

func runSuite(m *testing.M) int {
	suiteT = &suiteTB{}
	defer suiteT.runCleanups()
	startErr = start()
	if startErr != nil {
		fmt.Fprintf(os.Stderr, "fleet suite: %v\n", startErr)
	}
	return m.Run()
}

func start() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("starting the fleet suite: %v", r)
		}
	}()

	if _, binErr := itest.BinaryPathOr(); binErr != nil {
		skipReason = binErr.Error()
		fmt.Fprintf(os.Stderr, "fleet suite: %v; tests will skip\n", binErr)
		return nil
	}

	lab = itest.New(suiteT, itest.Options{
		Suite: suiteName,
		State: itest.StateClean,
		Roles: []string{itest.RoleHub, itest.RoleNode, itest.RoleNode},
	})
	if why := itest.MissingRoles(lab, itest.RoleHub, itest.RoleNode, itest.RoleNode); why != "" {
		return skipBecause(why)
	}
	hub = lab.Machine(itest.RoleHub)
	memberBoxes = lab.Nodes()
	memberNames = itest.NodeNames(len(memberBoxes))
	if err := itest.EnsureState(hub, itest.StateProvisioned); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(25*time.Minute))
	defer cancel()

	if err := itest.WaitForCaramelod(ctx, hub); err != nil {
		return err
	}
	if _, err := itest.EnsureGossFor(ctx, hub); err != nil {
		return err
	}
	if err := itest.DeployBinaryTo(hub); err != nil {
		return err
	}
	if err := itest.SeedImagesTo(hub, fleetImages...); err != nil {
		return err
	}
	if err := itest.EnsureCurlOn(hub); err != nil {
		return err
	}

	res, err := hub.Run(ctx, "systemctl cat "+csetup.EdgeSocketUnit+" >/dev/null 2>&1")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return skipBecause("the provisioned image has no edge (" + csetup.EdgeSocketUnit + " is not installed)")
	}
	res, err = hub.Run(ctx, "sudo test -s "+vaultKeyPath)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return skipBecause("the provisioned image has no vault key (" + vaultKeyPath + ")")
	}

	if pebble, err = itest.StartPebbleOn(hub); err != nil {
		return err
	}
	rememberRoot(hub.Alias, pebble.RootPEM)
	if err := itest.TrustOnBox(hub, itest.PebbleTrustName, pebble.MinicaPEM); err != nil {
		return err
	}
	if err := restartEdgeOn(ctx, hub); err != nil {
		return err
	}
	if internet, err = itest.NewEdgeClient(hub, rootFor(hub.Alias)); err != nil {
		return err
	}

	home = suiteT.TempDir()
	otherHome = suiteT.TempDir()
	peerHome = suiteT.TempDir()
	if machineAddr, err = hubClientAddress(); err != nil {
		return err
	}
	if err := writeHubHome(home, false); err != nil {
		return err
	}
	if err := writeHubHome(otherHome, false); err != nil {
		return err
	}
	if err := writeHubHome(peerHome, true); err != nil {
		return err
	}
	if _, err := hub.HostPortProto(itest.VPNPort, "udp"); err != nil {
		return err
	}
	tunnelAddr = hub.HostIP()
	for _, m := range memberBoxes {
		if err := itest.AddClientHost(home, m); err != nil {
			return err
		}
	}
	tmpDir = suiteT.TempDir()

	fmt.Fprintf(os.Stderr, "fleet suite: hub %s at %s, nodes %s, pebble %s\n",
		hub.Alias, machineAddr, strings.Join(memberAliases(), ", "), pebble.Directory)
	return nil
}

func memberAliases() []string {
	out := make([]string, 0, len(memberBoxes))
	for i, m := range memberBoxes {
		out = append(out, fmt.Sprintf("%s(%s)", memberNames[i], m.Alias))
	}
	return out
}

func skipBecause(reason string) error {
	skipReason = reason
	fmt.Fprintf(os.Stderr, "fleet suite: %s; tests will skip\n", reason)
	return nil
}

func restartEdgeOn(ctx context.Context, m *itest.Machine) error {
	res, err := m.Run(ctx, "sudo systemctl restart "+csetup.EdgeServiceUnit)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("restart %s on %s: exit %d: %s", csetup.EdgeServiceUnit, m.Alias, res.ExitCode,
			strings.TrimSpace(res.Stderr))
	}
	return waitForEdgeSocket(ctx, m, itest.Scale(60*time.Second))
}

func waitForEdgeSocket(ctx context.Context, m *itest.Machine, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for {
		if r, err := m.Run(ctx, "test -S "+edgeControlSocket); err == nil && r.ExitCode == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the edge's control socket on %s did not come back after a restart", m.Alias)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the edge's control socket on %s did not come back: %w", m.Alias, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}
