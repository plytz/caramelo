package cli

import (
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/setup"
)

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
