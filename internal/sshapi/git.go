package sshapi

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/git"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/state"
)

const (
	verbUploadPack  = "upload-pack"
	verbReceivePack = "receive-pack"
)

type gitKind int

const (
	notGit gitKind = iota

	gitServe

	gitRefused
)

type gitRequest struct {
	Verb string

	Path string

	Refused string
}

func parseGit(args []string) (gitRequest, gitKind) {
	if len(args) == 0 {
		return gitRequest{}, notGit
	}
	var verb string
	var rest []string
	switch {
	case strings.HasPrefix(args[0], "git-"):
		verb, rest = strings.TrimPrefix(args[0], "git-"), args[1:]
	case args[0] == "git":
		if len(args) == 1 {
			return gitRequest{Refused: "git needs a subcommand; caramelod serves only git push and git fetch"}, gitRefused
		}
		verb, rest = args[1], args[2:]
	default:
		return gitRequest{}, notGit
	}
	if verb != verbUploadPack && verb != verbReceivePack {
		return gitRequest{Verb: verb, Refused: fmt.Sprintf(
			"git %s is not available here; caramelod serves only git push (receive-pack) and git fetch (upload-pack)", verb)}, gitRefused
	}

	var path string
	for _, a := range rest {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if path != "" {
			return gitRequest{Verb: verb, Refused: fmt.Sprintf("git %s takes one repository, got %d", verb, len(rest))}, gitRefused
		}
		path = a
	}
	if path == "" {
		return gitRequest{Verb: verb, Refused: fmt.Sprintf("git %s: no repository given; push to ssh://…/<app>", verb)}, gitRefused
	}
	return gitRequest{Verb: verb, Path: path}, gitServe
}

func appFromGitPath(path string) (string, error) {
	p := strings.TrimSpace(path)
	for _, q := range []string{"'", `"`} {
		if len(p) >= 2 && strings.HasPrefix(p, q) && strings.HasSuffix(p, q) {
			p = strings.TrimSpace(p[1 : len(p)-1])
		}
	}
	p = strings.TrimPrefix(p, "~/")
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")
	p = strings.TrimSuffix(p, ".git")
	if p == "" {
		return "", errors.New("no app in the repository path")
	}
	if err := env.ValidateName("app", p); err != nil {
		return "", err
	}
	return p, nil
}

