package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/place"
)

const availabilityGolden = "testdata/availability.golden"

func TestGoldenAvailability(t *testing.T) {
	got := availabilityTable(t)
	if *update {
		if err := os.MkdirAll(filepath.Dir(availabilityGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(availabilityGolden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(availabilityGolden)
	if err != nil {
		t.Fatalf("%v (run `go test ./internal/cli -run Availability -update` to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s moved: a conditional changed.\n--- got ---\n%s\n--- want ---\n%s",
			availabilityGolden, got, string(want))
	}
}

func availabilityTable(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	header := []string{"COMMAND", "APPROVED"}
	for _, name := range place.CanonicalNames() {
		header = append(header, strings.ToUpper(name))
	}
	header = append(header, "HOLDS ON")
	fmt.Fprintln(w, strings.Join(header, "\t"))

	for _, cmd := range leaves(conditionalRoot(t)) {
		when, ok := whenOf(cmd)
		if !ok {
			continue
		}
		row := []string{leafName(cmd), approvedFamilies(cmd)}
		for _, name := range place.CanonicalNames() {
			row = append(row, yesOrNo(when.ok(canonicalOf(t, name))))
		}
		fmt.Fprintln(w, strings.Join(append(row, when.text), "\t"))
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func approvedFamilies(cmd *cobra.Command) string {
	if isMachinery(cmd) {
		return "machinery"
	}
	name := leafName(cmd)
	var families []string
	if slices.Contains(approvedOnCommander, name) {
		families = append(families, "commander")
	}
	if slices.Contains(approvedOnServer, name) {
		families = append(families, "server")
	}
	if len(families) == 0 {
		return "-"
	}
	return strings.Join(families, "+")
}

func yesOrNo(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}
