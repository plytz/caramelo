package git

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/plytz/caramelo/internal/runner"
)

var ErrNotFound = errors.New("not found")

type Ref struct {
	Name   string `json:"name"`
	Commit string `json:"commit"`
}

type Repo interface {
	InitBare(ctx context.Context, path string) error

	Exists(ctx context.Context, path string) (bool, error)

	BranchExists(ctx context.Context, repo, branch string) (bool, error)

	CreateBranch(ctx context.Context, repo, branch, from string) error

	DeleteBranch(ctx context.Context, repo, branch string, force bool) error

	WorktreeAdd(ctx context.Context, repo, path, branch string) error

	WorktreeRemove(ctx context.Context, repo, path string, force bool) error

	WorktreePrune(ctx context.Context, repo string) error

	RevParse(ctx context.Context, repo, ref string) (string, error)

	SymbolicRefHEAD(ctx context.Context, repo string) (string, error)

	SetHEAD(ctx context.Context, repo, branch string) error

	Branches(ctx context.Context, repo string) ([]string, error)

	Refs(ctx context.Context, repo string) ([]Ref, error)
}

type Sizer interface {
	RepoSize(ctx context.Context, repo string) (int64, error)
}

type CLI struct {
	Runner runner.Runner

	User string

	Env []string
}

func NewCLI(r runner.Runner, user string) *CLI { return &CLI{Runner: r, User: user} }

const TunnelSSHCommand = "ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"

func (c *CLI) env() []string {
	if len(c.Env) == 0 {
		return nil
	}
	return append([]string(nil), c.Env...)
}

var (
	_ Repo  = (*CLI)(nil)
	_ Sizer = (*CLI)(nil)
)

func (c *CLI) InitBare(ctx context.Context, path string) error {
	if path == "" {
		return fmt.Errorf("git init: no path given")
	}
	if _, err := c.runOK(ctx, "init", "--bare", path); err != nil {
		return err
	}
	if _, err := c.EnsureUpdateInstead(ctx, path); err != nil {
		return err
	}
	return nil
}

func (c *CLI) Exists(ctx context.Context, path string) (bool, error) {
	res, err := c.run(ctx, "rev-parse", "--resolve-git-dir", path)
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

func (c *CLI) BranchExists(ctx context.Context, repo, branch string) (bool, error) {
	args := c.in(repo, "show-ref", "--verify", "--quiet", refName(branch))
	res, err := c.run(ctx, args...)
	if err != nil {
		return false, err
	}
	switch res.ExitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, cmdErr(args, res)
	}
}

func (c *CLI) CreateBranch(ctx context.Context, repo, branch, from string) error {
	if from == "" {
		return fmt.Errorf("git branch %s: no starting point given", branch)
	}
	_, err := c.runOK(ctx, c.in(repo, "branch", branch, from)...)
	return err
}

func (c *CLI) DeleteBranch(ctx context.Context, repo, branch string, force bool) error {
	flag := "--delete"
	if force {
		flag = "-D"
	}
	args := c.in(repo, "branch", flag, branch)
	res, err := c.run(ctx, args...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 && !isNotFound(res) {
		return cmdErr(args, res)
	}
	return nil
}

func (c *CLI) WorktreeAdd(ctx context.Context, repo, path, branch string) error {
	_, err := c.runOK(ctx, c.in(repo, "worktree", "add", path, branch)...)
	return err
}

func (c *CLI) WorktreeRemove(ctx context.Context, repo, path string, force bool) error {
	args := c.in(repo, "worktree", "remove")
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	res, err := c.run(ctx, args...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 && !isNotAWorktree(res) {
		return cmdErr(args, res)
	}
	return nil
}

func (c *CLI) WorktreePrune(ctx context.Context, repo string) error {
	_, err := c.runOK(ctx, c.in(repo, "worktree", "prune")...)
	return err
}

func (c *CLI) RevParse(ctx context.Context, repo, ref string) (string, error) {
	args := c.in(repo, "rev-parse", "--verify", "--end-of-options", ref)
	res, err := c.run(ctx, args...)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		if isUnknownRevision(res) {
			return "", fmt.Errorf("%s: %w", ref, ErrNotFound)
		}
		return "", cmdErr(args, res)
	}
	return strings.TrimSpace(res.Stdout), nil
}

func (c *CLI) SymbolicRefHEAD(ctx context.Context, repo string) (string, error) {
	args := c.in(repo, "symbolic-ref", "HEAD")
	res, err := c.run(ctx, args...)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {

		return "", fmt.Errorf("HEAD of %s: %w", repo, ErrNotFound)
	}
	head := strings.TrimSpace(res.Stdout)
	if head == "" {
		return "", fmt.Errorf("HEAD of %s: %w", repo, ErrNotFound)
	}
	exists, err := c.BranchExists(ctx, repo, head)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("HEAD of %s points at %s, which does not exist: %w", repo, head, ErrNotFound)
	}
	return head, nil
}

