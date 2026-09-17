package cli

import (
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/setup"
)

func TestMachineAddIsRegisteredWithItsFlags(t *testing.T) {
	code, stdout, _ := run(t, "machine", "add", "--help")
	if code != ExitOK {
		t.Fatalf("exit code = %d", code)
	}
	for _, flag := range []string{
		"--name", "--edge", "--private", "--binary", "--release", "--acme-email", "--acme-ca", "--tls",
	} {
		if !strings.Contains(stdout, flag) {
			t.Errorf("machine add has no %s flag:\n%s", flag, stdout)
		}
	}
}

func TestAJoinThatWasSkippedIsDiagnosedAsSuch(t *testing.T) {
	skipped := bootstrapResult{Setup: setup.Report{Results: []setup.Result{
		{Step: setup.JoinStepName, Status: setup.StatusOK},
	}}}
	ran := bootstrapResult{Setup: setup.Report{Results: []setup.Result{
		{Step: setup.JoinStepName, Status: setup.StatusChanged},
	}}}

	if !joinSkipped(skipped) {
		t.Error("a join step that reported ok is a join that did not run")
	}
	if joinSkipped(ran) {
		t.Error("a join step that changed something did run")
	}
	if joinSkipped(bootstrapResult{}) {
		t.Error("a report with no join step at all is not a skipped join")
	}
}

func TestTheJoinTicketIsLiftedOutOfTheForwardedFlags(t *testing.T) {
	const ticket = "caramelo-join-v1.c3VwZXItc2VjcmV0"
	args, token := liftJoinToken([]string{"--edge", "--join-token=" + ticket, "--join-name=m1"})
	if token != ticket {
		t.Fatalf("token = %q, want the ticket", token)
	}
	if got := strings.Join(args, " "); got != "--edge --join-name=m1" {
		t.Fatalf("args = %q, want the ticket gone from them", got)
	}

	args, token = liftJoinToken([]string{"--join-token=-"})
	if token != "" || strings.Join(args, " ") != "--join-token=-" {
		t.Fatalf("liftJoinToken(-) = %q, %q; want it left where it is", args, token)
	}
}
