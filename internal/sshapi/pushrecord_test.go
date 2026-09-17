package sshapi

import (
	"strings"
	"testing"
)

const (
	oldSHA = "fedcba9876543210fedcba9876543210fedcba98"
	newSHA = "0123456789abcdef0123456789abcdef01234567"
)

func refLine(ref string) string { return "ref " + oldSHA + " " + newSHA + " " + ref + "\n" }

func TestParsePushRecordKeepsOnlyTheBranchesThatLanded(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		refs  []pushedRef
		since string
	}{
		{"one branch", refLine("refs/heads/feat-x"), []pushedRef{{"feat-x", newSHA}}, ""},
		{"two branches", refLine("refs/heads/feat-x") + refLine("refs/heads/main"),
			[]pushedRef{{"feat-x", newSHA}, {"main", newSHA}}, ""},
		{"a delete", "ref " + oldSHA + " " + zeroSHA + " refs/heads/feat-x\n", nil, ""},
		{"a tag", refLine("refs/tags/v1"), nil, ""},
		{"a note", refLine("refs/notes/commits"), nil, ""},
		{"the bare heads ref", refLine("refs/heads/"), nil, ""},
		{"a short sha", "ref " + oldSHA + " abc refs/heads/feat-x\n", nil, ""},
		{"an upper-case sha", "ref " + oldSHA + " " + strings.ToUpper(newSHA) + " refs/heads/feat-x\n", nil, ""},
		{"a line git never wrote", "ref refs/heads/feat-x\n", nil, ""},
		{"a line with a fifth field", "ref " + oldSHA + " " + newSHA + " refs/heads/feat-x extra\n", nil, ""},
		{"nothing at all", "", nil, ""},
		{"an option and a branch", "option caramelo.branch=blob-store\n" + refLine("refs/heads/feat-x"),
			[]pushedRef{{"feat-x", newSHA}}, "blob-store"},
		{"the last option wins", "option caramelo.branch=first\noption caramelo.branch=second\n" +
			refLine("refs/heads/feat-x"), []pushedRef{{"feat-x", newSHA}}, "second"},
		{"another tool's option", "option ci.skip=true\n" + refLine("refs/heads/feat-x"),
			[]pushedRef{{"feat-x", newSHA}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePushRecord([]byte(tc.in))
			if len(got.Refs) != len(tc.refs) {
				t.Fatalf("refs = %+v, want %+v", got.Refs, tc.refs)
			}
			for i, want := range tc.refs {
				if got.Refs[i] != want {
					t.Errorf("refs[%d] = %+v, want %+v", i, got.Refs[i], want)
				}
			}
			if got.Source != tc.since {
				t.Errorf("source = %q, want %q", got.Source, tc.since)
			}
		})
	}
}

func TestParsePushRecordDropsALabelItCannotTrust(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{"a plain branch", "blob-store", "blob-store"},
		{"a namespaced branch", "alex/blob-store", "alex/blob-store"},
		{"surrounding space", "  blob-store  ", "blob-store"},
		{"a space inside", "blob store", ""},
		{"a leading dash", "-oProxyCommand=id", ""},
		{"a parent path", "../../etc/passwd", ""},
		{"a semicolon", "blob;id", ""},
		{"a control character", "blob\x01store", ""},
		{"an absurd length", strings.Repeat("b", 256), ""},
		{"the longest we take", strings.Repeat("b", 255), strings.Repeat("b", 255)},
		{"nothing", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := "option caramelo.branch=" + tc.value + "\n" + refLine("refs/heads/feat-x")
			got := parsePushRecord([]byte(in))
			if got.Source != tc.want {
				t.Errorf("source = %q, want %q", got.Source, tc.want)
			}
			if len(got.Refs) != 1 {
				t.Errorf("a junk label cost us the ref: %+v", got.Refs)
			}
		})
	}
}
