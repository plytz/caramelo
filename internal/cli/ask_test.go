package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestConfirmWithNobodyThereSaysWhatToPass(t *testing.T) {
	a := &app{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	if _, err := a.confirm(context.Background(), "this changes the machine.\n"); !errors.Is(err, errNeedsYes) {
		t.Errorf("confirm = %v, want errNeedsYes", err)
	}
}

func TestConfirmNameWithNobodyThereReturnsTheCallersError(t *testing.T) {
	a := &app{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	needs := errors.New("destroying shop/production needs --force")
	ok, err := a.confirmName(context.Background(), needs, "", `Type "production" to confirm: `, "production")
	if ok {
		t.Error("confirmName said yes with nobody there")
	}
	if !errors.Is(err, needs) {
		t.Errorf("confirmName = %v, want the caller's error", err)
	}
}

func TestSecretValueWithNoTerminalIsAUsageError(t *testing.T) {
	a := &app{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	value, err := a.secretValue(context.Background(), "STRIPE_KEY")
	if value != "" {
		t.Errorf("secretValue = %q, want nothing", value)
	}
	var ue *usageError
	if !errors.As(err, &ue) {
		t.Fatalf("secretValue = %v, want a usage error", err)
	}
	for _, want := range []string{"STRIPE_KEY=VALUE", "--from-file", "terminal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestDestroyOfAProtectedEnvAsksForTheName(t *testing.T) {
	initializedCommander(t)
	fwd := &fakeForward{}
	fwd.install(t)
	code, _, stderr := run(t, "env", "destroy", "production", "--app", "shop", "--force")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "needs confirmation") {
		t.Errorf("stderr = %q, want it to say how to confirm", stderr)
	}
	if fwd.called {
		t.Error("the invocation was forwarded without an answer")
	}
}
