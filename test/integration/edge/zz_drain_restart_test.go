//go:build integration

package edge

import (
	"context"
	"strings"
	"testing"
	"time"

	cedge "github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/test/integration/itest"
)

func restartDaemon(t *testing.T, m *itest.Machine) {
	t.Helper()
	cmd := itest.AsUserSession(m, itest.CarameloUser, "systemctl --user restart caramelod")
	if res := onBox(t, m, cmd); res.ExitCode != 0 {
		t.Fatalf("restart caramelod: exit %d\nstdout:%s\nstderr:%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
	defer cancel()
	if err := itest.WaitForCaramelod(ctx, m); err != nil {
		t.Fatalf("caramelod did not come back after a restart: %v", err)
	}
}

func edgeStatusWhenAnswering(t *testing.T, within time.Duration) cedge.Status {
	t.Helper()
	deadline := time.Now().Add(within)
	last := "it never ran"
	for {
		res, err := clientExec(clientOpts{Dir: repo}, "edge", "status", "--json")
		switch {
		case err != nil:
			last = err.Error()
		case res.ExitCode != 0:
			last = strings.TrimSpace(res.Stderr)
		default:
			return decode[cedge.Status](t, "edge status", res.Stdout)
		}
		if time.Now().After(deadline) {
			t.Fatalf("edge status never answered after the daemon restarted: %s", last)
		}
		time.Sleep(itest.Scale(time.Second))
	}
}

func TestZZDrainSurvivesADaemonRestart(t *testing.T) {
	m := begin(t)
	needRepo(t)

	before := edgeStatus(t)
	if got := routeOf(t, "edge status", before.Routes, hostX).Drain; got != drainWindow {
		t.Fatalf("route %s drain = %s before the restart, want the configured %s", hostX, got, drainWindow)
	}

	restartDaemon(t, m)

	var after cedge.Status
	deadline := time.Now().Add(itest.Scale(3 * time.Minute))
	for {
		after = edgeStatusWhenAnswering(t, itest.Scale(2*time.Minute))
		if after.TableUpdatedAt.After(before.TableUpdatedAt) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the restarted daemon never pushed a table of its own (still stamped %s): "+
				"the drain below would be the one the old daemon left", after.TableUpdatedAt)
		}
		time.Sleep(itest.Scale(2 * time.Second))
	}
	if got := routeOf(t, "edge status", after.Routes, hostX).Drain; got != drainWindow {
		t.Fatalf("route %s drain = %s after caramelod restarted, want the configured %s", hostX, got, drainWindow)
	}
}