type GitRequest struct {
	Verb   string
	Path   string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type GitService interface {
	ServeGit(ctx context.Context, req GitRequest) int
}

var _ GitService = (*Daemon)(nil)

func (d *Daemon) ServeGit(ctx context.Context, req GitRequest) int {
	app, err := appFromGitPath(req.Path)
	if err != nil {
		fmt.Fprintf(req.Stderr, "caramelo: %v\n", err)
		return 2
	}
	repo := env.RepoPath(d.Config.DataDir, app)

	switch req.Verb {
	case verbReceivePack:
		if err := d.ensureApp(ctx, app, repo); err != nil {
			fmt.Fprintf(req.Stderr, "caramelo: %v\n", err)
			return 1
		}
	case verbUploadPack:
		if _, err := d.Store.App(ctx, app); err != nil {
			if errors.Is(err, state.ErrNotFound) {
				fmt.Fprintf(req.Stderr, "caramelo: no such app %q; push to it first to create it\n", app)
				return 1
			}
			fmt.Fprintf(req.Stderr, "caramelo: read app %q: %v\n", app, err)
			return 1
		}
	default:
		fmt.Fprintf(req.Stderr, "caramelo: git %s is not available here\n", req.Verb)
		return 2
	}

	cmd := runner.Cmd{
		Name:   "git",
		Args:   []string{req.Verb, repo},
		User:   d.Config.User,
		Stdin:  req.Stdin,
		Stdout: req.Stdout,
		Stderr: req.Stderr,
	}
	var record string
	if req.Verb == verbReceivePack {
		record = filepath.Join(repo, git.PushRecordDir, rand.Text())
		cmd.Env = []string{git.PushRecordEnv + "=" + record}
		defer func() { _ = os.Remove(record) }()
	}

	res, err := d.Runner.Run(ctx, cmd)
	if err != nil {
		fmt.Fprintf(req.Stderr, "caramelo: run git %s: %v\n", req.Verb, err)
		return 1
	}
	if res.ExitCode != 0 {
		return res.ExitCode
	}
	if req.Verb == verbReceivePack {

		if err := d.settleHEAD(ctx, app, repo); err != nil {
			fmt.Fprintf(req.Stderr, "caramelo: warning: %v\n", err)
		}
		d.recordPush(ctx, app, record, req.Stderr)

		d.syncMembers(ctx, app, req.Stderr)
	}
	return 0
}

func (d *Daemon) syncMembers(ctx context.Context, app string, errw io.Writer) {
	if d.resolver() == nil || d.forwarder() == nil {
		return
	}
	rows, err := d.Store.Directory(ctx, state.DirectoryFilter{App: app})
	if err != nil {
		fmt.Fprintf(errw, "caramelo: warning: read the directory of %s: %v\n", app, err)
		return
	}
	self := d.fleetName()
	for _, r := range rows {
		if r.Machine == "" || r.Machine == self {
			continue
		}
		loc, err := d.resolver().ResolveMachine(ctx, r.Machine)
		if err != nil || loc.Local || !loc.Reachable {
			fmt.Fprintf(errw, "caramelo: warning: %s holds %s/%s and could not be told about the push\n",
				r.Machine, r.App, r.Env)
			continue
		}
		argv := []string{"env", "sync", r.Env, "--app", r.App, "--json"}
		if err := d.callMachine(ctx, loc, argv, nil, io.Discard); err != nil {
			fmt.Fprintf(errw, "caramelo: warning: update %s/%s on %s: %v\n", r.App, r.Env, r.Machine, err)
		}
	}
}

func (d *Daemon) ensureApp(ctx context.Context, app, repo string) error {
	_, err := d.Store.App(ctx, app)
	switch {
	case err == nil:
	case errors.Is(err, state.ErrNotFound):
		if addErr := d.Store.AddApp(ctx, state.App{Name: app, RepoPath: repo}); addErr != nil && !errors.Is(addErr, state.ErrExists) {
			return fmt.Errorf("record app %q: %w", app, addErr)
		}
	default:
		return fmt.Errorf("read app %q: %w", app, err)
	}

	if _, err := d.run(ctx, "install", "-d", "-m", "0750", env.AppDir(d.Config.DataDir, app)); err != nil {
		return fmt.Errorf("create the directory of app %q: %w", app, err)
	}
	res, err := d.git(ctx, "init", "--bare", "--quiet", repo)
	if err != nil {
		return fmt.Errorf("create repository for %q: %w", app, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("create repository for %q: git init exited %d: %s", app, res.ExitCode, firstLine(res.Stderr))
	}

	if _, err := d.repoDriver().EnsureUpdateInstead(ctx, repo); err != nil {
		return fmt.Errorf("configure repository for %q: %w", app, err)
	}

	if r, ok := d.repoDriver().(git.PreReceiver); ok {
		if _, err := r.EnsurePreReceive(ctx, repo); err != nil {
			d.logf("install the pre-receive hook in %s: %v", repo, err)
		}
	}

	if r, ok := d.repoDriver().(git.PostReceiver); ok {
		if _, err := r.EnsurePostReceive(ctx, repo); err != nil {
			d.logf("install the post-receive hook in %s: %v", repo, err)
		}
	}
	return nil
}

func (d *Daemon) repoDriver() git.PushUpdater {
	if d.EnvManager != nil {
		if u, ok := d.EnvManager.Git.(git.PushUpdater); ok {
			return u
		}
	}
	return git.NewCLI(d.Runner, d.Config.User)
}

func (d *Daemon) settleHEAD(ctx context.Context, app, repo string) error {
	head, err := d.headBranch(ctx, repo)
	if err != nil {
		return err
	}
	if head == "" {
		branches, err := d.branches(ctx, repo)
		if err != nil {
			return err
		}
		if len(branches) == 0 {
			return nil
		}
		head = branches[0]
		for _, b := range branches {
			if b == "main" {
				head = b
				break
			}
		}
		res, err := d.git(ctx, "-C", repo, "symbolic-ref", "HEAD", "refs/heads/"+head)
		if err != nil {
			return fmt.Errorf("point HEAD of %q at %s: %w", app, head, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("point HEAD of %q at %s: git exited %d: %s", app, head, res.ExitCode, firstLine(res.Stderr))
		}
	}
	if err := d.Store.SetAppDefaultBranch(ctx, app, head); err != nil && !errors.Is(err, state.ErrNotFound) {
		return fmt.Errorf("record default branch of %q: %w", app, err)
	}
	return nil
}

func (d *Daemon) headBranch(ctx context.Context, repo string) (string, error) {
	res, err := d.git(ctx, "-C", repo, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read HEAD: %w", err)
	}
	ref := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 || ref == "" {

		return "", nil
	}
	check, err := d.git(ctx, "-C", repo, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return "", fmt.Errorf("resolve HEAD: %w", err)
	}
	if check.ExitCode != 0 || strings.TrimSpace(check.Stdout) == "" {
		return "", nil
	}
	return strings.TrimPrefix(ref, "refs/heads/"), nil
}

func (d *Daemon) branches(ctx context.Context, repo string) ([]string, error) {
	res, err := d.git(ctx, "-C", repo, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("list branches: git exited %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	var out []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if b := strings.TrimSpace(line); b != "" {
			out = append(out, b)
		}
	}
	return out, nil
}

func (d *Daemon) git(ctx context.Context, args ...string) (runner.Result, error) {
	return d.run(ctx, "git", args...)
}

func (d *Daemon) run(ctx context.Context, name string, args ...string) (runner.Result, error) {
	if d.Runner == nil {
		return runner.Result{}, errors.New("no command runner configured")
	}
	res, err := d.Runner.Run(ctx, runner.Cmd{Name: name, Args: args, User: d.Config.User})
	if err != nil {
		return res, err
	}
	if name != "git" && res.ExitCode != 0 {
		return res, fmt.Errorf("%s exited %d: %s", name, res.ExitCode, firstLine(res.Stderr))
	}
	return res, nil
}
