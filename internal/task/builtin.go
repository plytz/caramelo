package task

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

type builtin interface {
	check() (done bool, detail string, err error)
	apply() (detail string, err error)
}

func builtinOf(it *Item) builtin {
	switch {
	case it.Dir != nil:
		return dirTarget{*it.Dir}
	case it.File != nil:
		return fileTarget{*it.File}
	}
	return nil
}

type dirTarget struct{ spec DirSpec }

func (d dirTarget) check() (bool, string, error) {
	mode, err := modeOf(d.spec.Mode, 0o755)
	if err != nil {
		return false, "", err
	}
	fi, missing, err := statDir(d.spec.Path)
	switch {
	case err != nil:
		return false, "", err
	case missing:
		return false, d.spec.Path + " missing", nil
	case d.spec.Mode != "" && fi.Mode().Perm() != mode:
		return false, fmt.Sprintf("mode %s, want %s", octal(fi.Mode().Perm()), octal(mode)), nil
	}
	ok, detail, err := ownershipOK(d.spec.Path, d.spec.Owner, d.spec.Group)
	if err != nil {
		return false, "", err
	}
	return ok, detail, nil
}

func (d dirTarget) apply() (string, error) {
	mode, err := modeOf(d.spec.Mode, 0o755)
	if err != nil {
		return "", err
	}
	created := false
	_, missing, err := statDir(d.spec.Path)
	switch {
	case err != nil:
		return "", err
	case missing:
		if err := os.MkdirAll(d.spec.Path, mode); err != nil {
			return "", fmt.Errorf("create %s: %w", d.spec.Path, err)
		}
		created = true
	}
	if d.spec.Mode != "" {
		if err := os.Chmod(d.spec.Path, mode); err != nil {
			return "", fmt.Errorf("chmod %s: %w", d.spec.Path, err)
		}
	}
	note, err := setOwnership(d.spec.Path, d.spec.Owner, d.spec.Group)
	if err != nil {
		return "", err
	}
	detail := "mode " + octal(mode)
	if created {
		detail = "created, " + octal(mode)
	}
	if d.spec.Mode == "" {
		detail = d.spec.Path
		if created {
			detail = "created"
		}
	}
	return join(detail, note), nil
}

func statDir(path string) (fs.FileInfo, bool, error) {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, true, nil
	case err != nil:
		return nil, false, fmt.Errorf("stat %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		fi, err = os.Stat(path)
		if err != nil {
			return nil, false, fmt.Errorf("%s is a symlink that leads nowhere: %w", path, err)
		}
	}
	if !fi.IsDir() {
		return nil, false, fmt.Errorf("%s is not a directory", path)
	}
	return fi, false, nil
}

type fileTarget struct{ spec FileSpec }

func (f fileTarget) check() (bool, string, error) {
	mode, err := modeOf(f.spec.Mode, 0o644)
	if err != nil {
		return false, "", err
	}
	fi, err := os.Lstat(f.spec.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, f.spec.Path + " missing", nil
	case err != nil:
		return false, "", fmt.Errorf("stat %s: %w", f.spec.Path, err)
	case fi.Mode()&os.ModeSymlink != 0:
		return false, "", fmt.Errorf("%s is a symlink: a task never writes through a link", f.spec.Path)
	case fi.IsDir():
		return false, "", fmt.Errorf("%s is a directory", f.spec.Path)
	}
	b, err := os.ReadFile(f.spec.Path)
	if err != nil {
		return false, "", fmt.Errorf("read %s: %w", f.spec.Path, err)
	}
	if string(b) != f.spec.Content {
		return false, "content differs", nil
	}
	if f.spec.Mode != "" && fi.Mode().Perm() != mode {
		return false, fmt.Sprintf("mode %s, want %s", octal(fi.Mode().Perm()), octal(mode)), nil
	}
	ok, detail, err := ownershipOK(f.spec.Path, f.spec.Owner, f.spec.Group)
	if err != nil {
		return false, "", err
	}
	return ok, detail, nil
}

