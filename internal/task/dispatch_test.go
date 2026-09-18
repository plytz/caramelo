package task

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/setup/testutil"
)

func TestABareCarameloArgvIsDispatchedInProcess(t *testing.T) {
	for _, tc := range []struct {
		script string
		argv   []string
	}{
		{`caramelo vpn keygen --out /x`, []string{"caramelo", "vpn", "keygen", "--out", "/x"}},
		{`caramelo commander write-config --path "/x/a b/config.yaml" --name laptop`,
			[]string{"caramelo", "commander", "write-config", "--path", "/x/a b/config.yaml", "--name", "laptop"}},
		{`  caramelo   context   `, []string{"caramelo", "context"}},
	} {
		t.Run(tc.script, func(t *testing.T) {
			argv, ok := InProcess(tc.script)
			if !ok {
				t.Fatalf("%q is not dispatched in process", tc.script)
			}
			if strings.Join(argv, "\x00") != strings.Join(tc.argv, "\x00") {
				t.Errorf("argv = %q, want %q", argv, tc.argv)
			}
		})
	}
}

func TestEverythingElseIsForked(t *testing.T) {
	for _, script := range []string{
		`sudo caramelo vpn keygen --out /x`,
		`cd /x && caramelo vpn keygen --out /x`,
		`caramelo context --json | tee /x`,
		`caramelo context > /x`,
		`caramelo context; caramelo version`,
		"caramelo context\ncaramelo version",
		`caramelo vpn keygen --out=/x`,
		`caramelo vpn keygen --out ~/x`,
		`caramelo vpn keygen --out $HOME/x`,
		`test -s /x`,
		`caramelo vpn keygen --out "/x`,
	} {
		t.Run(script, func(t *testing.T) {
			if argv, ok := InProcess(script); ok {
				t.Errorf("%q was dispatched in process as %q", script, argv)
			}
		})
	}
}

func TestTheDispatcherRunsTheLeafAndTheRunnerNeverSeesIt(t *testing.T) {
	f := testutil.New()
	var mu sync.Mutex
	var seen [][]string
	e := fakeEngine(f)
	e.Leaf = func(ctx context.Context, argv []string) (runner.Result, error) {
		mu.Lock()
		seen = append(seen, argv)
		mu.Unlock()
		return runner.Result{Stdout: "/x/identity.key created\n"}, nil
	}
	rep := runFile(t, e, `version: 1
name: x
items:
  - name: identity
    cmd: caramelo vpn keygen --out "/x/identity.key"
`)
	res := resultOf(t, rep, "identity")
	if res.Status != StatusChanged || res.Detail != "/x/identity.key created" {
		t.Errorf("result = %+v", res)
	}
	if len(seen) != 1 || seen[0][1] != "vpn" {
		t.Errorf("the leaf was called with %q", seen)
	}
	if len(f.Calls()) != 0 {
		t.Errorf("the shell runner saw a dispatched item: %s", f.Transcript())
	}
}

func TestAForkedCarameloItemGoesThroughTheRunner(t *testing.T) {
	f := testutil.New()
	e := fakeEngine(f)
	e.Leaf = func(ctx context.Context, argv []string) (runner.Result, error) {
		t.Errorf("a forked item reached the dispatcher as %q", argv)
		return runner.Result{}, nil
	}
	runFile(t, e, `version: 1
name: x
items:
  - name: identity
    cmd: sudo caramelo vpn keygen --out "/x/identity.key"
`)
	if !f.Ran(sh(`sudo caramelo vpn keygen --out "/x/identity.key"`)) {
		t.Errorf("the forked item did not reach the runner: %s", f.Transcript())
	}
}

func TestALeafThatRefusesFailsItsItemWithItsOwnOutput(t *testing.T) {
	f := testutil.New()
	e := fakeEngine(f)
	e.Leaf = func(ctx context.Context, argv []string) (runner.Result, error) {
		return runner.Result{ExitCode: 2, Stderr: "caramelo: vpn keygen runs on a commander\n"}, nil
	}
	rep := runFile(t, e, `version: 1
name: x
items:
  - name: identity
    cmd: caramelo vpn keygen --out "/x/identity.key"
`)
	res := resultOf(t, rep, "identity")
	if res.Status != StatusFailed || res.Error != "cmd exit 2" {
		t.Errorf("result = %+v, want failed with exit 2", res)
	}
	if got := res.Commands[0].Stderr; !strings.Contains(got, "runs on a commander") {
		t.Errorf("stderr = %q, want the refusal", got)
	}
}
