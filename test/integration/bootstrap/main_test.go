//go:build integration

package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	setuppkg "github.com/plytz/caramelo/internal/setup"
	"github.com/plytz/caramelo/test/integration/itest"
)

var errSuiteAborted = errors.New("the bootstrap suite aborted while starting")

type suiteTB struct {
	testing.TB

	mu       sync.Mutex
	cleanups []func()
	failed   bool
}

func (b *suiteTB) Helper() {}

func (b *suiteTB) Name() string { return "bootstrap" }

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
	dir, err := os.MkdirTemp("", "caramelo-bootstrap-suite")
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

const machineName = "lab"

const commanderBin = itest.RemoteBin

type result struct {
	Target string `json:"target"`
	Probe  struct {
		OS        string `json:"os"`
		Arch      string `json:"arch"`
		Privilege string `json:"privilege"`
	} `json:"probe"`
	Setup   setuppkg.Report `json:"setup"`
	Machine *struct {
		Name    string `json:"name"`
		Address string `json:"address"`
		Default bool   `json:"default"`
		Config  string `json:"config"`
	} `json:"machine"`
	Verified bool            `json:"verified"`
	Status   json.RawMessage `json:"status"`
}

var (
	suiteT    *suiteTB
	lab       *itest.Lab
	machine   *itest.Machine
	commander *itest.Machine

	boxArch             string
	commanderHome       string
	commanderConfigPath string
	peerKeyDir          string
	skipReason          string
	startErr            error

	first    result
	firstRaw string
	firstErr error
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
			err = fmt.Errorf("starting the bootstrap suite: %v", r)
		}
	}()

	lab = itest.New(suiteT, itest.Options{
		Suite:      "bootstrap",
		Roles:      []string{itest.RoleHub},
		Commanders: []string{itest.RoleCommander},
	})
	machine = lab.Machine(itest.RoleHub)
	commander = lab.Commander(itest.RoleCommander)
	commanderHome = commander.Home()
	commanderConfigPath = commanderHome + "/.config/caramelo/config.yaml"
	peerKeyDir = commanderHome + "/.config/caramelo/vpn"

	ctx, cancel := context.WithTimeout(context.Background(), lab.Budget().Setup)
	defer cancel()

	if boxArch, err = machine.Arch(ctx); err != nil {
		return err
	}
	if _, err := itest.EnsureGossFor(ctx, machine); err != nil {
		return err
	}
	if _, err := itest.InstallCommanderBinaries(ctx, commander, machine); err != nil {
		if errors.Is(err, itest.ErrNoBinary) {
			skipReason = err.Error()
			fmt.Fprintf(os.Stderr, "bootstrap suite: %v; tests will skip\n", err)
			return nil
		}
		return err
	}

	first, firstRaw, firstErr = bootstrap(ctx, "--name", machineName)
	return nil
}

func bootstrap(ctx context.Context, extra ...string) (result, string, error) {
	target, err := itest.SSHTarget(ctx, machine)
	if err != nil {
		return result{}, "", err
	}
	args := append([]string{commanderBin, "server", "setup", "--target", target, "--yes", "--json"}, extra...)
	res, err := commander.Run(ctx, strings.Join(args, " "))
	raw := res.Stdout + "\n--- stderr ---\n" + res.Stderr
	if err != nil {
		return result{}, raw, fmt.Errorf("server setup --target %s: %w", target, err)
	}
	var out result
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &out); jsonErr != nil {
		return out, raw, fmt.Errorf("server setup --target: exit %d; stdout is not a bootstrap result: %w",
			res.ExitCode, jsonErr)
	}
	if res.ExitCode != 0 {
		return out, raw, fmt.Errorf("server setup --target: exit %d", res.ExitCode)
	}
	return out, raw, nil
}

func begin(t *testing.T) *itest.Machine {
	t.Helper()
	if startErr != nil {
		t.Fatalf("bootstrap suite: %v", startErr)
	}
	if skipReason != "" {
		t.Skip("bootstrap suite: " + skipReason)
	}
	return machine
}
