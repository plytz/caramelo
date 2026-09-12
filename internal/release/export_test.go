package release

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTar(t *testing.T, entries []*tar.Header, bodies map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for _, h := range entries {
		body := bodies[h.Name]
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractRefusesNamesThatLeaveTheExport(t *testing.T) {
	for _, tc := range []struct {
		name, entry, want string
	}{
		{"a climbing path", "../escaped.txt", "climbs out"},
		{"a deeper climb", "a/../../escaped.txt", "climbs out"},
		{"an absolute path", "/etc/cron.d/evil", "absolute"},
		{"nothing at all", ".", "names no file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := writeTar(t,
				[]*tar.Header{{Name: tc.entry, Typeflag: tar.TypeReg, Mode: 0o644}},
				map[string]string{tc.entry: "x"})
			dir := t.TempDir()
			err := extract(archive, dir)
			if err == nil {
				t.Fatalf("extract accepted %q", tc.entry)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("extract = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestExtractSymlinks(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	archive := writeTar(t, []*tar.Header{
		{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader},
		{Name: "lib/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "lib/real.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "inside", Typeflag: tar.TypeSymlink, Linkname: "lib/real.txt"},
		{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: outside},
		{Name: "climb", Typeflag: tar.TypeSymlink, Linkname: "../../etc"},

		{Name: "escape/planted.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "bin/run", Typeflag: tar.TypeReg, Mode: 0o755},

		{Name: "dev/null", Typeflag: tar.TypeChar, Mode: 0o666},
	}, map[string]string{"lib/real.txt": "hello", "escape/planted.txt": "planted", "bin/run": "#!/bin/sh\n"})

	if err := extract(archive, dir); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(dir, "inside")); err != nil || target != "lib/real.txt" {
		t.Errorf("the inside symlink is %q, %v", target, err)
	}
	for _, gone := range []string{"climb", filepath.Join("dev", "null")} {
		if _, err := os.Lstat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s was unpacked", gone)
		}
	}

	if info, err := os.Lstat(filepath.Join(dir, "escape")); err != nil {
		t.Errorf("escape/: %v", err)
	} else if info.Mode()&os.ModeSymlink != 0 {
		t.Error("the escaping symlink was created")
	}

	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Errorf("%s holds %d entries, want none", outside, len(entries))
	}
	if _, err := os.Stat(filepath.Join(dir, "escape", "planted.txt")); err != nil {
		t.Errorf("the planted file is nowhere at all: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "bin", "run"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("bin/run is %v, want it executable", info.Mode().Perm())
	}
	if plain, err := os.Stat(filepath.Join(dir, "lib", "real.txt")); err != nil {
		t.Fatal(err)
	} else if plain.Mode().Perm()&0o111 != 0 {
		t.Errorf("lib/real.txt is %v, want it not executable", plain.Mode().Perm())
	}
}

func TestExtractRefusesTwoEntriesForOnePath(t *testing.T) {
	archive := writeTar(t, []*tar.Header{
		{Name: "app.js", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "app.js", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"app.js": "x"})
	if err := extract(archive, t.TempDir()); err == nil {
		t.Error("a tar with two entries for one path was unpacked")
	}
}

func TestBuildDirStaysInsideTheCommit(t *testing.T) {
	export := t.TempDir()
	mkdir := func(parts ...string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(append([]string{export}, parts...)...), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(export, name), []byte("FROM alpine\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	mkdir("svc")
	write("Dockerfile")
	write(filepath.Join("svc", "Dockerfile.prod"))
	if err := os.Symlink("/", filepath.Join(export, "everything")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "Dockerfile"), []byte("FROM alpine\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(export, "elsewhere")); err != nil {
		t.Fatal(err)
	}

	if dir, err := buildDir(export, "", ""); err != nil || dir != mustEval(t, export) {
		t.Errorf(`buildDir(export, "", "") = %q, %v`, dir, err)
	}
	if dir, err := buildDir(export, "svc", "Dockerfile.prod"); err != nil ||
		dir != filepath.Join(mustEval(t, export), "svc") {
		t.Errorf("buildDir(export, svc) = %q, %v", dir, err)
	}

	for _, tc := range []struct {
		name, sub, dockerfile, want string
	}{
		{"a symlink to the root", "everything", "", "outside the commit"},
		{"a symlink to another directory", "elsewhere", "", "outside the commit"},
		{"a context the commit does not have", "nope", "", "is not in the commit"},
		{"a dockerfile the commit does not have", "svc", "Dockerfile.missing", "is not in the build context"},
		{"a dockerfile above the context", "svc", "../Dockerfile", "outside the build context"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildDir(export, tc.sub, tc.dockerfile); err == nil {
				t.Fatalf("buildDir accepted %q / %q", tc.sub, tc.dockerfile)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("buildDir = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func mustEval(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func TestExportOfAMissingCommit(t *testing.T) {
	h := newHarness(t)
	h.commit("first", goApp)
	envDir := filepath.Join(h.data, "apps", "shop", "envs", h.branch)
	_, cleanup, err := h.b.export(t.Context(), envDir, h.repo, strings.Repeat("f", 40), "ffffffffffff")
	defer cleanup()
	if err == nil {
		t.Fatal("a commit that is not there was exported")
	}
	if !strings.Contains(err.Error(), "git archive") {
		t.Errorf("export = %v", err)
	}

	if entries, err := os.ReadDir(filepath.Join(envDir, exportDirName)); err == nil && len(entries) != 0 {
		t.Errorf("the failed export left %v", names(entries))
	}
}
