package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"runtime"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	t.Logf("caramelo %s -> exit %d\nstdout: %q\nstderr: %q", strings.Join(args, " "), code, stdout.String(), stderr.String())
	return code, stdout.String(), stderr.String()
}

func TestVersionHuman(t *testing.T) {
	code, stdout, stderr := run(t, "version")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	for _, want := range []string{"caramelo " + version, runtime.Version(), runtime.GOOS + "/" + runtime.GOARCH} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
	if !strings.HasSuffix(stdout, "\n") || strings.Count(stdout, "\n") != 1 {
		t.Errorf("stdout = %q, want exactly one line", stdout)
	}
}

func TestVersionJSON(t *testing.T) {
	for _, args := range [][]string{{"version", "--json"}, {"--json", "version"}} {
		code, stdout, stderr := run(t, args...)
		if code != ExitOK {
			t.Fatalf("%v: exit code = %d, want %d", args, code, ExitOK)
		}
		if stderr != "" {
			t.Errorf("%v: stderr = %q, want empty", args, stderr)
		}
		if !strings.HasSuffix(stdout, "\n") || strings.Count(stdout, "\n") != 1 {
			t.Errorf("%v: stdout = %q, want exactly one JSON line", args, stdout)
		}

		var got map[string]any
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("%v: stdout is not a JSON object: %v\n%s", args, err, stdout)
		}
		want := map[string]any{
			"version": version,
			"go":      runtime.Version(),
			"os":      runtime.GOOS,
			"arch":    runtime.GOARCH,
		}
		if len(got) != len(want) {
			t.Errorf("%v: got keys %v, want %v", args, got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%v: %s = %q, want %q", args, k, got[k], v)
			}
		}
	}
}

func TestUnknownCommandIsUsageError(t *testing.T) {
	code, stdout, stderr := run(t, "bogus")
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty (diagnostics belong on stderr)", stdout)
	}
	if !strings.Contains(stderr, `unknown command "bogus"`) {
		t.Errorf("stderr = %q, want it to mention the unknown command", stderr)
	}
}

func TestUnknownFlagIsUsageError(t *testing.T) {
	code, stdout, stderr := run(t, "version", "--bogus")
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "bogus") {
		t.Errorf("stderr = %q, want it to mention the flag", stderr)
	}
}

func TestExtraArgsIsUsageError(t *testing.T) {
	code, _, _ := run(t, "version", "extra")
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
}

func TestRootPrintsHelp(t *testing.T) {
	commanderPlace(t)
	code, stdout, _ := run(t)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(stdout, "version") || !strings.Contains(stdout, "--json") {
		t.Errorf("help output = %q, want it to list the version command and --json flag", stdout)
	}
	if strings.Contains(stdout, "completion") {
		t.Errorf("help output lists the completion command; it should be disabled:\n%s", stdout)
	}
	if !strings.HasPrefix(stdout, "laptop, commander · ") {
		t.Errorf("bare caramelo does not open with the header of this place:\n%s", stdout)
	}
}

func TestPrinterResult(t *testing.T) {
	var out, errw bytes.Buffer
	p := &Printer{Out: &out, Err: &errw, JSON: true}
	p.Infof("working on %s", "it")
	if err := p.Result(map[string]int{"n": 1}, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != "{\"n\":1}\n" {
		t.Errorf("JSON out = %q", out.String())
	}
	if errw.String() != "working on it\n" {
		t.Errorf("stderr = %q", errw.String())
	}

	out.Reset()
	p.JSON = false
	if err := p.Result(nil, func(w io.Writer) error { _, err := io.WriteString(w, "hi\n"); return err }); err != nil {
		t.Fatal(err)
	}
	if out.String() != "hi\n" {
		t.Errorf("human out = %q", out.String())
	}
}
