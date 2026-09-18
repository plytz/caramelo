package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func dirFile(t *testing.T, body string) *File {
	t.Helper()
	f, err := Parse([]byte(body), "x.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func runBuiltin(t *testing.T, body string, dryRun bool) Report {
	t.Helper()
	e := &Engine{Version: "0.0.1", DryRun: dryRun}
	rep := runFile(t, e, body)
	return rep
}

func dirItem(path, mode string) string {
	return `version: 1
name: x
items:
  - name: one
    dir:
      path: ` + path + `
      mode: '` + mode + `'
`
}

func TestADirBuiltinCreatesRepairsAndLeavesAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "made")
	body := dirItem(dir, "0700")

	rep := runBuiltin(t, body, false)
	res := resultOf(t, rep, "one")
	if res.Status != StatusChanged || res.Detail != "created, 0700" {
		t.Fatalf("first run = %+v, want changed", res)
	}
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("%s: %v %v", dir, fi, err)
	}

	if res := resultOf(t, runBuiltin(t, body, false), "one"); res.Status != StatusOK {
		t.Errorf("second run = %+v, want ok", res)
	}

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	res = resultOf(t, runBuiltin(t, body, false), "one")
	if res.Status != StatusChanged {
		t.Errorf("a wrong mode = %+v, want changed", res)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("%s was not repaired: %v %v", dir, fi, err)
	}
}

func TestADirBuiltinRefusesAPathThatIsNoDirectory(t *testing.T) {
	base := t.TempDir()
	plain := filepath.Join(base, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	toFile := filepath.Join(base, "to-file")
	if err := os.Symlink(plain, toFile); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, path, want string }{
		{"a file", plain, "is not a directory"},
		{"a symlink to a file", toFile, "is not a directory"},
		{"a symlink that leads nowhere", dangling, "leads nowhere"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := resultOf(t, runBuiltin(t, dirItem(tc.path, "0700"), false), "one")
			if res.Status != StatusFailed || !strings.Contains(res.Error, tc.want) {
				t.Errorf("result = %+v, want failed saying %q", res, tc.want)
			}
		})
	}
}

func TestADirBuiltinWorksThroughASymlinkToADirectory(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	body := dirItem(link, "0700")
	if res := resultOf(t, runBuiltin(t, body, false), "one"); res.Status != StatusChanged {
		t.Fatalf("result = %+v, want the mode repaired through the link", res)
	}
	if fi, err := os.Stat(target); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("%s: %v %v", target, fi, err)
	}
	if res := resultOf(t, runBuiltin(t, body, false), "one"); res.Status != StatusOK {
		t.Errorf("second run = %+v, want ok", res)
	}
}

func TestAModeWithASpecialBitIsRefusedByTheFileAndByTheBuiltin(t *testing.T) {
	_, err := Parse([]byte(dirItem("/srv/shared", "1777")), "x.yaml")
	if err == nil || !strings.Contains(err.Error(), "sticky") {
		t.Errorf("err = %v, want a refusal naming the bit it cannot set", err)
	}
	if _, err := modeOf("1777", 0o755); err == nil || !strings.Contains(err.Error(), "sticky") {
		t.Errorf("modeOf = %v, want a refusal naming the bit it cannot set", err)
	}
	if mode, err := modeOf("0700", 0o755); err != nil || mode != 0o700 {
		t.Errorf("modeOf(0700) = %v, %v", mode, err)
	}
}

func TestADirBuiltinUnderADryRunChangesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "made")
	res := resultOf(t, runBuiltin(t, dirItem(dir, "0700"), true), "one")
	if res.Status != StatusWouldChange {
		t.Errorf("result = %+v, want would-change", res)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("%s was created by a dry run", dir)
	}
}

func fileItem(path, mode, content string) string {
	return `version: 1
name: x
items:
  - name: one
    file:
      path: ` + path + `
      mode: '` + mode + `'
      content: ` + content + `
`
}

func TestAFileBuiltinWritesRepairsAndLeavesAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	body := fileItem(path, "0600", `"hello\n"`)

	res := resultOf(t, runBuiltin(t, body, false), "one")
	if res.Status != StatusChanged || res.Detail != "created, 0600" {
		t.Fatalf("first run = %+v", res)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "hello\n" {
		t.Fatalf("content = %q (%v)", b, err)
	}

	if res := resultOf(t, runBuiltin(t, body, false), "one"); res.Status != StatusOK {
		t.Errorf("second run = %+v, want ok", res)
	}

	if err := os.WriteFile(path, []byte("stale content that must go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := resultOf(t, runBuiltin(t, body, false), "one"); res.Status != StatusChanged {
		t.Errorf("a changed content = %+v, want changed", res)
	}
	b, err = os.ReadFile(path)
	if err != nil || strings.Contains(string(b), "stale") {
		t.Errorf("the old content survived: %q (%v)", b, err)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if res := resultOf(t, runBuiltin(t, body, false), "one"); res.Status != StatusChanged {
		t.Errorf("a wrong mode = %+v, want changed", res)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("%s was not repaired: %v %v", path, fi, err)
	}
}

func TestAFileBuiltinRefusesASymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	res := resultOf(t, runBuiltin(t, fileItem(link, "0600", "hi"), false), "one")
	if res.Status != StatusFailed || !strings.Contains(res.Error, "is a symlink") {
		t.Errorf("result = %+v, want failed naming the symlink", res)
	}
	if b, err := os.ReadFile(target); err != nil || len(b) != 0 {
		t.Errorf("the task wrote through the link: %q (%v)", b, err)
	}
}

func TestOwnerAndGroupAreSkippedWithANoteWhenNotRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the owner is set rather than skipped")
	}
	dir := filepath.Join(t.TempDir(), "made")
	res := resultOf(t, runBuiltin(t, `version: 1
name: x
items:
  - name: one
    dir:
      path: `+dir+`
      mode: '0700'
      owner: root
      group: root
`, false), "one")
	if res.Status != StatusChanged {
		t.Fatalf("result = %+v, want changed", res)
	}
	if !strings.Contains(res.Detail, notRootNote) {
		t.Errorf("detail = %q, want it to say the owner was left alone", res.Detail)
	}
}

func TestABuiltinNeverReachesTheRunner(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "made")
	f := dirFile(t, dirItem(dir, "0700"))
	e := &Engine{Version: "0.0.1"}
	rep, err := e.Execute(t.Context(), f)
	if err != nil {
		t.Fatalf("a built-in needed a runner: %v", err)
	}
	if res := resultOf(t, rep, "one"); res.Status != StatusChanged {
		t.Errorf("result = %+v", res)
	}
	if got := resultOf(t, rep, "one").Commands; len(got) != 0 {
		t.Errorf("a built-in recorded commands: %+v", got)
	}
}
