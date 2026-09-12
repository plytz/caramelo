package git

import (
	"context"
	"fmt"
	"strings"
)

const (
	MirrorRemote = "origin"

	PreReceiveHookName = "hooks/pre-receive"

	PushCheckCommand = "caramelo push-check"

	PushRefusedCode = 3
)

const PreReceiveHook = `#!/bin/sh
# Installed and kept up to date by caramelo. Edits are overwritten.
#
# Asks caramelod whether these refs may land: a branch checked out in an
# environment on *another* machine of the fleet is refused while that worktree
# has uncommitted work, which is the rule git itself enforces for the worktrees
# on this machine (receive.denyCurrentBranch = updateInstead).
#
# The repository is $GIT_DIR; the refs arrive on standard input.
#
# The binary is looked for on PATH and then at the path setup installs it at,
# because a hook runs in whatever environment git was given and that is not a
# login shell's: on a machine where /usr/local/bin is not on it, the check
# would be skipped silently on every push, which is the one way a check can be
# worse than none.
set -e
caramelo=$(command -v caramelo 2>/dev/null || true)
if [ -z "$caramelo" ] && [ -x /usr/local/bin/caramelo ]; then
	caramelo=/usr/local/bin/caramelo
fi
if [ -z "$caramelo" ]; then
	exit 0
fi
# The repository as an absolute path. git sets GIT_DIR to "." and runs the hook
# with the repository as the working directory, and the command is answered by
# caramelod — another process, with another working directory — so a dot would
# reach it meaning the daemon's own directory and the check would pass without
# asking anybody.
repo=$(cd "${GIT_DIR:-$PWD}" 2>/dev/null && pwd) || repo="${GIT_DIR:-$PWD}"
refs=$(cat)
status=0
printf '%s' "$refs" | "$caramelo" push-check --repo "$repo" || status=$?
# Only this one code refuses. Anything else — a caramelo too old to know the
# verb, a daemon that is restarting, a socket that is busy — lets the push land.
# The "|| status=$?" is what keeps set -e from deciding that for us.
if [ "$status" -eq 3 ]; then
	exit 1
fi
exit 0
`

type PreReceiver interface {
	EnsurePreReceive(ctx context.Context, repo string) (bool, error)
}

type Mirrorer interface {
	EnsureMirror(ctx context.Context, repo, url string) (bool, error)

	FetchBranch(ctx context.Context, repo, branch string) error

	FetchMirror(ctx context.Context, repo string) error

	UpdateWorktree(ctx context.Context, repo, path, branch string) error
}

var (
	_ PreReceiver = (*CLI)(nil)
	_ Mirrorer    = (*CLI)(nil)
)

func (c *CLI) EnsurePreReceive(ctx context.Context, repo string) (bool, error) {
	if repo == "" {
		return false, fmt.Errorf("install the pre-receive hook: no repository given")
	}
	return c.ensureHook(ctx, repo, PreReceiveHookName, PreReceiveHook)
}

func (c *CLI) EnsureMirror(ctx context.Context, repo, url string) (bool, error) {
	if repo == "" || url == "" {
		return false, fmt.Errorf("mirror %q: a repository and a URL are both needed", repo)
	}
	res, err := c.run(ctx, c.in(repo, "remote", "get-url", MirrorRemote)...)
	if err != nil {
		return false, err
	}
	if res.ExitCode == 0 {
		if strings.TrimSpace(res.Stdout) == url {
			return false, nil
		}
		if _, err := c.runOK(ctx, c.in(repo, "remote", "set-url", MirrorRemote, url)...); err != nil {
			return false, err
		}
		return true, c.setMirrorFetch(ctx, repo)
	}
	if _, err := c.runOK(ctx, c.in(repo, "remote", "add", MirrorRemote, url)...); err != nil {
		return false, err
	}
	return true, c.setMirrorFetch(ctx, repo)
}

func (c *CLI) setMirrorFetch(ctx context.Context, repo string) error {
	key := "remote." + MirrorRemote + ".fetch"
	_, err := c.runOK(ctx, c.in(repo, "config", key, mirrorRefspec)...)
	return err
}

const mirrorRefspec = "+refs/heads/*:refs/remotes/" + MirrorRemote + "/*"

func remoteRef(branch string) string { return "refs/remotes/" + MirrorRemote + "/" + branch }

func (c *CLI) FetchBranch(ctx context.Context, repo, branch string) error {
	if branch == "" {
		return fmt.Errorf("fetch from %s: no branch given", MirrorRemote)
	}
	spec := "+" + refName(branch) + ":" + remoteRef(branch)
	args := c.in(repo, "fetch", "--no-tags", MirrorRemote, spec)
	res, err := c.run(ctx, args...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		if isNotFound(res) || strings.Contains(res.Stderr, "couldn't find remote ref") {
			return fmt.Errorf("fetch branch %s from %s: %w", branch, MirrorRemote, ErrNotFound)
		}
		return cmdErr(args, res)
	}
	return nil
}

func (c *CLI) FetchMirror(ctx context.Context, repo string) error {
	_, err := c.runOK(ctx, c.in(repo, "fetch", "--no-tags", MirrorRemote)...)
	return err
}

func (c *CLI) Dirty(ctx context.Context, worktree string) (bool, []string, error) {
	if worktree == "" {
		return false, nil, fmt.Errorf("git status: no worktree given")
	}
	args := []string{"-C", worktree, "status", "--porcelain", "--untracked-files=no"}
	res, err := c.run(ctx, args...)
	if err != nil {
		return false, nil, err
	}
	if res.ExitCode != 0 {
		return false, nil, cmdErr(args, res)
	}
	changed := lines(res.Stdout)
	return len(changed) > 0, changed, nil
}

func MirrorURL(hubAddr, app string) string {
	return "ssh://caramelo@" + hubAddr + "/" + app
}

func (c *CLI) UpdateWorktree(ctx context.Context, repo, path, branch string) error {
	dirty, changed, err := c.Dirty(ctx, path)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("the checkout at %s has uncommitted work (%s): "+
			"commit or stash it there before pushing", path, strings.Join(changed, ", "))
	}
	args := c.in(path, "checkout", "--quiet", "--force", "-B", branch, remoteRef(branch))
	res, err := c.run(ctx, args...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return cmdErr(args, res)
	}
	return nil
}
