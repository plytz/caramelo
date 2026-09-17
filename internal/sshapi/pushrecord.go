package sshapi

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	pushSourceOption = "caramelo.branch"

	pushSourceMax = 255

	zeroSHA = "0000000000000000000000000000000000000000"
)

type pushedRef struct {
	Branch string
	Commit string
}

type pushRecord struct {
	Refs   []pushedRef
	Source string
}

func parsePushRecord(b []byte) pushRecord {
	var rec pushRecord
	var source string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if rest, ok := strings.CutPrefix(line, "option "); ok {
			if key, value, cut := strings.Cut(rest, "="); cut && key == pushSourceOption {
				source = value
			}
			continue
		}
		if !strings.HasPrefix(line, "ref ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 4 || !isSHA(fields[1]) || !isSHA(fields[2]) || fields[2] == zeroSHA {
			continue
		}
		branch, ok := strings.CutPrefix(fields[3], "refs/heads/")
		if !ok || branch == "" {
			continue
		}
		rec.Refs = append(rec.Refs, pushedRef{Branch: branch, Commit: fields[2]})
	}
	rec.Source = sanitizeSource(source)
	return rec
}

func isSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

func sanitizeSource(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > pushSourceMax || strings.HasPrefix(v, "-") || strings.Contains(v, "..") {
		return ""
	}
	for _, c := range v {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.' || c == '/':
		default:
			return ""
		}
	}
	return v
}

func (d *Daemon) recordPush(ctx context.Context, app, path string, errw io.Writer) {
	if d.EnvManager == nil {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	rec := parsePushRecord(b)
	if len(rec.Refs) == 0 {
		return
	}
	sess, _ := SessionFrom(ctx)
	identity := sess.Author()
	for _, r := range rec.Refs {
		if _, err := d.EnvManager.RecordPush(ctx, app, r.Branch, r.Commit, rec.Source, identity); err != nil {
			fmt.Fprintf(errw, "caramelo: warning: record the push into %s/%s: %v\n", app, r.Branch, err)
		}
	}
}