func (f fileTarget) apply() (string, error) {
	mode, err := modeOf(f.spec.Mode, 0o644)
	if err != nil {
		return "", err
	}
	created := false
	fi, err := os.Lstat(f.spec.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		created = true
	case err != nil:
		return "", fmt.Errorf("stat %s: %w", f.spec.Path, err)
	case fi.Mode()&os.ModeSymlink != 0:
		return "", fmt.Errorf("%s is a symlink: a task never writes through a link", f.spec.Path)
	case fi.IsDir():
		return "", fmt.Errorf("%s is a directory", f.spec.Path)
	}
	dir := filepath.Dir(f.spec.Path)
	tmp, err := os.CreateTemp(dir, ".caramelo-task-")
	if err != nil {
		return "", fmt.Errorf("write %s: %w", f.spec.Path, err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(f.spec.Content); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write %s: %w", f.spec.Path, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("write %s: %w", f.spec.Path, err)
	}
	if err := os.Chmod(name, mode); err != nil {
		return "", fmt.Errorf("chmod %s: %w", f.spec.Path, err)
	}
	if err := os.Rename(name, f.spec.Path); err != nil {
		return "", fmt.Errorf("rename %s: %w", f.spec.Path, err)
	}
	note, err := setOwnership(f.spec.Path, f.spec.Owner, f.spec.Group)
	if err != nil {
		return "", err
	}
	detail := "written, " + octal(mode)
	if created {
		detail = "created, " + octal(mode)
	}
	return join(detail, note), nil
}

const notRootNote = "owner and group left alone: not root"

func ownershipOK(path, owner, group string) (bool, string, error) {
	if owner == "" && group == "" {
		return true, "", nil
	}
	if os.Geteuid() != 0 {
		return true, notRootNote, nil
	}
	uid, gid, err := lookupIDs(owner, group)
	if err != nil {
		return false, "", err
	}
	cur, curGid, err := ownerOf(path)
	if err != nil {
		return false, "", err
	}
	if (uid >= 0 && cur != uid) || (gid >= 0 && curGid != gid) {
		return false, "owner or group differs", nil
	}
	return true, "", nil
}

func setOwnership(path, owner, group string) (string, error) {
	if owner == "" && group == "" {
		return "", nil
	}
	if os.Geteuid() != 0 {
		return notRootNote, nil
	}
	uid, gid, err := lookupIDs(owner, group)
	if err != nil {
		return "", err
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return "", fmt.Errorf("chown %s: %w", path, err)
	}
	return "", nil
}

func lookupIDs(owner, group string) (int, int, error) {
	uid, gid := -1, -1
	if owner != "" {
		u, err := user.Lookup(owner)
		if err != nil {
			return 0, 0, fmt.Errorf("look up the user %q: %w", owner, err)
		}
		n, err := strconv.Atoi(u.Uid)
		if err != nil {
			return 0, 0, fmt.Errorf("the user %q has no numeric id: %w", owner, err)
		}
		uid = n
	}
	if group != "" {
		g, err := user.LookupGroup(group)
		if err != nil {
			return 0, 0, fmt.Errorf("look up the group %q: %w", group, err)
		}
		n, err := strconv.Atoi(g.Gid)
		if err != nil {
			return 0, 0, fmt.Errorf("the group %q has no numeric id: %w", group, err)
		}
		gid = n
	}
	return uid, gid, nil
}

func modeOf(s string, fallback os.FileMode) (os.FileMode, error) {
	if s == "" {
		return fallback, nil
	}
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("mode %q: a mode is three or four octal digits, e.g. 0700", s)
	}
	if n > 0o777 {
		return 0, fmt.Errorf("mode %q: a task's mode is permissions only, and %q asks for a setuid, setgid or sticky bit", s, s[:1])
	}
	return os.FileMode(n), nil
}

func octal(m os.FileMode) string { return fmt.Sprintf("0%03o", m.Perm()) }

func join(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}
