package ui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/progress"
)

func at(sec int) time.Time {
	return time.Date(2026, 9, 10, 12, 0, sec, 0, time.UTC)
}

func TestFeedLineIsOneLinePerEvent(t *testing.T) {
	line := FeedLine(progress.Event{
		App: "shop", Env: "production", Service: "web", Replica: 2,
		Action: "rollout", Step: "flip", Status: progress.StatusOK,
		Detail: "active", Identity: "ci", At: at(5),
	})
	for _, want := range []string{"shop/production", "ok", "rollout flip web-2", "active", "(ci)"} {
		if !strings.Contains(line, want) {
			t.Errorf("feed line %q is missing %q", line, want)
		}
	}
	if strings.Contains(line, "\n") {
		t.Errorf("feed line has a newline in it: %q", line)
	}

	if got := FeedLine(progress.Event{Action: "health", Status: progress.StatusOK, At: at(5)}); !strings.Contains(got, "machine") {
		t.Errorf("feed line = %q, want it to name the machine", got)
	}
}

func TestConfirmPrintsThePlanAndThePrompt(t *testing.T) {
	var out strings.Builder
	q := Question{In: strings.NewReader("y\n"), Out: &out}
	ok, err := q.Confirm("this changes the machine.\n")
	if err != nil || !ok {
		t.Fatalf("Confirm = %v, %v", ok, err)
	}
	if want := "this changes the machine.\nProceed? [y/N] "; out.String() != want {
		t.Errorf("prompt = %q, want %q", out.String(), want)
	}
}

func TestConfirmAnswers(t *testing.T) {
	for _, tc := range []struct {
		answer string
		yes    bool
	}{{"y\n", true}, {"Y\n", true}, {"yes\n", true}, {"n\n", false}, {"\n", false}, {"maybe\n", false}} {
		var out strings.Builder
		ok, err := Question{In: strings.NewReader(tc.answer), Out: &out}.Confirm("")
		if err != nil {
			t.Fatalf("%q: %v", tc.answer, err)
		}
		if ok != tc.yes {
			t.Errorf("Confirm(%q) = %v, want %v", tc.answer, ok, tc.yes)
		}
	}
}

func TestQuestionWithNobodyThereIsNotARefusal(t *testing.T) {
	if _, err := (Question{In: strings.NewReader(""), Out: &strings.Builder{}}).Confirm(""); err != ErrNoTerminal {
		t.Errorf("Confirm at EOF = %v, want ErrNoTerminal", err)
	}
	if _, err := (Question{Out: &strings.Builder{}}).Confirm(""); err != ErrNoTerminal {
		t.Errorf("Confirm with no reader = %v, want ErrNoTerminal", err)
	}
	if _, err := (Question{Out: &strings.Builder{}}).Secret("Value for STRIPE_KEY"); err != ErrNoTerminal {
		t.Errorf("Secret with no reader = %v, want ErrNoTerminal", err)
	}
}

func TestConfirmNameNeedsTheExactWord(t *testing.T) {
	for _, tc := range []struct {
		typed string
		match bool
	}{{"production\n", true}, {"  production  \n", true}, {"Production\n", false}, {"prod\n", false}} {
		var out strings.Builder
		ok, err := Question{In: strings.NewReader(tc.typed), Out: &out}.
			ConfirmName("destroy shop/production.\n", "Type \"production\" to confirm: ", "production")
		if err != nil {
			t.Fatalf("%q: %v", tc.typed, err)
		}
		if ok != tc.match {
			t.Errorf("ConfirmName(%q) = %v, want %v", tc.typed, ok, tc.match)
		}
		if !strings.HasPrefix(out.String(), "destroy shop/production.\n") {
			t.Errorf("prompt = %q, want the plan first", out.String())
		}
	}
}

func TestSecretReadsOneLine(t *testing.T) {
	var out strings.Builder
	got, err := Question{In: strings.NewReader("sk_live_123\n"), Out: &out}.Secret("Value for STRIPE_KEY")
	if err != nil {
		t.Fatalf("Secret = %v", err)
	}
	if got != "sk_live_123" {
		t.Errorf("secret = %q", got)
	}
	if want := "Value for STRIPE_KEY: "; out.String() != want {
		t.Errorf("prompt = %q, want %q", out.String(), want)
	}
}

func TestSecretWithNothingTypedIsNotAValue(t *testing.T) {
	var out strings.Builder
	got, err := Question{In: strings.NewReader("\n"), Out: &out}.Secret("Value for STRIPE_KEY")
	if !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("Secret() = %q, %v; want ErrNoAnswer", got, err)
	}
	if got != "" {
		t.Errorf("secret = %q, want nothing", got)
	}
}

func TestSecretAtEOFIsNoTerminal(t *testing.T) {
	var out strings.Builder
	got, err := Question{In: strings.NewReader(""), Out: &out}.Secret("Value for STRIPE_KEY")
	if !errors.Is(err, ErrNoTerminal) {
		t.Fatalf("Secret() = %q, %v; want ErrNoTerminal", got, err)
	}
}