func (c *CLI) SetHEAD(ctx context.Context, repo, branch string) error {
	if branch == "" {
		return fmt.Errorf("git symbolic-ref HEAD: no branch given")
	}
	_, err := c.runOK(ctx, c.in(repo, "symbolic-ref", "HEAD", refName(branch))...)
	return err
}

func (c *CLI) Branches(ctx context.Context, repo string) ([]string, error) {
	res, err := c.runOK(ctx, c.in(repo, "for-each-ref", "--format=%(refname:short)", "refs/heads")...)
	if err != nil {
		return nil, err
	}
	out := lines(res.Stdout)
	sort.Strings(out)
	return out, nil
}

func (c *CLI) Refs(ctx context.Context, repo string) ([]Ref, error) {
	res, err := c.runOK(ctx, c.in(repo, "for-each-ref", "--format=%(objectname) %(refname)")...)
	if err != nil {
		return nil, err
	}
	var refs []Ref
	for _, line := range lines(res.Stdout) {
		commit, name, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		refs = append(refs, Ref{Name: strings.TrimSpace(name), Commit: commit})
	}
	return refs, nil
}

func (c *CLI) RepoSize(ctx context.Context, repo string) (int64, error) {
	if c.Runner == nil {
		return 0, fmt.Errorf("du -sk %s: no command runner configured", repo)
	}
	res, err := c.Runner.Run(ctx, runner.Cmd{Name: "du", Args: []string{"-sk", repo}, User: c.User})
	if err != nil {
		return 0, fmt.Errorf("du -sk %s: %w", repo, err)
	}
	if res.ExitCode != 0 {
		return 0, cmdErr([]string{"du", "-sk", repo}, res)
	}
	field, _, _ := strings.Cut(strings.TrimSpace(res.Stdout), "\t")
	kb, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("du -sk %s: cannot read %q: %w", repo, firstLine(res.Stdout), err)
	}
	return kb * 1024, nil
}

func (c *CLI) in(repo string, args ...string) []string {
	return append([]string{"-C", repo}, args...)
}

func (c *CLI) run(ctx context.Context, args ...string) (runner.Result, error) {
	if c.Runner == nil {
		return runner.Result{}, fmt.Errorf("git %s: no command runner configured", strings.Join(args, " "))
	}
	res, err := c.Runner.Run(ctx, runner.Cmd{Name: "git", Args: args, User: c.User, Env: c.env()})
	if err != nil {
		return res, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return res, nil
}

func (c *CLI) runOK(ctx context.Context, args ...string) (runner.Result, error) {
	res, err := c.run(ctx, args...)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, cmdErr(args, res)
	}
	return res, nil
}

func cmdErr(args []string, res runner.Result) error {
	msg := firstLine(res.Stderr)
	if msg == "" {
		msg = firstLine(res.Stdout)
	}
	if msg == "" {
		return fmt.Errorf("git %s: exit %d", strings.Join(args, " "), res.ExitCode)
	}
	return fmt.Errorf("git %s: exit %d: %s", strings.Join(args, " "), res.ExitCode, msg)
}

func refName(branch string) string {
	if strings.HasPrefix(branch, "refs/") {
		return branch
	}
	return "refs/heads/" + branch
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func isNotFound(res runner.Result) bool {
	return strings.Contains(strings.ToLower(res.Stderr), "not found")
}

func isNotAWorktree(res runner.Result) bool {
	s := strings.ToLower(res.Stderr)
	return strings.Contains(s, "is not a working tree") || strings.Contains(s, "no such file or directory")
}

func isUnknownRevision(res runner.Result) bool {
	s := strings.ToLower(res.Stderr)
	for _, m := range []string{
		"needed a single revision",
		"unknown revision",
		"not a valid object name",
		"ambiguous argument",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
