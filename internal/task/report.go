package task

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/runner"
)

type Command struct {
	Kind       string `json:"kind"`
	Script     string `json:"script"`
	Exit       int    `json:"exit"`
	DurationMS int64  `json:"duration_ms"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
}

type Result struct {
	Name       string    `json:"name"`
	Desc       string    `json:"desc,omitempty"`
	Block      bool      `json:"block,omitempty"`
	Status     string    `json:"status"`
	Detail     string    `json:"detail,omitempty"`
	Error      string    `json:"error,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	Commands   []Command `json:"commands,omitempty"`
	Results    []Result  `json:"results,omitempty"`
}

type Report struct {
	RunID       string   `json:"run_id"`
	Task        string   `json:"task"`
	Machine     string   `json:"machine,omitempty"`
	Version     string   `json:"version"`
	DryRun      bool     `json:"dry_run"`
	Changed     int      `json:"changed"`
	WouldChange int      `json:"would_change,omitempty"`
	Failed      int      `json:"failed"`
	Skipped     int      `json:"skipped"`
	OK          int      `json:"ok"`
	NotRun      int      `json:"not_run,omitempty"`
	DurationMS  int64    `json:"duration_ms"`
	Results     []Result `json:"results"`

	Path string `json:"-"`

	ReportErr error `json:"-"`
}

func NewRunID(t time.Time) string { return t.UTC().Format("20060102T150405.000Z") }

func (r *Report) count() {
	r.Changed, r.WouldChange, r.Failed, r.Skipped, r.OK, r.NotRun = 0, 0, 0, 0, 0, 0
	var walk func(rs []Result)
	walk = func(rs []Result) {
		for _, res := range rs {
			if res.Block {
				walk(res.Results)
				continue
			}
			switch res.Status {
			case StatusOK:
				r.OK++
			case StatusChanged:
				r.Changed++
			case StatusWouldChange:
				r.WouldChange++
			case StatusSkipped:
				r.Skipped++
			case StatusFailed:
				r.Failed++
			case StatusNotRun:
				r.NotRun++
			}
		}
	}
	walk(r.Results)
}

func (r Report) FirstFailure() (Result, bool) {
	var found Result
	ok := false
	var walk func(rs []Result)
	walk = func(rs []Result) {
		for _, res := range rs {
			if ok {
				return
			}
			if res.Block {
				walk(res.Results)
				continue
			}
			if res.Status == StatusFailed {
				found, ok = res, true
				return
			}
		}
	}
	walk(r.Results)
	return found, ok
}

type ReportStore struct {
	Dir string

	User string

	Run runner.Runner
}

const (
	reportDirMode  = 0o700
	reportFileMode = 0o600
	serverDirMode  = "0750"
	serverFileMode = "0640"
)

func (s ReportStore) write(ctx context.Context, rep Report) (string, error) {
	if s.Dir == "" {
		return "", nil
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode the report of %s: %w", rep.Task, err)
	}
	path := filepath.Join(s.Dir, rep.RunID+".json")
	if s.User == "" {
		if err := os.MkdirAll(s.Dir, reportDirMode); err != nil {
			return "", fmt.Errorf("create %s: %w", s.Dir, err)
		}
		if err := os.Chmod(s.Dir, reportDirMode); err != nil {
			return "", fmt.Errorf("chmod %s: %w", s.Dir, err)
		}
		if err := os.WriteFile(path, append(b, '\n'), reportFileMode); err != nil {
			return "", fmt.Errorf("write %s: %w", path, err)
		}
		if err := os.Chmod(path, reportFileMode); err != nil {
			return "", fmt.Errorf("chmod %s: %w", path, err)
		}
		return path, nil
	}
	if s.Run == nil {
		return "", fmt.Errorf("write %s as %s: no runner", path, s.User)
	}
	for _, c := range []runner.Cmd{
		{Name: "install", Args: []string{"-d", "-m", serverDirMode, "--", s.Dir}, User: s.User},
		{Name: "tee", Args: []string{"--", path}, User: s.User, Stdin: strings.NewReader(string(b) + "\n")},
		{Name: "chmod", Args: []string{serverFileMode, "--", path}, User: s.User},
	} {
		res, err := s.Run.Run(ctx, c)
		if err != nil {
			return "", fmt.Errorf("write %s as %s: %w", path, s.User, err)
		}
		if res.ExitCode != 0 {
			return "", fmt.Errorf("write %s as %s: %s: exit %d: %s",
				path, s.User, c.Name, res.ExitCode, firstLine(res.Stderr))
		}
	}
	return path, nil
}

func firstLine(s string) string {
	s = strings.TrimRight(s, "\n")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
